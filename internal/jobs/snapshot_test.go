package jobs

import (
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func TestValidateSnapshot(t *testing.T) {
	cases := []struct {
		in   Inputs
		want string // "" means valid
	}{
		{Inputs{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "before-scoring"}, ""},
		{Inputs{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "ok2", Description: "round 2", VMState: true}, ""},
		{Inputs{Kind: pods.KindSnapshot, Teams: "1"}, "a snapshot needs a name"},
		{Inputs{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "2nd"}, "must start with a letter"},
		{Inputs{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "current"}, "reserved"},
		{Inputs{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "ok2", Description: strings.Repeat("x", MaxSnapshotDescription+1)},
			"description is too long"},
		{Inputs{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "ok2", Description: strings.Repeat("é", MaxSnapshotDescription)}, ""},
	}
	for _, tc := range cases {
		err := tc.in.Validate()
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("Validate(%+v) = %v, want %q", tc.in, err, tc.want)
		}
	}
}

func TestBuildPlanSnapshot(t *testing.T) {
	f := newFake(
		proxmox.VM{VMID: 10105, Name: "team01-dc", Node: "n1", Status: "running"},
		proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "n1", Status: "running"},
	)
	cfg := testCfg()
	in := Inputs{Kind: pods.KindSnapshot, Teams: "1", VMs: []string{"team01-dc"}, Snapshot: "before-scoring", Description: "round 2", VMState: true}
	plan, err := BuildPlan(bg, pods.NewPlanner(f, cfg), in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != pods.KindSnapshot || len(plan.Items) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	it := plan.Items[0]
	if it.Name != "team01-dc" || it.Snapshot != "before-scoring" || it.Description != "round 2" || !it.VMState || it.Blocked != "" {
		t.Errorf("item = %+v", it)
	}
	in.Snapshot = "initial" // the fake's VMs have it, and it is the baseline
	if _, err := BuildPlan(bg, pods.NewPlanner(f, cfg), in); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Errorf("BuildPlan(initial) = %v, want the baseline refused", err)
	}
}

// A snapshot job's fingerprint covers what it writes: the name, the
// description and the RAM flag.
func TestFingerprintCoversSnapshotInputs(t *testing.T) {
	base := pods.Item{Name: "team01-dc", VMID: 10105, Steps: []pods.Step{pods.StepSnapshot}, Snapshot: "aa"}
	fp := func(it pods.Item) string {
		return Fingerprint(&pods.Plan{Kind: pods.KindSnapshot, Teams: []string{"01"}, Items: []pods.Item{it}})
	}
	seen := map[string]string{fp(base): "base"}
	for what, it := range map[string]pods.Item{
		"name":        {Name: base.Name, VMID: base.VMID, Steps: base.Steps, Snapshot: "bb"},
		"description": {Name: base.Name, VMID: base.VMID, Steps: base.Steps, Snapshot: "aa", Description: "x"},
		"vmstate":     {Name: base.Name, VMID: base.VMID, Steps: base.Steps, Snapshot: "aa", VMState: true},
	} {
		got := fp(it)
		if other, dup := seen[got]; dup {
			t.Errorf("changing the %s leaves the fingerprint as for %s", what, other)
		}
		seen[got] = what
	}
}

// Plans of the other kinds keep the fingerprints they had, so jobs queued
// before snapshots existed don't go stale.
func TestFingerprintOfOtherKindsUnchanged(t *testing.T) {
	p := &pods.Plan{Kind: pods.KindReset, Teams: []string{"01"}, Items: []pods.Item{{Name: "team01-dc", VMID: 10101,
		Steps: []pods.Step{pods.StepStop, pods.StepRollback, pods.StepStart}, Snapshot: "initial"}}}
	if got, want := Fingerprint(p), "9abe0dd5bde6bb7281302f2bf6d80c049f128167d6c8082b117bff293280ccb7"; got != want {
		t.Errorf("reset fingerprint = %s, want %s as before", got, want)
	}
}

// A snapshot job waits for, and holds up, any job on the same teams.
func TestSnapshotLockKeysOverlapOtherJobs(t *testing.T) {
	snap := LockKeys(&pods.Plan{Kind: pods.KindSnapshot, Teams: []string{"01", "02"}})
	for _, other := range []*pods.Plan{
		{Kind: pods.KindPower, Teams: []string{"02"}},
		{Kind: pods.KindReset, Teams: []string{"01"}},
		{Kind: pods.KindDeploy, Teams: []string{"01"}, Templates: []pods.TemplateSpec{{Name: "dc.x.tpl", VMID: 9001}}},
		{Kind: pods.KindSnapshot, Teams: []string{"02", "03"}},
	} {
		if !slices.ContainsFunc(LockKeys(other), func(k string) bool { return slices.Contains(snap, k) }) {
			t.Errorf("snapshot keys %v share none with %s keys %v", snap, other.Kind, LockKeys(other))
		}
	}
	if keys := LockKeys(&pods.Plan{Kind: pods.KindPower, Teams: []string{"03"}}); slices.ContainsFunc(keys, func(k string) bool { return slices.Contains(snap, k) }) {
		t.Errorf("snapshot of 01-02 conflicts with power on 03: %v", keys)
	}
}

// A snapshot job takes its snapshot; the same job confirmed twice runs
// once: the second finds the VMs have it, so its plan no longer matches
// and it ends stale without touching them.
func TestWorkerRunsSnapshotJobOnce(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	in := Inputs{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "before-scoring", Description: "round 2"}
	first := submit(t, st, f, in)
	second := submit(t, st, f, in)

	w := newWorker(st, f)
	w.RunJob(bg, claim(t, st))
	if j := job(t, st, first); j.Status != store.StatusSucceeded {
		t.Fatalf("first job = %s (%s)", j.Status, j.Error)
	}
	items, _ := st.Items(bg, first)
	for _, it := range items {
		if it.Status != store.ItemDone || it.Step != string(pods.StepSnapshot) {
			t.Errorf("item = %+v", it)
		}
	}
	w.RunJob(bg, claim(t, st))
	if j := job(t, st, second); j.Status != store.StatusStale {
		t.Errorf("second job = %s (%s), want stale", j.Status, j.Error)
	}
	for _, vmid := range []int{10105, 10121} {
		if got := f.taken(vmid); !slices.Equal(got, []string{"before-scoring"}) {
			t.Errorf("VM %d snapshots taken = %v, want one", vmid, got)
		}
	}
}
