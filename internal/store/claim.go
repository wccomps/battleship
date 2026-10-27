package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// claimLockID serializes claims, so two workers can't both start jobs whose
// lock keys overlap.
const claimLockID = 727071

// beforeClaimUpdate is a test hook between Claim's SELECT and UPDATE.
var beforeClaimUpdate func(id int64)

// blocksSQL holds when job o keeps pending job j waiting: their lock keys
// conflict and o is running, or pending and older. Keys conflict when they
// are equal, except that two "read:" keys never do: "read:K" conflicts only
// with K (see migration 013).
const blocksSQL = `(o.lock_writes && j.lock_writes OR o.lock_writes && j.lock_reads OR o.lock_reads && j.lock_writes)
	AND (o.status = 'running' OR (o.status = 'pending' AND o.id < j.id))`

// Claim starts the oldest pending job nothing blocks (see blocksSQL), or
// returns nil. Overlapping jobs run in creation order. ReapStale reaps it
// once it has no heartbeat for staleAfter, which must be positive.
func (s *Store) Claim(ctx context.Context, worker string, staleAfter time.Duration) (*Job, error) {
	if staleAfter <= 0 {
		return nil, fmt.Errorf("claiming a job: stale_after %v isn't positive", staleAfter)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, claimLockID); err != nil {
		return nil, err
	}
	var id int64
	err = tx.QueryRow(ctx, `SELECT j.id FROM jobs j
		WHERE j.status = 'pending'
		  AND NOT EXISTS (SELECT 1 FROM jobs o WHERE `+blocksSQL+`)
		ORDER BY j.id LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if beforeClaimUpdate != nil {
		beforeClaimUpdate(id)
	}
	job, err := scanJob(tx.QueryRow(ctx, `UPDATE jobs SET status = 'running', claimed_by = $2,
		started_at = now(), heartbeat_at = now(), stale_after = make_interval(secs => $3)
		WHERE id = $1 AND status = 'pending' AND NOT cancel_requested RETURNING `+jobColumns, id, worker, staleAfter.Seconds()))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if err := notify(ctx, tx, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &job, nil
}

// Heartbeat records that worker still runs the job and reports whether a
// cancel was requested. ErrLostClaim means the worker must stop. It uses the
// liveness pool.
func (s *Store) Heartbeat(ctx context.Context, jobID int64, worker string) (cancel bool, err error) {
	err = s.liveness.QueryRow(ctx, `UPDATE jobs SET heartbeat_at = now()
		WHERE id = $1 AND claimed_by = $2 AND status = 'running' RETURNING cancel_requested`,
		jobID, worker).Scan(&cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrLostClaim
	}
	return cancel, err
}

// ItemOutcome is an item's final status and error.
type ItemOutcome struct {
	Status string
	Error  string
	// LeftConfig, for an interrupted item: LeftUntouched, LeftConverged, or
	// "" if it may have changed part of the config.
	LeftConfig string
}

// How an interrupted item left its VM's config (ItemOutcome.LeftConfig).
const (
	LeftUntouched = "untouched" // no config change sent: the previous job still describes the VM
	LeftConverged = "converged" // all config steps finished; only power steps were left
)

// Outcome is how a job ended.
type Outcome struct {
	Status  string
	Summary json.RawMessage // may be nil
	Error   string
	Items   map[string]ItemOutcome // by item name
}

// Finish records the job's outcome and ends its other items (endItems).
// ErrLostClaim if the job is no longer this worker's.
func (s *Store) Finish(ctx context.Context, jobID int64, worker string, o Outcome) error {
	if !JobStatus(o.Status).Final() {
		return fmt.Errorf("finish job %d: %q is not a final status", jobID, o.Status)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE jobs SET status = $3, finished_at = now(), summary = $4, error = $5
		WHERE id = $1 AND claimed_by = $2 AND status = 'running'`, jobID, worker, o.Status, o.Summary, o.Error)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLostClaim
	}
	for name, it := range o.Items {
		if _, err := tx.Exec(ctx, `UPDATE job_items SET status = $3, error = $4, left_config = $5
			WHERE job_id = $1 AND name = $2 AND status NOT IN `+keptOnOutcomeSQL, jobID, name, it.Status, it.Error, it.LeftConfig); err != nil {
			return err
		}
	}
	if err := endItems(ctx, tx, []int64{jobID}, JobStatus(o.Status).Stopped()); err != nil {
		return err
	}
	if err := notify(ctx, tx, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// endItems ends the items of jobs that just ended, and is the one place that
// decides ItemNotRun: an item that never reached a step. If stopped, other
// unfinished items become ItemInterrupted.
func endItems(ctx context.Context, tx pgx.Tx, jobIDs []int64, stopped bool) error {
	_, err := tx.Exec(ctx, `UPDATE job_items SET
			status = CASE WHEN step = '' THEN 'not run' ELSE 'interrupted' END
		WHERE job_id = ANY($1) AND (
			(step = '' AND status IN `+notRunNoStepSQL+`)
			OR ($2 AND status IN `+unfinishedItemsSQL+`))`, jobIDs, stopped)
	return err
}

// ReapStale marks running jobs with no heartbeat for their stale_after
// (see Claim; after for jobs an older battleship claimed without one) as
// interrupted and returns their IDs.
func (s *Store) ReapStale(ctx context.Context, after time.Duration) ([]int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `UPDATE jobs SET status = 'interrupted', finished_at = now(),
			error = 'its worker stopped sending heartbeats; re-run it to finish'
		WHERE status = 'running' AND heartbeat_at < now() - COALESCE(stale_after, make_interval(secs => $1))
		RETURNING id`, after.Seconds())
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	if err := endItems(ctx, tx, ids, true); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := notify(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	return ids, tx.Commit(ctx)
}

// Blockers returns the jobs a pending job is waiting for (see blocksSQL).
func (s *Store) Blockers(ctx context.Context, jobID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT o.id FROM jobs j JOIN jobs o ON `+blocksSQL+`
		WHERE j.id = $1 AND j.status = 'pending' ORDER BY o.id`, jobID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// BlockersOf is Blockers for many jobs in one query. Jobs that wait for
// nothing are absent.
func (s *Store) BlockersOf(ctx context.Context, jobIDs []int64) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT j.id, o.id FROM jobs j JOIN jobs o ON `+blocksSQL+`
		WHERE j.id = ANY($1) AND j.status = 'pending' ORDER BY j.id, o.id`, jobIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, blocker int64
		if err := rows.Scan(&id, &blocker); err != nil {
			return nil, err
		}
		out[id] = append(out[id], blocker)
	}
	return out, rows.Err()
}

// ReservedKeys are the lock keys pending and running jobs other than except
// write (lock_writes): what they will create or change. Planning reads them
// so it doesn't hand out a VMID or slot a queued job will create (see
// pods.Planner.Reserved).
func (s *Store) ReservedKeys(ctx context.Context, except int64) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT k FROM jobs, unnest(lock_writes) AS k
		WHERE status IN ('pending', 'running') AND id <> $1`, except)
	if err != nil {
		return nil, fmt.Errorf("reading queued jobs' lock keys: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("reading queued jobs' lock keys: %w", err)
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out, nil
}
