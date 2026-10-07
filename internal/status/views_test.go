package status

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// asUser is the fake cluster seen through a credential: it records whose
// each listing was.
type asUser struct {
	*fakeAPI
	cred func() proxmox.Credential
	log  *credLog
}

type credLog struct {
	mu    sync.Mutex
	lists []string // the ticket of each ClusterVMs call
}

func (l *credLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lists)
}

func (a asUser) ClusterVMs(ctx context.Context) ([]proxmox.VM, error) {
	a.log.mu.Lock()
	a.log.lists = append(a.log.lists, a.cred().Ticket)
	a.log.mu.Unlock()
	return a.fakeAPI.ClusterVMs(ctx)
}

func ticket(user, t string) proxmox.Credential {
	return proxmox.TicketCredential(user, t, "csrf", time.Now())
}

type viewsHarness struct {
	t     *testing.T
	api   *fakeAPI
	clock *fakeClock
	log   *credLog
	views *Views
}

func newViewsHarness(t *testing.T) *viewsHarness {
	t.Helper()
	h := &viewsHarness{t: t, api: newFakeAPI(), clock: newFakeClock(), log: &credLog{}}
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	cfg := testConfig()
	cfg.Web.StatusPoll = 5 * time.Second
	cfg.Web.DriftScan = time.Hour
	bind := func(cred func() proxmox.Credential) pods.API { return asUser{fakeAPI: h.api, cred: cred, log: h.log} }
	var err error
	h.views, err = NewViews(bind, &fakeHistory{clock: h.clock}, apply.NewLimits(cfg.Concurrency), cfg,
		Options{Clock: h.clock, Logf: t.Logf}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.views.Close)
	return h
}

// waitLists waits until the cluster has been listed n times.
func (h *viewsHarness) waitLists(n int) []string {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := h.log.all(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%d listings after 10s, want %d", len(h.log.all()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestViewPollsAsItsUser(t *testing.T) {
	h := newViewsHarness(t)
	v, release := h.views.Open("alice@r", ticket("alice@r", "A1"))
	defer release()
	if got := h.waitLists(1); got[0] != "A1" {
		t.Fatalf("listed as %v", got)
	}
	if err := v.WaitFirstPoll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g := v.Grid(); g.Stale || len(g.Rows) == 0 {
		t.Fatalf("grid = %+v", g)
	}
}

// Several tabs of one user share one view; another user gets their own.
func TestViewsAreSharedPerUser(t *testing.T) {
	h := newViewsHarness(t)
	a1, r1 := h.views.Open("alice@r", ticket("alice@r", "A1"))
	defer r1()
	a2, r2 := h.views.Open("alice@r", ticket("alice@r", "A2"))
	defer r2()
	b, r3 := h.views.Open("bob@r", ticket("bob@r", "B1"))
	defer r3()
	if a1 != a2 || a1 == b {
		t.Fatal("views aren't one per user")
	}
	// Alice's view may make its first listing before her second tab hands
	// it A2 (A1 was valid then); from then on it lists with A2.
	got := h.waitLists(2)
	slices.Sort(got)
	if (got[0] != "A1" && got[0] != "A2") || got[1] != "B1" {
		t.Fatalf("first listings %v, want one of alice's tickets and bob's", got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(h.log.all(), "A2") {
		if time.Now().After(deadline) {
			t.Fatalf("listings %v: alice's view never used her newest ticket", h.log.all())
		}
		h.clock.Advance(5 * time.Second)
		time.Sleep(time.Millisecond)
	}
}

// A view polls while held and through its linger, then stops.
func TestViewStopsAfterItsLastStream(t *testing.T) {
	h := newViewsHarness(t)
	v, release := h.views.Open("alice@r", ticket("alice@r", "A1"))
	h.waitLists(1)
	h.clock.BlockUntilWait(t, 5*time.Second) // the next poll's wait
	h.clock.Advance(5 * time.Second)
	h.waitLists(2)
	release()
	h.clock.BlockUntil(t, 3) // the next poll's and scan's waits, and the linger
	h.clock.Advance(time.Minute)
	select {
	case <-v.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the view kept running after its last stream")
	}
	n := len(h.log.all())
	h.clock.Advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	if got := len(h.log.all()); got != n {
		t.Fatalf("listed %d more times with nobody watching", got-n)
	}
	// A new stream starts a new view.
	v2, release2 := h.views.Open("alice@r", ticket("alice@r", "A2"))
	defer release2()
	if v2 == v {
		t.Fatal("reused a stopped view")
	}
	if got := h.waitLists(n + 1); got[n] != "A2" {
		t.Fatalf("listed as %q", got[n])
	}
}

// A stream that comes back during the linger keeps the view.
func TestViewSurvivesAReconnect(t *testing.T) {
	h := newViewsHarness(t)
	v, release := h.views.Open("alice@r", ticket("alice@r", "A1"))
	h.waitLists(1)
	release()
	v2, release2 := h.views.Open("alice@r", ticket("alice@r", "A1"))
	defer release2()
	h.clock.Advance(2 * time.Minute)
	select {
	case <-v.Done():
		t.Fatal("the view stopped while a stream held it")
	case <-time.After(50 * time.Millisecond):
	}
	if v2 != v {
		t.Fatal("a reconnect during the linger made a new view")
	}
}

// The view polls with the newest ticket any of its user's requests had.
func TestViewUsesTheNewestTicket(t *testing.T) {
	h := newViewsHarness(t)
	_, release := h.views.Open("alice@r", ticket("alice@r", "A1"))
	defer release()
	h.waitLists(1)
	later := ticket("alice@r", "A2")
	later.Issued = later.Issued.Add(time.Hour)
	_, r2 := h.views.Open("alice@r", later)
	r2()
	_, r3 := h.views.Open("alice@r", ticket("alice@r", "A0")) // older
	r3()
	h.clock.BlockUntilWait(t, 5*time.Second) // the next poll's, not the drift scan's
	h.clock.Advance(5 * time.Second)
	if got := h.waitLists(2); got[1] != "A2" {
		t.Fatalf("second listing as %q, want the newest ticket", got[1])
	}
}
