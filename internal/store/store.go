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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrateLockID serializes schema migrations across replicas.
const migrateLockID = 727070

// Store is the job database. It keeps two connection pools: the main one,
// for everything else, and a small one for job heartbeats and readiness
// pings only, so page traffic that takes every main connection can't make
// a running job look dead or a healthy replica look unready. A third,
// single connection holds the process's slots (see AcquireSlot), and each
// listener has one of its own (see Notifications).
type Store struct {
	pool     *pgxpool.Pool // everything but heartbeats and pings
	liveness *pgxpool.Pool // Heartbeat and Ping only, so a busy main pool can't delay them
	slots    *slots        // AcquireSlot's own connection
	// connCfg opens connections outside the pools: each listener's
	// (Notifications), so none holds up Close, and Migrate's.
	connCfg *pgx.ConnConfig
}

const (
	// DefaultMaxConns is the main pool's size when Options.MaxConns is 0.
	DefaultMaxConns = 16
	// LivenessConns is the size of the pool for heartbeats and pings. It
	// is on top of the main pool's.
	LivenessConns = 2
)

// Options tune Open. Zero values take the defaults.
type Options struct {
	// MaxConns is the most connections the main pool opens (database.
	// max_conns); default DefaultMaxConns. The store opens up to
	// LivenessConns more for heartbeats and pings, one for slots, and one
	// per listener (Notifications).
	MaxConns int
}

// sessionParams bound how long any session waits, so one dead or hung
// connection can't stall every replica: a transaction left idle is aborted,
// lock waits give up, and the server notices a vanished client in about a
// minute instead of two hours.
var sessionParams = map[string]string{
	"idle_in_transaction_session_timeout": "30s",
	"lock_timeout":                        "10s",
	"tcp_keepalives_idle":                 "30",
	"tcp_keepalives_interval":             "10",
	"tcp_keepalives_count":                "3",
}

// Open connects to Postgres with a main pool of opts.MaxConns connections
// and a pool of LivenessConns for heartbeats and pings. Call Migrate before
// using the store.
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
	// The URL may set pool_min_conns; the liveness pool opens its two
	// connections only as needed.
	lcfg := cfg.Copy()
	lcfg.MaxConns = LivenessConns
	lcfg.MinConns = 0
	lcfg.MinIdleConns = 0
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
	conn := cfg.ConnConfig.Copy() // read-only from here: both connect with it as is
	s := &Store{pool: pool, liveness: liveness, slots: &slots{cfg: conn}, connCfg: conn}
	if err := pool.Ping(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	return s, nil
}

// Close closes both pools and the slot session.
func (s *Store) Close() {
	s.pool.Close()
	s.liveness.Close()
	s.slots.close()
}

// Ping checks that the database answers, for readiness checks, on the
// liveness pool.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.liveness.Ping(ctx); err != nil {
		return fmt.Errorf("pinging database: %w", err)
	}
	return nil
}

// Migrate applies embedded migrations that haven't run yet. Replicas starting
// together take turns through an advisory lock, so each migration runs once.
func (s *Store) Migrate(ctx context.Context) error {
	// A connection of its own, whose lock_timeout is off: wait as long as
	// another replica's migration takes, and let migrations' DDL wait for
	// running transactions, instead of giving up at the pool's 10s. Closing
	// it also ends the advisory lock.
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
