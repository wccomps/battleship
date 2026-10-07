package apply

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) add(e Event) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() }

func (r *recorder) find(item string, status EventStatus) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.events {
		if e.Item == item && e.Status == status {
			out = append(out, e)
		}
	}
	return out
}

func testExecutor(f *fakeAPI, rec *recorder) *Executor {
	return &Executor{
		API:     f,
		Cfg:     config.Default(),
		OnEvent: rec.add,
		Sleep:   testSleep(0),
	}
}

// testSleep is a Sleep that returns at once, except that a stopped
// template build's wait for its copy (copyStopWait) takes stopWait, or
// outlasts the wait if 0.
func testSleep(stopWait time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		if d != copyStopWait {
			return nil
		}
		if stopWait == 0 {
			<-ctx.Done()
			return ctx.Err()
		}
		select {
		case <-time.After(stopWait):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func deployTeak(t *testing.T, f *fakeAPI, teams ...string) *pods.Plan {
	t.Helper()
	plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: "teak.*", Teams: teams, Snapshot: true})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func sorted(s []string) []string { s = append([]string(nil), s...); sort.Strings(s); return s }

func TestDeployFromScratch(t *testing.T) {
	f := newCluster()
	rec := &recorder{}
	res := testExecutor(f, rec).Run(context.Background(), deployTeak(t, f, "01", "02"))

	if len(res.Failed) != 0 || !reflect.DeepEqual(sorted(res.Succeeded), []string{"team01-teak", "team02-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	tpl := f.vms[9021]
	if tpl == nil || !tpl.Template || tpl.Name != "teak.tango.delta.tpl" {
		t.Fatalf("template = %+v", tpl)
	}
	vm := f.vms[10121]
	if vm.Pool != "pool-01" || vm.Status != "running" || !reflect.DeepEqual(vm.Snapshots, []string{"initial"}) {
		t.Errorf("vm = %+v", vm.VM)
	}
	want := map[string]string{
		"net0":       "virtio=BC:24:11:00:01:21,bridge=ext01",
		"net1":       "virtio=BC:24:11:00:01:22,bridge=int01",
		"ipconfig0":  "ip=10.50.101.2/30,gw=10.50.101.1",
		"nameserver": "10.50.101.1",
		"cicustom":   "vendor=competitions:snippets/ssh-keys.yaml",
		"scsi0":      "competitions:9021/base-9021-disk-0.qcow2/10121/vm-10121-disk-0.qcow2,mbps_rd=300,mbps_wr=300,size=32G",
		"ide2":       "competitions:vm-121-cloudinit,media=cdrom",
	}
	for k, v := range want {
		if vm.Config[k] != v {
			t.Errorf("config[%s] = %q, want %q", k, vm.Config[k], v)
		}
	}
	if n := f.called("cloudinit:10121"); n != 1 {
		t.Errorf("cloud-init regenerated %d times, want 1", n)
	}
}

func TestRerunConvergesWithoutRecloning(t *testing.T) {
	f := newCluster()
	rec := &recorder{}
	testExecutor(f, rec).Run(context.Background(), deployTeak(t, f, "01"))

	// Simulate an interrupted deploy: wrong bridge, no snapshot, stopped.
	vm := f.vms[10121]
	vm.Config["net0"] = "virtio=BC:24:11:00:01:21,bridge=vmbr0"
	vm.Snapshots = nil
	vm.Status = "stopped"

	res := testExecutor(f, rec).Run(context.Background(), deployTeak(t, f, "01"))
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if n := f.called("clone:10121"); n != 1 {
		t.Errorf("cloned %d times, want 1", n)
	}
	if vm.Config["net0"] != "virtio=BC:24:11:00:01:21,bridge=ext01" || len(vm.Snapshots) != 1 || vm.Status != "running" {
		t.Errorf("not converged: net0=%q snaps=%v status=%s", vm.Config["net0"], vm.Snapshots, vm.Status)
	}
}

func TestTransientErrorsAreRetried(t *testing.T) {
	f := newCluster()
	fileExists := &proxmox.APIError{Status: 500, Message: "mkdir /mnt/pve/competitions/images/10121: File exists"}
	f.failOn("cloudinit:10121", fileExists, fileExists)

	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if n := f.called("cloudinit:10121"); n != 3 {
		t.Errorf("cloud-init called %d times, want 3", n)
	}
}

func TestPermanentFailureIsIsolatedAndRetriedOnce(t *testing.T) {
	f := newCluster()
	// A permanent error other than a missing privilege, which is never
	// retried (TestForbiddenItemIsNotRetried).
	denied := &proxmox.APIError{Method: "PUT", Path: "/x", Status: 400, Message: "Parameter verification failed."}
	f.failOn("setconfig:10121", denied, denied)
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), deployTeak(t, f, "01", "02"))
	if _, failed := res.Failed["team01-teak"]; !failed || !reflect.DeepEqual(res.Succeeded, []string{"team02-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("setconfig:10121"); n != 2 {
		t.Errorf("setconfig called %d times, want 2 (main pass + retry round)", n)
	}
	msgs := rec.find("team01-teak", EventFailed)
	if len(msgs) == 0 || msgs[0].Message != "PUT /x: 400 Parameter verification failed." {
		t.Errorf("failure events = %+v", msgs)
	}
}

func TestRetryRoundRecoversFailedClone(t *testing.T) {
	f := newCluster()
	f.failOn("clone:10121", errors.New("clone failed: unexpected error"))
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), deployTeak(t, f, "01"))
	if len(res.Failed) != 0 || f.called("clone:10121") != 2 {
		t.Fatalf("result = %+v, clone calls = %d", res, f.called("clone:10121"))
	}
	if len(rec.find("", EventInfo)) < 2 || !strings.HasPrefix(rec.find("", EventInfo)[0].Message, "retry round 1") {
		t.Errorf("info events = %+v", rec.find("", EventInfo))
	}
}

func TestTeardown(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, nil)
	f.add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil)
	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	delete(f.vms, 10125) // someone deleted it by hand after the preview
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), plan)
	if len(res.Failed) != 0 || len(res.Succeeded) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if _, ok := f.vms[10121]; ok || f.called("shutdown:10121:") != 1 {
		t.Errorf("team01-teak not shut down and deleted: calls=%v", f.calls)
	}
	if ev := rec.find("team01-oak", EventSkipped); len(ev) != 1 || ev[0].Message != "already deleted" {
		t.Errorf("oak events = %+v", ev)
	}
}

func TestReset(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, nil, "initial")
	plan, err := testPlanner(f).Reset(context.Background(), []string{"01"}, []string{"teak"}, "initial")
	if err != nil {
		t.Fatal(err)
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	var got []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "power:") || strings.HasPrefix(c, "rollback:") {
			got = append(got, c)
		}
	}
	want := []string{"power:10121:stop", "rollback:10121:initial", "power:10121:start"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// cancelTeardownDuringShutdown tears down team01-teak and stops the run
// with cause while its shutdown task is under way. finish says how the
// task ends, given the step's context; it may block on it.
func cancelTeardownDuringShutdown(t *testing.T, grace time.Duration, cause error, finish func(ctx context.Context) error, halt ...chan struct{}) (*fakeAPI, *recorder, Result) {
	t.Helper()
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, nil)
	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	f.waitGate = func(sctx context.Context, upid string) error {
		if !strings.Contains(upid, ":qmshutdown:") {
			return nil
		}
		cancel(cause)
		return finish(sctx)
	}
	rec := &recorder{}
	ex := testExecutor(f, rec)
	ex.Cfg.Jobs.CancelGrace = grace
	if len(halt) > 0 {
		ex.Halt = halt[0]
	}
	return f, rec, ex.Run(ctx, plan)
}

