package pods

import (
	"context"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
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

func TestResetBaselineUsesSnaptime(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "fresh_clone_20261003000000", "fresh_clone_20261002034615")
	f.SetSnapTime(10121, "fresh_clone_20261003000000", 1000)
	f.SetSnapTime(10121, "fresh_clone_20261002034615", 2000)
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
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "fresh_clone_20261002034615")
	f.Add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil, "initial")
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

// resetSnapshots are snapshot names a team VM may have: the baseline, old-
// tool baselines and near misses of both.
var resetSnapshots = []string{"initial", "fresh_clone_20261001", "fresh_clone_20261002", "fresh_clone_", "fresh_clone", "Initial",
	"initial2", "xfresh_clone_1", "before-scoring", "round2"}

// resetTargetModel is what a reset to snapshot rolls a VM with snaps back
// to: snapshot itself if it has it; for the baseline, "initial", else the
// newest fresh_clone_* by time, then name.
func resetTargetModel(snaps []proxmox.Snapshot, snapshot string) (string, bool) {
	if snapshot != "" {
		return snapshot, slices.ContainsFunc(snaps, func(s proxmox.Snapshot) bool { return s.Name == snapshot })
	}
	var best *proxmox.Snapshot
	for i, s := range snaps {
		if s.Name == "initial" {
			return s.Name, true
		}
		if strings.HasPrefix(s.Name, "fresh_clone_") && (best == nil || s.Time > best.Time || s.Time == best.Time && s.Name > best.Name) {
			best = &snaps[i]
		}
	}
	if best == nil {
		return "", false
	}
	return best.Name, true
}

func checkReset(t interface{ Fatalf(string, ...any) }, vms map[string][]proxmox.Snapshot, snapshot string) {
	f := podstest.New("n1")
	vmid := 10101
	for name, snaps := range vms {
		var names []string
		times := map[string]int64{}
		for _, s := range snaps {
			names = append(names, s.Name)
			times[s.Name] = s.Time
		}
		f.Add(proxmox.VM{VMID: vmid, Name: name, Node: "n1"}, nil, names...)
		f.VMs[vmid].SnapTimes = times
		vmid++
	}
	plan, err := NewPlanner(f, config.Default()).Reset(context.Background(), []string{"01", "02"}, nil, snapshot)
	if err != nil {
		t.Fatalf("Reset(%q): %v", snapshot, err)
	}
	if len(plan.Items) != len(vms) {
		t.Fatalf("Reset(%q) items = %+v", snapshot, plan.Items)
	}
	for _, it := range plan.Items {
		want, ok := resetTargetModel(vms[it.Name], snapshot)
		if (it.Blocked == "") != ok || ok && it.Snapshot != want || it.Baseline != (snapshot == "") {
			t.Fatalf("Reset(%q) of %s with %+v: snapshot %q, blocked %q; want %q, ok %v", snapshot, it.Name, vms[it.Name], it.Snapshot, it.Blocked, want, ok)
		}
	}
}

// A reset rolls each VM back to a snapshot it has: the one asked for, or
// its baseline, and is blocked for a VM without it.
func TestPropResetTargetsExist(t *testing.T) {
	targets := []string{"", "initial", "before-scoring", "round2", "Initial", " initial", "initial ", "current", "fresh_clone_*", "*",
		"fresh_clone_20261001", "init", "initial\x00"}
	for _, target := range targets {
		for _, s := range resetSnapshots {
			checkReset(t, map[string][]proxmox.Snapshot{"team01-dc": {{Name: s}}, "team02-dc": nil}, target)
		}
		checkReset(t, map[string][]proxmox.Snapshot{"team01-dc": {{Name: "fresh_clone_20261001", Time: 2}, {Name: "fresh_clone_20261002", Time: 1}},
			"team02-dc": {{Name: "fresh_clone_b", Time: 1}, {Name: "fresh_clone_a", Time: 1}}}, target)
	}
	rapid.Check(t, func(t *rapid.T) {
		vms := map[string][]proxmox.Snapshot{}
		for _, name := range []string{"team01-dc", "team01-web", "team02-dc"} {
			names := rapid.SliceOfNDistinct(rapid.OneOf(rapid.SampledFrom(resetSnapshots), rapid.StringMatching(`fresh_clone_[0-9]{1,2}`)),
				0, 4, rapid.ID).Draw(t, name)
			snaps := []proxmox.Snapshot{}
			for _, n := range names {
				snaps = append(snaps, proxmox.Snapshot{Name: n, Time: int64(rapid.IntRange(0, 2).Draw(t, n+" time"))})
			}
			vms[name] = snaps
		}
		target := rapid.OneOf(rapid.SampledFrom(targets), rapid.SampledFrom(resetSnapshots), rapid.String()).Draw(t, "target")
		checkReset(t, vms, target)
	})
}
