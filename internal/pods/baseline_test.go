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

func TestHasBaseline(t *testing.T) {
	d := config.Default().Deploy
	tests := []struct {
		name  string
		snaps []proxmox.Snapshot
		want  bool
	}{
		{"none", nil, false},
		{"has snapshot_name", snaps("initial", "pre-inject"), true},
		{"other snapshots only", snaps("before-scoring", "fresh_clone_20261002034615"), false},
		{"unfinished snapshot_name is skipped", []proxmox.Snapshot{{Name: "initial", State: "prepare"}}, false},
	}
	for _, tt := range tests {
		if has := HasBaseline(d, tt.snaps); has != tt.want {
			t.Errorf("%s: HasBaseline = %v, want %v", tt.name, has, tt.want)
		}
	}
}

func TestResetExplicitSnapshotIsExact(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "before-scoring")
	f.Add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil, "initial")
	rs, err := testPlanner(f).Reset(context.Background(), []string{"01"}, nil, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if it := rs.Items[1]; it.Name != "team01-teak" || !strings.HasPrefix(it.Blocked, `no snapshot "initial"`) {
		t.Errorf("teak = %+v, want blocked: it has no initial", it)
	}
	if it := rs.Items[0]; it.Snapshot != "initial" || it.Blocked != "" {
		t.Errorf("oak = %+v", it)
	}
}

var resetSnapshots = []string{"initial", "Initial", "initial2", "before-scoring", "round2"}

// resetTargetModel is what a reset to snapshot rolls a VM with snaps back
// to: snapshot itself (or "initial" when snapshot is "") if it has it finished.
func resetTargetModel(snaps []proxmox.Snapshot, snapshot string) (string, bool) {
	if snapshot == "" {
		snapshot = "initial"
	}
	return snapshot, slices.ContainsFunc(snaps, func(s proxmox.Snapshot) bool { return s.Name == snapshot && s.State == "" })
}

func checkReset(t interface{ Fatalf(string, ...any) }, vms map[string][]proxmox.Snapshot, snapshot string) {
	f := podstest.New("n1")
	n, offset := NewNaming(config.Default().Naming), 0
	for name, snaps := range vms {
		team, _, _ := n.ParseVMName(name)
		vmid := n.CloneVMID(team, offset)
		offset++
		var names []string
		times, states := map[string]int64{}, map[string]string{}
		for _, s := range snaps {
			names = append(names, s.Name)
			times[s.Name], states[s.Name] = s.Time, s.State
		}
		f.Add(proxmox.VM{VMID: vmid, Name: name, Node: "n1"}, nil, names...)
		f.VMs[vmid].SnapTimes, f.VMs[vmid].SnapStates = times, states
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
		if (it.Blocked == "") != ok || it.Snapshot != want {
			t.Fatalf("Reset(%q) of %s with %+v: snapshot %q, blocked %q; want %q, ok %v", snapshot, it.Name, vms[it.Name], it.Snapshot, it.Blocked, want, ok)
		}
	}
}

// A reset rolls each VM back to a snapshot it has: the one asked for, or
// "initial" when empty, and is blocked for a VM without it.
func TestPropResetTargetsExist(t *testing.T) {
	targets := []string{"", "initial", "before-scoring", "round2", "Initial", " initial", "initial ", "current", "*", "init", "initial\x00"}
	for _, target := range targets {
		for _, s := range resetSnapshots {
			checkReset(t, map[string][]proxmox.Snapshot{"team01-dc": {{Name: s}}, "team02-dc": {{Name: s, State: "prepare"}}}, target)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		vms := map[string][]proxmox.Snapshot{}
		for _, name := range []string{"team01-dc", "team01-web", "team02-dc"} {
			names := rapid.SliceOfNDistinct(rapid.SampledFrom(resetSnapshots), 0, 4, rapid.ID).Draw(t, name)
			snaps := []proxmox.Snapshot{}
			for _, n := range names {
				snaps = append(snaps, proxmox.Snapshot{Name: n, Time: int64(rapid.IntRange(0, 2).Draw(t, n+" time")),
					State: rapid.SampledFrom([]string{"", "", "prepare", "delete"}).Draw(t, n+" state")})
			}
			vms[name] = snaps
		}
		target := rapid.OneOf(rapid.SampledFrom(targets), rapid.SampledFrom(resetSnapshots), rapid.String()).Draw(t, "target")
		checkReset(t, vms, target)
	})
}
