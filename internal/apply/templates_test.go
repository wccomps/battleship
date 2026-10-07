package apply

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// sevenMasters is a cluster whose seven running masters, h1..h7, are all on
// spruce, like the production deploy that built its templates one by one.
// Master 10i builds template 900i, which team 01 clones to 1010i.
func sevenMasters() *fakeAPI {
	f := newFakeAPI("cedar", "birch", "spruce")
	for i := 1; i <= 7; i++ {
		f.add(proxmox.VM{VMID: 100 + i, Name: fmt.Sprintf("h%d.kilo.alpha", i), Node: "spruce", Tags: "dev", Status: "running"},
			map[string]string{"net0": fmt.Sprintf("virtio=BC:24:11:00:01:%02d,bridge=vmbr0", i)})
	}
	return f
}

func deploySeven(t *testing.T, f *fakeAPI, hosts ...string) *pods.Plan {
	t.Helper()
	plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: "*.kilo.alpha", Teams: []string{"01"}, Hosts: hosts})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// cloneSource is the source VMID of a clone task's UPID: a master for a
// template build, a template for a team VM.
func cloneSource(upid string) int {
	parts := strings.Split(upid, ":")
	n, _ := strconv.Atoi(parts[6])
	return n
}

type gateWaiter struct {
	upid    string
	release chan error
}

// cloneGate holds clone task waits until the test releases them. A UPID is
// held once: a later wait for the same task (the master restart confirming a
// cut-off clone finished) passes at once.
type cloneGate struct {
	t        *testing.T
	kind     string // the task type held, e.g. qmclone
	hold     func(upid string) bool
	arrive   chan *gateWaiter
	mu       sync.Mutex
	seen     map[string]bool
	held     []*gateWaiter
	arrived  map[string]bool
	released map[string]bool
	capacity int // the most waits that may be held at once
	// inflight counts held waits from the moment they start, including any
	// not yet taken by the test; maxInflight is its peak.
	inflight, maxInflight int
}

func newCloneGate(t *testing.T, f *fakeAPI, capacity int, hold func(upid string) bool) *cloneGate {
	return newTaskGate(t, f, capacity, "qmclone", hold)
}

// newTaskGate is a cloneGate for tasks of another kind, such as qmdestroy.
func newTaskGate(t *testing.T, f *fakeAPI, capacity int, kind string, hold func(upid string) bool) *cloneGate {
	g := &cloneGate{t: t, kind: kind, hold: hold, arrive: make(chan *gateWaiter), seen: map[string]bool{},
		arrived: map[string]bool{}, released: map[string]bool{}, capacity: capacity}
	f.waitGate = g.wait
	return g
}

func (g *cloneGate) wait(ctx context.Context, upid string) error {
	if !strings.Contains(upid, ":"+g.kind+":") || !g.hold(upid) {
		return nil
	}
	g.mu.Lock()
	again := g.seen[upid]
	g.seen[upid] = true
	g.mu.Unlock()
	if again {
		return nil
	}
	g.mu.Lock()
	g.inflight++
	g.maxInflight = max(g.maxInflight, g.inflight)
	if g.inflight > g.capacity {
		g.t.Errorf("%d %s tasks in flight at once, cap is %d (latest %s)", g.inflight, g.kind, g.capacity, upid)
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.inflight--
		g.mu.Unlock()
	}()
	w := &gateWaiter{upid: upid, release: make(chan error, 1)}
	select {
	case g.arrive <- w:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-w.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// next waits for the next held clone.
func (g *cloneGate) next() *gateWaiter {
	g.t.Helper()
	select {
	case w := <-g.arrive:
		g.held = append(g.held, w)
		g.arrived[w.upid] = true
		return w
	case <-time.After(10 * time.Second):
		g.t.Fatalf("no further clone started; in flight: %v", g.upids())
		return nil
	}
}

func (g *cloneGate) peak() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxInflight
}

func (g *cloneGate) upids() []string {
	var out []string
	for _, w := range g.held {
		out = append(out, w.upid)
	}
	return out
}

// releaseUPID lets the held clone with this UPID finish with err.
func (g *cloneGate) releaseUPID(upid string, err error) {
	g.t.Helper()
	for i, w := range g.held {
		if w.upid == upid {
			g.held = append(g.held[:i], g.held[i+1:]...)
			g.released[upid] = true
			w.release <- err
			return
		}
	}
	g.t.Fatalf("%s is not held", upid)
}

// drive releases held clones, oldest first, whenever capacity are held or no
// clone that has not started yet can start (startable reports how many can),
// until total clones have finished.
func (g *cloneGate) drive(total int, startable func() int) {
	g.t.Helper()
	for done := 0; done < total; {
		if len(g.held) > 0 && (len(g.held) >= g.capacity || startable() == 0) {
			g.releaseUPID(g.held[0].upid, nil)
			done++
			continue
		}
		g.next()
	}
}

func masterClone(i int) string { return upid("spruce", "qmclone", 100+i) }
func itemClone(i int) string   { return upid("spruce", "qmclone", 9000+i) }

// runAsync runs plan in the background. Cleanup cancels it and waits, so a
// failed test does not leave the run blocked in the gate.
func runAsync(t *testing.T, ex *Executor, plan *pods.Plan) (context.CancelFunc, <-chan Result) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	finished := make(chan struct{})
	go func() { done <- ex.Run(ctx, plan); close(finished) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Error("run did not stop after cancel")
		}
	})
	return cancel, done
}

