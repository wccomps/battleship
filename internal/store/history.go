package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ItemResult is how the most recent finished job that converges or
// removes VMs (a deploy, reset or teardown) and touched a VM left it.
type ItemResult struct {
	JobID      int64
	JobKind    string
	FinishedAt time.Time // when the job finished
	Status     string    // ItemDone, ItemFailed, ItemRemoved or ItemInterrupted
	Step       string    // the last step the item reported: done, or cut off under way
	Error      string
	LeftConfig string // for an interrupted item: LeftConverged, or "" (see ItemOutcome)
}

// lastItemResultsSQL picks, for each name, the item of the most recently
// finished job that touched that VM: it reached a final outcome or was
// interrupted after reaching a step (blocked and not-run items didn't,
// legacy ones included: see legacyNotRunSQL). Nor does an item interrupted
// before it sent anything that changes the config (LeftUntouched): the VM
// is as the job before left it. Only jobs of the kinds the caller names
// count. Jobs are ordered by finished_at, not ID, so a job that
// finished first doesn't hide an older job's later outcome; the IS NOT NULL
// keeps DESC from putting unfinished jobs first.
const lastItemResultsSQL = `SELECT n.name, x.job_id, x.kind, x.finished_at, x.status, x.step, x.error, x.left_config
	FROM unnest($1::text[]) AS n(name)
	CROSS JOIN LATERAL (
		SELECT i.job_id, j.kind, j.finished_at, i.status, i.step, i.error, i.left_config
		FROM job_items i JOIN jobs j ON j.id = i.job_id
		WHERE i.name = n.name
		  AND j.finished_at IS NOT NULL
		  AND j.kind = ANY($2::text[])
		  AND i.status IN ` + touchedItemsSQL + `
		  AND NOT ` + legacyNotRunSQL + `
		  AND NOT (i.status = 'interrupted' AND i.left_config = '` + LeftUntouched + `')
		ORDER BY j.finished_at DESC, j.id DESC
		LIMIT 1
	) x`

// LastItemResults returns, for each VM name that a finished job of one of
// kinds touched, how the most recent such job left it. Names none
// touched are absent. It is one query, using the job_items_by_name
// index.
func (s *Store) LastItemResults(ctx context.Context, names, kinds []string) (map[string]ItemResult, error) {
	out := map[string]ItemResult{}
	if len(names) == 0 {
		return out, nil
	}
	seen := make(map[string]bool, len(names))
	distinct := make([]string, 0, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			distinct = append(distinct, n)
		}
	}
	rows, err := s.pool.Query(ctx, lastItemResultsSQL, distinct, kinds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var r ItemResult
		if err := rows.Scan(&name, &r.JobID, &r.JobKind, &r.FinishedAt, &r.Status, &r.Step, &r.Error, &r.LeftConfig); err != nil {
			return nil, err
		}
		out[name] = r
	}
	return out, rows.Err()
}

// Now returns the database server's clock. Job times (finished_at) are the
// server's, so a caller comparing its own times with them measures its
// offset from the server with this.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var now time.Time
	err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}

// LastJobOf returns the newest job, of any kind and status, with an item
// for the VM name, and that item; ErrNotFound if no job has one. A VM's
// page shows it as its last job.
func (s *Store) LastJobOf(ctx context.Context, name string) (Job, Item, error) {
	row := s.pool.QueryRow(ctx, `SELECT i.job_id, i.idx, i.name, i.team, i.vmid, i.step, `+itemStatusSQL+`, i.error
		FROM job_items i JOIN jobs j ON j.id = i.job_id WHERE i.name = $1 ORDER BY i.job_id DESC LIMIT 1`, name)
	var it Item
	err := row.Scan(&it.JobID, &it.Idx, &it.Name, &it.Team, &it.VMID, &it.Step, &it.Status, &it.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, Item{}, ErrNotFound
	}
	if err != nil {
		return Job{}, Item{}, err
	}
	j, err := s.Job(ctx, it.JobID)
	return j, it, err
}
