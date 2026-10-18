package status

import (
	"reflect"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
)

// A VM with several set tags or a bare "set-" tag is drifted: no teardown
// of one set takes it. One with none, as every VM deployed before sets,
// isn't: the cell only records that it has no set.
func TestSetTagsDrift(t *testing.T) {
	h := newHarness(t, nil)
	for i, tags := range []string{"set-kilo.alpha", "", "dev;set-kilo.alpha,SET-Tango.Delta", "set-kilo.alpha;SET-KILO.ALPHA", "set-"} {
		vm := teamVM("0"+string(rune('1'+i)), "dc", 10101+100*i)
		vm.Tags = tags
		h.api.add(vm, cleanConfig(vm.Name[4:6]), "initial")
	}
	h.poll(t)
	g := h.p.Grid()
	for team, want := range map[string]pods.SetRecord{"01": {"kilo.alpha"}, "02": nil, "04": {"kilo.alpha"}} {
		if c := cell(t, g, team, "dc"); c.State != StateRunning || c.Drift != nil || !reflect.DeepEqual(c.Sets, want) {
			t.Errorf("pod %s: %s, sets %q, drift %+v; want running, sets %q, no drift", team, c.State, c.Sets, c.Drift, want)
		}
	}
	want := []Drift{{Kind: DriftSets,
		Reason: "has several set tags (set-kilo.alpha, set-tango.delta), so no teardown of one set takes it; remove all but its own in Proxmox"}}
	if c := cell(t, g, "03", "dc"); c.State != StateDrifted || !reflect.DeepEqual(c.Drift, want) {
		t.Errorf("several set tags: %s %+v, want drift %+v", c.State, c.Drift, want)
	}
	wantEmpty := []Drift{{Kind: DriftSets,
		Reason: "has an empty set tag (set-), so no teardown of one set takes it; remove it in Proxmox"}}
	if c := cell(t, g, "05", "dc"); c.State != StateDrifted || !reflect.DeepEqual(c.Drift, wantEmpty) || c.Sets.Label() != pods.UnknownSet {
		t.Errorf("empty set tag: %s %+v label=%q, want drift %+v", c.State, c.Drift, c.Sets.Label(), wantEmpty)
	}
}

// Adding or changing a VM's set tag replaces the grid, even when nothing
// else on the cell changes.
func TestSetTagChangeReachesGrid(t *testing.T) {
	h := newHarness(t, nil)
	vm := teamVM("01", "dc", 10101)
	h.api.add(vm, cleanConfig("01"), "initial")
	h.poll(t)
	v1 := h.p.Grid().Version

	h.api.Mu.Lock()
	h.api.VMs[10101].Tags = "set-kilo.alpha"
	h.api.VMs[10101].Config["tags"] = "set-kilo.alpha"
	h.api.Mu.Unlock()
	h.poll(t)

	g := h.p.Grid()
	if g.Version == v1 {
		t.Fatalf("grid version stayed %d after a set tag was added", g.Version)
	}
	if got := cell(t, g, "01", "dc").Sets; !reflect.DeepEqual(got, pods.SetRecord{"kilo.alpha"}) {
		t.Fatalf("cell sets = %q, want [kilo.alpha]", got)
	}
}

// Every field of Cell participates in change detection (sameContent) and is
// copied independently by clone.
func TestSameContentChecksEveryCellField(t *testing.T) {
	base := Grid{
		Hosts: []string{"dc"},
		Rows: []Row{{Team: "01", Cells: []Cell{{
			Team: "01", Host: "dc", Name: "team01-dc", VMID: 10101,
			State: StateRunning, Power: "running", Node: "n1", Pool: "pool-01",
			Sets:  pods.SetRecord{"kilo.alpha"},
			Drift: []Drift{{Kind: DriftNotes, Reason: "has notes"}},
		}}}},
	}
	if !sameContent(base, base.clone()) {
		t.Fatal("clone differs from base")
	}
	rv := reflect.ValueOf(base.Rows[0].Cells[0])
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		other := base.clone()
		cv := reflect.ValueOf(&other.Rows[0].Cells[0]).Elem().Field(i)
		switch f.Type.Kind() {
		case reflect.String:
			cv.SetString(cv.String() + "-changed")
		case reflect.Int:
			cv.SetInt(cv.Int() + 1)
		case reflect.Slice:
			cv.Set(reflect.MakeSlice(f.Type, 0, 0))
		default:
			t.Fatalf("unhandled Cell field %s of kind %s", f.Name, f.Type.Kind())
		}
		if sameContent(base, other) {
			t.Errorf("Cell.%s is ignored by sameContent", f.Name)
		}
	}
}
