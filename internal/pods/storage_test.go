package pods

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// storageLockTimeout is the error of a task that timed out on the shared
// storage's cluster lock, as seen in production while four templates were
// being built at once.
func storageLockTimeout(node, kind string, vmid int) error {
	return &proxmox.TaskError{
		UPID:       upid(node, kind, vmid),
		ExitStatus: "cfs-lock 'storage-competitions' error: got lock request timeout",
		LogTail:    []string{"TASK ERROR: cfs-lock 'storage-competitions' error: got lock request timeout"},
	}
}

func wantUnconvertedError(t *testing.T, err error, extra ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("no error, want the half-converted template to fail the build")
	}
	for _, want := range append([]string{"scsi0 (competitions:9021/vm-9021-disk-0.qcow2)", "unconverted", "rebuild"}, extra...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// The production incident: the qmtemplate task set template: 1, then timed
// out on the storage lock before renaming the disk to base-*. Restarting it
// "succeeded" on the already-template VM, and every linked clone then failed.
// The convert must not be restarted, and the half-conversion must fail the
// build.
func TestHalfConvertedTemplateFailsTheBuild(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.convertLeavesDisk = map[int]bool{9021: true}
	f.failOn("wait:"+upid("cedar", "qmtemplate", 9021), storageLockTimeout("cedar", "qmtemplate", 9021))

	rec := &recorder{}
	res := testExecutor(f, rec).Run(context.Background(), plan)
	// The retry round finds the half-converted template and refuses to reuse it.
	wantUnconvertedError(t, res.Failed["team01-teak"], "deploy with rebuild")
	failed := rec.find("teak.tango.delta.tpl", EventFailed)
	if len(failed) == 0 {
		t.Fatal("no failed event for the template build")
	}
	wantUnconvertedError(t, errors.New(failed[0].Message), "lock request timeout")
	if n := f.called("template:9021"); n != 1 {
		t.Errorf("convert started %d times, want 1 (a restart hides the half-conversion)", n)
	}
	if n := f.called("clone:10121"); n != 0 {
		t.Errorf("team VM cloned %d times from a half-converted template, want 0", n)
	}
}

// Cleanup must not call a half-converted template complete: it is a VM this
// run created that can never be cloned, so it is removed like any other
// unfinished one, and the next run rebuilds it cleanly.
func TestCleanupRemovesHalfConvertedTemplate(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.convertLeavesDisk = map[int]bool{9021: true}

	rec := &recorder{}
	res := testExecutor(f, rec).Run(context.Background(), plan)
	for _, e := range rec.find("teak.tango.delta.tpl", EventInfo) {
		if strings.Contains(e.Message, "already complete") {
			t.Errorf("cleanup called the half-converted template complete: %q", e.Message)
		}
	}
	if _, ok := f.vms[9021]; ok {
		t.Error("half-converted template 9021 was kept, want it removed")
	}
	if !slices.Contains(res.Removed, "teak.tango.delta.tpl") {
		t.Errorf("Removed = %v, want the template", res.Removed)
	}
	why := false
	for _, e := range rec.events {
		why = why || (e.Item == "teak.tango.delta.tpl" && strings.Contains(e.Message, "half-converted"))
	}
	if !why {
		t.Errorf("no event says the template was half-converted: %+v", rec.find("teak.tango.delta.tpl", EventInfo))
	}
}

// A convert task that reports OK but leaves a vm- disk is caught too.
func TestConvertIsVerifiedEvenWhenTheTaskSucceeds(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.convertLeavesDisk = map[int]bool{9021: true}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	wantUnconvertedError(t, res.Failed["team01-teak"])
	if n := f.called("clone:10121"); n != 0 {
		t.Errorf("team VM cloned %d times from a half-converted template, want 0", n)
	}
}

// A failed convert task that left the VM unconverted is not restarted
// either; its error is reported as is.
func TestFailedConvertIsNotRestarted(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.waitHook = func(_ context.Context, u string) error {
		if strings.Contains(u, ":qmtemplate:") {
			f.vms[9021].Template = false // the task failed before doing anything
			return storageLockTimeout("cedar", "qmtemplate", 9021)
		}
		return nil
	}
	cfg := testExecutor(f, &recorder{})
	cfg.Cfg.Retry.Rounds = 0
	res := cfg.Run(context.Background(), plan)
	err := res.Failed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), "converting to template") || !strings.Contains(err.Error(), "lock request timeout") {
		t.Errorf("error = %v", err)
	}
	// Once in the build, and once more by cleanup completing the copy it
	// left (safe: nothing was converted); never a restart of the task.
	if n := f.called("template:9021"); n != 2 {
		t.Errorf("convert started %d times, want 2", n)
	}
}