// A step under way when the job is cancelled finishes, and the job records
// it; the next step doesn't start, and nothing reads as failed.
func TestCancelLetsTheStepUnderWayFinish(t *testing.T) {
	f, rec, res := cancelTeardownDuringShutdown(t, time.Hour, ErrCancelRequested, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond): // the task ends on its own
			return nil
		}
	})
	if err := res.Interrupted["team01-teak"]; err == nil || err.Error() != "cancelled before delete" || len(res.Failed) != 0 {
		t.Errorf("result = %+v, want team01-teak interrupted before delete", res)
	}
	if len(rec.find("team01-teak", EventDone)) != 1 {
		t.Errorf("want the stop recorded done: %+v", rec.events)
	}
	// The delete never started, so the item's last step stays the stop.
	if ev := rec.find("team01-teak", EventInterrupted); len(ev) != 1 || ev[0].Step != "" || ev[0].Message != "cancelled before delete" {
		t.Errorf("interrupted events = %+v", ev)
	}
	// Only a shutdown ran: the VM's config is as the job before left it.
	if left := LeftBy(res.Interrupted["team01-teak"]); left != LeftUntouched {
		t.Errorf("left = %q, want untouched", left)
	}
	if len(rec.find("team01-teak", EventFailed)) != 0 {
		t.Errorf("failed events after a cancel: %+v", rec.find("team01-teak", EventFailed))
	}
	if f.called("delete:") != 0 {
		t.Errorf("the delete started after the cancel: %v", f.calls)
	}
	if ev := rec.find("", EventInfo); len(ev) == 0 || ev[len(ev)-1].Message != "finished: 0 succeeded, 0 failed, 1 interrupted, 0 blocked" {
		t.Errorf("last line = %+v", ev)
	}
}

// A step still under way when the grace runs out is cut off, and says its
// task may still be running.
func TestCancelCutsAStepOffAfterTheGrace(t *testing.T) {
	_, rec, res := cancelTeardownDuringShutdown(t, 20*time.Millisecond, ErrCancelRequested, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	want := "cancelled during stop; a Proxmox task it started may still be running"
	if err := res.Interrupted["team01-teak"]; err == nil || err.Error() != want {
		t.Errorf("result = %+v, want %q", res, want)
	}
	if ev := rec.find("team01-teak", EventInterrupted); len(ev) != 1 || ev[0].Step != pods.StepStop || ev[0].Message != want {
		t.Errorf("interrupted events = %+v", ev)
	}
	if len(rec.find("team01-teak", EventFailed)) != 0 {
		t.Errorf("failed events after a cancel: %+v", rec.find("team01-teak", EventFailed))
	}
}

// After a cancel, no step sends Proxmox anything new: a VM waiting for a
// delete slot isn't deleted once the slot frees, while the delete already
// under way finishes and is recorded.
func TestCancelSendsNothingNew(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "stopped"}, nil)
	f.add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "cedar", Status: "stopped"}, nil)
	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var once sync.Once
	f.waitGate = func(sctx context.Context, upid string) error {
		if !strings.Contains(upid, ":qmdestroy:") {
			return nil
		}
		// The first delete holds the only slot while the job is cancelled,
		// and ends well within the grace.
		once.Do(func() {
			cancel(ErrCancelRequested)
			time.Sleep(50 * time.Millisecond)
		})
		return nil
	}
	rec := &recorder{}
	ex := testExecutor(f, rec)
	ex.Cfg.Concurrency.Deletes = 1
	ex.Cfg.Jobs.CancelGrace = time.Hour
	start := time.Now()
	res := ex.Run(ctx, plan)
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s", took)
	}
	if n := f.called("delete:"); n != 1 {
		t.Errorf("%d deletes sent, want only the one under way at the cancel: %v", n, f.calls)
	}
	if len(res.Succeeded) != 1 || len(res.Interrupted) != 1 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v, want one deleted and one interrupted", res)
	}
	// The other VM was waiting for the slot, or hadn't got that far.
	for name, err := range res.Interrupted {
		if !strings.HasPrefix(err.Error(), "cancelled before ") {
			t.Errorf("%s: %v, want it cancelled before sending anything", name, err)
		}
		if len(rec.find(name, EventFailed)) != 0 {
			t.Errorf("%s failed events = %+v", name, rec.find(name, EventFailed))
		}
	}
}

// A reset cut off while a task is under way says how it left the VM's
// config: changed part way during the rollback, converged once only the
// start was left.
func TestCancelSaysHowItLeftTheConfig(t *testing.T) {
	for _, tc := range []struct {
		task string
		left Left
		msg  string
	}{
		{":qmrollback:", LeftChanged, "cancelled during rollback; a Proxmox task it started may still be running"},
		{":qmstart:", LeftConverged, "cancelled during start; a Proxmox task it started may still be running"},
	} {
		t.Run(tc.task, func(t *testing.T) {
			f := newCluster()
			f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "stopped"}, nil, "initial")
			plan, err := testPlanner(f).Reset(context.Background(), []string{"01"}, []string{"teak"}, "initial")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			f.waitGate = func(sctx context.Context, upid string) error {
				if !strings.Contains(upid, tc.task) {
					return nil
				}
				cancel(ErrCancelRequested)
				<-sctx.Done() // still running when the grace runs out
				return sctx.Err()
			}
			rec := &recorder{}
			ex := testExecutor(f, rec)
			ex.Cfg.Jobs.CancelGrace = 20 * time.Millisecond
			res := ex.Run(ctx, plan)
			err = res.Interrupted["team01-teak"]
			if err == nil || err.Error() != tc.msg || LeftBy(err) != tc.left {
				t.Errorf("interrupted = %v (left %q), want %q (left %q)", err, LeftBy(err), tc.msg, tc.left)
			}
			if len(rec.find("team01-teak", EventFailed)) != 0 {
				t.Errorf("failed events after a cancel: %+v", rec.find("team01-teak", EventFailed))
			}
		})
	}
}

// A shutdown during a cancel's grace (Halt) ends it at once, so a
// cancelled run still returns within StopBudget.
func TestHaltEndsTheGrace(t *testing.T) {
	halt := make(chan struct{})
	start := time.Now()
	_, _, res := cancelTeardownDuringShutdown(t, time.Hour, ErrCancelRequested, func(ctx context.Context) error {
		close(halt)
		<-ctx.Done()
		return ctx.Err()
	}, halt)
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s", took)
	}
	if err := res.Interrupted["team01-teak"]; err == nil || !strings.HasPrefix(err.Error(), "cancelled during stop") {
		t.Errorf("result = %+v", res)
	}
}

// A retry round cancelled before it sends anything leaves the config as an
// earlier round converged it: every config step finished in round one,
// which failed only at start.
func TestCancelSeesAnEarlierRoundConverged(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.failOn("power:10121:start", &proxmox.APIError{Status: 400, Message: "start: bad request"})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	startFailed := false
	f.onRecord = func(key string) {
		switch {
		case key == "power:10121:start":
			startFailed = true
		case startFailed && key == "config:10121": // round two's network step reads first
			cancel(ErrCancelRequested)
		}
	}
	rec := &recorder{}
	res := testExecutor(f, rec).Run(ctx, plan)
	err := res.Interrupted["team01-teak"]
	if err == nil || LeftBy(err) != LeftConverged {
		t.Fatalf("interrupted = %v (left %q), want converged; result %+v", err, LeftBy(err), res)
	}
}

