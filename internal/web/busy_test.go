package web

import (
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// A flood of job log notices (via Postgres, as across replicas) reads no
// busy VMs and sends grid streams nothing; job start and end still do.
func TestLogLinesDontWakeGridStreams(t *testing.T) {
	h := newListeningHarness(t)
	h.poll()
	op := h.login(asOperator)
	var views int64
	c := h.openSSE(&op, "/events/grid")
	if ev := c.next(); ev.Event != "grid" {
		t.Fatalf("first event %+v, want the grid", ev)
	}

	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	ev := c.next()
	if ev.Event != "patch" {
		t.Fatalf("after a job was made: %+v, want a patch", ev)
	}
	contains(t, "patch after a job was made", ev.Data, `<a class="cell is-busy" href="/logs/`+itoa(id)+`"`)
	// The job starts: its marks stay as they were, so the stream reads the
	// busy VMs and sends nothing.
	reads := h.srv.busyReads.Load()
	h.start(id)
	for deadline := time.Now().Add(10 * time.Second); h.srv.busyReads.Load() == reads; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the job's start made no busy read within 10s")
		}
	}

	seq := h.hub.Seq()
	for range 50 {
		h.event(id, "team01-dc", "power", "started", "")
	}
	// Each log line's notice takes a sequence number once the hub has it.
	for deadline := time.Now().Add(10 * time.Second); h.hub.Seq() < seq+50; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the hub had %d of 50 log notices after 10s", h.hub.Seq()-seq)
		}
	}
	reads, views = h.srv.busyReads.Load(), h.srv.gridViews.Load()

	// The job ends: its marks go. The stream handles messages in order, so
	// by this patch it has handled anything the log lines woke it for.
	h.finish(id, store.Outcome{Status: store.StatusSucceeded})
	ev = c.next()
	if ev.Event != "patch" {
		t.Fatalf("after the job ended: %+v, want a patch", ev)
	}
	lacks(t, "patch after the job ended", ev.Data, `class="cell is-busy"`)
	if n := h.srv.busyReads.Load() - reads; n != 1 {
		t.Errorf("the job's end, after 50 log lines, made %d busy reads, want 1", n)
	}
	if n := h.srv.gridViews.Load() - views; n != 1 {
		t.Errorf("the job's end, after 50 log lines, built %d grid views, want 1", n)
	}
}
