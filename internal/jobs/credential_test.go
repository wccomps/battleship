package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

const testSealKey = "a test seal key that is long enough!!"

func testCreds(t *testing.T) Credentials {
	t.Helper()
	cfg := testCfg()
	cfg.Database.SealKey = testSealKey
	c, err := NewCredentials(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// aliceTicket is a fresh ticket of alice's.
func aliceTicket() proxmox.Credential {
	now := time.Now()
	c := proxmox.TicketCredential("alice@auth.example.org", "PVE:alice:1", "csrf-alice-1", now)
	c.LoginAt = now
	return c
}

func TestCredentialsSealForOneJob(t *testing.T) {
	creds := testCreds(t)
	in := aliceTicket()
	jc, err := creds.Seal(in)(7)
	if err != nil {
		t.Fatal(err)
	}
	jc.JobID = 7
	for _, secret := range []string{in.Ticket, in.CSRF} {
		if strings.Contains(jc.Sealed, secret) {
			t.Fatalf("sealed %q shows a secret", jc.Sealed)
		}
	}
	if jc.User != in.User || jc.RenewAfter.IsZero() {
		t.Fatalf("row = %+v", jc)
	}
	out, err := creds.Open(jc)
	if err != nil {
		t.Fatal(err)
	}
	if out.Ticket != in.Ticket || out.CSRF != in.CSRF || out.User != in.User {
		t.Fatalf("opened %v, sealed %v", out, in)
	}
	jc.JobID = 8
	if _, err := creds.Open(jc); err == nil {
		t.Fatal("a credential sealed for job 7 opened as job 8's")
	}
}

func TestRowLapsed(t *testing.T) {
	creds := testCreds(t)
	in := aliceTicket()
	at := in.Issued
	ticket, err := creds.row(7, in)
	if err != nil {
		t.Fatal(err)
	}
	if creds.RowLapsed(ticket, at.Add(time.Minute)) {
		t.Error("a fresh ticket's row lapsed")
	}
	if !creds.RowLapsed(ticket, at.Add(proxmox.TicketLifetime+time.Second)) {
		t.Error("an expired ticket's row didn't lapse")
	}
}

func TestNewCredentialsNeedsASealKey(t *testing.T) {
	if _, err := NewCredentials(testCfg()); err == nil {
		t.Fatal("NewCredentials without database.seal_key succeeded")
	}
}

func TestSubmitStoresTheSubmittersCredential(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	creds := testCreds(t)
	cfg := testCfg()
	in := Inputs{Kind: pods.KindTeardown, Teams: "1"}
	p, err := BuildPlan(bg, pods.NewPlanner(f, cfg), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Submit(bg, st, in, p, Submitter{User: "alice"}); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("Submit without a credential = %v, want ErrNoCredential", err)
	}
	cred := aliceTicket()
	id, err := Submit(bg, st, in, p, Submitter{User: "alice", Credential: cred, Seal: creds})
	if err != nil {
		t.Fatal(err)
	}
	jc, err := st.JobCredential(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := creds.Open(jc)
	if err != nil || got.Ticket != cred.Ticket {
		t.Fatalf("stored %v, %v", got, err)
	}
}

// fakeRenewer answers RenewTicket like Proxmox: tickets it knows are
// renewed, others refused with 401.
type fakeRenewer struct {
	mu      sync.Mutex
	valid   map[string]bool
	err     error // if set, every renewal fails with it
	renewed []string
	n       int
	delay   time.Duration // each renewal takes this long
}

func (r *fakeRenewer) RenewTicket(_ context.Context, c proxmox.Credential, now time.Time) (proxmox.Credential, error) {
	time.Sleep(r.delay)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renewed = append(r.renewed, c.Ticket)
	if r.err != nil {
		return proxmox.Credential{}, r.err
	}
	if !r.valid[c.Ticket] {
		return proxmox.Credential{}, &proxmox.APIError{Method: "POST", Path: "/access/ticket", Status: 401, Message: "authentication failure"}
	}
	r.n++
	next := proxmox.TicketCredential(c.User, c.Ticket+"+r", "csrf+r", now)
	next.LoginAt = c.LoginAt
	r.valid[next.Ticket] = true
	return next, nil
}

func (r *fakeRenewer) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.renewed...)
}

// storeJobWith stores a pending teardown job holding cred, issued at
// issued.
func storeJobWith(t *testing.T, st *store.Store, creds Credentials, cred proxmox.Credential) int64 {
	t.Helper()
	f := teamVMs()
	cfg := testCfg()
	in := Inputs{Kind: pods.KindTeardown, Teams: "1"}
	p, err := BuildPlan(bg, pods.NewPlanner(f, cfg), in)
	if err != nil {
		t.Fatal(err)
	}
	id, err := Submit(bg, st, in, p, Submitter{User: "alice", Credential: cred, Seal: creds})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newRenewer(st *store.Store, creds Credentials, r *fakeRenewer, now time.Time) *Renewer {
	return &Renewer{Store: st, Proxmox: r, Credentials: creds, Now: func() time.Time { return now }}
}

func TestRenewerRenewsDueTickets(t *testing.T) {
	st := storetest.New(t)
	creds := testCreds(t)
	cred := aliceTicket()
	cred.Issued = time.Now().Add(-61 * time.Minute)
	cred.LoginAt = cred.Issued
	id := storeJobWith(t, st, creds, cred)
	fresh := storeJobWith(t, st, creds, aliceTicket())
	r := &fakeRenewer{valid: map[string]bool{cred.Ticket: true}}

	renewed, lapsed := newRenewer(st, creds, r, time.Now()).RenewDue(bg)
	if renewed != 1 || lapsed != 0 {
		t.Fatalf("renewed %d, lapsed %d", renewed, lapsed)
	}
	jc, err := st.JobCredential(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := creds.Open(jc)
	if got.Ticket != cred.Ticket+"+r" || !got.LoginAt.Equal(cred.LoginAt.Truncate(time.Microsecond)) {
		t.Fatalf("after renewal: %v (login %v, want %v)", got, got.LoginAt, cred.LoginAt)
	}
	if !jc.RenewAfter.After(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("next renewal at %v", jc.RenewAfter)
	}
	if calls := r.calls(); len(calls) != 1 || calls[0] != cred.Ticket {
		t.Fatalf("renewal calls %v, want only the due ticket (not job %d's)", calls, fresh)
	}
}

// A ticket listed as due but no longer due when claimed (another replica
// renewed it in between) is neither renewed nor counted.
func TestRenewerCountsOnlyWhatItClaimed(t *testing.T) {
	st := storetest.New(t)
	creds := testCreds(t)
	cred := aliceTicket()
	cred.Issued = time.Now().Add(-61 * time.Minute)
	storeJobWith(t, st, creds, cred)
	r := &fakeRenewer{valid: map[string]bool{cred.Ticket: true}}
	rn := newRenewer(st, creds, r, time.Now())
	var mu sync.Mutex
	listed := false
	rn.Now = func() time.Time { // lists at now, claims an hour earlier, when it wasn't due
		mu.Lock()
		defer mu.Unlock()
		if !listed {
			listed = true
			return time.Now()
		}
		return time.Now().Add(-time.Hour)
	}
	if renewed, lapsed := rn.RenewDue(bg); renewed+lapsed != 0 {
		t.Fatalf("renewed %d, lapsed %d; want nothing claimed", renewed, lapsed)
	}
	if len(r.calls()) != 0 {
		t.Fatalf("renewal calls %v", r.calls())
	}
}

func TestRenewerDropsRefusedTickets(t *testing.T) {
	st := storetest.New(t)
	creds := testCreds(t)
	cred := aliceTicket()
	cred.Issued = time.Now().Add(-61 * time.Minute)
	id := storeJobWith(t, st, creds, cred)
	r := &fakeRenewer{valid: map[string]bool{}}
	if _, lapsed := newRenewer(st, creds, r, time.Now()).RenewDue(bg); lapsed != 1 {
		t.Fatalf("lapsed = %d", lapsed)
	}
	if _, err := st.JobCredential(bg, id); !errors.Is(err, store.ErrNoCredential) {
		t.Fatalf("a refused ticket was kept: %v", err)
	}
}

func TestRenewerDropsExpiredAndTooOldTicketsWithoutAsking(t *testing.T) {
	st := storetest.New(t)
	creds := testCreds(t)
	expired := aliceTicket()
	expired.Issued = time.Now().Add(-2*time.Hour - time.Minute)
	old := aliceTicket()
	old.Ticket = "PVE:alice:old"
	old.Issued = time.Now().Add(-61 * time.Minute)
	old.LoginAt = time.Now().Add(-13 * time.Hour) // past the 12h ticket_max_age
	a := storeJobWith(t, st, creds, expired)
	b := storeJobWith(t, st, creds, old)
	r := &fakeRenewer{valid: map[string]bool{expired.Ticket: true, old.Ticket: true}}
	if _, lapsed := newRenewer(st, creds, r, time.Now()).RenewDue(bg); lapsed != 2 {
		t.Fatalf("lapsed = %d", lapsed)
	}
	for _, id := range []int64{a, b} {
		if _, err := st.JobCredential(bg, id); !errors.Is(err, store.ErrNoCredential) {
			t.Fatalf("job %d kept its credential", id)
		}
	}
	if calls := r.calls(); len(calls) != 0 {
		t.Fatalf("asked Proxmox to renew %v", calls)
	}
}

func TestRenewerKeepsTicketsWhenProxmoxIsUnreachable(t *testing.T) {
	st := storetest.New(t)
	creds := testCreds(t)
	cred := aliceTicket()
	cred.Issued = time.Now().Add(-61 * time.Minute)
	id := storeJobWith(t, st, creds, cred)
	r := &fakeRenewer{valid: map[string]bool{cred.Ticket: true}, err: &proxmox.APIError{Status: 595, Message: "no route"}}
	if renewed, lapsed := newRenewer(st, creds, r, time.Now()).RenewDue(bg); renewed+lapsed != 0 {
		t.Fatalf("renewed %d lapsed %d", renewed, lapsed)
	}
	if _, err := st.JobCredential(bg, id); err != nil {
		t.Fatalf("an unreachable Proxmox dropped the ticket: %v", err)
	}
}

// Open refuses a credential row it can't read, never panicking: junk, or a
// box sealed for another job.
func TestPropCredentialOpenRefusesJunk(t *testing.T) {
	creds := testCreds(t)
	box, err := creds.Seal(aliceTicket())(7)
	if err != nil {
		t.Fatal(err)
	}
	rapid.Check(t, func(t *rapid.T) {
		jc := box
		jc.JobID = rapid.Int64Range(-1, 9).Draw(t, "job")
		if rapid.Bool().Draw(t, "junk") {
			jc.Sealed = rapid.SampledFrom([]string{"", "v1:"}).Draw(t, "prefix") + rapid.String().Draw(t, "sealed")
		} else if jc.JobID == 7 {
			return
		}
		if _, err := creds.Open(jc); err == nil {
			t.Fatalf("Open(%+v) succeeded", jc)
		}
	})
}
