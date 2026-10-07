package apply

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func countCalls(f *podstest.Fake, key string) int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if c == key {
			n++
		}
	}
	return n
}

// A step refused for a missing privilege (403) is never retried, not even
// in a retry round.
func TestForbiddenItemIsNotRetried(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, map[string]string{"name": "team01-teak"})
	f.FailOn("power:10121:stop", &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10121, VM.PowerMgmt)"})
	plan, err := testPlanner(f).Power(context.Background(), []string{"01"}, []string{"teak"}, "stop")
	if err != nil {
		t.Fatal(err)
	}
	ex := testExecutor(f, &recorder{})
	ex.Cfg.Retry.Rounds = 2
	res := ex.Run(context.Background(), plan)
	if n := countCalls(f, "power:10121:stop"); n != 1 {
		t.Fatalf("a forbidden power call was made %d times, want 1", n)
	}
	if _, ok := res.Failed["team01-teak"]; !ok {
		t.Fatalf("result = %+v, want team01-teak failed", res)
	}
}

var forbidden = &proxmox.APIError{Status: 403, Message: "Permission check failed (/nodes/cedar, Sys.Audit)"}

// newRunningMaster has the teak master running, so a build stops it.
func newRunningMaster() *podstest.Fake {
	f := newCluster()
	f.VMs[121].Status = "running"
	return f
}

// A stop the build couldn't follow (403) but that has ended doesn't leave
// the master stopped: the restart doesn't trip over the same 403.
func TestMasterRestartedAfterUnfollowableStop(t *testing.T) {
	f := newRunningMaster()
	stop := podstest.UPID("cedar", "qmstop", 121)
	f.WaitHook = func(_ context.Context, id string) error {
		if id == stop {
			return forbidden
		}
		return nil
	}
	rec := &recorder{}
	ex := testExecutor(f, rec)
	ex.Cfg.Retry.Rounds = 0
	ex.Run(context.Background(), deployTeak(t, f, "01"))
	if n := f.Called("power:121:start"); n != 1 || f.VMs[121].Status != "running" {
		t.Fatalf("master started %d times, want 1; events %+v", n, rec.find("teak.tango.delta.tpl", EventFailed))
	}
}

// An unfollowable clone is judged by its target's lock (Proxmox doesn't
// lock the source), so a running clone isn't declared over.
func TestUnfollowableCloneWaitsOnItsTarget(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	cloneTask := podstest.UPID("cedar", "qmclone", 9021)
	f.WaitHook = func(_ context.Context, id string) error {
		if id == cloneTask {
			f.WaitHook = nil
			f.LockConfig = map[int]int{10121: 3} // the clone is still writing the target
			return forbidden
		}
		return nil
	}
	rec := &recorder{}
	testExecutor(f, rec).Run(context.Background(), plan)
	var msg string
	for _, ev := range rec.find("team01-teak", EventFailed) {
		if strings.Contains(ev.Message, "couldn't follow task") {
			msg = ev.Message
		}
	}
	if !strings.Contains(msg, "VM 10121 is no longer locked") {
		t.Fatalf("unfollowable clone judged by %q, want its target 10121", msg)
	}
	f.Mu.Lock()
	left := f.LockConfig[10121]
	f.Mu.Unlock()
	if left != 0 {
		t.Fatalf("stopped waiting with the target still locked (%d reads left)", left)
	}
}

// Waiting for an unfollowable task's VM to unlock stops when reads are
// refused too (403/401), rather than polling forever holding locks.
func TestUnfollowableStopsOnRefusedReads(t *testing.T) {
	for _, status := range []int{401, 403} {
		f := newCluster()
		f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, map[string]string{"name": "team01-teak"})
		plan, err := testPlanner(f).Power(context.Background(), []string{"01"}, []string{"teak"}, "stop")
		if err != nil {
			t.Fatal(err)
		}
		stop := podstest.UPID("cedar", "qmstop", 10121)
		f.WaitHook = func(_ context.Context, id string) error {
			if id == stop {
				f.WaitHook = nil
				for range 100000 {
					f.FailOn("config:10121", &proxmox.APIError{Status: status, Message: "refused"})
				}
				return forbidden
			}
			return nil
		}
		done := make(chan struct{})
		go func() { defer close(done); testExecutor(f, &recorder{}).Run(context.Background(), plan) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%d: still polling the VM after Proxmox refused its reads", status)
		}
		if n := f.Called("config:10121"); n > 5 {
			t.Errorf("%d: read the config %d times after refusals", status, n)
		}
	}
}

// If authorization lapses mid-build, the stopped master is reported in the
// result, doomed waits are skipped, and a never-started clone isn't called
// possibly running.
func TestLapseDuringBuildReportsStoppedMaster(t *testing.T) {
	f := newRunningMaster()
	stop := podstest.UPID("cedar", "qmstop", 121)
	f.WaitHook = func(_ context.Context, id string) error {
		if id == stop {
			return &proxmox.APIError{Status: 401, Message: "authentication failure"}
		}
		return nil
	}
	rec := &recorder{}
	res := testExecutor(f, rec).Run(context.Background(), deployTeak(t, f, "01"))
	err := res.CleanupFailed["teak.tango.delta"]
	if err == nil || !strings.Contains(err.Error(), "left stopped") || !strings.Contains(err.Error(), "start it in Proxmox") {
		t.Fatalf("CleanupFailed = %v, want the stopped master", res.CleanupFailed)
	}
	if advice := CleanupAdvice("teak.tango.delta", err); strings.Contains(advice, "remove it in Proxmox") {
		t.Errorf("advice %q tells to remove the master", advice)
	}
	if n := f.Called("wait:" + stop); n != 1 {
		t.Errorf("waited on the stop task %d times after the lapse, want 1", n)
	}
	for _, ev := range rec.events {
		if strings.Contains(ev.Message, "clone task may still be running") {
			t.Errorf("event %q, but no clone started", ev.Message)
		}
	}
}
