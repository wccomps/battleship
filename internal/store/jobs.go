package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Job statuses. Pending and running jobs are active; the rest are final.
const (
	StatusPending               = "pending"
	StatusRunning               = "running"
	StatusSucceeded             = "succeeded"
	StatusCompletedWithFailures = "completed_with_failures"
	StatusFailed                = "failed"      // could not run at all, e.g. planning failed
	StatusCancelled             = "cancelled"   // someone cancelled it
	StatusInterrupted           = "interrupted" // its worker stopped sending heartbeats
	StatusStale                 = "stale"       // the cluster changed since the preview
)

const (
	ItemPending     = "pending"
	ItemRunning     = "running"
	ItemDone        = "done"
	ItemFailed      = "failed"
	ItemBlocked     = "blocked"
	ItemRemoved     = "removed"     // created, didn't finish, and cleaned up
	ItemInterrupted = "interrupted" // its job was interrupted or cancelled after it reached a step
	// ItemNotRun: its job ended (or was cancelled) before it reached a
	// step, so it never touched its VM.
	ItemNotRun = "not run"
)

// legacyNotRunSQL matches, in job_items i joined to jobs j, an item an
// older battleship ended without marking it not run: its job finished
// before it reached a step, and it was left pending or running, or marked
// interrupted. Such binaries may still run beside this one (another
// replica, a CLI) after migration 005 converted the rows already there.
const legacyNotRunSQL = `(j.finished_at IS NOT NULL AND i.step = '' AND i.status IN ('pending', 'running', 'interrupted'))`

// itemStatusSQL is an item's status as the store reports it, with legacy
// rows read as ItemNotRun.
const itemStatusSQL = `CASE WHEN ` + legacyNotRunSQL + ` THEN 'not run' ELSE i.status END`

var (
	ErrNotFound   = errors.New("job not found")
	ErrLostClaim  = errors.New("job is no longer claimed by this worker")
	ErrNotActive  = errors.New("job already finished")
	ErrEmptyInput = errors.New("job needs a kind, inputs, a plan, lock keys and a creator")
)

type Job struct {
	ID              int64
	Kind            string
	Inputs          json.RawMessage
	Plan            json.RawMessage
	Fingerprint     string
	LockKeys        []string
	Status          string
	CreatedBy       string
	CreatedRole     string // the creator's battleship role, for jobs from before roles were removed
	CreatedAs       string // the Proxmox user (or API token) the job acts as
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
	ClaimedBy       string
	CancelRequested bool
	CancelledBy     string
	Summary         json.RawMessage
	Error           string
}

// Active reports whether the job is pending or running.
func (j Job) Active() bool { return j.Status == StatusPending || j.Status == StatusRunning }

// CancelCameTooLate reports whether someone asked to cancel the job and it
// ended anyway as its work did: the cancel stopped nothing.
func (j Job) CancelCameTooLate() bool {
	switch j.Status {
	case StatusSucceeded, StatusCompletedWithFailures, StatusFailed:
		return j.CancelRequested
	}
	return false
}

type NewJob struct {
	Kind        string
	Inputs      json.RawMessage
	Plan        json.RawMessage
	Fingerprint string
	LockKeys    []string
	CreatedBy   string
	CreatedAs   string // the Proxmox user (or API token ID) it acts as
	Items       []NewItem
	// Preview, if set, is the web preview this job submits. CreateJob
	// fails with ErrPreviewNotFound, ErrPreviewUsed or ErrPreviewExpired,
	// storing nothing, unless the preview exists, hasn't expired and no job
	// has submitted it yet.
	Preview *PreviewClaim
	// Credential, if set, gives the credential the job runs with, sealed
	// for the job's ID, which it is passed. CreateJob stores it with the
	// job, or stores nothing if it fails.
	Credential func(jobID int64) (JobCredential, error)
}

// NewItem is one VM from the confirmed plan. A non-empty Blocked stores the
// item as blocked with that reason.
type NewItem struct {
	Name    string
	Team    string
	VMID    int
	Blocked string
}

type Item struct {
	JobID  int64
	Idx    int
	Name   string
	Team   string
	VMID   int
	Step   string
	Status string
	Error  string
	// Steps is how each step that reported an outcome last ended: done,
	// skipped, failed or interrupted, by step name.
	Steps map[string]string
}