func waitResult(t *testing.T, done <-chan Result) Result {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
		return Result{}
	}
}

// checkMasters asserts that every master that was stopped was started again
// after its clone task, and that all are running.
func checkMasters(t *testing.T, f *fakeAPI) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 1; i <= 7; i++ {
		vmid := 100 + i
		stops, starts, lastStop, lastStart, lastClone := 0, 0, -1, -1, -1
		for k, c := range f.calls {
			switch c {
			case fmt.Sprintf("power:%d:stop", vmid):
				stops, lastStop = stops+1, k
			case fmt.Sprintf("power:%d:start", vmid):
				starts, lastStart = starts+1, k
			case "wait:" + masterClone(i):
				lastClone = k
			}
		}
		if stops != starts {
			t.Errorf("master h%d stopped %d times, started %d times", i, stops, starts)
		}
		if stops > 0 && (lastStart < lastStop || lastStart < lastClone) {
			t.Errorf("master h%d: last start at %d, before its stop (%d) or clone wait (%d)", i, lastStart, lastStop, lastClone)
		}
		if st := f.vms[vmid].Status; st != "running" {
			t.Errorf("master h%d is %s, want running", i, st)
		}
	}
}

func TestTemplatesBuildConcurrentlyUpToCap(t *testing.T) {
	f := sevenMasters()
	plan := deploySeven(t, f)
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Concurrency.TemplateBuilds = 4
	isMaster := func(upid string) bool { return cloneSource(upid) < 9000 }
	g := newCloneGate(t, f, 4, isMaster)
	_, done := runAsync(t, ex, plan)

	g.drive(7, func() int { return 7 - len(g.arrived) })
	res := waitResult(t, done)
	if n := g.peak(); n != 4 {
		t.Errorf("at most %d template clones in flight, want 4", n)
	}
	if len(res.Failed) != 0 || len(res.Succeeded) != 7 {
		t.Errorf("result = %+v", res)
	}
	for i := 1; i <= 7; i++ {
		if tpl := f.vms[9000+i]; tpl == nil || !tpl.Template {
			t.Errorf("template %d = %+v", 9000+i, tpl)
		}
	}
	checkMasters(t, f)
}

// Template and team clones from one node share its clone slots.
func TestTemplateAndTeamClonesShareNodeSlots(t *testing.T) {
	f := sevenMasters()
	plan := deploySeven(t, f)
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Concurrency.TemplateBuilds = 4
	ex.Cfg.Concurrency.ClonesPerNode = 2
	ex.Limits = NewLimits(ex.Cfg.Concurrency) // shared with any other executor
	g := newCloneGate(t, f, 2, func(string) bool { return true })
	_, done := runAsync(t, ex, plan)

	g.drive(14, func() int {
		n := 0
		for i := 1; i <= 7; i++ {
			if !g.arrived[masterClone(i)] {
				n++
			}
			if g.released[masterClone(i)] && !g.arrived[itemClone(i)] {
				n++
			}
		}
		return n
	})
	res := waitResult(t, done)
	if n := g.peak(); n != 2 {
		t.Errorf("at most %d clones in flight, want 2", n)
	}
	if len(res.Failed) != 0 || len(res.Succeeded) != 7 {
		t.Errorf("result = %+v", res)
	}
	checkMasters(t, f)
}

func TestItemsStartWhenTheirOwnTemplateIsReady(t *testing.T) {
	f := sevenMasters()
	plan := deploySeven(t, f, "h1", "h2")
	g := newCloneGate(t, f, 3, func(string) bool { return true })
	_, done := runAsync(t, testExecutor(f, &recorder{}), plan)

	first, second := g.next(), g.next()
	if cloneSource(first.upid) > 9000 || cloneSource(second.upid) > 9000 {
		t.Fatalf("held = %v, want both template clones", g.upids())
	}
	g.releaseUPID(first.upid, nil)
	ready := cloneSource(first.upid) - 100
	if w := g.next(); w.upid != itemClone(ready) {
		t.Fatalf("next clone = %s, want team01-h%d's while the other template is still building", w.upid, ready)
	}
	g.releaseUPID(itemClone(ready), nil)
	g.releaseUPID(second.upid, nil)
	other := cloneSource(second.upid) - 100
	g.next()
	g.releaseUPID(itemClone(other), nil)
	if res := waitResult(t, done); len(res.Failed) != 0 || len(res.Succeeded) != 2 {
		t.Errorf("result = %+v", res)
	}
}

