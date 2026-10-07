package apply

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// A reset that names no snapshot rolls each VM back to its own baseline:
// snapshot_name if it has it, else its newest pattern match.
func TestResetDefaultsToEachVMsBaseline(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "fresh_clone_20261001000000", "fresh_clone_20261002034615", "pre-inject")
	f.Add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil, "fresh_clone_20260901000000")
	f.Add(proxmox.VM{VMID: 10130, Name: "team01-notes", Node: "cedar"}, nil, "fresh_clone_20261002034615", "initial")
	f.Add(proxmox.VM{VMID: 10131, Name: "team01-mail", Node: "cedar"}, nil, "before-scoring")

	rs, err := testPlanner(f).Reset(context.Background(), []string{"01"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]pods.Item{}
	for _, it := range rs.Items {
		got[it.Name] = it
	}
	want := map[string]string{
		"team01-teak":  "fresh_clone_20261002034615",
		"team01-oak":   "fresh_clone_20260901000000",
		"team01-notes": "initial",
	}
	for name, snap := range want {
		it := got[name]
		if it.Snapshot != snap || !it.Baseline || it.Blocked != "" {
			t.Errorf("%s = snapshot %q baseline %v blocked %q; want baseline %q", name, it.Snapshot, it.Baseline, it.Blocked, snap)
		}
	}
	if it := got["team01-mail"]; it.Blocked == "" || !strings.HasPrefix(it.Blocked, `no "initial" or fresh_clone_* baseline snapshot`) || !strings.Contains(it.Blocked, "before-scoring") {
		t.Errorf("team01-mail = %+v, want blocked with no baseline", it)
	}
	if l := pods.SnapshotLabel(got["team01-teak"]); l != "baseline (fresh_clone_20261002034615)" {
		t.Errorf("label = %q", l)
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), rs)
	if len(res.Failed) != 0 || f.Called("rollback:10121:fresh_clone_20261002034615") != 1 || f.Called("rollback:10125:fresh_clone_20260901000000") != 1 || f.Called("rollback:10130:initial") != 1 {
		t.Errorf("result %+v, calls %v", res, f.Calls)
	}
}

// A converge deploy doesn't take snapshot_name on a VM that already has an
// old-tool baseline: that would capture a played-in VM as the baseline.
func TestDeployKeepsPatternBaseline(t *testing.T) {
	f := newCluster()
	testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	vm := f.VMs[10121]
	vm.Snapshots = []string{"fresh_clone_20261002034615"}
	vm.Status = "running"
	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	if len(res.Failed) != 0 || !reflect.DeepEqual(vm.Snapshots, []string{"fresh_clone_20261002034615"}) {
		t.Errorf("result %+v, snapshots %v", res, vm.Snapshots)
	}
}
