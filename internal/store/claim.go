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

// beforeClaimUpdate is a test-only hook between Claim's SELECT of a pending
// job and its UPDATE to running, where tests inject a cancel.
var beforeClaimUpdate func(id int64)

// Claim starts the oldest pending job that can run now and returns it, or nil
// if none can. A job can run when no running job shares a lock key with it
// and no older pending job does either, so overlapping jobs run in the order
// they were created.
func (s *Store) Claim(ctx context.Context, worker string) (*Job, error) {
	return s.claim(ctx, worker, nil)
}

// ClaimJob starts the given pending job if it can run now, by the same rules
// as Claim. It returns nil while the job waits for an overlapping job (see
// Blockers), ErrNotFound for an unknown job, and ErrNotActive once the job is
// no longer pending: cancelled, or claimed by someone else.
func (s *Store) ClaimJob(ctx context.Context, id int64, worker string) (*Job, error) {
	return s.claim(ctx, worker, &id)
}

// blocksSQL holds when job o keeps pending job j waiting: o shares a lock
// key with j and is running, or is pending and older. Claim starts only
// jobs nothing blocks; Blockers and BlockersOf list what does.
const blocksSQL = `o.lock_keys && j.lock_keys AND (o.status = 'running' OR (o.status = 'pending' AND o.id < j.id))`

// claim starts the oldest runnable pending job or, if only is set, that job.
func (s *Store) claim(ctx context.Context, worker string, only *int64) (*Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, claimLockID); err != nil {
		return nil, err
	}
	// notClaimable explains why job *only wasn't claimed: nil while it waits.
	notClaimable := func() (*Job, error) {
		if only == nil {
			return nil, nil
		}
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, *only).Scan(&status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil, ErrNotFound
		case err != nil:
			return nil, err
		case status != StatusPending:
			return nil, fmt.Errorf("job %d is %s: %w", *only, status, ErrNotActive)
		}
		return nil, nil
	}
	var id int64
	err = tx.QueryRow(ctx, `SELECT j.id FROM jobs j
		WHERE j.status = 'pending' AND ($1::bigint IS NULL OR j.id = $1)
		  AND NOT EXISTS (SELECT 1 FROM jobs o WHERE `+blocksSQL+`)
		ORDER BY j.id LIMIT 1`, only).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return notClaimable()
	}
	if err != nil {
		return nil, err
	}
	if beforeClaimUpdate != nil {
		beforeClaimUpdate(id)
	}
	job, err := scanJob(tx.QueryRow(ctx, `UPDATE jobs SET status = 'running', claimed_by = $2,
		started_at = now(), heartbeat_at = now() WHERE id = $1 AND status = 'pending' AND NOT cancel_requested RETURNING `+jobColumns, id, worker))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notClaimable()
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

// Heartbeat records that worker is still running the job and reports whether
// a cancel was requested. ErrLostClaim means the job is no longer this
// worker's to run, e.g. it was marked interrupted; the worker must stop. It
// uses the liveness pool.
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
	// LeftConfig, for an interrupted item, is how it left its VM's config:
	// LeftUntouched, LeftConverged, or "" when it may have changed part of it.
	LeftConfig string
}

// How an interrupted item left its VM's config (ItemOutcome.LeftConfig).
const (
	LeftUntouched = "untouched" // nothing that changes the config was sent: the job before still says how the VM is
	LeftConverged = "converged" // every step that changes the config finished: only power steps were left
)

// Outcome is how a job ended.
type Outcome struct {
	Status  string
	Summary json.RawMessage // may be nil
	Error   string
	Items   map[string]ItemOutcome // by item name
}

// Finish records the job's final status, summary and item outcomes, then
// ends the job's other items (see endItems). It returns ErrLostClaim if the
// job is no longer this worker's.
func (s *Store) Finish(ctx context.Context, jobID int64, worker string, o Outcome) error {
	finalStatuses := map[string]bool{
		StatusSucceeded:             true,
		StatusCompletedWithFailures: true,
		StatusFailed:                true,
		StatusCancelled:             true,
		StatusInterrupted:           true,
		StatusStale:                 true,
	}
	if !finalStatuses[o.Status] {
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
		if _, err := tx.Exec(ctx, `UPDATE job_items SET status = $3, error = $4, left_config = $5, updated_at = now()
			WHERE job_id = $1 AND name = $2 AND status NOT IN ('blocked', 'interrupted')`, jobID, name, it.Status, it.Error, it.LeftConfig); err != nil {
			return err
		}
	}
	if err := endItems(ctx, tx, []int64{jobID}, o.Status == StatusInterrupted || o.Status == StatusCancelled); err != nil {
		return err
	}
	if err := notify(ctx, tx, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// endItems ends the items of jobs that just ended. This is the one place
// that decides an item didn't run: one that never reached a step is
// ItemNotRun, whether it was still pending or already marked interrupted.
// When the jobs were stopped (interrupted or cancelled), any other item still
// pending or running is ItemInterrupted.
func endItems(ctx context.Context, tx pgx.Tx, jobIDs []int64, stopped bool) error {
	_, err := tx.Exec(ctx, `UPDATE job_items SET
			status = CASE WHEN step = '' THEN 'not run' ELSE 'interrupted' END, updated_at = now()
		WHERE job_id = ANY($1) AND (
			(step = '' AND status IN ('pending', 'running', 'interrupted'))
			OR ($2 AND status IN ('pending', 'running')))`, jobIDs, stopped)
	return err
}

// ReapStale marks running jobs whose last heartbeat is older than after as
// interrupted, along with their unfinished items, and returns their IDs. Any
// worker may call it.
func (s *Store) ReapStale(ctx context.Context, after time.Duration) ([]int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `UPDATE jobs SET status = 'interrupted', finished_at = now(),
			error = 'its worker stopped sending heartbeats; re-run it to finish'
		WHERE status = 'running' AND heartbeat_at < now() - make_interval(secs => $1)
		RETURNING id`, after.Seconds())
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
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

// Blockers returns the jobs a pending job is waiting for: running jobs, and
// older pending jobs, that share a lock key with it.
func (s *Store) Blockers(ctx context.Context, jobID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT o.id FROM jobs j JOIN jobs o ON `+blocksSQL+`
		WHERE j.id = $1 AND j.status = 'pending' ORDER BY o.id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// BlockersOf is Blockers for each of jobIDs, in one query: the IDs of the
// jobs each pending one waits for, in order. Jobs that wait for nothing
// (or aren't pending, or don't exist) are absent from the map.
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
