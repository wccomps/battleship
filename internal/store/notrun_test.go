package store_test

import (
	"testing"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// Rows written before items had a "not run" status get it from migration
// 005: finished jobs' items that never reached a step, whether left pending
// or marked interrupted. Items of active jobs, and items that reached a
// step, keep theirs.
func TestMigrationMarksOldItemsNotRun(t *testing.T) {
	s := storetest.New(t)
	stale := create(t, s, "team:01")
	interrupted := create(t, s, "team:02")
	stepped := create(t, s, "team:03")
	active := create(t, s, "team:04")
	conn, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE jobs SET status = 'stale', finished_at = now() WHERE id = $1`, []any{stale}},
		{`UPDATE jobs SET status = 'interrupted', finished_at = now() WHERE id = ANY($1)`, []any{[]int64{interrupted, stepped}}},
		{`UPDATE job_items SET status = 'interrupted' WHERE job_id = ANY($1) AND status <> 'blocked'`, []any{[]int64{interrupted, stepped}}},
		{`UPDATE job_items SET step = 'stop' WHERE job_id = $1 AND status <> 'blocked'`, []any{stepped}},
	} {
		if _, err := conn.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	if _, err := conn.Exec(ctx, store.MigrationSQL("005_item_not_run.sql")); err != nil {
		t.Fatal(err)
	}
	want := map[int64][]string{
		stale:       {store.ItemNotRun, store.ItemBlocked},
		interrupted: {store.ItemNotRun, store.ItemBlocked},
		stepped:     {store.ItemInterrupted, store.ItemBlocked},
		active:      {store.ItemPending, store.ItemBlocked},
	}
	for id, statuses := range want {
		items, err := s.Items(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for i, it := range items {
			if it.Status != statuses[i] {
				t.Errorf("job %d item %s = %s, want %s", id, it.Name, it.Status, statuses[i])
			}
		}
	}
}

// An older battleship still running beside this one (another replica, or a
// CLI) ends jobs the old way after migration 005: items that never reached
// a step stay pending, or become interrupted with no step. The store reads
// them as not run.
func TestOldBinaryRowsReadAsNotRun(t *testing.T) {
	s := storetest.New(t)
	finished := create(t, s, "team:01") // team01-teak left pending
	stopped := create(t, s, "team:02")  // team01-teak interrupted with no step
	conn, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE jobs SET status = 'stale', finished_at = now() WHERE id = $1`, []any{finished}},
		{`UPDATE jobs SET status = 'interrupted', finished_at = now() WHERE id = $1`, []any{stopped}},
		{`UPDATE job_items SET status = 'interrupted' WHERE job_id = $1 AND status <> 'blocked'`, []any{stopped}},
	} {
		if _, err := conn.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	for _, id := range []int64{finished, stopped} {
		items, err := s.Items(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if items[0].Status != store.ItemNotRun || items[1].Status != store.ItemBlocked {
			t.Errorf("job %d items = %s, %s, want not run, blocked", id, items[0].Status, items[1].Status)
		}
	}
	_, it, err := s.LastJobOf(ctx, "team01-teak")
	if err != nil {
		t.Fatal(err)
	}
	if it.JobID != stopped || it.Status != store.ItemNotRun {
		t.Errorf("LastJobOf item = job %d %s, want job %d not run", it.JobID, it.Status, stopped)
	}
	got, err := s.LastItemResults(ctx, []string{"team01-teak"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := got["team01-teak"]; ok {
		t.Errorf("LastItemResults has %+v for a VM no job touched", r)
	}
}

// Migration 008 carries the old power-step rule into left_config for rows
// written before 007: an item interrupted at stop, start or power left the
// config as the job before did. Others keep ”.
func TestMigrationMarksOldPowerStepInterruptionsUntouched(t *testing.T) {
	s := storetest.New(t)
	stopped := create(t, s, "team:01")
	rolled := create(t, s, "team:02")
	conn, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE jobs SET status = 'cancelled', finished_at = now() WHERE id = ANY($1)`, []any{[]int64{stopped, rolled}}},
		{`UPDATE job_items SET status = 'interrupted', step = 'stop', left_config = '' WHERE job_id = $1 AND status <> 'blocked'`, []any{stopped}},
		{`UPDATE job_items SET status = 'interrupted', step = 'rollback', left_config = '' WHERE job_id = $1 AND status <> 'blocked'`, []any{rolled}},
	} {
		if _, err := conn.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	if _, err := conn.Exec(ctx, store.MigrationSQL("008_left_config_backfill.sql")); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{stopped: store.LeftUntouched, rolled: ""} {
		var left []string
		rows, err := conn.Query(ctx, `SELECT left_config FROM job_items WHERE job_id = $1 AND status = 'interrupted'`, id)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				t.Fatal(err)
			}
			left = append(left, l)
		}
		rows.Close()
		if len(left) == 0 {
			t.Fatalf("job %d has no interrupted items", id)
		}
		for _, l := range left {
			if l != want {
				t.Errorf("job %d left_config = %q, want %q", id, l, want)
			}
		}
	}
}
