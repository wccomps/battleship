package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// withStore gives e a job database. Commands share it and don't close it.
func withStore(t *testing.T, e *env) *store.Store {
	t.Helper()
	st := storetest.New(t)
	t.Setenv("BATTLESHIP_DATABASE_URL", "postgres://battleship@db.example/battleship")
	e.d.openStore = func(context.Context, config.Database) (*store.Store, func(), error) { return st, func() {}, nil }
	e.d.user = "cli:tester"
	return st
}

func (e *env) reset() { e.stdout.Reset(); e.stderr.Reset() }

func TestQueueStoresConfirmedPlan(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	st := withStore(t, e)

	if code := e.run("teardown", "-teams", "7", "-yes", "-queue"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, e.stderr.String())
	}
	if !strings.Contains(e.stdout.String(), "Queued job 1.") {
		t.Errorf("stdout = %s", e.stdout.String())
	}
	if len(e.api.deleted) != 0 {
		t.Errorf("queueing deleted %v; only the worker should", e.api.deleted)
	}
	list, err := st.Jobs(context.Background(), 10)
	if err != nil || len(list) != 1 || list[0].Status != store.StatusPending || list[0].CreatedBy != "cli:tester" {
		t.Fatalf("jobs = %+v, %v", list, err)
	}
	items, _ := st.Items(context.Background(), list[0].ID)
	if len(items) != 1 || items[0].Name != "team07-dc" {
		t.Errorf("items = %+v", items)
	}
}

func TestQueueNeedsDatabase(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	t.Setenv("BATTLESHIP_DATABASE_URL", "")
	if code := e.run("teardown", "-teams", "7", "-yes", "-queue"); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(e.stderr.String(), "-queue: database.url") {
		t.Errorf("stderr = %s", e.stderr.String())
	}
}

func TestJobsListShowCancel(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	withStore(t, e)
	if code := e.run("teardown", "-teams", "7", "-yes", "-queue"); code != 0 {
		t.Fatalf("queue: exit %d", code)
	}

	e.reset()
	if code := e.run("jobs", "list"); code != 0 || !strings.Contains(e.stdout.String(), "teams 7") {
		t.Errorf("list: exit %d, stdout:\n%s", code, e.stdout.String())
	}
	e.reset()
	if code := e.run("jobs", "show", "1"); code != 0 {
		t.Fatalf("show: exit %d, stderr: %s", code, e.stderr.String())
	}
	for _, want := range []string{"Job 1: teardown teams 7 (pending)", "team07-dc", "Created by cli:tester"} {
		if !strings.Contains(e.stdout.String(), want) {
			t.Errorf("show missing %q:\n%s", want, e.stdout.String())
		}
	}
	e.reset()
	if code := e.run("jobs", "cancel", "1"); code != 0 || !strings.Contains(e.stdout.String(), "cancelled before it started") {
		t.Errorf("cancel: exit %d, stdout: %s", code, e.stdout.String())
	}
	e.reset()
	if code := e.run("jobs", "cancel", "1"); code != 1 || !strings.Contains(e.stderr.String(), "already finished") {
		t.Errorf("second cancel: exit %d, stderr: %s", code, e.stderr.String())
	}
	e.reset()
	if code := e.run("jobs", "show", "99"); code != 1 {
		t.Errorf("show missing job: exit %d", code)
	}
}

func TestJobsArgErrors(t *testing.T) {
	e := newEnv(t, false, "")
	withStore(t, e)
	for _, args := range [][]string{
		{"jobs"},
		{"jobs", "explode"},
		{"jobs", "show"},
		{"jobs", "show", "abc"},
		{"jobs", "show", "1", "2"},
		{"jobs", "list", "extra"},
	} {
		e.reset()
		if code := run(context.Background(), append(args, "-config", e.cfg), e.d); code != 2 {
			t.Errorf("run(%v) = %d, want 2 (stderr: %s)", args, code, e.stderr.String())
		}
	}
}