// Other stops, such as a shutdown, cut the step off at once.
func TestOtherStopsDontWait(t *testing.T) {
	start := time.Now()
	_, _, res := cancelTeardownDuringShutdown(t, time.Hour, context.Canceled, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s", took)
	}
	if err := res.Interrupted["team01-teak"]; err == nil || !strings.HasPrefix(err.Error(), "stopped during stop") {
		t.Errorf("result = %+v", res)
	}
}

func TestCancelledRunReportsNotRun(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := testExecutor(f, &recorder{}).Run(ctx, plan)
	if err := res.Interrupted["team01-teak"]; err == nil || err.Error() != "stopped before it started" || len(res.Failed) != 0 {
		t.Errorf("result = %+v, want team01-teak interrupted as not run", res)
	}
	if len(res.Succeeded) != 0 {
		t.Errorf("Succeeded = %v, want none", res.Succeeded)
	}
}

func TestRebuildDeletesOldTemplateOnItsNode(t *testing.T) {
	f := newCluster()
	// The old template lives on a different node than its master.
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "spruce", Template: true}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	tpl := f.vms[9021]
	if tpl == nil || !tpl.Template || tpl.Node != "cedar" {
		t.Errorf("rebuilt template = %+v, want a template on the master's node", tpl)
	}
	if f.vms[10121] == nil {
		t.Error("team01-teak was not cloned from the rebuilt template")
	}
	if n := f.called("volume:"); n != 0 {
		t.Errorf("%d blind volume deletes, want 0", n)
	}
}

func TestHalfBuiltTemplateIsResumed(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	locked := &proxmox.APIError{Status: 500, Message: "VM is locked (clone)"}
	f.failOn("template:9021", locked, locked, locked, locked, locked, locked)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if tpl := f.vms[9021]; tpl == nil || !tpl.Template {
		t.Errorf("template = %+v, want converted", tpl)
	}
	if f.vms[10121] == nil {
		t.Error("team01-teak was not cloned")
	}
	if n := f.called("clone:9021"); n != 1 {
		t.Errorf("master cloned %d times, want 1", n)
	}
}

func TestRebuildDeleteFailureExplainsLinkedClones(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "spruce", Template: true}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	busy := &proxmox.APIError{Status: 400, Message: "can't remove base volume with linked clones"}
	f.failOn("delete:9021", busy, busy)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	err = res.Failed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), "linked-clone disks still use it") {
		t.Errorf("error = %v", err)
	}
	if n := f.called("volume:"); n != 0 {
		t.Errorf("%d blind volume deletes, want 0", n)
	}
}

func TestTransientClusterListingIsRetried(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.failOn("cluster", &proxmox.APIError{Status: 500, Message: "internal error"})

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if tpl := f.vms[9021]; tpl == nil || !tpl.Template {
		t.Errorf("template = %+v", tpl)
	}
}

func TestFailedCloudInitRegenIsRepairedByRetryRound(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.failOn("cloudinit:10121", &proxmox.APIError{Status: 400, Message: "Parameter verification failed."})

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 || !reflect.DeepEqual(res.Succeeded, []string{"team01-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("cloudinit:10121"); n != 2 {
		t.Errorf("cloud-init called %d times, want 2", n)
	}
}

func TestMasterIsRestartedAfterFailedTemplateClone(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	f.failOn("clone:9021", errors.New("clone failed"), errors.New("clone failed"))

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if _, failed := res.Failed["team01-teak"]; !failed {
		t.Fatalf("result = %+v", res)
	}
	if st := f.vms[121].Status; st != "running" {
		t.Errorf("master status = %s, want running", st)
	}
}

func TestClonePostTimeoutDoesNotRecloneOrFail(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.cloneThenFail = map[int]error{10121: &proxmox.APIError{Status: 500, Message: "Connection timed out"}}
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if n := f.called("clone:10121"); n != 1 {
		t.Errorf("clone POSTed %d times, want 1", n)
	}
	for _, e := range rec.find("team01-teak", EventFailed) {
		t.Errorf("unexpected failure event: %+v", e)
	}
}

func TestSnapshotOfRunningVMIsRefused(t *testing.T) {
	f := newCluster()
	testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	vm := f.vms[10121]
	vm.Snapshots = nil
	vm.Status = "running"

	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	err := res.Failed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("error = %v", err)
	}
	if len(vm.Snapshots) != 0 {
		t.Errorf("snapshots = %v, want none", vm.Snapshots)
	}
}

// cancelDuringMasterClone makes the master clone's first wait cancel the run
// and fail, and answers later waits for that task with detachedWait.
func cancelDuringMasterClone(f *fakeAPI, cancel func(), detachedWait func(ctx context.Context) error) {
	first := true
	f.waitHook = func(ctx context.Context, upid string) error {
		if !strings.Contains(upid, ":qmclone:121:") {
			return nil
		}
		if first {
			first = false
			cancel()
			return context.Canceled
		}
		return detachedWait(ctx)
	}
}

func TestMasterRestartWaitsForCloneTask(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	cancelDuringMasterClone(f, cancel, func(ctx context.Context) error {
		if ctx.Err() != nil {
			return errors.New("detached wait ran on the cancelled context")
		}
		return nil
	})

	testExecutor(f, &recorder{}).Run(ctx, plan)
	var got []string
	for _, c := range f.calls {
		if strings.Contains(c, ":qmclone:121:") || c == "power:121:start" {
			got = append(got, c[:strings.Index(c, ":")])
		}
	}
	// The cut-off copy is stopped, then waited for, then the master starts.
	if !reflect.DeepEqual(got, []string{"wait", "stoptask", "wait", "power"}) {
		t.Errorf("calls = %v", f.calls)
	}
	if st := f.vms[121].Status; st != "running" {
		t.Errorf("master status = %s, want running", st)
	}
}

func TestMasterStaysStoppedIfCloneTaskCannotBeAwaited(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	cancelDuringMasterClone(f, cancel, func(context.Context) error { return errors.New("no answer") })
	rec := &recorder{}

	testExecutor(f, rec).Run(ctx, plan)
	if st := f.vms[121].Status; st != "stopped" {
		t.Errorf("master status = %s, want stopped", st)
	}
	if f.called("power:121:start") != 0 {
		t.Error("master was started while its clone task may still run")
	}
	ev := rec.find("teak.tango.delta.tpl", EventFailed)
	found := false
	for _, e := range ev {
		if strings.HasPrefix(e.Message, "master teak.tango.delta left stopped: its clone task may still be running") {
			found = true
		}
	}
	if !found {
		t.Errorf("events = %+v", ev)
	}
}

func TestCloneWaitsOutLockedVMBeforeReposting(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.cloneThenFail = map[int]error{10121: &proxmox.APIError{Status: 500, Message: "Connection timed out"}}
	f.lockConfig = map[int]int{10121: 2}
	f.lockName = "backup"
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if n := f.called("clone:10121"); n != 1 {
		t.Errorf("clone POSTed %d times, want 1", n)
	}
	if n := f.called("locked-write:"); n != 0 {
		t.Errorf("%d writes reached a locked VM", n)
	}
	seen := false
	for _, e := range rec.find("team01-teak", EventInfo) {
		seen = seen || strings.Contains(e.Message, "backup")
	}
	if !seen {
		t.Error("no event reported the lock holder")
	}
}

