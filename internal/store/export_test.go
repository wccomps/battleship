package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SetBeforeClaimUpdate sets the test hook that runs after Claim selects a
// pending job but before it updates to running. Pass nil to reset.
func SetBeforeClaimUpdate(f func(id int64)) {
	beforeClaimUpdate = f
}

// SetBeforeEventCommit sets the test hook that runs after AddEvent stores
// an event but before it commits. Pass nil to reset.
func SetBeforeEventCommit(f func(jobID int64, ev Event)) {
	beforeEventCommit = f
}

// SetBeforeCancelRecheck sets the test hook that runs inside RequestCancel's
// transaction when it found no active job, before it checks the job exists.
// Pass nil to reset.
func SetBeforeCancelRecheck(f func(jobID int64)) {
	beforeCancelRecheck = f
}

// Acquire gives tests a connection from the store's pool.
func Acquire(ctx context.Context, s *Store) (*pgxpool.Conn, error) { return s.pool.Acquire(ctx) }

const ClaimLockID, MigrateLockID = claimLockID, migrateLockID

// LastItemResultsSQL is the query behind LastItemResults, for EXPLAIN.
const LastItemResultsSQL = lastItemResultsSQL

// HoldMainPool acquires every connection of the store's main pool and
// returns a function that releases them, so tests can saturate it.
func HoldMainPool(ctx context.Context, s *Store) (func(), error) {
	n := int(s.pool.Config().MaxConns)
	conns := make([]*pgxpool.Conn, 0, n)
	release := func() {
		for _, c := range conns {
			c.Release()
		}
	}
	for range n {
		c, err := s.pool.Acquire(ctx)
		if err != nil {
			release()
			return nil, err
		}
		conns = append(conns, c)
	}
	return release, nil
}

// PoolSizes reports the most connections each of the store's pools opens.
func PoolSizes(s *Store) (main, liveness int) {
	return int(s.pool.Config().MaxConns), int(s.liveness.Config().MaxConns)
}

// SlotConnConfig is the config the store's slot session connects with, so
// tests can wrap its dialer.
func SlotConnConfig(s *Store) *pgx.ConnConfig { return s.slots.cfg }
