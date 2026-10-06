package pods

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

func countCalls(f *fakeAPI, key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

// A step Proxmox refuses for a missing privilege (403) is never tried
// again, not even in a retry round.
func TestForbiddenItemIsNotRetried(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, map[string]string{"name": "team01-teak"})
	f.failOn("power:10121:stop", &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10121, VM.PowerMgmt)"})
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

// newRunningMaster is the cluster with the teak master running, so a
// template build stops it for the copy.
func newRunningMaster() *fakeAPI {
	f := newCluster()
	f.vms[121].Status = "running"
	return f
}

// Item 2: a stop the build couldn't follow (403), and which was then seen
// to have ended (the master unlocked), doesn't leave the master stopped:
// the deferred restart doesn't trip over the same 403 again.
func TestMasterRestartedAfterUnfollowableStop(t *testing.T) {
	f := newRunningMaster()
	stop := upid("cedar", "qmstop", 121)
	f.waitHook = func(_ context.Context, id string) error {
		if id == stop {
			return forbidden
		}
		return nil
	}
	rec := &recorder{}
	ex := testExecutor(f, rec)
	ex.Cfg.Retry.Rounds = 0
	ex.Run(context.Background(), deployTeak(t, f, "01"))
	if n := f.called("power:121:start"); n != 1 || f.vms[121].Status != "running" {
		t.Fatalf("master started %d times, want 1; events %+v", n, rec.find("teak.tango.delta.tpl", EventFailed))
	}
}

// Item 3: a clone task the job can't follow is judged by its TARGET's lock
// (Proxmox doesn't lock the source), so a clone still running isn't
// declared over.
func TestUnfollowableCloneWaitsOnItsTarget(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	cloneTask := upid("cedar", "qmclone", 9021)
	f.waitHook = func(_ context.Context, id string) error {
		if id == cloneTask {
			f.waitHook = nil
			f.lockConfig = map[int]int{10121: 3} // the clone is still writing the target
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
	f.mu.Lock()
	left := f.lockConfig[10121]
	f.mu.Unlock()
	if left != 0 {
		t.Fatalf("stopped waiting with the target still locked (%d reads left)", left)
	}
}

// Item 4: waiting for an unfollowable task's VM to unlock stops when Proxmox
// refuses the reads too (403 or 401), instead of polling forever while the
// job holds its locks.
func TestUnfollowableStopsOnRefusedReads(t *testing.T) {
	for _, status := range []int{401, 403} {
		f := newCluster()
		f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, map[string]string{"name": "team01-teak"})
		plan, err := testPlanner(f).Power(context.Background(), []string{"01"}, []string{"teak"}, "stop")
		if err != nil {
			t.Fatal(err)
		}
		stop := upid("cedar", "qmstop", 10121)
		f.waitHook = func(_ context.Context, id string) error {
			if id == stop {
				f.waitHook = nil
				for range 100000 {
					f.failOn("config:10121", &proxmox.APIError{Status: status, Message: "refused"})
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
		if n := f.called("config:10121"); n > 5 {
			t.Errorf("%d: read the config %d times after refusals", status, n)
		}
	}
}

// Item 5: when the job's authorization lapses during a template build, the
// stopped master is reported in the result (not only an event), the waits
// that can only fail are skipped, and nothing claims a clone that never
// started may still be running.
func TestLapseDuringBuildReportsStoppedMaster(t *testing.T) {
	f := newRunningMaster()
	stop := upid("cedar", "qmstop", 121)
	f.waitHook = func(_ context.Context, id string) error {
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
	if n := f.called("wait:" + stop); n != 1 {
		t.Errorf("waited on the stop task %d times after the lapse, want 1", n)
	}
	for _, ev := range rec.events {
		if strings.Contains(ev.Message, "clone task may still be running") {
			t.Errorf("event %q, but no clone started", ev.Message)
		}
	}
}

// Item 17: when Proxmox won't list the user's privileges, the access says
// so once and is taken as holding nothing, rather than asking again for
// every cell of every grid render.
func TestAccessRemembersAFailedListing(t *testing.T) {
	f := &fakePerms{err: &proxmox.APIError{Status: 500, Message: "down"}}
	acc := NewAccess(f)
	for range 10 {
		if p, _ := acc.Approx(context.Background(), "/vms/10101"); len(p) != 0 {
			t.Fatalf("Approx = %v", p)
		}
		if ok, _ := acc.Anywhere(context.Background(), "VM.PowerMgmt"); ok {
			t.Fatal("Anywhere = true")
		}
		if _, err := acc.Privileges(context.Background(), "/vms/10101"); err == nil {
			t.Fatal("Privileges hid the failure")
		}
	}
	if len(f.calls) != 1 {
		t.Fatalf("asked Proxmox %d times: %v", len(f.calls), f.calls)
	}
}

// Item 21: rewiring a NIC needs SDN.Use on the vnet it leaves as well as
// the one it joins. A new clone's NICs start on its template's bridges;
// when one is a team vnet of the configured zone, the preview checks it.
// Other bridges (and an existing VM's, unknown at plan time) are left to
// Proxmox.
func TestNetworkNeedsSDNUseOnTheBridgeLeft(t *testing.T) {
	cfg := config.Default()
	tpl := TemplateSpec{Name: "teak.x.tpl", VMID: 9021, Exists: true, Interfaces: 1, Bridges: []string{"int07"}}
	item := Item{Team: "01", Name: "team01-teak", VMID: 10121, Template: tpl.Name, Steps: []Step{StepClone, StepNetwork}}
	acc := grants{"/vms/9021": {"VM.Clone"}, "/pool/pool-01": {"VM.Allocate", "VM.Config.Network"},
		"/storage/competitions": {"Datastore.AllocateSpace"}, "/sdn/zones/teams/int01": {"SDN.Use"}}
	plan := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
	if err := BlockUnpermitted(context.Background(), plan, acc, cfg); err != nil {
		t.Fatal(err)
	}
	if b := plan.Items[0].Blocked; b != "you don't have SDN.Use on /sdn/zones/teams/int07" {
		t.Fatalf("blocked %q", b)
	}
	// A bridge outside the zone's team vnets isn't checked here.
	tpl.Bridges = []string{"vmbr0"}
	plan = &Plan{Kind: KindDeploy, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
	_ = BlockUnpermitted(context.Background(), plan, acc, cfg)
	if b := plan.Items[0].Blocked; b != "" {
		t.Fatalf("blocked %q for vmbr0", b)
	}
}

func TestTemplateSpecReadsBridges(t *testing.T) {
	f := newCluster()
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Templates[0].Bridges; len(got) != 2 || got[0] != "vmbr0" || got[1] != "vmbr1" {
		t.Fatalf("bridges = %v", got)
	}
}