// An existing template that an earlier run left half-converted is not
// reused: every linked clone from it would fail.
func TestHalfConvertedExistingTemplateIsNotReused(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, map[string]string{
		"name":  "teak.tango.delta.tpl",
		"scsi0": "competitions:9021/vm-9021-disk-0.qcow2,size=32G",
	})
	plan := deployTeak(t, f, "01")

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	wantUnconvertedError(t, res.Failed["team01-teak"], "deploy with rebuild")
	if n := f.called("clone:10121"); n != 0 {
		t.Errorf("team VM cloned %d times from a half-converted template, want 0", n)
	}
}

// A good existing template is still reused.
func TestConvertedExistingTemplateIsReused(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, map[string]string{
		"name":  "teak.tango.delta.tpl",
		"scsi0": "competitions:9021/base-9021-disk-0.qcow2,size=32G",
		"ide2":  "competitions:vm-9021-cloudinit,media=cdrom",
	})
	plan := deployTeak(t, f, "01")

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if n := f.called("clone:9021"); n != 0 {
		t.Errorf("template rebuilt %d times, want reused", n)
	}
}

// teakTeamVM adds team01-teak, a linked clone of template 9021 with an EFI
// disk, and returns its volumes.
func teakTeamVM(f *fakeAPI) []string {
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, map[string]string{
		"name": "teak.tango.delta.tpl", "scsi0": "competitions:9021/base-9021-disk-0.qcow2,size=32G",
	})
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, map[string]string{
		"name":     "team01-teak",
		"scsi0":    "competitions:9021/base-9021-disk-0.qcow2/10121/vm-10121-disk-0.qcow2,size=32G",
		"efidisk0": "competitions:10121/vm-10121-disk-1.qcow2,efitype=4m",
		"ide2":     "competitions:vm-10121-cloudinit,media=cdrom",
		"ide0":     "local:iso/win.iso,media=cdrom",
	})
	return []string{
		"competitions:10121/vm-10121-disk-1.qcow2",
		"competitions:9021/base-9021-disk-0.qcow2/10121/vm-10121-disk-0.qcow2",
	}
}

// A destroy task that ends OK but timed out on the storage lock while
// freeing the disks leaves them on the storage. They are freed, and the
// info event says which.
func TestDeleteFreesDisksTheDestroyLeft(t *testing.T) {
	f := newCluster()
	left := teakTeamVM(f)
	plan := teardownTeam01(t, f)
	f.deleteKeepsDisks = map[int]bool{10121: true}
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if vols := f.volumesOf(10121); len(vols) != 0 {
		t.Errorf("volumes left = %q, want none", vols)
	}
	if n := f.called("volume:"); n != 2 {
		t.Errorf("%d volume frees, want 2", n)
	}
	if n := f.called("volume:local:"); n != 0 {
		t.Errorf("the ISO was freed %d times; CD-ROMs are never the VM's disks", n)
	}
	found := false
	for _, e := range rec.find("team01-teak", EventInfo) {
		found = found || (strings.Contains(e.Message, left[0]) && strings.Contains(e.Message, left[1]))
	}
	if !found {
		t.Errorf("info events = %+v, want one naming %q", rec.find("team01-teak", EventInfo), left)
	}
}

