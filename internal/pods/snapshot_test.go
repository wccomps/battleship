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
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial")
	f.add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil, "initial", "before-scoring")
	f.add(proxmox.VM{VMID: 10221, Name: "team02-teak", Node: "cedar"}, nil)
	f.add(proxmox.VM{VMID: 10225, Name: "team02-oak", Node: "birch"}, nil)
	f.failOn("snapshots:10225", errors.New("boom"))
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
	f.failOn("snapshots:10121", context.Canceled)
	if _, err := testPlanner(f).Snapshot(ctx, SnapshotRequest{Teams: []string{"01"}, Name: "x1"}); err == nil {
		t.Error("cancelled context should return an error")
	}
}

func TestSnapshotPlanRefusesBadAndBaselineNames(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
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
	if f.called("snapshots:") != 0 || f.called("snapshot:") != 0 {
		t.Errorf("calls = %v, want none for a refused name", f.calls)
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
				f.add(proxmox.VM{VMID: 10000 + team*100 + 21 + 4*i, Name: name, Node: "cedar"}, nil, snaps...)
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

func snapshotPlan(t *testing.T, f *fakeAPI, req SnapshotRequest) *Plan {
	t.Helper()
	if req.Teams == nil {
		req.Teams = []string{"01"}
	}
	plan, err := testPlanner(f).Snapshot(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestTakeSnapshot(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, nil, "initial")
	plan := snapshotPlan(t, f, SnapshotRequest{Name: "before-scoring", Description: "round 2", VMState: true})
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), plan)
	if len(res.Failed) != 0 || !slices.Equal(res.Succeeded, []string{"team01-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	want := []proxmox.SnapshotRequest{{Name: "before-scoring", Description: "round 2", VMState: true}}
	if !slices.Equal(f.snapshotReqs, want) {
		t.Errorf("requests = %+v, want %+v", f.snapshotReqs, want)
	}
	if got := f.vms[10121].Snapshots; !slices.Equal(got, []string{"initial", "before-scoring"}) {
		t.Errorf("snapshots = %v", got)
	}
	if f.called("power:") != 0 {
		t.Errorf("calls = %v: taking a snapshot doesn't change power", f.calls)
	}
	if done := rec.find("team01-teak", EventDone); len(done) != 1 || done[0].Step != StepSnapshot {
		t.Errorf("done events = %+v", done)
	}
}

// A snapshot of the name that appeared after the plan was made (someone
// took one in Proxmox) is never replaced: the item fails and nothing is
// sent.
func TestTakeSnapshotNeverReplacesOne(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial")
	plan := snapshotPlan(t, f, SnapshotRequest{Name: "before-scoring"})
	f.vms[10121].Snapshots = append(f.vms[10121].Snapshots, "before-scoring")

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	err := res.Failed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), `already has a snapshot named "before-scoring", taken since the preview; battleship never replaces a snapshot`) {
		t.Errorf("error = %v", err)
	}
	if n := f.called("snapshot:"); n != 0 {
		t.Errorf("snapshot POSTed %d times, want 0", n)
	}
}

// A request whose answer was lost took the snapshot: the retry finds it
// and doesn't send it again, and the item succeeds.
func TestTakeSnapshotLostAnswerIsNotResent(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
	plan := snapshotPlan(t, f, SnapshotRequest{Name: "before-scoring"})
	f.snapshotThenFail = map[int]error{10121: &proxmox.APIError{Status: 500, Message: "Connection timed out"}}
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), plan)
	if len(res.Failed) != 0 || !slices.Equal(res.Succeeded, []string{"team01-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("snapshot:"); n != 1 {
		t.Errorf("snapshot POSTed %d times, want 1", n)
	}
	for _, e := range rec.find("team01-teak", EventFailed) {
		t.Errorf("unexpected failure event: %+v", e)
	}
}

// The same holds across retry rounds: a round whose request's answer was
// lost for good fails the item, and the next round finds the snapshot
// this run took and accepts it rather than calling it someone else's.
func TestTakeSnapshotRetryRoundAcceptsItsOwnSnapshot(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
	plan := snapshotPlan(t, f, SnapshotRequest{Name: "before-scoring"})
	f.snapshotThenFail = map[int]error{10121: &proxmox.APIError{Status: 400, Message: "bad gateway, permanently"}}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 || !slices.Equal(res.Succeeded, []string{"team01-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("snapshot:"); n != 1 {
		t.Errorf("snapshot POSTed %d times, want 1", n)
	}
}

// A task that ended OK is checked: the snapshot must be there, finished.
func TestTakeSnapshotIsVerified(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
	f.add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil)
	plan := snapshotPlan(t, f, SnapshotRequest{Name: "before-scoring"})
	f.snapshotNoop = map[int]bool{10121: true}
	f.snapshotLeaves = map[int]string{10125: "prepare"}

	exec := testExecutor(f, &recorder{})
	exec.Cfg.Retry.Rounds = 0
	res := exec.Run(context.Background(), plan)
	if err := res.Failed["team01-teak"]; err == nil || !strings.Contains(err.Error(), `the snapshot task ended, but team01-teak has no snapshot "before-scoring"`) {
		t.Errorf("teak error = %v", err)
	}
	if err := res.Failed["team01-oak"]; err == nil || !strings.Contains(err.Error(), `snapshot "before-scoring" of team01-oak was left unfinished (snapstate prepare); delete it in Proxmox`) {
		t.Errorf("oak error = %v", err)
	}
}

// Deploy's baseline follows the same rule: a request whose answer was lost
// took the baseline, so the retry finds it rather than POSTing again and
// failing "already used".
func TestBaselineLostAnswerIsNotResent(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.snapshotThenFail = map[int]error{10121: &proxmox.APIError{Status: 500, Message: "Connection timed out"}}

	exec := testExecutor(f, &recorder{})
	exec.Cfg.Retry.Rounds = 0
	res := exec.Run(context.Background(), plan)
	if len(res.Failed) != 0 || !slices.Equal(res.Succeeded, []string{"team01-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("snapshot:10121"); n != 1 {
		t.Errorf("baseline POSTed %d times, want 1", n)
	}
}
