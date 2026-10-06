package pods

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
)

// userCfgTimeout is the error of a qmdestroy task that deleted the VM's disks
// and then timed out on the cluster-wide user.cfg lock while removing the VM
// from its pool and ACLs, as seen in production for VM 11904.
func userCfgTimeout(node string, vmid int) error {
	return &proxmox.TaskError{
		UPID:       upid(node, "qmdestroy", vmid),
		ExitStatus: "access permissions cleanup for VM 10121 failed: cfs-lock 'file-user_cfg' error: got lock request timeout",
		LogTail: []string{
			"trying to acquire cfs lock 'storage-competitions' ...",
			"trying to acquire cfs lock 'file-user_cfg' ...",
			"trying to acquire cfs lock 'file-user_cfg' ...",
			"trying to acquire cfs lock 'file-user_cfg' ...",
			"TASK ERROR: access permissions cleanup for VM 10121 failed: cfs-lock 'file-user_cfg' error: got lock request timeout",
		},
	}
}

func teardownTeam01(t *testing.T, f *fakeAPI) *Plan {
	t.Helper()
	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func wantHalfDeletedAdvice(t *testing.T, msg string) {
	t.Helper()
	for _, want := range []string{"half-deleted", "locked as destroyed", "qm destroy 10121 --skiplock --purge", "cedar"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

// The production incident: the first destroy task dies on the user.cfg lock
// after deleting the disks, leaving lock=destroyed; every later destroy is
// refused. The VM must end failed with advice, never done.
func TestHalfDeletedVMFailsWithAdvice(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, map[string]string{"name": "team01-teak"})
	plan := teardownTeam01(t, f)
	f.halfDestroy = map[int]bool{10121: true}
	f.failOn("wait:"+upid("cedar", "qmdestroy", 10121), userCfgTimeout("cedar", 10121))
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), plan)
	err := res.Failed["team01-teak"]
	if err == nil || len(res.Succeeded) != 0 {
		t.Fatalf("result = %+v, want team01-teak failed", res)
	}
	wantHalfDeletedAdvice(t, proxmox.Describe(err))
	if ev := rec.find("team01-teak", EventDone); len(ev) != 1 || ev[0].Step != StepStop {
		t.Errorf("done events = %+v, want only the stop", ev)
	}
	if ev := rec.find("team01-teak", EventSkipped); len(ev) != 0 {
		t.Errorf("skipped events = %+v, want none (not \"already deleted\")", ev)
	}
	// The re-POST can only fail on the destroyed lock.
	if n := f.called("delete:10121"); n != 1 {
		t.Errorf("delete POSTed %d times, want 1", n)
	}
	if vm := f.vms[10121]; vm == nil || vm.Config["lock"] != "destroyed" {
		t.Errorf("fake VM = %+v, want the half-deleted VM left alone", vm)
	}
}

// A VM already in lock=destroyed is half-deleted: Proxmox would refuse a
// delete, so none is sent.
func TestDestroyedLockIsNotRetried(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"},
		map[string]string{"name": "team01-teak", "lock": "destroyed"})
	plan := teardownTeam01(t, f)
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Retry.Rounds = 0

	res := ex.Run(context.Background(), plan)
	err := res.Failed["team01-teak"]
	if err == nil {
		t.Fatalf("result = %+v, want team01-teak failed", res)
	}
	wantHalfDeletedAdvice(t, proxmox.Describe(err))
	if n := f.called("delete:10121"); n != 0 {
		t.Errorf("delete POSTed %d times, want 0", n)
	}
}

func TestDestroyedLockIsNotRetryable(t *testing.T) {
	ex := testExecutor(newCluster(), &recorder{})
	ex.Cfg.Retry.TransientPatterns = []string{"is locked", "timeout", "can't lock file"}
	ex.init()
	for _, err := range []error{
		&proxmox.APIError{Status: 500, Message: "VM is locked (destroyed)"},
		&proxmox.TaskError{UPID: "x", ExitStatus: "VM is locked (destroyed)"},
	} {
		if ex.retryable(err) || ex.restartable(err) {
			t.Errorf("%v is retried, want it permanent", err)
		}
	}
	if !ex.retryable(&proxmox.APIError{Status: 500, Message: "VM is locked (clone)"}) {
		t.Error("an ordinary lock must stay retryable")
	}
}

