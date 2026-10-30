package store_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// ticketCred is what a caller hands CreateJob: already sealed for the job.
func ticketCred(issued time.Time) func(int64) (store.JobCredential, error) {
	return func(id int64) (store.JobCredential, error) {
		return store.JobCredential{User: "jdoe@auth.example.org",
			Sealed: "v1:sealed-for-" + itoa(id), IssuedAt: issued, LoginAt: issued.Add(-time.Hour), RenewAfter: issued.Add(time.Hour)}, nil
	}
}

func itoa(n int64) string { return time.Duration(n).String() }

func createWithCred(t *testing.T, s *store.Store, issued time.Time, keys ...string) int64 {
	t.Helper()
	nj := newJob(keys...)
	nj.Credential = ticketCred(issued)
	id, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func hasCred(t *testing.T, s *store.Store, id int64) bool {
	t.Helper()
	_, err := s.JobCredential(ctx, id)
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNoCredential):
		return false
	}
	t.Fatal(err)
	return false
}

func TestCreateJobStoresItsCredentialSealedForItsID(t *testing.T) {
	s := storetest.New(t)
	id := createWithCred(t, s, t0, "team:01")
	c, err := s.JobCredential(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.JobID != id || c.User != "jdoe@auth.example.org" ||
		c.Sealed != "v1:sealed-for-"+itoa(id) || !c.IssuedAt.Equal(t0) || !c.LoginAt.Equal(t0.Add(-time.Hour)) ||
		!c.RenewAfter.Equal(t0.Add(time.Hour)) {
		t.Fatalf("credential = %+v", c)
	}
}

func TestCreateJobWithoutACredentialStoresNone(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	if hasCred(t, s, id) {
		t.Fatal("a job created without a credential has one")
	}
}

func TestCreateJobFailsWhenSealingFails(t *testing.T) {
	s := storetest.New(t)
	nj := newJob("team:01")
	nj.Credential = func(int64) (store.JobCredential, error) { return store.JobCredential{}, errors.New("no key") }
	if _, err := s.CreateJob(ctx, nj); err == nil {
		t.Fatal("CreateJob succeeded although sealing failed")
	}
	jobs, err := s.Jobs(ctx, 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("jobs after a failed create = %v, %v", jobs, err)
	}
}

func TestCredentialDeletedOnEveryFinalStatus(t *testing.T) {
	for _, status := range []string{store.StatusSucceeded, store.StatusCompletedWithFailures, store.StatusFailed,
		store.StatusCancelled, store.StatusInterrupted, store.StatusStale} {
		t.Run(status, func(t *testing.T) {
			s := storetest.New(t)
			id := createWithCred(t, s, t0, "team:01")
			if _, err := storetest.ClaimJob(s, id, "w1", time.Hour); err != nil {
				t.Fatal(err)
			}
			if !hasCred(t, s, id) {
				t.Fatal("a running job lost its credential")
			}
			if err := s.Finish(ctx, id, "w1", store.Outcome{Status: status}); err != nil {
				t.Fatal(err)
			}
			if hasCred(t, s, id) {
				t.Fatalf("a %s job kept its credential", status)
			}
		})
	}
}

func TestCredentialDeletedWhenAPendingJobIsCancelled(t *testing.T) {
	s := storetest.New(t)
	id := createWithCred(t, s, t0, "team:01")
	if err := s.RequestCancel(ctx, id, "lead"); err != nil {
		t.Fatal(err)
	}
	if hasCred(t, s, id) {
		t.Fatal("a cancelled pending job kept its credential")
	}
}

func TestCredentialKeptWhileARunningJobStops(t *testing.T) {
	s := storetest.New(t)
	id := createWithCred(t, s, t0, "team:01")
	if _, err := storetest.ClaimJob(s, id, "w1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestCancel(ctx, id, "lead"); err != nil {
		t.Fatal(err)
	}
	if !hasCred(t, s, id) {
		t.Fatal("a running job lost its credential when asked to stop: its cleanup needs it")
	}
}

func TestCredentialDeletedWhenAJobIsReaped(t *testing.T) {
	s := storetest.New(t)
	id := createWithCred(t, s, t0, "team:01")
	if _, err := storetest.ClaimJob(s, id, "w1", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	ids, err := s.ReapStale(ctx, time.Hour)
	if err != nil || len(ids) != 1 {
		t.Fatalf("reaped %v, %v", ids, err)
	}
	if hasCred(t, s, id) {
		t.Fatal("a reaped job kept its credential")
	}
}

func TestDueCredentialsListsTicketsToRenew(t *testing.T) {
	s := storetest.New(t)
	old := createWithCred(t, s, t0, "team:01")
	fresh := createWithCred(t, s, t0.Add(30*time.Minute), "team:02")
	due, err := s.DueCredentials(ctx, t0.Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0] != old {
		t.Fatalf("due = %v, want only %d (not %d)", due, old, fresh)
	}
}

func TestRenewCredentialUpdatesTheRow(t *testing.T) {
	s := storetest.New(t)
	id := createWithCred(t, s, t0, "team:01")
	now := t0.Add(61 * time.Minute)
	claimed, err := s.RenewCredential(ctx, id, now, func(cur store.JobCredential) (store.JobCredential, store.Renewal, error) {
		cur.Sealed, cur.IssuedAt, cur.RenewAfter = "v1:renewed", now, now.Add(time.Hour)
		return cur, store.Renewed, nil
	})
	if err != nil || !claimed {
		t.Fatalf("RenewCredential = %v, %v", claimed, err)
	}
	c, err := s.JobCredential(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sealed != "v1:renewed" || !c.IssuedAt.Equal(now) || !c.RenewAfter.Equal(now.Add(time.Hour)) || !c.LoginAt.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("after renewal: %+v", c)
	}
}

func TestRenewCredentialLapsedDeletesTheRow(t *testing.T) {
	s := storetest.New(t)
	id := createWithCred(t, s, t0, "team:01")
	if _, err := s.RenewCredential(ctx, id, t0.Add(3*time.Hour), func(cur store.JobCredential) (store.JobCredential, store.Renewal, error) {
		return cur, store.RenewalLapsed, nil
	}); err != nil {
		t.Fatal(err)
	}
	if hasCred(t, s, id) {
		t.Fatal("a lapsed credential was kept")
	}
}

func TestRenewCredentialFailureKeepsTheRow(t *testing.T) {
	s := storetest.New(t)
	id := createWithCred(t, s, t0, "team:01")
	boom := errors.New("proxmox unreachable")
	if _, err := s.RenewCredential(ctx, id, t0.Add(61*time.Minute), func(cur store.JobCredential) (store.JobCredential, store.Renewal, error) {
		return cur, store.RenewalFailed, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the renewal's", err)
	}
	c, err := s.JobCredential(ctx, id)
	if err != nil || c.Sealed != "v1:sealed-for-"+itoa(id) {
		t.Fatalf("after a failed renewal: %+v, %v", c, err)
	}
}

func TestRenewCredentialSkipsWhatIsNotDue(t *testing.T) {
	s := storetest.New(t)
	id := createWithCred(t, s, t0, "team:01")
	called := false
	claimed, err := s.RenewCredential(ctx, id, t0.Add(30*time.Minute), func(cur store.JobCredential) (store.JobCredential, store.Renewal, error) {
		called = true
		return cur, store.Renewed, nil
	})
	if err != nil || claimed || called {
		t.Fatalf("renewing a credential that isn't due: claimed=%v called=%v err=%v", claimed, called, err)
	}
	if claimed, err := s.RenewCredential(ctx, 999999, t0.Add(30*time.Minute), nil); err != nil || claimed {
		t.Fatalf("renewing a missing credential: %v %v", claimed, err)
	}
}

// Two replicas renewing the same row at once: one renews, the other finds
// it claimed (or, once the first is done, no longer due).
func TestRenewCredentialClaimsEachRowOnce(t *testing.T) {
	s := storetest.New(t)
	id := createWithCred(t, s, t0, "team:01")
	now := t0.Add(61 * time.Minute)
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	renew := func(cur store.JobCredential) (store.JobCredential, store.Renewal, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		cur.Sealed, cur.IssuedAt, cur.RenewAfter = "v1:renewed", now, now.Add(time.Hour)
		return cur, store.Renewed, nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := s.RenewCredential(ctx, id, now, renew); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	claimed, err := s.RenewCredential(ctx, id, now, renew)
	if err != nil || claimed {
		t.Fatalf("second replica: claimed=%v err=%v while the first held the row", claimed, err)
	}
	releaseOnce.Do(func() { close(release) })
	wg.Wait()
	if claimed, err := s.RenewCredential(ctx, id, now, renew); err != nil || claimed {
		t.Fatalf("after the first renewed: claimed=%v err=%v", claimed, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("renewed %d times", n)
	}
}

func TestSessionProxmoxLoginState(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("sess-1"))
	if err := s.SetSessionPVELogin(ctx, "sess-1", "state-hash", "/logs", "198.51.100.2:8006"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Session(ctx, "sess-1")
	if got.PVELoginState != "state-hash" || got.PVELoginNext != "/logs" || got.PVELoginEndpoint != "198.51.100.2:8006" {
		t.Fatalf("login state = %q %q %q", got.PVELoginState, got.PVELoginNext, got.PVELoginEndpoint)
	}
	if err := s.EndProxmoxLogin(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Session(ctx, "sess-1")
	if got.PVELoginState != "" || got.PVELoginNext != "" || got.PVELoginEndpoint != "" {
		t.Fatalf("login state kept after the login ended: %q %q %q", got.PVELoginState, got.PVELoginNext, got.PVELoginEndpoint)
	}
	if err := s.EndProxmoxLogin(ctx, "nope"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("ending an unknown session's login: %v", err)
	}
}