// With several templates in flight, one copy task fails and the run is
// cancelled while others are mid-clone: every stopped master is restarted,
// finished copies are completed, this run's leftovers are removed, and
// nothing else is touched.
func TestConcurrentTemplatesFailAndCancel(t *testing.T) {
	f := sevenMasters()
	// team01-h6 already exists, so h6's template is not needed.
	f.add(proxmox.VM{VMID: 10106, Name: "team01-h6", Node: "cedar", Status: "running"},
		map[string]string{"name": "team01-h6", "net0": "virtio=BC:24:11:00:01:06,bridge=ext01"}, "initial")
	plan := deploySeven(t, f)
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Concurrency.TemplateBuilds = 4
	g := newCloneGate(t, f, 6, func(string) bool { return true })
	cancel, done := runAsync(t, ex, plan)

	var ws []*gateWaiter
	for len(ws) < 4 {
		ws = append(ws, g.next())
	}
	ok, bad := cloneSource(ws[0].upid)-100, cloneSource(ws[1].upid)-100
	g.releaseUPID(ws[1].upid, &proxmox.TaskError{UPID: ws[1].upid, ExitStatus: "clone failed: no space left"})
	g.releaseUPID(ws[0].upid, nil)
	// Six templates are needed (not h6's); with two done, the other four
	// build at once. Wait for all of them and for ok's team VM, so nothing is
	// left starting when the cancel comes.
	cut := map[int]bool{}
	for _, w := range ws[2:] {
		cut[cloneSource(w.upid)-100] = true
	}
	for !g.arrived[itemClone(ok)] || len(cut) < 4 {
		if src := cloneSource(g.next().upid); src < 9000 {
			cut[src-100] = true
		}
	}
	cancel()
	res := waitResult(t, done)

	checkMasters(t, f)
	if f.called("power:106:") != 0 {
		t.Error("h6's master was touched though its template was not needed")
	}
	if tpl := f.vms[9000+ok]; tpl == nil || !tpl.Template {
		t.Errorf("finished template h%d = %+v", ok, tpl)
	}
	if f.vms[9000+bad] != nil || f.called(fmt.Sprintf("template:%d", 9000+bad)) != 0 {
		t.Errorf("failed copy h%d was not removed", bad)
	}
	if f.vms[10100+ok] != nil {
		t.Errorf("cancelled team01-h%d was left behind", ok)
	}
	var wantCompleted []string
	for i := range cut {
		wantCompleted = append(wantCompleted, fmt.Sprintf("h%d.kilo.alpha.tpl", i))
		if tpl := f.vms[9000+i]; tpl == nil || !tpl.Template {
			t.Errorf("copy cut off by the cancel h%d = %+v, want completed", i, tpl)
		}
	}
	if fmt.Sprint(res.Completed) != fmt.Sprint(sorted(wantCompleted)) {
		t.Errorf("Completed = %v, want %v", res.Completed, sorted(wantCompleted))
	}
	wantRemoved := sorted([]string{fmt.Sprintf("h%d.kilo.alpha.tpl", bad), fmt.Sprintf("team01-h%d", ok)})
	if fmt.Sprint(res.Removed) != fmt.Sprint(wantRemoved) || len(res.CleanupFailed) != 0 {
		t.Errorf("Removed = %v, want %v; CleanupFailed = %v", res.Removed, wantRemoved, res.CleanupFailed)
	}
	if n := f.called("delete:"); n != 2 {
		t.Errorf("%d deletes, want 2: %v", n, f.calls)
	}
	if f.vms[10106] == nil {
		t.Error("team01-h6, which this run did not create, was removed")
	}
	// Templates never started leave no VM behind.
	for i := 1; i <= 7; i++ {
		if vm := f.vms[9000+i]; vm != nil && !vm.Template {
			t.Errorf("unconverted copy left at %d: %+v", 9000+i, vm)
		}
	}
}

// A team VM that already exists never uses its template: a failed build of
// that template (needed only by teams that still need cloning) must not
// fail it.
func TestExistingTeamVMIgnoresFailedTemplateBuild(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, map[string]string{
		"name": "team01-teak", "net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0", "net1": "virtio=BC:24:11:00:01:22,bridge=vmbr1",
	}, "initial")
	plan := deployTeak(t, f, "01", "02")
	f.failOn("clone:9021", &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/121, VM.Clone)"},
		&proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/121, VM.Clone)"})

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if res.Failed["team01-teak"] != nil || !slices.Contains(res.Succeeded, "team01-teak") {
		t.Errorf("team01-teak: failed = %v, succeeded = %v; want it to succeed", res.Failed["team01-teak"], res.Succeeded)
	}
	if res.Failed["team02-teak"] == nil {
		t.Errorf("team02-teak did not fail; the build should have")
	}
}