// A clean destroy needs no frees.
func TestCleanDeleteFreesNothing(t *testing.T) {
	f := newCluster()
	teakTeamVM(f)
	plan := teardownTeam01(t, f)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if n := f.called("volume:"); n != 0 {
		t.Errorf("%d volume frees, want 0", n)
	}
	if n := f.called("content:competitions:10121"); n != 1 {
		t.Errorf("storage listed %d times, want 1", n)
	}
}

// The production incident: deleting the old template for a rebuild left
// its base disk behind. It is freed before the new template is built.
func TestRebuildFreesOldTemplateDisk(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, map[string]string{
		"name": "teak.tango.delta.tpl", "scsi0": "competitions:9021/base-9021-disk-0.qcow2,size=32G",
	})
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	f.deleteKeepsDisks = map[int]bool{9021: true}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if n := f.called("volume:competitions:9021/base-9021-disk-0.qcow2"); n != 1 {
		t.Errorf("old base disk freed %d times, want 1", n)
	}
}

func wantLeftoverAdvice(t *testing.T, err error, vols ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("no error, want the delete to fail on its leftover disks")
	}
	msg := proxmox.Describe(err)
	if strings.Contains(msg, "delete: delete:") {
		t.Errorf("message %q names the step twice", msg)
	}
	for _, v := range vols {
		if !strings.Contains(msg, "pvesm free "+v) {
			t.Errorf("message %q lacks %q", msg, "pvesm free "+v)
		}
	}
	if !strings.Contains(msg, "cedar") {
		t.Errorf("message %q lacks the node", msg)
	}
}

// Leftovers that cannot be freed fail the delete with the commands to free
// them by hand, and a retry round that finds the VM gone does not hide that
// as "already deleted".
func TestLeftoverDisksThatCannotBeFreedFailWithAdvice(t *testing.T) {
	f := newCluster()
	left := teakTeamVM(f)
	plan := teardownTeam01(t, f)
	f.deleteKeepsDisks = map[int]bool{10121: true}
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/storage/competitions, Datastore.Allocate)"}
	for _, v := range left {
		f.failOn("volume:"+v, denied, denied, denied, denied)
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	wantLeftoverAdvice(t, res.Failed["team01-teak"], left...)
	if len(res.Succeeded) != 0 {
		t.Errorf("succeeded = %v, want none", res.Succeeded)
	}
}

// A retry round frees what the first attempt could not.
func TestLeftoverDisksAreRetriedNextRound(t *testing.T) {
	f := newCluster()
	left := teakTeamVM(f)
	plan := teardownTeam01(t, f)
	f.deleteKeepsDisks = map[int]bool{10121: true}
	f.failOn("volume:"+left[0], &proxmox.APIError{Status: 403, Message: "Permission check failed"})

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if vols := f.volumesOf(10121); len(vols) != 0 {
		t.Errorf("volumes left = %q, want none", vols)
	}
}

// Proxmox refuses to free a base volume that linked clones still use; that
// refusal is surfaced as is.
func TestLeftoverBaseVolumeInUseIsSurfaced(t *testing.T) {
	f := newCluster()
	teakTeamVM(f)
	f.deleteKeepsDisks = map[int]bool{9021: true}
	ex := testExecutor(f, &recorder{})
	ex.init()
	ex.present = map[int]proxmox.VM{}

	err := ex.deleteVM(context.Background(), "cedar", 9021, "teak.tango.delta.tpl")
	wantLeftoverAdvice(t, err, "competitions:9021/base-9021-disk-0.qcow2")
	if err != nil && !strings.Contains(err.Error(), "still in use by linked clone") {
		t.Errorf("error %q lacks Proxmox's refusal", err)
	}
	if vols := f.volumesOf(10121); len(vols) != 2 {
		t.Errorf("team VM volumes = %q, want both kept", vols)
	}
}