func TestLockedPrecheckIsRetriedWithoutConfiguredPatterns(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.cloneThenFail = map[int]error{10121: &proxmox.APIError{Status: 500, Message: "Connection timed out"}}
	f.lockConfig = map[int]int{10121: 1}
	rec := &recorder{}
	ex := testExecutor(f, rec)
	ex.Cfg.Retry.TransientPatterns = nil

	res := ex.Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if got := rec.find("team01-teak", EventFailed); len(got) != 0 {
		t.Errorf("failure events = %+v, want the lock retried inside the step", got)
	}
}

func TestMasterRestartWaitsForLockToClear(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	// The master becomes locked after it has been stopped.
	f.waitHook = func(_ context.Context, upid string) error {
		if strings.Contains(upid, ":qmstop:121:") {
			f.lockConfig = map[int]int{121: 2}
		}
		return nil
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if n := f.called("locked-write:"); n != 0 {
		t.Errorf("%d writes reached the locked master", n)
	}
	if st := f.vms[121].Status; st != "running" {
		t.Errorf("master status = %s, want running", st)
	}
}

func TestMasterStaysStoppedIfLockNeverClears(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	f.lockConfig = map[int]int{121: 1000}
	f.lockName = "backup"
	plan := deployTeak(t, f, "01")
	rec := &recorder{}
	ex := testExecutor(f, rec)
	polls := 0
	ex.Sleep = func(context.Context, time.Duration) error {
		if polls++; polls > 3 {
			return context.DeadlineExceeded // the wait budget ran out
		}
		return nil
	}

	ex.Run(context.Background(), plan)
	if st := f.vms[121].Status; st != "stopped" {
		t.Errorf("master status = %s, want stopped", st)
	}
	found := false
	for _, e := range rec.find("teak.tango.delta.tpl", EventFailed) {
		found = found || (strings.Contains(e.Message, "left stopped") && strings.Contains(e.Message, "still locked (lock: backup)"))
	}
	if !found {
		t.Errorf("events = %+v", rec.find("teak.tango.delta.tpl", EventFailed))
	}
}

func TestUnexpectedNonTemplateIsNotConvertedEvenIfUnplanned(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	// Appears after the plan was made, and this run did not create it.
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar"}, nil)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	err := res.Failed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), "wasn't created by this run") {
		t.Errorf("result = %+v", res)
	}
	if f.called("template:9021") != 0 {
		t.Error("converted a VM this run did not create")
	}
}

func TestExistingNonTemplateIsNotConvertedWhenPlanned(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	plan := deployTeak(t, f, "01")
	f.vms[9021].Template = false // changed after the plan was made

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	err := res.Failed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), "not a template") {
		t.Errorf("result = %+v", res)
	}
	if f.called("template:9021") != 0 {
		t.Error("converted a VM the planner did not create")
	}
}

func TestMasterNotRestartedWhileCloneTargetIsLocked(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	// The clone POST times out after Proxmox accepted it, and the target stays
	// locked by the in-flight clone through every pre-check retry.
	f.cloneThenFail = map[int]error{9021: &proxmox.APIError{Status: 500, Message: "Connection timed out"}}
	f.lockConfig = map[int]int{9021: 7}
	f.powerHook = func(vmid int, action string) {
		if vmid == 121 && action == "start" && f.lockConfig[9021] > 0 {
			t.Errorf("master started while the clone target is still locked (%d reads left)", f.lockConfig[9021])
		}
	}

	testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if st := f.vms[121].Status; st != "running" {
		t.Errorf("master status = %s, want running once the target unlocked", st)
	}
}

func TestMasterStaysStoppedIfLockCannotBeConfirmed(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	boom := &proxmox.APIError{Status: 500, Message: "boom"}
	for i := 0; i < 50; i++ {
		f.failOn("config:121", boom)
	}
	rec := &recorder{}
	ex := testExecutor(f, rec)
	polls := 0
	ex.Sleep = func(context.Context, time.Duration) error {
		if polls++; polls > 3 {
			return context.DeadlineExceeded
		}
		return nil
	}

	ex.Run(context.Background(), plan)
	if st := f.vms[121].Status; st != "stopped" {
		t.Errorf("master status = %s, want stopped", st)
	}
	found := false
	for _, e := range rec.find("teak.tango.delta.tpl", EventFailed) {
		found = found || strings.Contains(e.Message, "could not check that nothing is working on the")
	}
	if !found {
		t.Errorf("events = %+v", rec.find("teak.tango.delta.tpl", EventFailed))
	}
}

func TestRunDoesNotMutateThePlan(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "spruce", Template: true}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	before := append([]pods.TemplateSpec(nil), plan.Templates...)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if !reflect.DeepEqual(plan.Templates, before) {
		t.Errorf("plan changed: %+v, was %+v", plan.Templates, before)
	}
}

func TestRebuildRefusedWhenTeamVMsAppearAfterPlanning(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: "teak.*", Teams: []string{"02"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	err = res.Failed["team02-teak"]
	if err == nil || !strings.Contains(err.Error(), "team VMs such as team01-teak still use this template") {
		t.Errorf("result = %+v", res)
	}
	if f.vms[9021] == nil || f.called("delete:9021") != 0 {
		t.Error("template was deleted")
	}
}

func TestMasterIsNotStoppedWhenNothingWillClone(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
	plan := deployTeak(t, f, "01")

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if f.called("power:121:") != 0 || f.called("clone:9021") != 0 {
		t.Errorf("master was touched: %v", f.calls)
	}
}

func TestItemWithoutStepsFails(t *testing.T) {
	f := newCluster()
	plan := &pods.Plan{Kind: pods.KindPower, Items: []pods.Item{{Name: "team01-teak", VMID: 10121, Node: "cedar"}}}
	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if err := res.Failed["team01-teak"]; err == nil || !strings.Contains(err.Error(), "no steps planned") {
		t.Errorf("result = %+v", res)
	}
}

func TestFailedRefreshKeepsEarlierItemError(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10121, VM.Config.Network)"}
	f.failOn("setconfig:10121", denied, denied)
	armed := false
	f.onRecord = func(key string) {
		if key == "setconfig:10121" && !armed {
			armed = true
			for i := 0; i < 6; i++ {
				f.failOn("cluster", &proxmox.APIError{Status: 500, Message: "pve is down"})
			}
		}
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	err := res.Failed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), "network") {
		t.Errorf("error = %v, want the original network step error", err)
	}
}

func TestShutdownTimeoutIsNotResent(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, nil)
	plan, err := testPlanner(f).Power(context.Background(), []string{"01"}, nil, "shutdown")
	if err != nil {
		t.Fatal(err)
	}
	timeout := &proxmox.TaskError{UPID: "x", ExitStatus: "timeout waiting on systemd"}
	key := "wait:" + upid("cedar", "qmshutdown", 10121)
	f.failOn(key, timeout, timeout, timeout, timeout, timeout, timeout)
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Retry.Rounds = 0

	res := ex.Run(context.Background(), plan)
	if _, failed := res.Failed["team01-teak"]; !failed {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("power:10121:shutdown"); n != 1 {
		t.Errorf("shutdown POSTed %d times, want 1", n)
	}
}

func TestMasterStoppedDuringCancelIsRestarted(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	first := true
	f.waitHook = func(_ context.Context, upid string) error {
		if strings.Contains(upid, ":qmstop:121:") && first {
			first = false
			cancel()
			return context.Canceled
		}
		return nil
	}

	testExecutor(f, &recorder{}).Run(ctx, plan)
	if st := f.vms[121].Status; st != "running" {
		t.Errorf("master status = %s, want running", st)
	}
}

