// Package store keeps jobs, their items and their progress events in
// Postgres, and coordinates workers: claiming, heartbeats, locks and
// cancellation. It also keeps the web app's login sessions.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrateLockID serializes schema migrations across replicas.
const migrateLockID = 727070

// Store is the job database. Heartbeats and pings each have their own small
// pool, so neither page traffic nor a flood of pings can make a job look dead
// or a replica unready. Slots (AcquireSlot) and each listener (Notifications)
// get their own connection.
type Store struct {
	pool     *pgxpool.Pool // everything but heartbeats and pings
	liveness *pgxpool.Pool // Heartbeat only
	ping     *pgxpool.Pool // Ping only, one connection
	slots    *slots        // AcquireSlot's own connection
	// connCfg opens non-pool connections (listeners, Migrate), so listeners
	// don't hold up Close.
	connCfg    *pgx.ConnConfig
	pings      singleflight.Group
	beforePing func() // test hook, run while the shared ping holds its connection
}

const (
	// DefaultMaxConns is the main pool's size when Options.MaxConns is 0.
	DefaultMaxConns = 16
	// LivenessConns sizes the heartbeat pool, on top of the main pool.
	LivenessConns = 2
)

// Options tune Open. Zero values take the defaults.
type Options struct {
	// MaxConns sizes the main pool (database.max_conns). The store also
	// opens LivenessConns, one ping connection, one slot connection and
	// one per listener.
	MaxConns int
}

// sessionParams bound every session's waits so one hung connection can't
// stall every replica; the keepalives make the server notice a vanished
// client in about a minute instead of two hours.
var sessionParams = map[string]string{
	"idle_in_transaction_session_timeout": "30s",
	"lock_timeout":                        "10s",
	"tcp_keepalives_idle":                 "30",
	"tcp_keepalives_interval":             "10",
	"tcp_keepalives_count":                "3",
}

// Open connects to Postgres. Call Migrate before using the store.
func Open(ctx context.Context, url string, opts Options) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// The parse error quotes the URL; report only the reason.
		reason := "invalid database URL"
		if inner := errors.Unwrap(err); inner != nil {
			reason += ": " + inner.Error()
		}
		return nil, fmt.Errorf("connecting to database: %s", reason)
	}
	for k, v := range sessionParams {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	maxConns := opts.MaxConns
	if maxConns <= 0 {
		maxConns = DefaultMaxConns
	}
	cfg.MaxConns = int32(maxConns)
	// Ignore any pool_min_conns from the URL for the liveness and ping pools.
	lcfg := cfg.Copy()
	lcfg.MaxConns = LivenessConns
	lcfg.MinConns = 0
	lcfg.MinIdleConns = 0
	pcfg := lcfg.Copy()
	pcfg.MaxConns = 1
	if cfg.MinConns > cfg.MaxConns {
		cfg.MinConns = cfg.MaxConns
	}
	if cfg.MinIdleConns > cfg.MaxConns {
		cfg.MinIdleConns = cfg.MaxConns
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	liveness, err := pgxpool.NewWithConfig(ctx, lcfg)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	ping, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		pool.Close()
		liveness.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	conn := cfg.ConnConfig.Copy() // read-only from here: both connect with it as is
	s := &Store{pool: pool, liveness: liveness, ping: ping, slots: &slots{cfg: conn}, connCfg: conn}
	if err := pool.Ping(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	return s, nil
}

// Close closes the pools and the slot session.
func (s *Store) Close() {
	s.pool.Close()
	s.liveness.Close()
	s.ping.Close()
	s.slots.close()
}

// pingHold bounds the ping concurrent Ping calls share, which no caller's
// cancellation ends.
const pingHold = 5 * time.Second

// Ping checks the database answers, on the ping pool (readiness).
// Concurrent calls share one ping, so a flood doesn't queue for the one
// connection.
func (s *Store) Ping(ctx context.Context) error {
	ch := s.pings.DoChan("ping", func() (any, error) {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pingHold)
		defer cancel()
		c, err := s.ping.Acquire(pctx)
		if err != nil {
			return nil, err
		}
		defer c.Release()
		if s.beforePing != nil {
			s.beforePing()
		}
		return nil, c.Ping(pctx)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return fmt.Errorf("pinging database: %w", r.Err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("pinging database: %w", ctx.Err())
	}
}

// Migrate applies pending embedded migrations, once across replicas
// (advisory lock).
func (s *Store) Migrate(ctx context.Context) error {
	// Own connection without lock_timeout, so it waits out another replica's
	// migration and DDL waits for running transactions. Closing it releases
	// the advisory lock.
	cfg := s.connCfg.Copy()
	cfg.RuntimeParams["lock_timeout"] = "0"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockID); err != nil {
		return fmt.Errorf("locking migrations: %w", err)
	}

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS battleship_schema_migrations (
		version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		version, err := migrationVersion(f)
		if err != nil {
			return err
		}
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM battleship_schema_migrations WHERE version = $1)`, version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return fmt.Errorf("migration %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO battleship_schema_migrations (version) VALUES ($1)`, version); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// migrationVersion reads the number prefix of "migrations/001_jobs.sql".
func migrationVersion(path string) (int, error) {
	base := path[strings.LastIndex(path, "/")+1:]
	num, _, _ := strings.Cut(base, "_")
	v, err := strconv.Atoi(num)
	if err != nil {
		return 0, fmt.Errorf("migration %s: name must start with a number", path)
	}
	return v, nil
}
