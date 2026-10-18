package jobs

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// twoSets is two pods, each with a VM of kilo.alpha and one of tango.delta.
func twoSets() pods.Planner {
	return pods.NewPlanner(newFake(
		proxmox.VM{VMID: 10101, Name: "team01-dc", Node: "n1", Status: "running", Tags: "set-kilo.alpha"},
		proxmox.VM{VMID: 10102, Name: "team01-web", Node: "n1", Status: "running", Tags: "set-tango.delta"},
		proxmox.VM{VMID: 10201, Name: "team02-dc", Node: "n1", Status: "running", Tags: "set-kilo.alpha"},
		proxmox.VM{VMID: 10202, Name: "team02-web", Node: "n1", Status: "running", Tags: "set-tango.delta"},
	), testCfg())
}

// A teardown stored before sets has no set: it still tears down
// everything in its pods. One with a set takes only that set's VMs.
func TestTeardownInputsWithAndWithoutASet(t *testing.T) {
	var old Inputs
	if err := json.Unmarshal([]byte(`{"kind":"teardown","teams":"1-2"}`), &old); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(bg, twoSets(), old)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := itemNames(plan), []string{"team01-dc", "team01-web", "team02-dc", "team02-web"}; !slices.Equal(got, want) {
		t.Errorf("an old teardown plans %v, want %v", got, want)
	}
	set := old
	set.Set = "Kilo.Alpha"
	plan, err = BuildPlan(bg, twoSets(), set)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := itemNames(plan), []string{"team01-dc", "team02-dc"}; !slices.Equal(got, want) {
		t.Errorf("a teardown of kilo.alpha plans %v, want %v", got, want)
	}
	raw, _ := json.Marshal(set)
	if string(raw) != `{"kind":"teardown","teams":"1-2","set":"Kilo.Alpha"}` {
		t.Errorf("stored as %s", raw)
	}
	for _, bad := range []Inputs{
		{Kind: pods.KindTeardown, Teams: "1-2", Set: "*.kilo.alpha"},
		{Kind: pods.KindTeardown, Teams: "1-2", Set: "kilo alpha"},
		{Kind: pods.KindTeardown, Teams: "1-2", Set: "\xff"},
	} {
		if _, err := BuildPlan(bg, twoSets(), bad); err == nil {
			t.Errorf("%+v planned", bad)
		}
	}
}

// A retry of a set's teardown stays within the set.
func TestRetryOfASetTeardown(t *testing.T) {
	in := Inputs{Kind: pods.KindTeardown, Teams: "1-2", Set: "kilo.alpha"}
	plan, err := BuildPlan(bg, twoSets(), in)
	if err != nil {
		t.Fatal(err)
	}
	job := finishedJob(t, store.StatusCompletedWithFailures, in, *plan)
	retry, err := RetryInputs(job, []store.Item{
		{Name: "team01-dc", Team: "01", Status: store.ItemDone},
		{Name: "team02-dc", Team: "02", Status: store.ItemFailed, Error: "delete: boom"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if retry.Set != "kilo.alpha" || retry.Teams != "2" {
		t.Errorf("retry = %+v", retry)
	}
	again, err := BuildPlan(bg, twoSets(), retry)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(again); !slices.Equal(got, []string{"team02-dc"}) {
		t.Errorf("the retry plans %v", got)
	}
}
