package apply

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func sortedTags(s string) []string {
	t := pods.Tags(s)
	slices.Sort(t)
	return t
}

// A deploy records each pod VM's set in its tags, keeping the tags its clone
// copied; the master and template are left as they are.
func TestDeployTagsTheSet(t *testing.T) {
	f := newCluster()
	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01", "02"))
	if len(res.Failed) != 0 || len(res.Succeeded) != 2 {
		t.Fatalf("result = %+v", res)
	}
	for _, vmid := range []int{10121, 10221} {
		if got, want := sortedTags(f.VMs[vmid].Config["tags"]), []string{"dev", "set-tango.delta", "tango.delta"}; !slices.Equal(got, want) {
			t.Errorf("VM %d tags = %v, want %v", vmid, got, want)
		}
	}
	for _, vmid := range []int{121, 9021} {
		if got := f.VMs[vmid].Config["tags"]; got != "dev;tango.delta" {
			t.Errorf("VM %d tags = %q, want its own", vmid, got)
		}
	}
}

// A redeploy tags a VM deployed before deploys tagged sets, and replaces a
// set tag that names another set.
func TestRedeployRetagsTheSet(t *testing.T) {
	f := newCluster()
	testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01", "02"))
	f.VMs[10121].Config["tags"] = "dev"                                   // deployed before set tags
	f.VMs[10221].Config["tags"] = "Set-Kilo.Alpha;dev;set-teak.x;keep-me" // edited by hand
	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01", "02"))
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if got, want := sortedTags(f.VMs[10121].Config["tags"]), []string{"dev", "set-tango.delta"}; !slices.Equal(got, want) {
		t.Errorf("untagged VM tags = %v, want %v", got, want)
	}
	if got, want := sortedTags(f.VMs[10221].Config["tags"]), []string{"dev", "keep-me", "set-tango.delta"}; !slices.Equal(got, want) {
		t.Errorf("retagged VM tags = %v, want %v", got, want)
	}
	if f.Called("clone:10121") != 1 {
		t.Error("the redeploy recloned")
	}
}

// Two sets deployed side by side in a pod: tearing one down deletes its VMs
// and leaves the other's running.
func TestTeardownOfOneSetLeavesTheOther(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 140, Name: "elm.kilo.alpha", Node: "cedar", Tags: "dev"},
		map[string]string{"net0": "virtio=BC:24:11:00:01:40,bridge=vmbr0"})
	for _, pattern := range []string{"teak.*", "*.kilo.alpha"} {
		plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: pattern, Teams: []string{"01"}})
		if err != nil {
			t.Fatal(err)
		}
		if res := testExecutor(f, &recorder{}).Run(context.Background(), plan); len(res.Failed) != 0 {
			t.Fatalf("deploying %s: %v", pattern, res.Failed)
		}
	}
	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil, "kilo.alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Name != "team01-elm" || plan.Items[0].Set != "kilo.alpha" {
		t.Fatalf("items = %+v", plan.Items)
	}
	if len(plan.Kept) != 1 || plan.Kept[0].Name != "team01-teak" || plan.Kept[0].Why != "of tango.delta" {
		t.Errorf("kept = %+v", plan.Kept)
	}
	if res := testExecutor(f, &recorder{}).Run(context.Background(), plan); len(res.Failed) != 0 || len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, ok := f.VMs[10140]; ok {
		t.Error("team01-elm is still there")
	}
	if vm, ok := f.VMs[10121]; !ok || vm.Status != "running" {
		t.Errorf("team01-teak = %+v, want running", vm)
	}
}

// A destroy that fails halfway leaves only lock: destroyed (no name, no
// tags). Replanning a set teardown still reports the half-deleted VM as
// blocked with the qm destroy --skiplock advice.
func TestHalfDestroyedSetVMShowsInNextSetTeardown(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Pool: "pool-01", Tags: "set-tango.delta"},
		map[string]string{"tags": "set-tango.delta", "scsi0": "competitions:vm-10121-disk-0.qcow2"})
	f.HalfDestroy = map[int]bool{10121: true}
	f.FailOn("wait:"+podstest.UPID("cedar", "qmdestroy", 10121), userCfgTimeout("cedar", 10121))

	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil, "tango.delta")
	if err != nil {
		t.Fatal(err)
	}
	if res := testExecutor(f, &recorder{}).Run(context.Background(), plan); len(res.Failed) != 1 {
		t.Fatalf("first run = %+v", res)
	}

	for _, set := range []pods.Set{"tango.delta", "kilo.alpha", ""} {
		after, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil, set)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Items) != 1 || after.Items[0].Name != "team01-half-deleted-10121" || !strings.Contains(after.Items[0].Blocked, "skiplock") {
			t.Errorf("set %q: items = %+v, kept = %+v; want blocked team01-half-deleted-10121", set, after.Items, after.Kept)
		}
	}
}