// A delete task that reports OK while the VM is still there is not done.
func TestDeleteIsVerified(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, map[string]string{"name": "team01-teak"})
	plan := teardownTeam01(t, f)
	f.deleteLeaves = map[int]bool{10121: true}
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Retry.Rounds = 0

	res := ex.Run(context.Background(), plan)
	if err := res.Failed["team01-teak"]; err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("result = %+v, want team01-teak failed because it still exists", res)
	}
}

// A VM that vanished from the listing by name but whose VMID holds a
// half-deleted VM is not "already deleted".
func TestHalfDeletedVMIsNotAlreadyDeleted(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, map[string]string{"name": "team01-teak"})
	plan := teardownTeam01(t, f)
	f.vms[10121].Name = "VM 10121"
	f.vms[10121].Config = map[string]string{"lock": "destroyed"}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	err := res.Failed["team01-teak"]
	if err == nil {
		t.Fatalf("result = %+v, want team01-teak failed", res)
	}
	wantHalfDeletedAdvice(t, proxmox.Describe(err))
	if n := f.called("delete:10121"); n != 0 {
		t.Errorf("delete POSTed %d times, want 0", n)
	}
}

func TestCleanupAdviceForHalfDeletedVM(t *testing.T) {
	err := &halfDeletedError{vmid: 10121, node: "cedar"}
	msg := CleanupAdvice("team01-teak", err)
	wantHalfDeletedAdvice(t, msg)
	if strings.Contains(msg, "remove it in Proxmox") {
		t.Errorf("advice %q should give the half-delete instructions only", msg)
	}
}

// Deletes take a slot of concurrency.deletes, so many workers never run more
// destroy tasks at once than the cap: Proxmox times out on its user.cfg lock
// under too many concurrent destroys.
func TestDeletesAreCapped(t *testing.T) {
	f := newCluster()
	var teams []string
	for i := 1; i <= 8; i++ {
		team := FormatTeam(i)
		teams = append(teams, team)
		f.add(proxmox.VM{VMID: 10021 + 100*i, Name: "team" + team + "-teak", Node: "cedar"}, nil)
	}
	plan, err := testPlanner(f).Teardown(context.Background(), teams, nil)
	if err != nil {
		t.Fatal(err)
	}
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Concurrency.Deletes = 3
	g := newTaskGate(t, f, 3, "qmdestroy", func(string) bool { return true })

	_, done := runAsync(t, ex, plan)
	g.drive(8, func() int { return 8 - len(g.arrived) })
	res := waitResult(t, done)
	if len(res.Succeeded) != 8 {
		t.Fatalf("result = %+v", res)
	}
	if p := g.peak(); p != 3 {
		t.Errorf("peak concurrent deletes = %d, want 3", p)
	}
}

func TestDeletesShareLimitsAcrossExecutors(t *testing.T) {
	cfg := testExecutor(newCluster(), &recorder{}).Cfg
	cfg.Concurrency.Deletes = 2
	if got := cap(NewLimits(cfg.Concurrency).deletes.local); got != 2 {
		t.Errorf("deletes slots = %d, want 2", got)
	}
}

// Teardown shuts VMs down cleanly so routers release their DHCP leases, with
// Proxmox hard-stopping them after the timeout.
func TestTeardownShutsDownGracefully(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, nil)
	plan := teardownTeam01(t, f)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 || len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("shutdown:10121:1m0s:true"); n != 1 {
		t.Errorf("graceful shutdown with 60s timeout and forceStop sent %d times, want 1; calls=%v", n, f.calls)
	}
	if n := f.called("power:10121:stop"); n != 0 {
		t.Errorf("hard stop sent %d times, want 0", n)
	}
	if _, ok := f.vms[10121]; ok {
		t.Error("VM not deleted")
	}
}