func TestWorkerCommandRunsQueuedJob(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	st := withStore(t, e)
	if code := e.run("teardown", "-teams", "7", "-yes", "-queue"); code != 0 {
		t.Fatalf("queue: exit %d", code)
	}

	// The worker acts only as each job's submitter, never with a token of
	// its own.
	t.Setenv("BATTLESHIP_PROXMOX_TOKEN_ID", "")
	t.Setenv("BATTLESHIP_PROXMOX_TOKEN_SECRET", "")
	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan int, 1)
	go func() { exit <- run(ctx, []string{"worker", "-config", e.cfg}, e.d) }()
	deadline := time.Now().Add(15 * time.Second)
	var j store.Job
	for time.Now().Before(deadline) {
		var err error
		if j, err = st.Job(context.Background(), 1); err == nil && !j.Active() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if code := <-exit; code != 0 {
		t.Errorf("worker exit %d, stderr: %s", code, e.stderr.String())
	}
	if j.Status != store.StatusSucceeded {
		t.Fatalf("job status = %s (%s)", j.Status, j.Error)
	}
	if !isWorkerID(j.ClaimedBy, "worker") || !strings.Contains(e.stdout.String(), "worker "+j.ClaimedBy+" waiting for jobs") {
		t.Errorf("claimed by %q, stdout:\n%s", j.ClaimedBy, e.stdout.String())
	}
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	if len(e.api.deleted) != 1 || e.api.deleted[0] != 10701 {
		t.Errorf("deleted = %v", e.api.deleted)
	}
}

// isWorkerID reports whether id is a jobs.NewWorkerID(prefix) from this
// process.
func isWorkerID(id, prefix string) bool {
	host, _ := os.Hostname()
	rest, ok := strings.CutPrefix(id, fmt.Sprintf("%s-%s-%d-", prefix, host, os.Getpid()))
	return ok && regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(rest)
}

// syncBuffer is safe to read while a command writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// fastPoll makes e's config poll the job database every 100ms.
func fastPoll(t *testing.T, e *env) {
	t.Helper()
	f, err := os.OpenFile(e.cfg, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("[jobs]\npoll = \"100ms\"\n"); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond until it holds, or fails the test after 15s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fakeAPI) deletedIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.deleted...)
}

func TestDirectRunGoesThroughJobs(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	st := withStore(t, e)

	if code := e.run("teardown", "-teams", "7", "-yes"); code != 0 {
		t.Fatalf("exit %d, stdout:\n%s\nstderr: %s", code, e.stdout.String(), e.stderr.String())
	}
	j, err := st.Job(context.Background(), 1)
	if err != nil || j.Status != store.StatusSucceeded || !isWorkerID(j.ClaimedBy, "cli") || j.CreatedBy != "cli:tester" {
		t.Fatalf("job = %s, claimed by %q, created by %q, %v", j.Status, j.ClaimedBy, j.CreatedBy, err)
	}
	if got := e.api.deletedIDs(); len(got) != 1 || got[0] != 10701 {
		t.Errorf("deleted = %v", got)
	}
	out := e.stdout.String()
	for _, want := range []string{"Job 1 queued", "team07-dc delete", "Job 1 succeeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestDirectRunReportsFailures(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"), vm(10702, "team07-web"))
	e.api.deleteErr[10702] = &proxmox.APIError{Status: 500, Message: "disk on fire"}
	st := withStore(t, e)

	if code := e.run("teardown", "-teams", "7", "-yes"); code != 1 {
		t.Fatalf("exit %d, want 1; stdout:\n%s", code, e.stdout.String())
	}
	if j, _ := st.Job(context.Background(), 1); j.Status != store.StatusCompletedWithFailures {
		t.Errorf("job status = %s", j.Status)
	}
	if out := e.stdout.String(); !strings.Contains(out, "1 failed:") || !strings.Contains(out, "team07-web: ") {
		t.Errorf("stdout:\n%s", out)
	}
}

