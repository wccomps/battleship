package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
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
	if len(e.api.Deleted) != 0 {
		t.Errorf("queueing deleted %v; only the worker should", e.api.Deleted)
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

// startWorker runs a job worker on e's store and fake Proxmox until the
// test ends, as battleship serve would.
func startWorker(t *testing.T, e *env, st *store.Store) {
	t.Helper()
	cfg, err := config.Load(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := jobs.OpenCredentials(context.Background(), cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w := &jobs.Worker{Store: st, Cfg: cfg, Bind: bindAs(e.d.newAPI(cfg.Proxmox)), Credentials: creds,
		ID: jobs.NewWorkerID("test"), Logf: t.Logf}
	go func() { defer close(done); w.Run(ctx) }() //nolint:errcheck
	t.Cleanup(func() { cancel(); <-done })
}

func TestDirectRunFollowsQueuedJob(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	st := withStore(t, e)
	fastPoll(t, e)
	startWorker(t, e, st)

	if code := e.run("teardown", "-teams", "7", "-yes"); code != 0 {
		t.Fatalf("exit %d, stdout:\n%s\nstderr: %s", code, e.stdout.String(), e.stderr.String())
	}
	j, err := st.Job(context.Background(), 1)
	if err != nil || j.Status != store.StatusSucceeded || !isWorkerID(j.ClaimedBy, "test") || j.CreatedBy != "cli:tester" {
		t.Fatalf("job = %s, claimed by %q, created by %q, %v", j.Status, j.ClaimedBy, j.CreatedBy, err)
	}
	if got := e.api.DeletedIDs(); len(got) != 1 || got[0] != 10701 {
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
	fastPoll(t, e)
	startWorker(t, e, st)

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

// With no worker running, Ctrl-C cancels the job the CLI is following.
func TestCtrlCCancelsPendingJob(t *testing.T) {
	e := newEnv(t, false, "", vm(10101, "team01-dc"))
	st := withStore(t, e)
	fastPoll(t, e)
	out := &syncBuffer{}
	e.d.stdout = out

	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan int, 1)
	go func() { exit <- run(ctx, []string{"teardown", "-teams", "1", "-yes", "-config", e.cfg}, e.d) }()
	waitFor(t, "the job to be queued", func() bool { return strings.Contains(out.String(), "Job 1 queued") })
	cancel()
	select {
	case code := <-exit:
		if code == 0 {
			t.Error("exit 0 after Ctrl-C")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the CLI kept following after Ctrl-C")
	}
	if j, _ := st.Job(context.Background(), 1); j.Status != store.StatusCancelled {
		t.Errorf("job = %s, want cancelled", j.Status)
	}
	if got := e.api.DeletedIDs(); len(got) != 0 {
		t.Errorf("deleted %v", got)
	}
}
