package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func TestWorkerActsOnlyAsTheSubmitter(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	newWorker(st, f).RunJob(bg, claim(t, st))
	if j := job(t, st, id); j.Status != store.StatusSucceeded {
		t.Fatalf("job = %s: %s", j.Status, j.Error)
	}
	used := f.usedCreds()
	if len(used) == 0 {
		t.Fatal("no call went through the job's credential")
	}
	for _, c := range used {
		if c.User != "alice@auth.example.org" || c.Ticket != "PVE:alice:1" {
			t.Fatalf("a call carried %v, not the submitter's ticket", c)
		}
	}
	if _, err := st.JobCredential(bg, id); err == nil {
		t.Fatal("a finished job kept its credential")
	}
}

// A job whose credential lapsed before it started changes nothing.
func TestWorkerPendingJobWithLapsedCredentialIsStale(t *testing.T) {
	cases := map[string]func(t *testing.T, st *store.Store, id int64){
		"dropped by the renewer": func(t *testing.T, st *store.Store, id int64) {
			r := &fakeRenewer{valid: map[string]bool{}}
			newRenewer(st, mustCreds(), r, time.Now().Add(61*time.Minute)).RenewDue(bg)
		},
		"expired": nil, // the job sits past the ticket's 2h with nobody renewing
	}
	for name, lapse := range cases {
		t.Run(name, func(t *testing.T) {
			st := storetest.New(t)
			f := teamVMs()
			id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
			w := newWorker(st, f)
			if lapse != nil {
				lapse(t, st, id)
			} else {
				w.now = func() time.Time { return time.Now().Add(proxmox.TicketLifetime + time.Minute) }
			}
			w.RunJob(bg, claim(t, st))
			j := job(t, st, id)
			if j.Status != store.StatusStale || j.Error != "authorization lapsed; preview it again" {
				t.Fatalf("job = %s: %q", j.Status, j.Error)
			}
			if n := len(f.usedCreds()); n != 0 {
				t.Fatalf("a lapsed job made %d calls", n)
			}
		})
	}
}

// Proxmox refusing the ticket while the job runs (the user was disabled,
// or the ticket ran out) stops the job: no new steps, cleanup, and the job
// ends interrupted.
func TestWorkerRunningJobStopsWhenProxmoxRefusesTheTicket(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate, f.waiting = make(chan struct{}), make(chan struct{}, 10)
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	w.Cfg.Concurrency.Workers = 1
	done := make(chan struct{})
	go func() { defer close(done); w.RunJob(bg, claim(t, st)) }()
	<-f.waiting // the first VM's first task is running
	f.Mu.Lock()
	f.accept = func(proxmox.Credential) bool { return false } // alice is disabled in Proxmox
	f.Mu.Unlock()
	close(f.gate)
	<-done
	j := job(t, st, id)
	if j.Status != store.StatusInterrupted || !strings.Contains(j.Error, "authorization lapsed") {
		t.Fatalf("job = %s: %q", j.Status, j.Error)
	}
	if got := f.DeletedIDs(); len(got) != 0 {
		t.Fatalf("deleted %v after the ticket was refused", got)
	}
	items, _ := st.Items(bg, id)
	for _, it := range items {
		if it.Status == store.ItemDone {
			t.Fatalf("item %s done after the ticket was refused", it.Name)
		}
	}
}

// The renewer dropping the ticket (Proxmox refused to renew it, or the
// login passed proxmox.ticket_max_age) stops a running job too.
func TestWorkerRunningJobStopsWhenItsCredentialIsDropped(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate, f.waiting = make(chan struct{}), make(chan struct{}, 10)
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	w.Cfg.Concurrency.Workers = 1
	done := make(chan struct{})
	go func() { defer close(done); w.RunJob(bg, claim(t, st)) }()
	<-f.waiting
	r := &fakeRenewer{valid: map[string]bool{}}
	if _, lapsed := newRenewer(st, mustCreds(), r, time.Now().Add(61*time.Minute)).RenewDue(bg); lapsed != 1 {
		t.Fatal("the renewer didn't drop the ticket")
	}
	// The job stops by itself within a heartbeat or so, abandoning the
	// wait; the gate never opens.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the job kept running after its credential was dropped")
	}
	j := job(t, st, id)
	if j.Status != store.StatusInterrupted || !strings.Contains(j.Error, "authorization lapsed") {
		t.Fatalf("job = %s: %q", j.Status, j.Error)
	}
	if got := f.DeletedIDs(); len(got) > 1 {
		t.Fatalf("deleted %v: the job kept starting steps after its credential was dropped", got)
	}
}