// startBlocker stores an older job on team 01 that no worker runs.
func startBlocker(t *testing.T, st *store.Store) int64 {
	t.Helper()
	id, err := st.CreateJob(context.Background(), store.NewJob{
		Kind: "power", Inputs: json.RawMessage(`{"kind":"power","teams":"1","action":"stop"}`),
		Plan: json.RawMessage(`{}`), Fingerprint: "x", LockKeys: []string{"team:01"}, CreatedBy: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDirectRunWaitsForOverlappingJob(t *testing.T) {
	e := newEnv(t, false, "", vm(10101, "team01-dc"))
	st := withStore(t, e)
	fastPoll(t, e)
	out := &syncBuffer{}
	e.d.stdout = out
	blocker := startBlocker(t, st)

	exit := make(chan int, 1)
	go func() { exit <- e.run("teardown", "-teams", "1", "-yes") }()
	waitFor(t, "the direct run to report it is waiting", func() bool {
		return strings.Contains(out.String(), fmt.Sprintf("waiting for job(s) %d", blocker))
	})
	if !strings.Contains(out.String(), "(to give up on job 2: Ctrl-C, or battleship jobs cancel 2)") {
		t.Errorf("waiting message doesn't say how to cancel job 2:\n%s", out.String())
	}
	// Several polls go by; it must still not touch the team.
	time.Sleep(300 * time.Millisecond)
	if got := e.api.deletedIDs(); len(got) != 0 {
		t.Fatalf("deleted %v while job %d was active on the team", got, blocker)
	}
	if j, _ := st.Job(context.Background(), 2); j.Status != store.StatusPending {
		t.Fatalf("direct job = %s, want pending", j.Status)
	}
	if err := st.RequestCancel(context.Background(), blocker, "alice"); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit %d, stdout:\n%s\nstderr: %s", code, out.String(), e.stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("direct run didn't proceed after the blocker was cancelled")
	}
	if got := e.api.deletedIDs(); len(got) != 1 || got[0] != 10101 {
		t.Errorf("deleted = %v", got)
	}
	if strings.Count(out.String(), "waiting for job(s)") != 1 {
		t.Errorf("waiting message repeated though the blockers didn't change:\n%s", out.String())
	}
}

func TestCtrlCWhileWaitingCancelsJob(t *testing.T) {
	e := newEnv(t, false, "", vm(10101, "team01-dc"))
	st := withStore(t, e)
	fastPoll(t, e)
	out := &syncBuffer{}
	e.d.stdout = out
	startBlocker(t, st)

	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan int, 1)
	go func() { exit <- run(ctx, []string{"teardown", "-teams", "1", "-yes", "-config", e.cfg}, e.d) }()
	waitFor(t, "the direct run to wait", func() bool { return strings.Contains(out.String(), "waiting for job(s)") })
	cancel()
	select {
	case code := <-exit:
		if code == 0 {
			t.Error("exit 0 after Ctrl-C")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("direct run kept waiting after Ctrl-C")
	}
	if j, _ := st.Job(context.Background(), 2); j.Status != store.StatusCancelled {
		t.Errorf("direct job = %s, want cancelled", j.Status)
	}
	if got := e.api.deletedIDs(); len(got) != 0 {
		t.Errorf("deleted %v", got)
	}
}

// A direct run that was force-quit leaves its job running with no heartbeat.
// Without any worker to reap it, the next direct run must reap it itself.
func TestDirectRunReapsDeadJobWithoutWorker(t *testing.T) {
	e := newEnv(t, false, "", vm(10101, "team01-dc"))
	st := withStore(t, e)
	f, err := os.OpenFile(e.cfg, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[jobs]\npoll = \"100ms\"\nheartbeat = \"1s\"\nstale_after = \"5s\"\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	dead := startBlocker(t, st)
	if j, err := st.Claim(context.Background(), "dead-cli"); err != nil || j == nil || j.ID != dead {
		t.Fatalf("Claim = %+v, %v", j, err)
	}

	exit := make(chan int, 1)
	go func() { exit <- e.run("teardown", "-teams", "1", "-yes") }()
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit %d, stderr: %s", code, e.stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("direct run waited forever behind a dead job")
	}
	if j, _ := st.Job(context.Background(), dead); j.Status != store.StatusInterrupted {
		t.Errorf("dead job = %s, want interrupted", j.Status)
	}
	if j, _ := st.Job(context.Background(), dead+1); j.Status != store.StatusSucceeded {
		t.Errorf("direct job = %s, want succeeded", j.Status)
	}
	if got := e.api.deletedIDs(); len(got) != 1 || got[0] != 10101 {
		t.Errorf("deleted = %v", got)
	}
}
