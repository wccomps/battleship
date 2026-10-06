package status

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// taskAPI is the fake cluster with a task list per node and the caller's
// privileges at each node.
type taskAPI struct {
	*fakeAPI
	mu     sync.Mutex
	tasks  map[string][]proxmox.Task // by node
	audit  map[string]bool           // node -> Sys.Audit on /nodes/<node>
	failAt int                       // PermissionsAt fails this many more times
	asked  []string                  // NodeTasks calls, by node
	sinces []time.Time
}

func (a *taskAPI) NodeTasks(_ context.Context, node string, since time.Time) ([]proxmox.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, node)
	a.sinces = append(a.sinces, since)
	var out []proxmox.Task
	for _, t := range a.tasks[node] {
		if !t.Start.Before(since) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (a *taskAPI) Permissions(context.Context) (proxmox.Permissions, error) {
	return proxmox.Permissions{}, nil
}

func (a *taskAPI) PermissionsAt(_ context.Context, path string) (map[string]bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failAt > 0 {
		a.failAt--
		return nil, errors.New("connection reset")
	}
	for node, ok := range a.audit {
		if ok && path == "/nodes/"+node {
			return map[string]bool{"Sys.Audit": true}, nil
		}
	}
	return map[string]bool{}, nil
}

func (a *taskAPI) addTask(node, typ string, vmid int, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	upid := fmt.Sprintf("UPID:%s:%d:%d:%X:%s:%d:u:", node, len(a.tasks[node]), vmid, at.Unix(), typ, vmid)
	a.tasks[node] = append(a.tasks[node], proxmox.Task{UPID: upid, Type: typ, VMID: vmid, Start: at})
}

func newTaskHarness(t *testing.T, audit bool) (*harness, *taskAPI) {
	t.Helper()
	h := &harness{api: newFakeAPI(), clock: newFakeClock(), cfg: testConfig()}
	h.hist = &fakeHistory{clock: h.clock}
	a := &taskAPI{fakeAPI: h.api, audit: map[string]bool{"n1": audit}, tasks: map[string][]proxmox.Task{}}
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	h.api.add(teamVM("01", "web", 10102), cleanConfig("01"), "initial")
	h.cfg.Concurrency.ConfigCalls = 2
	h.lim = pods.NewLimits(h.cfg.Concurrency)
	p, err := NewPoller(a, h.hist, h.lim, h.cfg, Options{Clock: h.clock, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	h.p = p
	return h, a
}

// A task on a VM gets that VM's config read again at the next poll, long
// before the next full scan.
func TestPollRereadsVMsTouchedByTasks(t *testing.T) {
	h, a := newTaskHarness(t, true)
	h.poll(t)
	h.scan(t)
	if c, _ := h.p.Grid().Cell("01", "dc"); c.State == StateDrifted {
		t.Fatal("drifted before the change")
	}
	h.api.setConfig(10101, "net0", "virtio=BC:24:11:00:00:01,bridge=vmbr0")
	a.addTask("n1", "qmconfig", 10101, h.clock.Now())
	h.clock.Advance(time.Second)
	h.poll(t)
	if c, _ := h.p.Grid().Cell("01", "dc"); c.State != StateDrifted {
		t.Fatalf("after a task on it, dc is %s, want drifted", c.State)
	}
	if c, _ := h.p.Grid().Cell("01", "web"); c.State == StateDrifted {
		t.Fatal("web, which no task touched, changed")
	}
	// The next poll asks only for tasks since the last.
	a.mu.Lock()
	n := len(a.sinces)
	last := a.sinces[n-1]
	a.mu.Unlock()
	h.poll(t)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.sinces[len(a.sinces)-1].After(last) {
		t.Fatalf("asked for tasks since %v again", last)
	}
}

// Without Sys.Audit on the node, its task list is never asked for: the
// full scan alone finds changes.
func TestPollSkipsTasksWithoutSysAudit(t *testing.T) {
	h, a := newTaskHarness(t, false)
	h.poll(t)
	h.poll(t)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.asked) != 0 {
		t.Fatalf("asked for tasks of %v without Sys.Audit", a.asked)
	}
}

// A long task re-reads its VM when it starts and when it ends, not on every
// poll while it runs; a console's task (no VM change) holds nothing open.
func TestPollRereadsATaskWhenItStartsAndEnds(t *testing.T) {
	h, a := newTaskHarness(t, true)
	h.poll(t)
	start := h.clock.Now()
	a.mu.Lock()
	a.tasks["n1"] = append(a.tasks["n1"],
		proxmox.Task{UPID: "UPID:n1:1:1:1:qmclone:10101:u:", Type: "qmclone", VMID: 10101, Start: start, Running: true},
		proxmox.Task{UPID: "UPID:n1:2:2:2:vncproxy:10102:u:", Type: "vncproxy", VMID: 10102, Start: start, Running: true})
	a.mu.Unlock()
	reads := func() int { r, _, _ := h.api.counts(); return r }
	h.clock.Advance(time.Second)
	before := reads()
	h.poll(t)
	if got := reads() - before; got != 2 { // the clone's VM: config and snapshots
		t.Fatalf("first poll after the clone started read %d times, want 2", got)
	}
	for range 3 {
		before = reads()
		h.clock.Advance(5 * time.Second)
		h.poll(t)
		if got := reads() - before; got != 0 {
			t.Fatalf("a poll while the clone runs read %d times, want 0", got)
		}
	}
	a.mu.Lock()
	a.tasks["n1"][len(a.tasks["n1"])-2].Running = false
	a.mu.Unlock()
	before = reads()
	h.clock.Advance(5 * time.Second)
	h.poll(t)
	if got := reads() - before; got != 2 {
		t.Fatalf("the poll after the clone ended read %d times, want 2", got)
	}
	// Only the console runs now: the next poll asks from after it started.
	h.clock.Advance(5 * time.Second)
	h.poll(t)
	a.mu.Lock()
	defer a.mu.Unlock()
	if last := a.sinces[len(a.sinces)-1]; !last.After(start) {
		t.Fatalf("asked for tasks since %v: the console holds the window open", last)
	}
}

// A failed read of the node's privileges isn't taken for "no Sys.Audit":
// the next poll asks again rather than skipping the node for minutes.
func TestPollRetriesAFailedAuditCheck(t *testing.T) {
	h, a := newTaskHarness(t, true)
	a.mu.Lock()
	a.failAt = 1
	a.mu.Unlock()
	h.poll(t)
	h.poll(t)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.asked) == 0 {
		t.Fatal("never asked for the node's tasks after one failed privilege read")
	}
}

// Of two VMs with one name, a task on the one the cell doesn't show (the
// higher VMID) doesn't replace the shown one's scan.
func TestPollRereadsOnlyTheShownDuplicate(t *testing.T) {
	h, a := newTaskHarness(t, true)
	h.api.setConfig(10101, "net0", "virtio=BC:24:11:00:00:01,bridge=vmbr0") // dc drifts
	h.poll(t)
	h.scan(t)
	if c, _ := h.p.Grid().Cell("01", "dc"); c.State != StateDrifted {
		t.Fatalf("dc is %s, want drifted", c.State)
	}
	h.api.add(teamVM("01", "dc", 10199), cleanConfig("01"), "initial") // a clean duplicate
	a.addTask("n1", "qmconfig", 10199, h.clock.Now())
	h.clock.Advance(time.Second)
	h.poll(t)
	c, _ := h.p.Grid().Cell("01", "dc")
	if c.VMID != 10101 {
		t.Fatalf("cell shows %d, want 10101", c.VMID)
	}
	found := false
	for _, d := range c.Drift {
		found = found || d.Kind != DriftDuplicate
	}
	if !found {
		t.Fatalf("dc's own drift hidden by its duplicate's re-read: %+v", c.Drift)
	}
}