// storageTaskWatch holds every task that takes the storage lock (template
// conversions and template destroys) for 300ms, and records the most in
// flight at once. The hold is long enough that tasks which are not
// serialized are sure to be seen together.
type storageTaskWatch struct {
	mu            sync.Mutex
	inflight, max int
}

func watchStorageTasks(f *fakeAPI, templates ...int) *storageTaskWatch {
	w := &storageTaskWatch{}
	f.waitGate = func(ctx context.Context, u string) error {
		held := strings.Contains(u, ":qmtemplate:")
		for _, vmid := range templates {
			held = held || u == upid("cedar", "qmdestroy", vmid) || u == upid("birch", "qmdestroy", vmid)
		}
		if !held {
			return nil
		}
		w.mu.Lock()
		w.inflight++
		w.max = max(w.max, w.inflight)
		w.mu.Unlock()
		defer func() {
			w.mu.Lock()
			w.inflight--
			w.mu.Unlock()
		}()
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	return w
}

func (w *storageTaskWatch) peak() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.max
}

// Conversions and template destroys each take the shared storage's cluster
// lock; run together, they time out on it. Rebuilding teak (destroying its
// old template, then converting the new one) while oak's template is
// built must never run two of them at once, though the builds themselves
// run in parallel.
func TestConvertAndTemplateDeleteNeverOverlap(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, map[string]string{
		"name": "teak.tango.delta.tpl", "scsi0": "competitions:9021/base-9021-disk-0.qcow2,size=32G",
	})
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	w := watchStorageTasks(f, 9021, 9025)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if n := f.called("delete:9021"); n != 1 {
		t.Fatalf("old template deleted %d times, want 1", n)
	}
	if n := f.called("template:"); n != 2 {
		t.Fatalf("%d conversions, want 2", n)
	}
	if p := w.peak(); p != 1 {
		t.Errorf("%d storage-lock tasks ran at once, want 1", p)
	}
}

// A template delete whose first config read fails still takes the
// storage-ops slot: not knowing it is a template must not let its destroy
// run beside a conversion.
func TestTemplateDeleteAfterAFailedReadIsSerialized(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, map[string]string{
		"name": "teak.tango.delta.tpl", "template": "1", "scsi0": "competitions:9021/base-9021-disk-0.qcow2,size=32G",
	})
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	w := watchStorageTasks(f, 9021, 9025)
	f.failOn("config:9021", &proxmox.APIError{Status: 400, Message: "bad request"})

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if n := f.called("delete:9021"); n != 1 {
		t.Fatalf("old template deleted %d times, want 1", n)
	}
	if p := w.peak(); p != 1 {
		t.Errorf("%d storage-lock tasks ran at once, want 1", p)
	}
}

// Deleting team VMs doesn't take the storage-ops slot: their destroys run
// in parallel up to concurrency.deletes.
func TestTeamVMDeletesAreNotSerialized(t *testing.T) {
	f := newCluster()
	for _, team := range []string{"01", "02"} {
		f.add(proxmox.VM{VMID: 10021 + 100*atoi(team), Name: "team" + team + "-teak", Node: "cedar"},
			map[string]string{"name": "team" + team + "-teak"})
	}
	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01", "02"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := newTaskGate(t, f, 2, "qmdestroy", func(string) bool { return true })
	_, done := runAsync(t, testExecutor(f, &recorder{}), plan)
	a, b := g.next(), g.next() // both destroys in flight together
	g.releaseUPID(a.upid, nil)
	g.releaseUPID(b.upid, nil)
	if res := waitResult(t, done); len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// A VMID can be reused while its old disks wait for a retry: a new VM's
// disks get the same names. Leftovers are never freed while a VM holds the
// VMID, since they may be that VM's now.
func TestLeftoversAreNotFreedOnceTheVMIDIsReused(t *testing.T) {
	f := newCluster()
	left := teakTeamVM(f)
	plan := teardownTeam01(t, f)
	f.deleteKeepsDisks = map[int]bool{10121: true}
	denied := &proxmox.APIError{Status: 403, Message: "Permission check failed (/storage/competitions, Datastore.Allocate)"}
	f.failOn("volume:"+left[0], denied)
	f.onRecord = func(key string) {
		if key == "volume:"+left[0] && f.vms[10121] == nil {
			f.add(proxmox.VM{VMID: 10121, Name: "team01-other", Node: "cedar"}, map[string]string{
				"name": "team01-other", "efidisk0": left[0] + ",efitype=4m",
			})
		}
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if f.called("volume:"+left[0]) != 1 {
		t.Errorf("freed %s %d times, want only the first, denied, attempt", left[0], f.called("volume:"+left[0]))
	}
	err := res.Failed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), "team01-other") {
		t.Errorf("result = %+v, want team01-teak failed, naming the VM that now has VMID 10121", res)
	}
}