func TestTeardownHardStopsWhenShutdownFails(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, nil)
	plan := teardownTeam01(t, f)
	f.stuckShutdown = map[int]bool{10121: true}
	f.failOn("wait:"+upid("cedar", "qmshutdown", 10121),
		&proxmox.TaskError{UPID: upid("cedar", "qmshutdown", 10121), ExitStatus: "VM quit/powerdown failed - got timeout"})
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Teardown.ShutdownTimeout = 5 * time.Second

	res := ex.Run(context.Background(), plan)
	if len(res.Failed) != 0 || len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("shutdown:10121:5s:true"); n != 1 {
		t.Errorf("shutdown sent %d times, want 1; calls=%v", n, f.calls)
	}
	if n := f.called("power:10121:stop"); n != 1 {
		t.Errorf("hard stop sent %d times, want 1 after the failed shutdown", n)
	}
	if _, ok := f.vms[10121]; ok {
		t.Error("VM not deleted")
	}
}

func TestTeardownOfStoppedVMSendsNoStop(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
	plan := teardownTeam01(t, f)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if f.called("shutdown:") != 0 || f.called("power:") != 0 {
		t.Errorf("calls = %v, want no shutdown or stop for a stopped VM", f.calls)
	}
}

// A delete sent to the node a VM just left fails with "does not exist" there.
// That is not a deleted VM: the item must not count as done while the VM
// lives on another node.
func TestDeleteOfVMThatMovedIsNotCalledDone(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, map[string]string{"name": "team01-teak"})
	plan := teardownTeam01(t, f)
	f.onRecord = func(key string) {
		if key == "delete:10121" {
			f.vms[10121].Node = "birch" // migrated
		}
	}
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Retry.Rounds = 0

	res := ex.Run(context.Background(), plan)
	if f.vms[10121] == nil {
		t.Fatal("test setup: VM deleted")
	}
	err := res.Failed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), "birch") {
		t.Errorf("result = %+v, want team01-teak failed, naming the node it is on now", res)
	}
}

// The next round deletes it on its new node.
func TestDeleteOfVMThatMovedIsRetriedThere(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, map[string]string{"name": "team01-teak"})
	plan := teardownTeam01(t, f)
	moved := false
	f.onRecord = func(key string) {
		if key == "delete:10121" && !moved {
			moved = true
			f.vms[10121].Node = "birch"
		}
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.vms[10121] != nil || len(res.Succeeded) != 1 {
		t.Errorf("result = %+v, VM = %+v; want it deleted on its new node", res, f.vms[10121])
	}
}

// A delete whose answer was lost after Proxmox started the destroy is sent
// again and refused with "locked (destroyed)": that lock is our own destroy
// still running, not a half-deleted VM.
func TestResentDeleteWaitsForItsOwnDestroy(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, map[string]string{"name": "team01-teak"})
	plan := teardownTeam01(t, f)
	f.deleteThenFail = map[int]error{10121: &proxmox.APIError{Status: 596, Message: "Connection timed out"}}
	f.destroyReads = 3
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Retry.Rounds = 0

	res := ex.Run(context.Background(), plan)
	if len(res.Succeeded) != 1 || f.vms[10121] != nil {
		t.Errorf("result = %+v, VM = %+v; want team01-teak deleted", res, f.vms[10121])
	}
}

// A later teardown sees a VM an earlier one left half-deleted, though it
// has lost its name: the plan blocks it with the admin's command, and it
// keeps its disks from looking orphaned. One that kept its name is planned
// once, by name.
func TestTeardownPlansHalfDeletedVMs(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, map[string]string{"name": "team01-teak"})
	f.halfDestroy = map[int]bool{10121: true}
	f.failOn("wait:"+upid("cedar", "qmdestroy", 10121), userCfgTimeout("cedar", 10121))
	testExecutor(f, &recorder{}).Run(context.Background(), teardownTeam01(t, f))

	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Name != "team01-half-deleted-10121" || plan.Items[0].VMID != 10121 {
		t.Fatalf("items = %+v, want one for half-deleted VMID 10121", plan.Items)
	}
	wantHalfDeletedAdvice(t, plan.Items[0].Blocked)
	if other, _ := testPlanner(f).Teardown(context.Background(), []string{"02"}, nil); len(other.Items) != 0 {
		t.Errorf("team 02's teardown planned %+v", other.Items)
	}

	named := newCluster()
	named.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "stopped"}, map[string]string{"name": "team01-teak", "lock": "destroyed"})
	plan, err = testPlanner(named).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil || len(plan.Items) != 1 || plan.Items[0].Name != "team01-teak" {
		t.Fatalf("named half-deleted VM: items %+v, err %v; want it once, by name", plan, err)
	}
}