// A ticket the renewer renews mid-job is used from then on.
func TestWorkerPicksUpTheRenewedTicket(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	f.gate, f.waiting = make(chan struct{}), make(chan struct{}, 10)
	id := submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	w.Cfg.Concurrency.Workers = 1
	done := make(chan struct{})
	go func() { defer close(done); w.RunJob(bg, claim(t, st)) }()
	<-f.waiting
	r := &fakeRenewer{valid: map[string]bool{"PVE:alice:1": true}}
	if renewed, _ := newRenewer(st, mustCreds(), r, time.Now().Add(61*time.Minute)).RenewDue(bg); renewed != 1 {
		t.Fatal("not renewed")
	}
	time.Sleep(10 * w.Cfg.Jobs.Heartbeat) // the worker reloads it each heartbeat
	close(f.gate)
	<-done
	if j := job(t, st, id); j.Status != store.StatusSucceeded {
		t.Fatalf("job = %s: %s", j.Status, j.Error)
	}
	used := f.usedCreds()
	if last := used[len(used)-1]; last.Ticket != "PVE:alice:1+r" {
		t.Fatalf("the last call carried %q, not the renewed ticket", last.Ticket)
	}
}

// withPrivileges gives f a Proxmox access layer where alice may power
// only team01-dc, and returns a fresh ticket of hers.
func withPrivileges(t *testing.T, f *fakeAPI) proxmox.Credential {
	t.Helper()
	f.pve = pvetest.New(t)
	f.pve.AddUser("alice@auth.example.org", nil)
	f.pve.Grant("alice@auth.example.org", "/vms", "VM.Audit")
	f.pve.Grant("alice@auth.example.org", "/vms/10105", "VM.Audit", "VM.PowerMgmt")
	return f.pve.Ticket("alice@auth.example.org")
}

// submitAs previews in with cred's privileges, as the web app does, and
// stores the job with cred.
func submitAs(t *testing.T, st *store.Store, f *fakeAPI, in Inputs, cred proxmox.Credential) (int64, *pods.Plan) {
	t.Helper()
	cfg := testCfg()
	p, err := BuildPlan(bg, pods.NewPlanner(f.as(func() proxmox.Credential { return cred }, nil), cfg), in)
	if err != nil {
		t.Fatal(err)
	}
	id, err := Submit(bg, st, in, p, Submitter{User: "alice", Credential: cred, Seal: mustCreds()})
	if err != nil {
		t.Fatal(err)
	}
	return id, p
}

func TestWorkerPlansWithTheSubmittersPrivileges(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	cred := withPrivileges(t, f)
	id, plan := submitAs(t, st, f, Inputs{Kind: pods.KindPower, Teams: "1", Action: "stop"}, cred)
	if r := plan.Runnable(); len(r) != 1 || r[0].Name != "team01-dc" {
		t.Fatalf("runnable = %+v", r)
	}
	for _, it := range plan.Items {
		if it.Name == "team01-teak" && it.Blocked != "you don't have VM.PowerMgmt on /vms/10121" {
			t.Fatalf("teak blocked %q", it.Blocked)
		}
	}
	newWorker(st, f).RunJob(bg, claim(t, st))
	j := job(t, st, id)
	if j.Status != store.StatusCompletedWithFailures {
		t.Fatalf("job = %s: %s", j.Status, j.Error)
	}
	f.Mu.Lock()
	teak, dc := f.VMs[10121].Status, f.VMs[10105].Status
	f.Mu.Unlock()
	if teak != "running" || dc != "stopped" {
		t.Fatalf("teak %s, dc %s: the job acted beyond the submitter's privileges", teak, dc)
	}
}

// Privileges that change between preview and run make the job stale, and
// say so.
func TestWorkerStaleWhenPrivilegesChanged(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	cred := withPrivileges(t, f)
	id, _ := submitAs(t, st, f, Inputs{Kind: pods.KindPower, Teams: "1", Action: "stop"}, cred)
	f.pve.Revoke("alice@auth.example.org", "/vms/10105")
	newWorker(st, f).RunJob(bg, claim(t, st))
	j := job(t, st, id)
	if j.Status != store.StatusStale || !strings.Contains(j.Error, "your Proxmox privileges changed since the preview") {
		t.Fatalf("job = %s: %q", j.Status, j.Error)
	}
}