func TestMasterStoppedDuringCancelIsNeverSilentlyLeftStopped(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	f.waitHook = func(_ context.Context, upid string) error {
		if strings.Contains(upid, ":qmstop:121:") {
			cancel()
			return errors.New("no answer")
		}
		return nil
	}
	rec := &recorder{}

	testExecutor(f, rec).Run(ctx, plan)
	if f.vms[121].Status == "stopped" {
		found := false
		for _, e := range rec.find("teak.tango.delta.tpl", EventFailed) {
			found = found || strings.Contains(e.Message, "left stopped")
		}
		if !found {
			t.Error("master left stopped without an event")
		}
	}
}

func newlyBuiltTeamVMFails(t *testing.T) (*fakeAPI, *pods.Plan) {
	t.Helper()
	f := newCluster()
	return f, deployTeak(t, f, "01")
}

func TestCancelledRunRemovesTeamVMItCreated(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.onRecord = func(key string) {
		if key == "setconfig:10121" {
			cancel()
		}
	}
	rec := &recorder{}

	res := testExecutor(f, rec).Run(ctx, plan)
	if f.vms[10121] != nil {
		t.Error("half-built team VM was left behind")
	}
	if !reflect.DeepEqual(res.Removed, []string{"team01-teak"}) {
		t.Errorf("Removed = %v", res.Removed)
	}
	if err := res.Interrupted["team01-teak"]; !errors.Is(err, context.Canceled) || len(res.Failed) != 0 {
		t.Errorf("Interrupted = %v, Failed = %v; want team01-teak interrupted, the cancel reason kept", res.Interrupted, res.Failed)
	}
	if ev := rec.find("team01-teak", EventInfo); len(ev) == 0 || !strings.Contains(ev[len(ev)-1].Message, "removed team01-teak (created this run; cancelled before it finished)") {
		t.Errorf("events = %+v", ev)
	}
	if f.vms[9021] == nil || !f.vms[9021].Template {
		t.Error("finished template was not kept")
	}
}

func TestPermanentFailureRemovesTeamVMItCreated(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10121, VM.Config.Network)"}
	f.failOn("setconfig:10121", denied, denied)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.vms[10121] != nil {
		t.Error("half-built team VM was left behind")
	}
	if !reflect.DeepEqual(res.Removed, []string{"team01-teak"}) {
		t.Errorf("Removed = %v", res.Removed)
	}
	if err := res.Failed["team01-teak"]; err == nil || !strings.Contains(err.Error(), "VM.Config.Network") {
		t.Errorf("Failed = %v", err)
	}
}

func TestExistingTeamVMIsNeverRemoved(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, map[string]string{
		"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0",
		"net1": "virtio=BC:24:11:00:01:22,bridge=vmbr1",
	})
	plan := deployTeak(t, f, "01")
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10121, VM.Config.Network)"}
	f.failOn("setconfig:10121", denied, denied)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if _, failed := res.Failed["team01-teak"]; !failed {
		t.Fatalf("result = %+v", res)
	}
	if f.vms[10121] == nil || len(res.Removed) != 0 || f.called("delete:10121") != 0 {
		t.Errorf("pre-existing VM was touched: removed=%v calls=%v", res.Removed, f.calls)
	}
}

func TestCancelledTemplateCopyIsCompleted(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	cancelDuringMasterClone(f, cancel, func(context.Context) error { return nil })
	rec := &recorder{}

	res := testExecutor(f, rec).Run(ctx, plan)
	if tpl := f.vms[9021]; tpl == nil || !tpl.Template {
		t.Errorf("template = %+v, want the finished copy converted", tpl)
	}
	if !reflect.DeepEqual(res.Completed, []string{"teak.tango.delta.tpl"}) {
		t.Errorf("Completed = %v", res.Completed)
	}
	if st := f.vms[121].Status; st != "running" {
		t.Errorf("master status = %s, want running", st)
	}
	if ev := rec.find("teak.tango.delta.tpl", EventInfo); len(ev) == 0 ||
		ev[len(ev)-1].Message != "completed template teak.tango.delta.tpl after cancel" {
		t.Errorf("events = %+v", ev)
	}
}

func TestTemplateThatCannotBeConvertedIsRemoved(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/9021, VM.Config.Options)"}
	for i := 0; i < 10; i++ {
		f.failOn("template:9021", denied)
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.vms[9021] != nil {
		t.Error("unconvertible copy was left behind")
	}
	if !reflect.DeepEqual(res.Removed, []string{"teak.tango.delta.tpl"}) || len(res.Completed) != 0 {
		t.Errorf("Removed = %v, Completed = %v", res.Removed, res.Completed)
	}
}

func TestLockedVMIsNotForceRemoved(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	denied := &proxmox.APIError{Status: 400, Message: "Parameter verification failed."}
	f.failOn("setconfig:10121", denied, denied)
	f.lockConfig = map[int]int{10121: 1000}
	f.lockName = "backup"
	rec := &recorder{}
	ex := testExecutor(f, rec)
	polls := 0
	ex.Sleep = func(context.Context, time.Duration) error {
		if polls++; polls > 5 {
			return context.DeadlineExceeded // the cleanup budget ran out
		}
		return nil
	}

	res := ex.Run(context.Background(), plan)
	if f.vms[10121] == nil || f.called("delete:10121") != 0 {
		t.Error("locked VM was deleted")
	}
	if err := res.CleanupFailed["team01-teak"]; err == nil || len(res.Removed) != 0 {
		t.Errorf("CleanupFailed = %v, Removed = %v", res.CleanupFailed, res.Removed)
	}
	found := false
	for _, e := range rec.find("team01-teak", EventFailed) {
		found = found || (strings.Contains(e.Message, "could not remove team01-teak (10121)") &&
			strings.Contains(e.Message, "backup") && strings.HasSuffix(e.Message, "remove it in Proxmox"))
	}
	if !found {
		t.Errorf("events = %+v", rec.find("team01-teak", EventFailed))
	}
}

func TestSuccessfulDeployCleansUpNothing(t *testing.T) {
	f := newCluster()
	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01", "02"))
	if len(res.Failed) != 0 || len(res.Removed) != 0 || len(res.Completed) != 0 || len(res.CleanupFailed) != 0 {
		t.Errorf("result = %+v", res)
	}
	if n := f.called("delete:"); n != 0 {
		t.Errorf("%d delete calls, want 0", n)
	}
}

// appearAfterListing makes a foreign team01-teak show up at 10121 right after
// the executor lists the cluster, so it is missing from the executor's view
// when the clone step runs.
func appearAfterListing(f *fakeAPI, node string, cfg map[string]string, status string) {
	armed := true
	f.onRecord = func(key string) {
		if key == "cluster" && armed {
			armed = false
			f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: node, Status: status}, cfg)
		}
	}
}

func foreignConfig() map[string]string {
	return map[string]string{
		"name": "team01-teak",
		"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0",
		"net1": "virtio=BC:24:11:00:01:22,bridge=vmbr1",
	}
}

func TestPrecheckReadErrorDoesNotClaimForeignVM(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	appearAfterListing(f, plan.Items[0].Node, foreignConfig(), "running")
	f.failOn("config:10121", &proxmox.APIError{Status: 400, Message: "bad request"})

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.vms[10121] == nil || f.called("delete:10121") != 0 || f.called("power:10121:stop") != 0 {
		t.Errorf("foreign VM was touched: %v", f.calls)
	}
	if n := f.called("clone:10121"); n != 0 {
		t.Errorf("clone POSTed %d times after an unreadable pre-check, want 0", n)
	}
	if len(res.Removed) != 0 {
		t.Errorf("Removed = %v", res.Removed)
	}
}