// The advice for leftovers says to check the VMID is still free first.
func TestLeftoverAdviceWarnsAboutVMIDReuse(t *testing.T) {
	err := &leftoverDisksError{vmid: 10121, node: "cedar", vols: []string{"competitions:10121/vm-10121-disk-1.qcow2"}}
	if msg := err.Error(); !strings.Contains(msg, "qm config 10121") {
		t.Errorf("advice %q doesn't say to check that VMID 10121 is still free", msg)
	}
}

// A convert request whose answer was lost may have converted the VM. It is
// not sent again: Proxmox would refuse to convert a template, failing a
// good build.
func TestConvertWhoseAnswerWasLostIsNotResent(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.convertThenFail = map[int]error{9021: &url.Error{Op: "Post", URL: "https://pve", Err: errors.New("read: connection reset by peer")}}
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Retry.Rounds = 0

	res := ex.Run(context.Background(), plan)
	if len(res.Failed) != 0 || len(res.Succeeded) != 1 {
		t.Errorf("result = %+v, want the deploy to succeed", res)
	}
	if n := f.called("template:9021"); n != 1 {
		t.Errorf("convert sent %d times, want 1", n)
	}
}

// cancelDuringDestroy runs a teardown of team01-teak whose destroy task
// leaves the disks behind, and cancels the job while the run waits for
// that task, which then ends inside the grace.
func cancelDuringDestroy(t *testing.T, f *fakeAPI, plan *Plan) (*recorder, Result) {
	t.Helper()
	f.deleteKeepsDisks = map[int]bool{10121: true}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	f.waitGate = func(_ context.Context, u string) error {
		if strings.Contains(u, ":qmdestroy:") {
			cancel(ErrCancelRequested)
		}
		return nil
	}
	rec := &recorder{}
	ex := testExecutor(f, rec)
	ex.Cfg.Jobs.CancelGrace = time.Hour
	return rec, ex.Run(ctx, plan)
}

// A cancel during the destroy's wait holds back freeing the disks it left:
// the item reads interrupted, not failed with advice to free them by hand,
// and the log names the disks left.
func TestCancelDuringDestroyLeavesDisksAsAnInterruption(t *testing.T) {
	f := newCluster()
	left := teakTeamVM(f)
	rec, res := cancelDuringDestroy(t, f, teardownTeam01(t, f))

	err := res.Interrupted["team01-teak"]
	if err == nil || len(res.Failed) != 0 {
		t.Fatalf("result = %+v, want team01-teak interrupted", res)
	}
	if strings.Contains(proxmox.Describe(err), "pvesm") {
		t.Errorf("interruption %q gives hand-fix advice", proxmox.Describe(err))
	}
	if n := f.called("volume:"); n != 0 {
		t.Errorf("%d volume frees sent after the cancel, want 0", n)
	}
	if vols := f.volumesOf(10121); !slices.Equal(vols, left) {
		t.Errorf("volumes = %q, want %q left", vols, left)
	}
	found := false
	for _, ev := range rec.find("team01-teak", EventInfo) {
		found = found || strings.Contains(ev.Message, "stopped before it freed the disks the delete left on the storage: "+strings.Join(left, ", "))
	}
	if !found {
		t.Errorf("info events = %+v, want one naming the disks left", rec.find("team01-teak", EventInfo))
	}
}