type Event struct {
	ID      int64
	At      time.Time
	Item    string
	Step    string
	Status  string
	Message string
}

const jobColumns = `id, kind, inputs, plan, fingerprint, lock_keys, status, created_by, created_role, created_as, created_at,
	started_at, finished_at, coalesce(claimed_by, ''), cancel_requested,
	coalesce(cancelled_by, ''), summary, error`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Kind, &j.Inputs, &j.Plan, &j.Fingerprint, &j.LockKeys, &j.Status, &j.CreatedBy,
		&j.CreatedRole, &j.CreatedAs, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.ClaimedBy, &j.CancelRequested,
		&j.CancelledBy, &j.Summary, &j.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

// CreateJob stores a confirmed job and its items as pending. With
// nj.Preview set, it also marks that preview submitted by this job.
func (s *Store) CreateJob(ctx context.Context, nj NewJob) (int64, error) {
	if nj.Kind == "" || len(nj.Inputs) == 0 || len(nj.Plan) == 0 || len(nj.LockKeys) == 0 || nj.CreatedBy == "" {
		return 0, ErrEmptyInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if nj.Preview != nil {
		if err := claimPreview(ctx, tx, *nj.Preview); err != nil {
			return 0, err
		}
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO jobs (kind, inputs, plan, fingerprint, lock_keys, created_by, created_as)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		nj.Kind, nj.Inputs, nj.Plan, nj.Fingerprint, nj.LockKeys, nj.CreatedBy, nj.CreatedAs).Scan(&id)
	if err != nil {
		return 0, err
	}
	if nj.Credential != nil {
		c, err := nj.Credential(id)
		if err != nil {
			return 0, err
		}
		c.JobID = id
		if err := insertCredential(ctx, tx, c); err != nil {
			return 0, err
		}
	}
	if nj.Preview != nil {
		if _, err := tx.Exec(ctx, `UPDATE previews SET job_id = $3 WHERE session_id = $1 AND id = $2`,
			nj.Preview.SessionID, nj.Preview.ID, id); err != nil {
			return 0, err
		}
	}
	for i, it := range nj.Items {
		status := ItemPending
		if it.Blocked != "" {
			status = ItemBlocked
		}
		if _, err := tx.Exec(ctx, `INSERT INTO job_items (job_id, idx, name, team, vmid, status, error)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, i, it.Name, it.Team, it.VMID, status, it.Blocked); err != nil {
			return 0, err
		}
	}
	if err := notify(ctx, tx, id); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func (s *Store) Job(ctx context.Context, id int64) (Job, error) {
	return scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id))
}

// Jobs lists the most recent jobs, newest first.
func (s *Store) Jobs(ctx context.Context, limit int) ([]Job, error) {
	return s.JobsBefore(ctx, 0, limit)
}

// JobsBefore lists up to limit jobs with IDs below before, newest first; a
// before of 0 or less starts from the newest job. Pages of the job list
// pass the last ID of the previous page.
func (s *Store) JobsBefore(ctx context.Context, before int64, limit int) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+jobColumns+` FROM jobs
		WHERE $1::bigint <= 0 OR id < $1 ORDER BY id DESC LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// Items returns the job's items in plan order.
func (s *Store) Items(ctx context.Context, jobID int64) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.job_id, i.idx, i.name, i.team, i.vmid, i.step, `+itemStatusSQL+`, i.error, i.steps
		FROM job_items i JOIN jobs j ON j.id = i.job_id WHERE i.job_id = $1 ORDER BY i.idx`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.JobID, &it.Idx, &it.Name, &it.Team, &it.VMID, &it.Step, &it.Status, &it.Error, &it.Steps); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// Events returns up to limit events with IDs after afterID, oldest first.
func (s *Store) Events(ctx context.Context, jobID, afterID int64, limit int) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, at, item, step, status, message FROM job_events
		WHERE job_id = $1 AND id > $2 ORDER BY id LIMIT $3`, jobID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.ID, &ev.At, &ev.Item, &ev.Step, &ev.Status, &ev.Message); err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// beforeEventCommit is a test-only hook called after AddEvent stores an
// event but before its transaction commits, so tests can hold one event's
// commit back while another is added.
var beforeEventCommit func(jobID int64, ev Event)

// AddEvent records progress of a running job: the matching item becomes
// running at the event's step (unless blocked, interrupted or not run), and
// a failed event's message is its latest error; Finish, ReapStale and
// RequestCancel set final statuses. One job's events are added one at a
// time, so their IDs follow commit order. It returns ErrNotFound for an
// unknown job and ErrNotActive, storing nothing, for one that isn't running.
func (s *Store) AddEvent(ctx context.Context, jobID int64, ev Event) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Lock the job's row first, so one job's events commit in ID order (a
	// reader past an early ID would never see it committed late), and in
	// the job-row-then-items order Finish, RequestCancel, Heartbeat and
	// ReapStale use, so they can't deadlock.
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("job %d: %w", jobID, ErrNotFound)
		}
		return err
	}
	if status != StatusRunning {
		return fmt.Errorf("job %d is %s: %w", jobID, status, ErrNotActive)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO job_events (job_id, at, item, step, status, message)
		VALUES ($1, $2, $3, $4, $5, $6)`, jobID, ev.At, ev.Item, ev.Step, ev.Status, ev.Message); err != nil {
		return err
	}
	if ev.Item != "" {
		if _, err := tx.Exec(ctx, `UPDATE job_items SET
				step = CASE WHEN $3 <> '' THEN $3 ELSE step END,
				steps = CASE WHEN $3 <> '' AND $4 IN ('done', 'skipped', 'failed', 'interrupted')
					THEN steps || jsonb_build_object($3::text, $4::text) ELSE steps END,
				status = CASE WHEN status IN ('blocked', 'interrupted', 'not run') THEN status ELSE 'running' END,
				error = CASE WHEN $4 = 'failed' THEN $5 ELSE error END,
				updated_at = now()
			WHERE job_id = $1 AND name = $2`, jobID, ev.Item, ev.Step, ev.Status, ev.Message); err != nil {
			return err
		}
	}
	if err := notifyLog(ctx, tx, jobID); err != nil {
		return err
	}
	if beforeEventCommit != nil {
		beforeEventCommit(jobID, ev)
	}
	return tx.Commit(ctx)
}

// beforeCancelRecheck is a test-only hook called inside RequestCancel's
// transaction when its UPDATE matched no active job, before it checks
// whether the job exists, so tests can hold it there with the row's lock.
var beforeCancelRecheck func(jobID int64)

// RequestCancel cancels a pending job at once, marking its items not run,
// or asks a running job's worker to stop. It returns
// ErrNotActive for a job that already finished.
func (s *Store) RequestCancel(ctx context.Context, jobID int64, by string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	err = tx.QueryRow(ctx, `UPDATE jobs SET
			cancel_requested = true,
			cancelled_by = $2,
			status = CASE WHEN status = 'pending' THEN 'cancelled' ELSE status END,
			finished_at = CASE WHEN status = 'pending' THEN now() ELSE finished_at END
		WHERE id = $1 AND status IN ('pending', 'running') RETURNING status`, jobID, by).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		if beforeCancelRecheck != nil {
			beforeCancelRecheck(jobID)
		}
		// Ask within tx: the UPDATE above can still hold the row's lock, and
		// waiting for another pool connection while holding it can starve
		// the pool of the AddEvent calls waiting on that lock.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, jobID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return fmt.Errorf("job %d: %w", jobID, ErrNotActive)
	}
	if err != nil {
		return err
	}
	// A pending job never started, so none of its items ran.
	if status == StatusCancelled {
		if err := endItems(ctx, tx, []int64{jobID}, true); err != nil {
			return err
		}
	}
	if err := notifyCancel(ctx, tx, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BusyVMs names the VMs that active jobs still have to work on: each
// pending or running item of a pending or running job, with the oldest
// such job's ID. Blocked items aren't busy: their jobs skip them. It
// returns at most limit VMs, so a backlog of queued jobs can't make the
// answer unbounded, though the query still reads every active job's items.
func (s *Store) BusyVMs(ctx context.Context, limit int) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.name, min(j.id) FROM jobs j JOIN job_items i ON i.job_id = j.id
		WHERE j.status IN ('pending', 'running') AND i.status IN ('pending', 'running')
		GROUP BY i.name ORDER BY i.name LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var id int64
		if err := rows.Scan(&name, &id); err != nil {
			return nil, err
		}
		out[name] = id
	}
	return out, rows.Err()
}