func TestPrecheckFindingSameNameVMNeverClaimsIt(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	appearAfterListing(f, plan.Items[0].Node, foreignConfig(), "running")
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10121, VM.Config.Network)"}
	f.failOn("setconfig:10121", denied, denied)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if _, failed := res.Failed["team01-teak"]; !failed {
		t.Fatalf("result = %+v", res)
	}
	if f.called("clone:10121") != 0 {
		t.Error("cloned over an existing VM")
	}
	if f.vms[10121] == nil || len(res.Removed) != 0 || f.called("delete:10121") != 0 {
		t.Errorf("VM this run did not create was removed: %v", f.calls)
	}
}

func TestCleanupVerifiesByConfigWhenListingFails(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	// The POST fails after the VM was created, and the only listing this run
	// took was before that, so a stale listing does not know the VM.
	f.cloneThenFail = map[int]error{10121: errors.New("clone failed")}
	f.onRecord = func(key string) {
		if key == "clone:10121" {
			for i := 0; i < 6; i++ {
				f.failOn("cluster", &proxmox.APIError{Status: 500, Message: "pve is down"})
			}
		}
	}
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Retry.Rounds = 0

	res := ex.Run(context.Background(), plan)
	if f.vms[10121] != nil {
		t.Errorf("half-built VM leaked silently; result = %+v", res)
	}
	if !reflect.DeepEqual(res.Removed, []string{"team01-teak"}) {
		t.Errorf("Removed = %v, CleanupFailed = %v", res.Removed, res.CleanupFailed)
	}
}

func TestCleanupReportsWhenGoneCannotBeConfirmed(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	denied := &proxmox.APIError{Status: 400, Message: "Parameter verification failed."}
	f.failOn("setconfig:10121", denied, denied)
	seen := 0
	f.onRecord = func(key string) {
		if key == "setconfig:10121" {
			if seen++; seen == 2 {
				for i := 0; i < 6; i++ {
					f.failOn("cluster", &proxmox.APIError{Status: 500, Message: "pve is down"})
				}
				for i := 0; i < 20; i++ {
					f.failOn("config:10121", &proxmox.APIError{Status: 400, Message: "bad request"})
				}
			}
		}
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	err := res.CleanupFailed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), "could not confirm team01-teak (10121) is gone; check Proxmox") {
		t.Errorf("CleanupFailed = %v", res.CleanupFailed)
	}
	if f.vms[10121] == nil || f.called("delete:10121") != 0 {
		t.Error("deleted a VM whose identity could not be confirmed")
	}
}

func TestVMSwappedBeforeDeleteIsNotDeleted(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	denied := &proxmox.APIError{Status: 400, Message: "Parameter verification failed."}
	f.failOn("setconfig:10121", denied, denied)
	seen, reads := 0, 0
	f.onRecord = func(key string) {
		switch {
		case key == "setconfig:10121":
			seen++
		case key == "config:10121" && seen == 2:
			// Swap it just before cleanup's read that precedes the delete.
			if reads++; reads == 1 {
				f.vms[10121].Name = "someone-else"
				f.vms[10121].Config["name"] = "someone-else"
			}
		}
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.vms[10121] == nil || f.called("delete:10121") != 0 {
		t.Error("deleted a VM that is not the one this run created")
	}
	err := res.CleanupFailed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), "VMID 10121 now holds someone-else, which this run did not create; left alone. Check whether team01-teak needs removing.") {
		t.Errorf("CleanupFailed = %v", res.CleanupFailed)
	}
}

func TestFailedCopyTaskIsRemovedNotConverted(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.waitHook = func(_ context.Context, upid string) error {
		if strings.Contains(upid, ":qmclone:121:") {
			return &proxmox.TaskError{UPID: upid, ExitStatus: "clone failed: no space left"}
		}
		return nil
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.called("template:9021") != 0 {
		t.Error("converted a copy whose task failed")
	}
	if f.vms[9021] != nil || !reflect.DeepEqual(res.Removed, []string{"teak.tango.delta.tpl"}) {
		t.Errorf("leftover not removed: Removed = %v, CleanupFailed = %v", res.Removed, res.CleanupFailed)
	}
}

func TestNamelessLockedTargetIsWaitedOutBeforeDecidingItsGone(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	denied := &proxmox.APIError{Status: 400, Message: "Parameter verification failed."}
	f.failOn("setconfig:10121", denied, denied)
	seen := 0
	f.onRecord = func(key string) {
		if key == "setconfig:10121" {
			if seen++; seen == 2 {
				f.lockConfig = map[int]int{10121: 3}
				f.lockName = "clone"
				f.lockHidesName = true
			}
		}
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.vms[10121] != nil || !reflect.DeepEqual(res.Removed, []string{"team01-teak"}) {
		t.Errorf("Removed = %v, CleanupFailed = %v", res.Removed, res.CleanupFailed)
	}
	if f.lockConfig[10121] != 0 {
		t.Errorf("deleted with the lock still held (%d reads left)", f.lockConfig[10121])
	}
}

// A copy whose task's end was lost is completed by cleanup, but only once
// its lock clears.
func TestLockedCopyIsWaitedOutBeforeCompleting(t *testing.T) {
	f := newCluster() // the master is stopped, so the restart path does not wait
	plan := deployTeak(t, f, "01")
	f.failOn("wait:"+upid("cedar", "qmclone", 121), errors.New("polling task: connection reset by peer"))
	f.lockConfig = map[int]int{9021: 6}
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Retry.Rounds = 0

	res := ex.Run(context.Background(), plan)
	if n := f.called("locked-write:9021"); n != 0 {
		t.Errorf("%d writes reached the copy while it was locked", n)
	}
	if !reflect.DeepEqual(res.Completed, []string{"teak.tango.delta.tpl"}) || !f.vms[9021].Template {
		t.Errorf("Completed = %v, CleanupFailed = %v", res.Completed, res.CleanupFailed)
	}
}

func TestFinishedEventCountsCleanup(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	denied := &proxmox.APIError{Status: 400, Message: "Parameter verification failed."}
	f.failOn("setconfig:10121", denied, denied)
	rec := &recorder{}

	testExecutor(f, rec).Run(context.Background(), plan)
	infos := rec.find("", EventInfo)
	last := infos[len(infos)-1].Message
	if !strings.HasPrefix(last, "finished:") || !strings.Contains(last, "1 removed") {
		t.Errorf("last job event = %q", last)
	}
}

func TestFinishedEventOmitsEmptyCleanup(t *testing.T) {
	f := newCluster()
	rec := &recorder{}
	testExecutor(f, rec).Run(context.Background(), deployTeak(t, f, "01"))
	infos := rec.find("", EventInfo)
	last := infos[len(infos)-1].Message
	if !strings.HasPrefix(last, "finished: 1 succeeded") || strings.Contains(last, "cleanup") {
		t.Errorf("last job event = %q, want no cleanup counts when there was nothing to clean up", last)
	}
}

func notExist(vmid int) error {
	return &proxmox.APIError{Status: 500, Message: fmt.Sprintf("Configuration file 'nodes/x/qemu-server/%d.conf' does not exist", vmid)}
}

func TestLateFirstPostKeepsTeamVMOwned(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	// The first POST lands but the client times out; the retry's probe then
	// sees "does not exist" once, and its POST is told the VM already exists.
	f.cloneThenFail = map[int]error{10121: &proxmox.APIError{Status: 500, Message: "Connection timed out"}}
	armed := true
	f.onRecord = func(key string) {
		if key == "clone:10121" && armed {
			armed = false
			f.failOn("config:10121", notExist(10121))
		}
	}
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10121, VM.Config.Network)"}
	f.failOn("setconfig:10121", denied, denied)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.vms[10121] != nil || !reflect.DeepEqual(res.Removed, []string{"team01-teak"}) {
		t.Errorf("VM this run created leaked: Removed = %v, CleanupFailed = %v", res.Removed, res.CleanupFailed)
	}
}

func TestLateFirstPostKeepsTemplateCopyOwned(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.cloneThenFail = map[int]error{9021: &proxmox.APIError{Status: 500, Message: "Connection timed out"}}
	armed := true
	f.onRecord = func(key string) {
		if key == "clone:9021" && armed {
			armed = false
			f.failOn("config:9021", notExist(9021))
		}
	}
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/9021, VM.Config.Options)"}
	for i := 0; i < 10; i++ {
		f.failOn("template:9021", denied)
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.vms[9021] != nil || !reflect.DeepEqual(res.Removed, []string{"teak.tango.delta.tpl"}) {
		t.Errorf("template copy leaked: Removed = %v, CleanupFailed = %v", res.Removed, res.CleanupFailed)
	}
}

func TestForeignVMAppearingBeforePostIsNotClaimed(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	node := plan.Items[0].Node
	armed := true
	f.onRecord = func(key string) {
		if key == "clone:10121" && armed { // after the probe said absent
			armed = false
			f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: node}, foreignConfig())
		}
	}
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10121, VM.Config.Network)"}
	f.failOn("setconfig:10121", denied, denied)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.vms[10121] == nil || f.called("delete:10121") != 0 || len(res.Removed) != 0 {
		t.Errorf("foreign VM was removed: Removed = %v, calls = %v", res.Removed, f.calls)
	}
}

