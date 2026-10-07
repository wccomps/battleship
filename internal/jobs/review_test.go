package jobs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// Item 6: a 401 in the last round (here the only one) still ends the job
// interrupted as "authorization lapsed", not completed with failures.
func TestLapseInTheLastRoundIsInterrupted(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	id := submit(t, st, f, Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "stop"})
	// Proxmox stops accepting the ticket right after the power call: the
	// task wait, the item's last call, gets the 401.
	f.accept = func(proxmox.Credential) bool {
		f.Mu.Lock()
		defer f.Mu.Unlock()
		return f.VMs[10105].Status != "stopped"
	}
	w := newWorker(st, f)
	w.Cfg.Retry.Rounds = 0
	w.RunJob(bg, claim(t, st))
	j := job(t, st, id)
	if j.Status != store.StatusInterrupted || !strings.Contains(j.Error, "authorization lapsed") {
		t.Fatalf("job = %s: %q %s", j.Status, j.Error, j.Summary)
	}
}

// Item 7: a job reaped while it runs (its credential deleted by the
// trigger) is a claim this worker lost, not an authorization that lapsed.
func TestReapedJobIsALostClaimNotALapse(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate, f.waiting = make(chan struct{}), make(chan struct{}, 10)
	submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	var logs syncLog
	w.Logf = logs.add
	var reaped atomic.Bool
	beats := 0
	w.beat = func(ctx context.Context, jobID int64, worker string) (bool, error) {
		if !reaped.Load() {
			return false, nil // never updates heartbeat_at, so the job can be reaped
		}
		if beats++; beats < 5 { // the watcher gets a few looks first
			return false, nil
		}
		return false, store.ErrLostClaim
	}
	done := make(chan struct{})
	go func() { defer close(done); w.RunJob(bg, claim(t, st)) }()
	<-f.waiting
	time.Sleep(50 * time.Millisecond)
	if ids, err := st.ReapStale(bg, 10*time.Millisecond); err != nil || len(ids) != 1 {
		t.Fatalf("reaped %v, %v", ids, err)
	}
	reaped.Store(true)
	close(f.gate)
	<-done
	if l := logs.String(); strings.Contains(l, "credential is gone") || strings.Contains(l, "authorization lapsed") {
		t.Fatalf("a reaped job was taken for a lapse:\n%s", l)
	}
}

type syncLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *syncLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// Item 8: a pass renews several tickets at once, so a backlog of due
// tickets can't outlast the hour before they expire.
func TestRenewerRenewsInParallel(t *testing.T) {
	st := storetest.New(t)
	creds := testCreds(t)
	valid := map[string]bool{}
	for i := range 16 {
		cred := aliceTicket()
		cred.Ticket = fmt.Sprintf("PVE:alice:%d", i)
		cred.Issued = time.Now().Add(-61 * time.Minute)
		valid[cred.Ticket] = true
		storeJobWith(t, st, creds, cred)
	}
	r := &fakeRenewer{valid: valid, delay: 100 * time.Millisecond}
	start := time.Now()
	renewed, _ := newRenewer(st, creds, r, time.Now()).RenewDue(bg)
	if renewed != 16 {
		t.Fatalf("renewed %d", renewed)
	}
	if took := time.Since(start); took > 800*time.Millisecond {
		t.Fatalf("16 renewals of 100ms took %s: one at a time", took)
	}
}

// credsWithKey seals with another seal key, as a misconfigured replica.
func credsWithKey(t *testing.T, key string) Credentials {
	t.Helper()
	cfg := testCfg()
	cfg.Database.SealKey = key
	c, err := NewCredentials(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const otherSealKey = "a different seal key, also long enough!!"

// Item 11: a replica with the wrong database.seal_key can't open the
// tickets; it must not take that for a lapse and delete them.
func TestRenewerWithTheWrongKeyDeletesNothing(t *testing.T) {
	st := storetest.New(t)
	cred := aliceTicket()
	cred.Issued = time.Now().Add(-61 * time.Minute)
	id := storeJobWith(t, st, testCreds(t), cred)
	var logs syncLog
	r := &Renewer{Store: st, Proxmox: &fakeRenewer{valid: map[string]bool{cred.Ticket: true}},
		Credentials: credsWithKey(t, otherSealKey), Logf: logs.add}
	if renewed, lapsed := r.RenewDue(bg); renewed+lapsed != 0 {
		t.Fatalf("renewed %d, lapsed %d", renewed, lapsed)
	}
	if _, err := st.JobCredential(bg, id); err != nil {
		t.Fatalf("the credential was dropped: %v", err)
	}
	if !strings.Contains(logs.String(), "database.seal_key") {
		t.Fatalf("logs don't name the seal key:\n%s", logs.String())
	}
}

// Item 11: a worker with the wrong key fails the job saying so; it isn't
// "authorization lapsed".
func TestWorkerWithTheWrongKeyFailsNotLapsed(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	w.Credentials = credsWithKey(t, otherSealKey)
	w.RunJob(bg, claim(t, st))
	j := job(t, st, id)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "database.seal_key") || strings.Contains(j.Error, "lapsed") {
		t.Fatalf("job = %s: %q", j.Status, j.Error)
	}
}

// Item 11: processes check their seal key against the one the database
// was first used with, so a wrong key fails at startup.
func TestSealKeyCheck(t *testing.T) {
	st := storetest.New(t)
	if err := testCreds(t).CheckKey(bg, st); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if err := testCreds(t).CheckKey(bg, st); err != nil {
		t.Fatalf("same key again: %v", err)
	}
	err := credsWithKey(t, otherSealKey).CheckKey(bg, st)
	if err == nil || !strings.Contains(err.Error(), "database.seal_key") {
		t.Fatalf("another key: %v", err)
	}
}