// A later run of the plan that finds the VM gone frees what the earlier
// one left, read from the storage: it remembers nothing of that run.
func TestLaterRunFreesLeftoversFromStorage(t *testing.T) {
	f := newCluster()
	teakTeamVM(f)
	plan := teardownTeam01(t, f)
	cancelDuringDestroy(t, f, plan)
	f.waitGate = nil

	rec := &recorder{}
	res := testExecutor(f, rec).Run(context.Background(), plan)
	if len(res.Failed) != 0 || !slices.Equal(res.Succeeded, []string{"team01-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	if vols := f.volumesOf(10121); len(vols) != 0 {
		t.Errorf("volumes left = %q, want none", vols)
	}
	if ev := rec.find("team01-teak", EventSkipped); len(ev) != 1 || ev[0].Message != "already deleted" {
		t.Errorf("skipped events = %+v", ev)
	}
}

// ...but never while another VM holds the VMID: the disks at it are that
// VM's.
func TestLaterRunLeavesAReusedVMIDsDisks(t *testing.T) {
	f := newCluster()
	teakTeamVM(f)
	plan := teardownTeam01(t, f)
	cancelDuringDestroy(t, f, plan)
	f.waitGate = nil
	f.add(proxmox.VM{VMID: 10121, Name: "team01-other", Node: "cedar"}, map[string]string{"name": "team01-other"})
	before := f.volumesOf(10121)

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("volume:"); n != 0 {
		t.Errorf("%d volume frees, want 0", n)
	}
	if vols := f.volumesOf(10121); !slices.Equal(vols, before) {
		t.Errorf("volumes = %q, want %q kept", vols, before)
	}
}

// A later teardown job plans from what it lists, so a team VM deleted by a
// cut-off teardown has no item; its disks still on the storage do, and the
// job frees them. A host-filtered teardown, and another team's or a held
// VMID's disks, are left alone.
func TestTeardownPlansOrphanedDisks(t *testing.T) {
	f := newCluster()
	teakTeamVM(f)
	plan := teardownTeam01(t, f)
	cancelDuringDestroy(t, f, plan) // the VM is gone, its disks aren't
	f.waitGate = nil
	f.mu.Lock()
	f.vols["competitions:10221/vm-10221-disk-0.qcow2"] = 10221 // team 02's: not asked
	f.mu.Unlock()

	again, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Items) != 1 || again.Items[0].Name != "team01-disks-10121" || again.Items[0].VMID != 10121 ||
		!slices.Equal(again.Items[0].Steps, []Step{StepFreeDisks}) {
		t.Fatalf("items = %+v, want one free-disks for VMID 10121's disks", again.Items)
	}
	if filtered, _ := testPlanner(f).Teardown(context.Background(), []string{"01"}, []string{"teak"}); len(filtered.Items) != 0 {
		t.Errorf("host-filtered teardown planned %+v", filtered.Items)
	}
	res := testExecutor(f, &recorder{}).Run(context.Background(), again)
	if len(res.Failed) != 0 || len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if vols := f.volumesOf(10121); len(vols) != 0 {
		t.Errorf("volumes left = %q, want none", vols)
	}
	if vols := f.volumesOf(10221); len(vols) != 1 {
		t.Errorf("team 02's volumes = %q, want them kept", vols)
	}
}

// orphanedSetup leaves VMID 10121's disks on the storage with no VM, as a
// cut-off teardown does.
func orphanedSetup(t *testing.T) *fakeAPI {
	t.Helper()
	f := newCluster()
	teakTeamVM(f)
	cancelDuringDestroy(t, f, teardownTeam01(t, f))
	f.waitGate = nil
	return f
}