func TestVMWithAnotherNameAtTheTargetIsLeftAlone(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	appearAfterListing(f, plan.Items[0].Node, map[string]string{"name": "someone-else"}, "running")

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.called("clone:10121") != 0 {
		t.Error("POSTed a clone over a VM with another name")
	}
	if f.vms[10121] == nil || f.called("delete:10121") != 0 || len(res.Removed) != 0 {
		t.Errorf("another VM was removed: %v", f.calls)
	}
}

func TestCleanupAdviceOnlyOffersRemovalOfOwnedVMs(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	denied := &proxmox.APIError{Status: 400, Message: "Parameter verification failed."}
	f.failOn("setconfig:10121", denied, denied)
	seen := 0
	f.onRecord = func(key string) {
		if key == "setconfig:10121" {
			if seen++; seen == 2 {
				f.vms[10121].Name = "someone-else"
				f.vms[10121].Config["name"] = "someone-else"
			}
		}
	}
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), plan)
	err := res.CleanupFailed["team01-teak"]
	var notOurs *notOursError
	if !errors.As(err, &notOurs) {
		t.Fatalf("CleanupFailed = %v", res.CleanupFailed)
	}
	if strings.Contains(CleanupAdvice("team01-teak", err), "remove it in Proxmox") {
		t.Errorf("advice = %q", CleanupAdvice("team01-teak", err))
	}
	for _, e := range rec.find("team01-teak", EventFailed) {
		if strings.Contains(e.Message, "remove it in Proxmox") {
			t.Errorf("event advises deleting a foreign VM: %q", e.Message)
		}
	}
	plain := CleanupAdvice("team03-dc", errors.New("still locked (lock: backup)"))
	if plain != "Could not remove team03-dc: still locked (lock: backup); remove it in Proxmox." {
		t.Errorf("advice = %q", plain)
	}
}

func TestFailedMasterCloneTaskIsNotRestarted(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.waitHook = func(_ context.Context, upid string) error {
		if strings.Contains(upid, ":qmclone:121:") {
			return &proxmox.TaskError{UPID: upid, ExitStatus: "timeout waiting for storage lock"}
		}
		return nil
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if n := f.called("clone:9021"); n != 1 {
		t.Errorf("master cloned %d times, want 1", n)
	}
	if f.called("template:9021") != 0 || f.vms[9021] != nil || !reflect.DeepEqual(res.Removed, []string{"teak.tango.delta.tpl"}) {
		t.Errorf("Removed = %v, CleanupFailed = %v", res.Removed, res.CleanupFailed)
	}
}

func TestVMGoneBeforeCleanupIsReported(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	denied := &proxmox.APIError{Status: 400, Message: "Parameter verification failed."}
	f.failOn("setconfig:10121", denied, denied)
	seen := 0
	f.onRecord = func(key string) {
		if key == "setconfig:10121" {
			if seen++; seen == 2 {
				delete(f.vms, 10121)
			}
		}
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if !reflect.DeepEqual(res.AlreadyGone, []string{"team01-teak"}) || len(res.Removed) != 0 {
		t.Errorf("AlreadyGone = %v, Removed = %v", res.AlreadyGone, res.Removed)
	}
}

func TestAlreadyConvertedTemplateIsReported(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	first := true
	f.waitHook = func(_ context.Context, upid string) error {
		if strings.Contains(upid, ":qmtemplate:") && first { // the conversion's wait: it took effect, then the job was cancelled
			first = false
			cancel()
			return context.Canceled
		}
		return nil
	}
	rec := &recorder{}

	res := testExecutor(f, rec).Run(ctx, plan)
	if tpl := f.vms[9021]; tpl == nil || !tpl.Template || len(res.Removed) != 0 {
		t.Errorf("template = %+v, Removed = %v", tpl, res.Removed)
	}
	found := false
	for _, e := range rec.find("teak.tango.delta.tpl", EventInfo) {
		found = found || e.Message == "template teak.tango.delta.tpl was already complete"
	}
	if !found {
		t.Errorf("events = %+v", rec.find("teak.tango.delta.tpl", EventInfo))
	}
}

// A stop during the pause before a retry round leaves the failed item
// without its retry, so the run reports its rounds as skipped.
func TestStopDuringRetryPauseSkipsRounds(t *testing.T) {
	f := newCluster()
	denied := &proxmox.APIError{Status: 400, Message: "Parameter verification failed."}
	f.failOn("setconfig:10121", denied, denied)
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := testExecutor(f, &recorder{})
	ex.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel() // the lead cancels while the run waits for round 1
		return ctx.Err()
	}

	res := ex.Run(ctx, plan)
	if !res.RoundsSkipped {
		t.Errorf("RoundsSkipped = false, want true; result = %+v", res)
	}
	if err := res.Failed["team01-teak"]; err == nil || len(res.Interrupted) != 0 {
		t.Errorf("Failed = %v, Interrupted = %v; want the round-0 failure", err, res.Interrupted)
	}
	if n := f.called("setconfig:10121"); n != 1 {
		t.Errorf("setconfig called %d times, want 1 (no retry round ran)", n)
	}
}

// Runs that use all their rounds, or finish early, skip none.
func TestRoundsNotSkippedWhenRunCompletes(t *testing.T) {
	f := newCluster()
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10121, VM.Config.Network)"}
	f.failOn("setconfig:10121", denied, denied)
	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	if res.RoundsSkipped || len(res.Failed) != 1 {
		t.Errorf("failing run: RoundsSkipped = %v, failed = %v", res.RoundsSkipped, res.Failed)
	}

	// A stop that arrives after the last round leaves nothing skipped.
	f = newCluster()
	f.failOn("setconfig:10121", denied, denied)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := testExecutor(f, &recorder{})
	ex.OnEvent = func(ev Event) {
		if strings.HasPrefix(ev.Message, "finished:") {
			cancel()
		}
	}
	plan := deployTeak(t, f, "01")
	if res := ex.Run(ctx, plan); res.RoundsSkipped {
		t.Errorf("stop after the run: RoundsSkipped = true")
	}
	f = newCluster()
	if res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01")); res.RoundsSkipped || len(res.Failed) != 0 {
		t.Errorf("clean run: %+v", res)
	}
}

