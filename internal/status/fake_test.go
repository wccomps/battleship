package status

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// fakeAPI is the shared fake cluster on n1, with the failures the status
// tests set and a gate that holds and counts its VM reads. Its fields are
// guarded by the fake's Mu.
type fakeAPI struct {
	*podstest.Fake
	listErr error         // ClusterVMs fails with it
	readErr map[int]error // VMConfig fails with it for that VMID
	// gate, if set, holds every VMConfig and Snapshots call until it receives.
	gate chan struct{}
	// entered, if set, receives the VMID of each read as it starts.
	entered  chan int
	inFlight int
	max      int // most reads in flight at once
	reads    int // VMConfig and Snapshots calls
	lists    int // ClusterVMs calls
}

func newFakeAPI() *fakeAPI {
	f := &fakeAPI{Fake: podstest.New("n1"), readErr: map[int]error{}}
	f.Gate = f.hold
	return f
}

// add puts a VM in the cluster with a config and snapshots.
func (f *fakeAPI) add(vm proxmox.VM, cfg map[string]string, snaps ...string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Add(vm, cfg, snaps...)
}

func (f *fakeAPI) remove(vmid int) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	delete(f.VMs, vmid)
}

func (f *fakeAPI) setStatus(vmid int, status string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if vm, ok := f.VMs[vmid]; ok {
		vm.Status = status
	}
}

func (f *fakeAPI) setConfig(vmid int, key, value string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.VMs[vmid].Config[key] = value
}

func (f *fakeAPI) setSnaps(vmid int, snaps ...string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.VMs[vmid].Snapshots = snaps
}

func (f *fakeAPI) setListErr(err error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.listErr = err
}

func (f *fakeAPI) setReadErr(vmid int, err error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.readErr[vmid] = err
}

func (f *fakeAPI) counts() (reads, lists, max int) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.reads, f.lists, f.max
}

// hold is the fake's Gate. It counts listings and fails them with listErr.
// It counts each VM read, reports it on entered, waits for the gate, and
// fails a config read with readErr.
func (f *fakeAPI) hold(ctx context.Context, key string) (func(), error) {
	if key == "cluster" {
		f.Mu.Lock()
		defer f.Mu.Unlock()
		f.lists++
		return nil, f.listErr
	}
	kind, id, _ := strings.Cut(key, ":")
	if kind != "config" && kind != "snapshots" {
		return nil, nil
	}
	vmid, _ := strconv.Atoi(id)
	f.Mu.Lock()
	f.reads++
	f.inFlight++
	f.max = max(f.max, f.inFlight)
	gate, entered := f.gate, f.entered
	f.Mu.Unlock()
	done := func() {
		f.Mu.Lock()
		f.inFlight--
		f.Mu.Unlock()
	}
	if entered != nil {
		entered <- vmid
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return done, ctx.Err()
		}
	}
	if kind == "snapshots" {
		return done, nil
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return done, f.readErr[vmid]
}

// fakeHistory answers LastItemResults from a map.
type fakeHistory struct {
	mu      sync.Mutex
	results map[string]store.ItemResult
	err     error
	asked   [][]string
	clock   *fakeClock    // Now reads it, plus skew
	skew    time.Duration // how far the database's clock is ahead of the poller's
	nowErr  error
	nowHang bool // Now waits for its ctx to end, like a hung connection
}

func (h *fakeHistory) setSkew(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.skew = d
}

func (h *fakeHistory) Now(ctx context.Context) (time.Time, error) {
	h.mu.Lock()
	hang := h.nowHang
	h.mu.Unlock()
	if hang {
		<-ctx.Done()
		return time.Time{}, ctx.Err()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.nowErr != nil {
		return time.Time{}, h.nowErr
	}
	return h.clock.Now().Add(h.skew), nil
}

func (h *fakeHistory) set(name string, r store.ItemResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.results == nil {
		h.results = map[string]store.ItemResult{}
	}
	h.results[name] = r
}

func (h *fakeHistory) setErr(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = err
}

func (h *fakeHistory) LastItemResults(_ context.Context, names, _ []string) (map[string]store.ItemResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, slices.Clone(names))
	if h.err != nil {
		return nil, h.err
	}
	out := map[string]store.ItemResult{}
	for _, n := range names {
		if r, ok := h.results[n]; ok {
			out[n] = r
		}
	}
	return out, nil
}

// fakeClock only moves when Advance is called. BlockUntil lets a test wait,
// without sleeping, until goroutines are waiting on it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []clockWaiter
	changed chan struct{} // closed and replaced whenever a waiter is added
}

type clockWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), changed: make(chan struct{})}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, clockWaiter{at: c.now.Add(d), ch: ch})
	close(c.changed)
	c.changed = make(chan struct{})
	return ch
}

// Advance moves the clock and fires the waiters that are due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

// Waits returns how far in the future each pending waiter fires, soonest first.
func (c *fakeClock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.waiters))
	for i, w := range c.waiters {
		out[i] = w.at.Sub(c.now)
	}
	slices.Sort(out)
	return out
}

// BlockUntil waits until n waiters are pending. It fails the test if that
// doesn't happen within 10s, which only guards against a hang.
func (c *fakeClock) BlockUntil(t testing.TB, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		have, changed := len(c.waiters), c.changed
		c.mu.Unlock()
		if have >= n {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("clock: %d waiters after 10s, want %d", have, n)
		}
	}
}

// BlockUntilWait waits until a pending waiter fires d from now, such as a
// poller's next poll, rather than any waiter (its drift scan's has another
// length). It fails the test if that doesn't happen within 10s.
func (c *fakeClock) BlockUntilWait(t testing.TB, d time.Duration) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		found := slices.ContainsFunc(c.waiters, func(w clockWaiter) bool { return w.at.Sub(c.now) == d })
		changed := c.changed
		c.mu.Unlock()
		if found {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("clock: no waiter %s away after 10s; waits %v", d, c.Waits())
		}
	}
}

// testConfig is the default config.
func testConfig() config.Config {
	return config.Default()
}

// cleanConfig is a converged two-NIC team VM config: NICs on the team's
// bridges, disk limits set, no cloud-init.
func cleanConfig(team string) map[string]string {
	return map[string]string{
		"name":  "team" + team + "-x",
		"net0":  "virtio=BC:24:11:00:00:01,bridge=ext" + team,
		"net1":  "virtio=BC:24:11:00:00:02,bridge=int" + team,
		"scsi0": "competitions:base-9005-disk-0/vm-1-disk-0,mbps_rd=300,mbps_wr=300,size=32G",
		"ide2":  "none,media=cdrom",
	}
}

// teamVM is a running team VM in its team's pool.
func teamVM(team, host string, vmid int) proxmox.VM {
	return proxmox.VM{VMID: vmid, Name: "team" + team + "-" + host, Node: "n1", Status: "running", Pool: "pool-" + team}
}

// recv reads one message or fails after 10s (a hang guard, not a wait).
func recv(t testing.TB, ch <-chan Msg) Msg {
	t.Helper()
	m := recvSeq(t, ch)
	m.Seq = 0 // tests of Seq use recvSeq
	return m
}

// recvSeq is recv, keeping the message's Seq.
func recvSeq(t testing.TB, ch <-chan Msg) Msg {
	t.Helper()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("channel closed, want a message")
		}
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no message after 10s")
	}
	return Msg{}
}

// none checks that nothing is waiting on ch.
func none(t testing.TB, ch <-chan Msg) {
	t.Helper()
	select {
	case m, ok := <-ch:
		t.Fatalf("got %+v (open %v), want nothing", m, ok)
	default:
	}
}
