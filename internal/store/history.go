package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ItemResult is how the latest finished job of the given kinds left a VM.
type ItemResult struct {
	JobID      int64
	JobKind    string
	FinishedAt time.Time // when the job finished
	Status     string    // ItemDone, ItemFailed, ItemRemoved or ItemInterrupted
	Step       string    // the last step the item reported: done, or cut off under way
	Error      string
	LeftConfig string // for an interrupted item: LeftConverged, or "" (see ItemOutcome)
}

// lastItemResultsSQL picks, per name, the item of the most recently finished
// job that touched the VM. Blocked, not-run (legacyNotRunSQL too) and
// LeftUntouched items didn't touch it. Order is by finished_at, not ID, so
// an earlier-finishing job can't hide an older job's later outcome; IS NOT
// NULL keeps DESC from putting unfinished jobs first.
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

// LastItemResults returns, per VM name, how the latest finished job of
// kinds left it; untouched names are absent.
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

// Now returns the database clock, which job times use; callers measure
// their skew with it.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var now time.Time
	err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}

// LastJobOf returns the newest job of any kind with an item for the VM, and
// that item, or ErrNotFound.
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
