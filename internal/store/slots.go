package store

import (
	"context"
	"errors"
	"hash/fnv"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// slotPoll is roughly how often AcquireSlot retries while all slots are taken.
const slotPoll = time.Second

// slots holds this process's slots as session advisory locks on one
// connection. Postgres re-grants a lock a session already holds, so held
// stops this process taking a slot twice. gen counts sessions, so a release
// from a dead session can't free the slot retaken on its replacement.
type slots struct {
	cfg    *pgx.ConnConfig
	mu     sync.Mutex // guards the fields below; never held while connecting
	conn   *pgx.Conn
	gen    uint64
	held   map[[2]int32]bool
	closed bool
}

// slotQueryTimeout bounds each lock/unlock query on the shared session.
const slotQueryTimeout = 10 * time.Second

// AcquireSlot takes one of n database-wide slots called name, waiting until
// one is free or ctx ends. release is idempotent. If the slot session dies
// its slots free at once, even while holders still work.
//
// n is per caller: callers passing different n for one name run up to the
// largest n at once.
func (s *Store) AcquireSlot(ctx context.Context, name string, n int) (release func(), err error) {
	h := fnv.New32a()
	h.Write([]byte(name))
	key := int32(h.Sum32())
	for {
		for i := range int32(max(1, n)) {
			gen, ok, err := s.slots.tryLock(ctx, key, i)
			if err != nil {
				return nil, err
			}
			if ok {
				var once sync.Once
				return func() { once.Do(func() { s.slots.unlock(key, i, gen) }) }, nil
			}
		}
		wait := slotPoll/2 + rand.N(slotPoll)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// tryLock takes slot of key on the slot session if it is free, and reports
// the session's gen. A session found dead is replaced once.
func (l *slots) tryLock(ctx context.Context, key, slot int32) (gen uint64, ok bool, err error) {
	k := [2]int32{key, slot}
	for fresh := false; ; {
		l.mu.Lock()
		if l.conn != nil && l.conn.IsClosed() {
			l.dropLocked()
		}
		if l.conn == nil {
			l.mu.Unlock()
			if fresh {
				return 0, false, errors.New("slot session lost while connecting")
			}
			if err := l.connect(ctx); err != nil {
				return 0, false, err
			}
			fresh = true
			continue
		}
		if l.held[k] {
			l.mu.Unlock()
			return 0, false, nil
		}
		qctx, cancel := context.WithTimeout(ctx, slotQueryTimeout)
		err := l.conn.QueryRow(qctx, `SELECT pg_try_advisory_lock($1, $2)`, key, slot).Scan(&ok)
		cancel()
		if err != nil {
			l.dropLocked()
			l.mu.Unlock()
			if fresh || ctx.Err() != nil {
				return 0, false, err
			}
			continue
		}
		if ok {
			l.held[k] = true
		}
		gen = l.gen
		l.mu.Unlock()
		return gen, ok, nil
	}
}

// connect opens a new slot session unless another caller already did.
func (l *slots) connect(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, l.cfg)
	if err != nil {
		return err
	}
	l.mu.Lock()
	installed := l.conn == nil && !l.closed
	if installed {
		l.conn, l.held = conn, map[[2]int32]bool{}
		l.gen++
	}
	closed := l.closed
	l.mu.Unlock()
	if !installed {
		closeConn(conn)
	}
	if closed {
		return errors.New("store closed")
	}
	return nil
}

func (l *slots) unlock(key, slot int32, gen uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := [2]int32{key, slot}
	if gen != l.gen || !l.held[k] {
		return // the session that held it died
	}
	delete(l.held, k)
	ctx, cancel := context.WithTimeout(context.Background(), slotQueryTimeout)
	defer cancel()
	if _, err := l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, key, slot); err != nil {
		l.dropLocked() // closing the session frees the lock
	}
}

// dropLocked closes the slot session, which frees every slot it held.
func (l *slots) dropLocked() {
	if l.conn != nil {
		closeConn(l.conn)
	}
	l.conn, l.held = nil, nil
}

func closeConn(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Close(ctx) //nolint:errcheck
}

func (l *slots) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.dropLocked()
}