// A VM that is gone when cleanup reads it is reported gone at once, not
// after a round of retries: "does not exist" there means gone.
func TestCleanupDoesNotRetryGoneVM(t *testing.T) {
	f, plan := newlyBuiltTeamVMFails(t)
	denied := &proxmox.APIError{Status: 400, Message: "Parameter verification failed."}
	f.failOn("setconfig:10121", denied, denied)
	seen, reads := 0, -1
	f.onRecord = func(key string) {
		switch {
		case key == "setconfig:10121":
			if seen++; seen == 2 {
				delete(f.vms, 10121)
				for i := 0; i < 6; i++ {
					f.failOn("cluster", &proxmox.APIError{Status: 500, Message: "pve is down"})
				}
				reads = 0
			}
		case key == "config:10121" && reads >= 0:
			reads++
		}
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if !reflect.DeepEqual(res.AlreadyGone, []string{"team01-teak"}) {
		t.Errorf("AlreadyGone = %v, CleanupFailed = %v", res.AlreadyGone, res.CleanupFailed)
	}
	if reads != 1 {
		t.Errorf("cleanup read the gone VM's config %d times, want 1", reads)
	}
}

// A stopped job doesn't wait out a template copy that keeps its master down:
// it stops the copy task, restarts the master once the task has ended, and
// removes the unfinished copy rather than converting it.
func TestCancelledBuildStopsItsCopy(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	var once sync.Once
	f.stopHook = func(upid string) error {
		if strings.Contains(upid, ":qmclone:121:") {
			once.Do(func() { close(stopped) })
		}
		return nil
	}
	cancelDuringMasterClone(f, cancel, func(context.Context) error {
		select {
		case <-stopped:
			return &proxmox.TaskError{UPID: upid("cedar", "qmclone", 121), ExitStatus: "received interrupt"}
		case <-time.After(3 * time.Second):
			return errors.New("the copy is still running")
		}
	})
	rec := &recorder{}

	res := testExecutor(f, rec).Run(ctx, plan)
	if f.called("stoptask:"+upid("cedar", "qmclone", 121)) != 1 {
		t.Errorf("the copy task was not stopped; calls = %v", f.calls)
	}
	if st := f.vms[121].Status; st != "running" {
		t.Errorf("master status = %s, want running", st)
	}
	if f.called("template:9021") != 0 || len(res.Completed) != 0 {
		t.Errorf("the stopped copy was converted: Completed = %v", res.Completed)
	}
	if f.vms[9021] != nil || !slices.Contains(res.Removed, "teak.tango.delta.tpl") {
		t.Errorf("Removed = %v, CleanupFailed = %v; want the stopped copy removed", res.Removed, res.CleanupFailed)
	}
}

// A copy that can't be stopped holds a stopped run up for copyStopWait at
// most; the master is then left stopped, with an event saying so.
func TestCancelledBuildWaitsForItsCopyOnlySoLong(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	f.stopHook = func(string) error { return &proxmox.APIError{Status: 500, Message: "proxy timeout"} }
	cancelDuringMasterClone(f, cancel, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	rec := &recorder{}
	ex := testExecutor(f, rec)
	ex.Sleep = testSleep(50 * time.Millisecond)

	done := make(chan Result, 1)
	go func() { done <- ex.Run(ctx, plan) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of the cancel")
	}
	if st := f.vms[121].Status; st != "stopped" {
		t.Errorf("master status = %s, want stopped", st)
	}
	found := false
	for _, e := range rec.find("teak.tango.delta.tpl", EventFailed) {
		found = found || strings.Contains(e.Message, "left stopped")
	}
	if !found {
		t.Error("master left stopped without an event")
	}
}

// A copy from a master that was already stopped is stopped too when the job
// is: nothing waits for it, and it is removed rather than converted.
func TestCancelledBuildStopsItsCopyFromStoppedMaster(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	ctx, cancel := context.WithCancel(context.Background())
	cancelDuringMasterClone(f, cancel, func(context.Context) error { return nil })

	res := testExecutor(f, &recorder{}).Run(ctx, plan)
	if f.called("stoptask:"+upid("cedar", "qmclone", 121)) != 1 {
		t.Errorf("the copy task was not stopped; calls = %v", f.calls)
	}
	if f.called("template:9021") != 0 || len(res.Completed) != 0 {
		t.Errorf("the stopped copy was converted: Completed = %v", res.Completed)
	}
	if f.vms[9021] != nil || !slices.Contains(res.Removed, "teak.tango.delta.tpl") {
		t.Errorf("Removed = %v, CleanupFailed = %v; want the stopped copy removed", res.Removed, res.CleanupFailed)
	}
}

// A cancel that lands while a retry round is under way keeps the items
// still waiting for a worker from running. They read interrupted, not
// failed with an earlier round's error, as their config was left (here
// converged: every config step finished in round one, which failed only
// at start), and the rounds read as skipped.
func TestCancelDuringARetryRoundInterruptsTheItemsNotRun(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01", "02")
	bad := &proxmox.APIError{Status: 400, Message: "start: bad request"}
	f.failOn("power:10121:start", bad)
	f.failOn("power:10221:start", bad)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	startsFailed := 0
	f.onRecord = func(key string) {
		switch {
		case strings.HasPrefix(key, "power:") && strings.HasSuffix(key, ":start"):
			startsFailed++
		case startsFailed == 2 && (key == "config:10121" || key == "config:10221"): // round two's first item
			cancel(ErrCancelRequested)
		}
	}
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Concurrency.Workers = 1
	ex.Cfg.Retry.Rounds = 1
	res := ex.Run(ctx, plan)

	if len(res.Failed) != 0 || len(res.Interrupted) != 2 || !res.RoundsSkipped {
		t.Fatalf("result = %+v, want both interrupted, rounds skipped", res)
	}
	notRun := 0
	for name, err := range res.Interrupted {
		if err.Error() != "cancelled before it started" {
			continue
		}
		notRun++
		if left := LeftBy(err); left != LeftConverged {
			t.Errorf("%s left %q, want converged", name, left)
		}
	}
	if notRun != 1 {
		t.Errorf("interrupted = %v, want one not run in round two", res.Interrupted)
	}
	if n := f.called("power:"); n != 2 {
		t.Errorf("start POSTed %d times, want 2 (round one only)", n)
	}
}

// A rebuild deletes the old template once: a retry round after the new
// copy failed to convert resumes the copy rather than deleting it as the
// old template.
func TestRebuildRetryRoundResumesTheNewCopy(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "spruce", Template: true}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	f.failOn("template:9021", &proxmox.APIError{Status: 400, Message: "convert: bad request"})

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 || len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("delete:9021"); n != 1 {
		t.Errorf("VMID 9021 deleted %d times, want 1 (the old template only)", n)
	}
	if n := f.called("clone:9021"); n != 1 {
		t.Errorf("master copied %d times, want 1", n)
	}
}
