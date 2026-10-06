package pods

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

func snaps(names ...string) []proxmox.Snapshot {
	out := make([]proxmox.Snapshot, len(names))
	for i, n := range names {
		out[i] = proxmox.Snapshot{Name: n}
	}
	return out
}

func TestBaselineSnapshot(t *testing.T) {
	d := config.Default().Deploy
	tests := []struct {
		name  string
		snaps []proxmox.Snapshot
		want  string
	}{
		{"none", nil, ""},
		{"only snapshot_name", snaps("initial", "pre-inject"), "initial"},
		{"only an old-tool baseline", snaps("fresh_clone_20261002034615"), "fresh_clone_20261002034615"},
		{"newest by name", snaps("fresh_clone_20261002034615", "fresh_clone_20261003010101", "fresh_clone_20260901000000"), "fresh_clone_20261003010101"},
		{"snapshot_name wins over patterns", snaps("fresh_clone_20261003010101", "initial"), "initial"},
		{"non-matching names are not baselines", snaps("before-scoring", "fresh_clone"), ""},
		{"newest by snaptime when Proxmox gives it", []proxmox.Snapshot{
			{Name: "fresh_clone_20261003010101", Time: 100},
			{Name: "fresh_clone_20261002034615", Time: 200},
		}, "fresh_clone_20261002034615"},
	}
	for _, tt := range tests {
		got, ok := BaselineSnapshot(d, tt.snaps)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("%s: BaselineSnapshot = %q, %v; want %q", tt.name, got, ok, tt.want)
		}
		if has := HasBaseline(d, SnapshotNames(tt.snaps)); has != (tt.want != "") {
			t.Errorf("%s: HasBaseline = %v", tt.name, has)
		}
	}

	d.BaselinePatterns = nil
	if got, ok := BaselineSnapshot(d, snaps("fresh_clone_20261002034615")); ok {
		t.Errorf("with no patterns, BaselineSnapshot = %q, want none", got)
	}
}

// A reset that names no snapshot rolls each VM back to its own baseline:
// snapshot_name if it has it, else its newest pattern match.
func TestResetDefaultsToEachVMsBaseline(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "fresh_clone_20261001000000", "fresh_clone_20261002034615", "pre-inject")
	f.add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil, "fresh_clone_20260901000000")
	f.add(proxmox.VM{VMID: 10130, Name: "team01-notes", Node: "cedar"}, nil, "fresh_clone_20261002034615", "initial")
	f.add(proxmox.VM{VMID: 10131, Name: "team01-mail", Node: "cedar"}, nil, "before-scoring")

	rs, err := testPlanner(f).Reset(context.Background(), []string{"01"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Item{}
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
	if l := SnapshotLabel(got["team01-teak"]); l != "baseline (fresh_clone_20261002034615)" {
		t.Errorf("label = %q", l)
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), rs)
	if len(res.Failed) != 0 || f.called("rollback:10121:fresh_clone_20261002034615") != 1 || f.called("rollback:10125:fresh_clone_20260901000000") != 1 || f.called("rollback:10130:initial") != 1 {
		t.Errorf("result %+v, calls %v", res, f.calls)
	}
}

func TestResetBaselineUsesSnaptime(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "fresh_clone_20261003000000", "fresh_clone_20261002034615")
	f.setSnapTime(10121, "fresh_clone_20261003000000", 1000)
	f.setSnapTime(10121, "fresh_clone_20261002034615", 2000)
	rs, err := testPlanner(f).Reset(context.Background(), []string{"01"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if rs.Items[0].Snapshot != "fresh_clone_20261002034615" {
		t.Errorf("snapshot = %q, want the one with the latest snaptime", rs.Items[0].Snapshot)
	}
}

// A snapshot named explicitly is matched exactly, with no baseline fallback.
func TestResetExplicitSnapshotIsExact(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "fresh_clone_20261002034615")
	f.add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil, "initial")
	rs, err := testPlanner(f).Reset(context.Background(), []string{"01"}, nil, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if it := rs.Items[1]; it.Name != "team01-teak" || !strings.HasPrefix(it.Blocked, `no snapshot "initial"`) || it.Baseline {
		t.Errorf("teak = %+v, want blocked: it has no initial", it)
	}
	if it := rs.Items[0]; it.Snapshot != "initial" || it.Baseline || it.Blocked != "" {
		t.Errorf("oak = %+v", it)
	}
	if l := SnapshotLabel(rs.Items[0]); l != "initial" {
		t.Errorf("label = %q", l)
	}
}

// A converge deploy doesn't take snapshot_name on a VM that already has an
// old-tool baseline: that would capture a played-in VM as the baseline.
func TestDeployKeepsPatternBaseline(t *testing.T) {
	f := newCluster()
	testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	vm := f.vms[10121]
	vm.Snapshots = []string{"fresh_clone_20261002034615"}
	vm.Status = "running"
	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	if len(res.Failed) != 0 || !reflect.DeepEqual(vm.Snapshots, []string{"fresh_clone_20261002034615"}) {
		t.Errorf("result %+v, snapshots %v", res, vm.Snapshots)
	}
}
