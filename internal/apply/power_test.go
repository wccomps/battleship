package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// A power job skips a VM already in the state its action leaves, and acts
// on every other one.
func TestPowerSkipsVMsAlreadyInTheStateTheActionLeaves(t *testing.T) {
	for _, a := range pods.PowerActions {
		for _, status := range []string{"running", "stopped"} {
			f := newCluster()
			f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: status}, map[string]string{"name": "team01-teak"})
			plan, err := testPlanner(f).Power(context.Background(), []string{"01"}, []string{"teak"}, a.Value)
			if err != nil {
				t.Fatal(err)
			}
			rec := &recorder{}
			res := testExecutor(f, rec).Run(context.Background(), plan)
			if len(res.Succeeded) != 1 {
				t.Fatalf("%s on %s: result = %+v", a.Value, status, res)
			}
			skipped := len(rec.find("team01-teak", EventSkipped)) == 1
			if want := a.Leaves == status; skipped != want {
				t.Errorf("%s on a %s VM: skipped = %t, want %t", a.Value, status, skipped, want)
			}
			if a.Leaves != "" && f.vms[10121].Status != a.Leaves {
				t.Errorf("%s on a %s VM left it %s, want %s", a.Value, status, f.vms[10121].Status, a.Leaves)
			}
		}
	}
}

// A task the job can't follow (Proxmox answers 403 for its status, as for
// another user's task without Sys.Audit on the node) isn't taken for done:
// the step waits until the VM is no longer locked, fails saying the
// outcome is unknown, and the retry round, which checks the VM's state
// first, finds it already stopped.
func TestTaskThatCantBeFollowedIsCheckedNotAssumed(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, map[string]string{"name": "team01-teak"})
	stopTask := upid("cedar", "qmstop", 10121)
	f.waitHook = func(_ context.Context, id string) error { // runs with f.mu held
		if id != stopTask {
			return nil
		}
		f.waitHook = nil
		// The task is still running: the VM stays locked for two more reads.
		f.lockConfig, f.lockName = map[int]int{10121: 2}, "stop"
		return &proxmox.APIError{Status: 403, Message: "Permission check failed (/nodes/cedar, Sys.Audit)"}
	}
	plan, err := testPlanner(f).Power(context.Background(), []string{"01"}, []string{"teak"}, "stop")
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	ex := testExecutor(f, rec)
	ex.Cfg.Retry.Rounds = 1
	res := ex.Run(context.Background(), plan)
	failed := rec.find("team01-teak", EventFailed)
	if len(failed) != 1 || !strings.Contains(failed[0].Message, "whether it worked is unknown") || !strings.Contains(failed[0].Message, "no longer locked") {
		t.Fatalf("first round: %+v", failed)
	}
	if len(res.Succeeded) != 1 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v, want the retry round to find it stopped", res)
	}
	if skipped := rec.find("team01-teak", EventSkipped); len(skipped) != 1 {
		t.Fatalf("the retry round didn't check the state first: %+v", rec.find("team01-teak", EventDone))
	}
}