// A node refusing the listing (no Datastore.Allocate there) shows no
// disks, and the others are still asked; any other listing failure fails
// the plan rather than leave the disks out unsaid.
func TestOrphanedDisksListingErrors(t *testing.T) {
	f := orphanedSetup(t)
	f.failOn("content:competitions:10121", &proxmox.APIError{Status: 403, Message: "Permission check failed"})
	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil || len(plan.Items) != 1 {
		t.Fatalf("after a 403 on one node: plan %+v, err %v; want the item from another node", plan, err)
	}
	f.failOn("content:competitions:10121", &proxmox.APIError{Status: 500, Message: "storage timeout"})
	if _, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil); err == nil {
		t.Error("a 500 listing the storage planned anyway")
	}
}

// A VM the user can't see may hold an orphan's VMID: neither the planner
// nor, when it appears after planning, the executor frees its disks.
func TestOrphanedDisksOfAnUnseenVMAreKept(t *testing.T) {
	f := orphanedSetup(t)
	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil || len(plan.Items) != 1 {
		t.Fatalf("plan %+v, err %v", plan, err)
	}
	before := f.volumesOf(10121)
	f.mu.Lock()
	f.unseen = map[int]bool{10121: true}
	f.mu.Unlock()
	if again, _ := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil); len(again.Items) != 0 {
		t.Errorf("planned %+v for a held VMID", again.Items)
	}
	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if err := res.Failed["team01-disks-10121"]; err == nil || !strings.Contains(err.Error(), "a VM you can't see") {
		t.Fatalf("result = %+v, want the item failed naming the unseen holder", res)
	}
	if vols := f.volumesOf(10121); !slices.Equal(vols, before) {
		t.Errorf("volumes = %q, want %q kept", vols, before)
	}
}

// Listing the storage takes seconds per node: a shared one is listed on
// one node only, and a local one on each node that has it.
func TestOrphanedDisksListSharedStorageOnce(t *testing.T) {
	f := orphanedSetup(t)
	for _, c := range []struct {
		shared bool
		on     []string
		want   int
	}{
		{true, f.nodes, 1},
		{false, f.nodes[:2], 2},
	} {
		var res proxmox.Resources
		for _, n := range c.on {
			res.Storage = append(res.Storage, proxmox.StorageResource{Storage: "competitions", Node: n, Shared: c.shared})
		}
		before := f.called("content:competitions:10121")
		plan, err := NewPlanner(&resourceAPI{fakeAPI: f, res: res}, config.Default()).Teardown(context.Background(), []string{"01"}, nil)
		if err != nil || len(plan.Items) != 1 {
			t.Fatalf("shared %v: plan %+v, err %v", c.shared, plan, err)
		}
		if n := f.called("content:competitions:10121") - before; n != c.want {
			t.Errorf("shared %v: listed %d times, want %d", c.shared, n, c.want)
		}
	}
}

// A teardown of a few teams asks the storage about each VMID their VMs
// would have from the templates on the cluster; one of many teams lists
// the storage whole, which also finds disks of VMs whose template is gone.
func TestOrphanedDisksAskFewVMIDsOrListWhole(t *testing.T) {
	f := orphanedSetup(t)
	few := f.called("content:competitions:")
	if _, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.called("content:competitions:0"); n != 0 {
		t.Errorf("one team listed the whole storage %d times", n)
	}
	if n := f.called("content:competitions:") - few; n == 0 {
		t.Error("one team asked the storage about no VMID")
	}

	var teams []string
	for i := 1; i <= 50; i++ {
		teams = append(teams, fmt.Sprintf("%02d", i))
	}
	plan, err := testPlanner(f).Teardown(context.Background(), teams, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.called("content:competitions:0"); n == 0 {
		t.Error("50 teams didn't list the storage whole")
	}
	if len(plan.Items) != 1 || plan.Items[0].VMID != 10121 {
		t.Errorf("items = %+v, want the orphan of VMID 10121", plan.Items)
	}
}
