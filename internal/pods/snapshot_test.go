package pods

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/proxmox"
)

func TestSnapshotNameExamples(t *testing.T) {
	for _, name := range []string{"ab", "before-scoring", "Round_2", "x1", "a" + strings.Repeat("b", 39), "Current", "currentish"} {
		if err := CheckSnapshotName(name); err != nil {
			t.Errorf("CheckSnapshotName(%q) = %v, want nil", name, err)
		}
	}
	for name, why := range map[string]string{
		"":                            "needs a name",
		"a":                           "at least 2",
		"1st":                         "start with a letter",
		"_x":                          "start with a letter",
		"has space":                   "letters, digits",
		"dot.ted":                     "letters, digits",
		"café":                        "letters, digits",
		"a" + strings.Repeat("b", 40): "at most 40",
		"current":                     "reserved",
		"pending":                     "reserved",
		"PENDING":                     "reserved",
	} {
		err := CheckSnapshotName(name)
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("CheckSnapshotName(%q) = %v, want an error saying %q", name, err, why)
		}
	}
}

// proxmoxAccepts is Proxmox's rule for a new snapshot's name, written out
// independently of CheckSnapshotName: the pve-configid format (a letter,
// then at least one letter, digit, _ or -), at most 40 characters, and not
// one of the names its API reserves.
func proxmoxAccepts(s string) bool {
	if len(s) < 2 || len(s) > 40 || s == "current" || strings.ToLower(s) == "pending" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i == 0 && !letter {
			return false
		}
		if !letter && !(c >= '0' && c <= '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func TestPropSnapshotNameIsProxmoxGrammar(t *testing.T) {
	alphabet := []rune("abcXYZ019_- .é/current-PENDINGp")
	rapid.Check(t, func(t *rapid.T) {
		var name string
		if rapid.Bool().Draw(t, "grammatical") {
			name = rapid.StringMatching(`[A-Za-z][A-Za-z0-9_-]{0,41}`).Draw(t, "name")
		} else {
			name = string(rapid.SliceOfN(rapid.SampledFrom(alphabet), 0, 44).Draw(t, "runes"))
		}
		if rapid.IntRange(0, 9).Draw(t, "reserved") == 0 {
			name = rapid.SampledFrom([]string{"current", "pending", "Pending", "Current"}).Draw(t, "word")
		}
		got := CheckSnapshotName(name) == nil
		if want := proxmoxAccepts(name); got != want {
			t.Fatalf("CheckSnapshotName(%q) accepts: %t, Proxmox: %t", name, got, want)
		}
	})
}

func TestSnapshotPlan(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial")
	f.Add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil, "initial", "before-scoring")
	f.Add(proxmox.VM{VMID: 10221, Name: "team02-teak", Node: "cedar"}, nil)
	f.Add(proxmox.VM{VMID: 10225, Name: "team02-oak", Node: "birch"}, nil)
	f.FailOn("snapshots:10225", errors.New("boom"))
	plan, err := testPlanner(f).Snapshot(context.Background(), SnapshotRequest{
		Teams: []string{"1", "2"}, Name: "before-scoring", Description: "round 2", VMState: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != KindSnapshot || !slices.Equal(plan.Teams, []string{"01", "02"}) {
		t.Errorf("plan = %s over %v", plan.Kind, plan.Teams)
	}
	byName := map[string]Item{}
	for _, it := range plan.Items {
		byName[it.Name] = it
		if !slices.Equal(it.Steps, []Step{StepSnapshot}) || it.Snapshot != "before-scoring" || it.Description != "round 2" || !it.VMState {
			t.Errorf("%s = %+v", it.Name, it)
		}
	}
	if len(byName) != 4 {
		t.Fatalf("items = %+v", plan.Items)
	}
	if got := byName["team01-oak"].Blocked; got != `already has a snapshot named "before-scoring"; choose another name` {
		t.Errorf("team01-oak Blocked = %q", got)
	}
	if got := byName["team02-oak"].Blocked; !strings.HasPrefix(got, "cannot list snapshots: ") {
		t.Errorf("team02-oak Blocked = %q", got)
	}
	for _, n := range []string{"team01-teak", "team02-teak"} {
		if byName[n].Blocked != "" {
			t.Errorf("%s Blocked = %q", n, byName[n].Blocked)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.FailOn("snapshots:10121", context.Canceled)
	if _, err := testPlanner(f).Snapshot(ctx, SnapshotRequest{Teams: []string{"01"}, Name: "x1"}); err == nil {
		t.Error("cancelled context should return an error")
	}
}

func TestSnapshotPlanRefusesBadAndBaselineNames(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
	for name, why := range map[string]string{
		"initial":            `"initial" is the baseline snapshot a deploy takes (deploy.snapshot_name)`,
		"fresh_clone_202610": `"fresh_clone_202610" matches fresh_clone_*, so a reset to the baseline could pick it (deploy.baseline_patterns)`,
		"current":            "reserved",
		"9lives":             "start with a letter",
	} {
		_, err := testPlanner(f).Snapshot(context.Background(), SnapshotRequest{Teams: []string{"01"}, Name: name})
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("Snapshot(%q) = %v, want an error saying %q", name, err, why)
		}
	}
	if f.Called("snapshots:") != 0 || f.Called("snapshot:") != 0 {
		t.Errorf("calls = %v, want none for a refused name", f.Calls)
	}
}

// Whatever snapshots the VMs have, the planner never plans to take one a VM
// already has: every runnable item's VM lacks the name.
func TestPropSnapshotPlanNeverOverwrites(t *testing.T) {
	pool := []string{"aa", "bb", "before-scoring", "initial"}
	rapid.Check(t, func(t *rapid.T) {
		f := newCluster()
		has := map[string][]string{}
		for team := 1; team <= 3; team++ {
			for i, host := range []string{"teak", "oak"} {
				name := fmt.Sprintf("team%02d-%s", team, host)
				snaps := rapid.SliceOfDistinct(rapid.SampledFrom(pool), rapid.ID[string]).Draw(t, name)
				f.Add(proxmox.VM{VMID: 10000 + team*100 + 21 + 4*i, Name: name, Node: "cedar"}, nil, snaps...)
				has[name] = snaps
			}
		}
		want := rapid.SampledFrom(pool[:3]).Draw(t, "want")
		plan, err := testPlanner(f).Snapshot(context.Background(), SnapshotRequest{Teams: []string{"1", "2", "3"}, Name: want})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Items) != 6 {
			t.Fatalf("items = %d, want 6", len(plan.Items))
		}
		for _, it := range plan.Items {
			if slices.Contains(has[it.Name], want) != (it.Blocked != "") {
				t.Fatalf("%s has %v, want %q: Blocked = %q", it.Name, has[it.Name], want, it.Blocked)
			}
		}
	})
}
