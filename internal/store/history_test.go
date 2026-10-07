package store_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// runJob creates a job over items, claims it, reports a step on each item in
// started, and finishes it with status and the given item outcomes.
func runJob(t *testing.T, s *store.Store, kind string, items []store.NewItem, started []string,
	status string, outcomes map[string]store.ItemOutcome) int64 {
	t.Helper()
	nj := newJob("team:01")
	nj.Kind = kind
	nj.Items = items
	id, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(ctx, id, "w"); err != nil {
		t.Fatal(err)
	}
	for _, name := range started {
		if err := s.AddEvent(ctx, id, store.Event{At: time.Now(), Item: name, Step: "network", Status: "started"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(ctx, id, "w", store.Outcome{Status: status, Items: outcomes}); err != nil {
		t.Fatal(err)
	}
	return id
}

func items(names ...string) []store.NewItem {
	out := make([]store.NewItem, len(names))
	for i, n := range names {
		out[i] = store.NewItem{Name: n, Team: "01", VMID: 10100 + i}
	}
	return out
}

// driftKinds are the kinds status asks for (status.driftKinds).
var driftKinds = []string{"deploy", "reset", "teardown"}

func TestLastItemResults(t *testing.T) {
	s := storetest.New(t)

	// Job 1: dc fails, web succeeds.
	j1 := runJob(t, s, "deploy", items("team01-dc", "team01-web"), []string{"team01-dc", "team01-web"},
		store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
			"team01-dc":  {Status: store.ItemFailed, Error: "network: bridge missing"},
			"team01-web": {Status: store.ItemDone},
		})
	// Job 2: web fails now; dc was blocked, so job 2 didn't touch it.
	nj := items("team01-dc", "team01-web")
	nj[0].Blocked = "no snapshot"
	j2 := runJob(t, s, "reset", nj, []string{"team01-web"},
		store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
			"team01-web": {Status: store.ItemFailed, Error: "rollback: locked"},
		})
	// Job 3 is interrupted: mail started and is cut off, ftp never started.
	j3 := runJob(t, s, "deploy", items("team01-mail", "team01-ftp"), []string{"team01-mail"},
		store.StatusInterrupted, nil)
	// Job 4 is still pending and job 5 running: neither counts.
	create(t, s, "team:01")
	nj5 := newJob("team:02")
	nj5.Items = items("team01-web")
	id5, err := s.CreateJob(ctx, nj5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(ctx, id5, "w"); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastItemResults(ctx, []string{"team01-dc", "team01-web", "team01-mail", "team01-ftp", "team01-new", "team01-dc"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	type brief struct {
		JobID       int64
		Kind        string
		Status      string
		Step, Error string
	}
	short := map[string]brief{}
	for name, r := range got {
		if r.FinishedAt.IsZero() {
			t.Errorf("%s: FinishedAt is zero", name)
		}
		short[name] = brief{r.JobID, r.JobKind, r.Status, r.Step, r.Error}
	}
	want := map[string]brief{
		"team01-dc":   {j1, "deploy", store.ItemFailed, "network", "network: bridge missing"},
		"team01-web":  {j2, "reset", store.ItemFailed, "network", "rollback: locked"},
		"team01-mail": {j3, "deploy", store.ItemInterrupted, "network", ""},
	}
	if !reflect.DeepEqual(short, want) {
		t.Errorf("LastItemResults =\n %+v\nwant\n %+v", short, want)
	}
}

func TestLastItemResultsLaterSuccessClears(t *testing.T) {
	s := storetest.New(t)
	runJob(t, s, "deploy", items("team01-dc"), []string{"team01-dc"}, store.StatusCompletedWithFailures,
		map[string]store.ItemOutcome{"team01-dc": {Status: store.ItemFailed, Error: "boom"}})
	j2 := runJob(t, s, "deploy", items("team01-dc"), []string{"team01-dc"}, store.StatusSucceeded,
		map[string]store.ItemOutcome{"team01-dc": {Status: store.ItemDone}})
	got, err := s.LastItemResults(ctx, []string{"team01-dc"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["team01-dc"]; r.JobID != j2 || r.Status != store.ItemDone {
		t.Errorf("team01-dc = %+v, want done in job %d", r, j2)
	}
}

// Two jobs ran at once (their lock keys didn't overlap) and the newer one
// finished first: the older job's later outcome is the latest.
func TestLastItemResultsOrdersByFinish(t *testing.T) {
	s := storetest.New(t)
	start := func(key string) int64 {
		nj := newJob(key)
		nj.Kind = "deploy"
		nj.Items = items("team01-dc")
		id, err := s.CreateJob(ctx, nj)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimJob(ctx, id, "w"); err != nil {
			t.Fatal(err)
		}
		if err := s.AddEvent(ctx, id, store.Event{At: time.Now(), Item: "team01-dc", Step: "clone", Status: "started"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	older, newer := start("team:01"), start("template:dc.tpl")
	if err := s.Finish(ctx, newer, "w", store.Outcome{Status: store.StatusSucceeded,
		Items: map[string]store.ItemOutcome{"team01-dc": {Status: store.ItemDone}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, older, "w", store.Outcome{Status: store.StatusCompletedWithFailures,
		Items: map[string]store.ItemOutcome{"team01-dc": {Status: store.ItemFailed, Error: "clone: timeout"}}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastItemResults(ctx, []string{"team01-dc"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["team01-dc"]; r.JobID != older || r.Status != store.ItemFailed {
		t.Errorf("team01-dc = %+v, want failed in job %d", r, older)
	}
}

// A job cancelled while pending never touched its VMs.
func TestLastItemResultsSkipsCancelledPendingJob(t *testing.T) {
	s := storetest.New(t)
	ran := runJob(t, s, "deploy", items("team01-dc"), []string{"team01-dc"}, store.StatusCompletedWithFailures,
		map[string]store.ItemOutcome{"team01-dc": {Status: store.ItemFailed, Error: "boom"}})
	nj := newJob("team:01")
	nj.Kind = "deploy"
	nj.Items = items("team01-dc")
	id, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestCancel(ctx, id, "lead"); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastItemResults(ctx, []string{"team01-dc"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["team01-dc"]; r.JobID != ran {
		t.Errorf("team01-dc = %+v, want job %d", r, ran)
	}
}

func TestLastItemResultsEmpty(t *testing.T) {
	s := storetest.New(t)
	got, err := s.LastItemResults(ctx, nil, driftKinds)
	if err != nil || len(got) != 0 {
		t.Errorf("LastItemResults(nil) = %v, %v; want empty", got, err)
	}
}

func TestLastItemResultsUsesNameIndex(t *testing.T) {
	s := storetest.New(t)
	conn, err := store.Acquire(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var def string
	if err := conn.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'job_items_by_name'`).Scan(&def); err != nil {
		t.Fatalf("index job_items_by_name: %v", err)
	}
	// With sequential scans priced out, the plan must reach job_items through
	// the name index rather than reading the whole table.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN (FORMAT JSON) `+store.LastItemResultsSQL, []string{"team01-dc"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	var plan json.RawMessage
	for rows.Next() {
		if err := rows.Scan(&plan); err != nil {
			t.Fatal(err)
		}
	}
	rows.Close()
	if !strings.Contains(string(plan), `"Index Name": "job_items_by_name"`) {
		t.Errorf("plan doesn't use job_items_by_name:\n%s", plan)
	}
}

// A power job doesn't converge a VM's config, so it neither clears a failed
// deploy's result nor hides a deploy's from the drift scan.
func TestLastItemResultsSkipsPowerJobs(t *testing.T) {
	s := storetest.New(t)
	dep := runJob(t, s, "deploy", items("team01-dc", "team01-web"), []string{"team01-dc", "team01-web"},
		store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
			"team01-dc":  {Status: store.ItemFailed, Error: "network: bridge missing"},
			"team01-web": {Status: store.ItemDone},
		})
	runJob(t, s, "power", items("team01-dc", "team01-web"), []string{"team01-dc", "team01-web"},
		store.StatusSucceeded, map[string]store.ItemOutcome{
			"team01-dc":  {Status: store.ItemDone},
			"team01-web": {Status: store.ItemDone},
		})
	// A failed power job doesn't count either.
	runJob(t, s, "power", items("team01-web"), []string{"team01-web"},
		store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
			"team01-web": {Status: store.ItemFailed, Error: "start: locked"},
		})
	got, err := s.LastItemResults(ctx, []string{"team01-dc", "team01-web"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["team01-dc"]; r.JobID != dep || r.JobKind != "deploy" || r.Status != store.ItemFailed {
		t.Errorf("team01-dc = %+v, want the failed deploy %d", r, dep)
	}
	if r := got["team01-web"]; r.JobID != dep || r.Status != store.ItemDone {
		t.Errorf("team01-web = %+v, want the deploy %d", r, dep)
	}

	// A later successful reset clears the failed deploy.
	rst := runJob(t, s, "reset", items("team01-dc"), []string{"team01-dc"}, store.StatusSucceeded,
		map[string]store.ItemOutcome{"team01-dc": {Status: store.ItemDone}})
	got, err = s.LastItemResults(ctx, []string{"team01-dc"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["team01-dc"]; r.JobID != rst || r.Status != store.ItemDone {
		t.Errorf("team01-dc after a reset = %+v, want done in reset %d", r, rst)
	}
}

// Snapshot jobs don't count, whether they succeed or fail.
func TestLastItemResultsSkipsSnapshotJobs(t *testing.T) {
	s := storetest.New(t)
	dep := runJob(t, s, "deploy", items("team01-dc", "team01-web"), []string{"team01-dc", "team01-web"},
		store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
			"team01-dc":  {Status: store.ItemFailed, Error: "network: bridge missing"},
			"team01-web": {Status: store.ItemDone},
		})
	runJob(t, s, "snapshot", items("team01-dc", "team01-web"), []string{"team01-dc", "team01-web"},
		store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
			"team01-dc":  {Status: store.ItemDone},
			"team01-web": {Status: store.ItemFailed, Error: "snapshot: locked"},
		})
	got, err := s.LastItemResults(ctx, []string{"team01-dc", "team01-web"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["team01-dc"]; r.JobID != dep || r.Status != store.ItemFailed {
		t.Errorf("team01-dc = %+v, want the failed deploy %d", r, dep)
	}
	if r := got["team01-web"]; r.JobID != dep || r.Status != store.ItemDone {
		t.Errorf("team01-web = %+v, want the deploy %d", r, dep)
	}
}

// Every non-power job's outcome counts: a VM a teardown failed on is left
// drifted, and a teardown that removed a VM clears an older failure.
func TestLastItemResultsCountsTeardowns(t *testing.T) {
	s := storetest.New(t)
	runJob(t, s, "deploy", items("team01-dc", "team01-web"), []string{"team01-dc", "team01-web"},
		store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
			"team01-dc":  {Status: store.ItemFailed, Error: "network: bridge missing"},
			"team01-web": {Status: store.ItemDone},
		})
	td := runJob(t, s, "teardown", items("team01-dc", "team01-web", "team01-mail"),
		[]string{"team01-dc", "team01-web", "team01-mail"},
		store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
			"team01-dc":   {Status: store.ItemDone},
			"team01-web":  {Status: store.ItemFailed, Error: "destroy: locked"},
			"team01-mail": {Status: store.ItemFailed, Error: "destroy: locked"},
		})
	got, err := s.LastItemResults(ctx, []string{"team01-dc", "team01-web", "team01-mail"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["team01-dc"]; r.JobID != td || r.JobKind != "teardown" || r.Status != store.ItemDone {
		t.Errorf("team01-dc = %+v, want done in teardown %d (clearing the failed deploy)", r, td)
	}
	for _, name := range []string{"team01-web", "team01-mail"} {
		if r := got[name]; r.JobID != td || r.JobKind != "teardown" || r.Status != store.ItemFailed || r.Error != "destroy: locked" {
			t.Errorf("%s = %+v, want failed in teardown %d", name, r, td)
		}
	}
}

func TestNow(t *testing.T) {
	s := storetest.New(t)
	before := time.Now()
	now, err := s.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The test server runs on this host, so its clock is this one's.
	if d := now.Sub(before); d < -5*time.Second || d > 5*time.Second {
		t.Errorf("Now = %v, %v from the local clock", now, d)
	}
}

func TestLastJobOf(t *testing.T) {
	s := storetest.New(t)
	if _, _, err := s.LastJobOf(ctx, "team01-dc"); err != store.ErrNotFound {
		t.Fatalf("LastJobOf before any job: err = %v, want ErrNotFound", err)
	}
	j1 := runJob(t, s, "deploy", items("team01-dc", "team01-web"), []string{"team01-dc"},
		store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
			"team01-dc": {Status: store.ItemFailed, Error: "network: bridge missing"},
		})
	j, it, err := s.LastJobOf(ctx, "team01-dc")
	if err != nil || j.ID != j1 || it.Status != store.ItemFailed || it.Error != "network: bridge missing" {
		t.Fatalf("LastJobOf = job %d item %+v, %v; want job %d, failed", j.ID, it, err, j1)
	}
	// A power job counts, and so does one still pending.
	nj := newJob("team:01")
	nj.Kind = "power"
	nj.Items = items("team01-dc")
	id, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	j, it, err = s.LastJobOf(ctx, "team01-dc")
	if err != nil || j.ID != id || j.Kind != "power" || j.Status != store.StatusPending || it.Status != store.ItemPending {
		t.Fatalf("LastJobOf = job %d %s %s item %s, %v; want pending power job %d", j.ID, j.Kind, j.Status, it.Status, err, id)
	}
	if j, _, err := s.LastJobOf(ctx, "team01-web"); err != nil || j.ID != j1 {
		t.Fatalf("LastJobOf(team01-web) = job %d, %v; want %d", j.ID, err, j1)
	}
}

// A LeftUntouched interruption doesn't count, so the job before still does;
// part-way or converged interruptions count.
func TestLastItemResultsSkipsUntouchedInterruptions(t *testing.T) {
	s := storetest.New(t)
	names := items("team01-dc", "team01-web", "team01-mail")
	dep := runJob(t, s, "deploy", names, []string{"team01-dc", "team01-web", "team01-mail"},
		store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
			"team01-dc":   {Status: store.ItemFailed, Error: "network: bridge missing"},
			"team01-web":  {Status: store.ItemFailed, Error: "network: bridge missing"},
			"team01-mail": {Status: store.ItemFailed, Error: "network: bridge missing"},
		})
	nj := newJob("team:01")
	nj.Kind = "reset"
	nj.Items = names
	rs, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(ctx, rs, "w"); err != nil {
		t.Fatal(err)
	}
	for name, step := range map[string]string{"team01-dc": "rollback", "team01-web": "rollback", "team01-mail": "start"} {
		if err := s.AddEvent(ctx, rs, store.Event{At: time.Now(), Item: name, Step: step, Status: "interrupted"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(ctx, rs, "w", store.Outcome{Status: store.StatusCancelled, Items: map[string]store.ItemOutcome{
		"team01-dc":   {Status: store.ItemInterrupted, Error: "cancelled before rollback", LeftConfig: store.LeftUntouched},
		"team01-web":  {Status: store.ItemInterrupted, Error: "cancelled during rollback"},
		"team01-mail": {Status: store.ItemInterrupted, Error: "cancelled before start", LeftConfig: store.LeftConverged},
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastItemResults(ctx, []string{"team01-dc", "team01-web", "team01-mail"}, driftKinds)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["team01-dc"]; r.JobID != dep || r.Status != store.ItemFailed {
		t.Errorf("team01-dc = %+v, want the deploy %d's failure", r, dep)
	}
	if r := got["team01-web"]; r.JobID != rs || r.Status != store.ItemInterrupted || r.Step != "rollback" || r.LeftConfig != "" {
		t.Errorf("team01-web = %+v, want interrupted at rollback in reset %d", r, rs)
	}
	if r := got["team01-mail"]; r.JobID != rs || r.LeftConfig != store.LeftConverged {
		t.Errorf("team01-mail = %+v, want converged in reset %d", r, rs)
	}
}
