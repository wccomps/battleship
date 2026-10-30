package status

import (
	"reflect"
	"testing"
)

// A snapshot whose task died (snapstate set) can't be rolled back to: the
// scan and Detail treat it as no baseline, and Detail doesn't list it.
func TestUnfinishedSnapshotIsNoBaseline(t *testing.T) {
	h := newHarness(t, nil)
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial", "before-scoring")
	h.api.Mu.Lock()
	h.api.VMs[10101].SnapStates = map[string]string{"initial": "prepare"}
	h.api.Mu.Unlock()
	h.poll(t)
	h.scan(t)
	want := []Drift{{Kind: DriftSnapshot, Reason: `no "initial" baseline snapshot`}}
	if c := cell(t, h.p.Grid(), "01", "dc"); !reflect.DeepEqual(c.Drift, want) {
		t.Errorf("scan drift = %+v, want %+v", c.Drift, want)
	}
	d, err := h.p.Detail(ctx, "01", "dc")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Snapshots, []string{"before-scoring"}) || !reflect.DeepEqual(d.Cell.Drift, want) {
		t.Errorf("detail snapshots %v drift %+v", d.Snapshots, d.Cell.Drift)
	}
}

// Proxmox leaves pool out of /cluster/resources when the token can't audit
// pools, so an empty pool is unknown, not wrong; a different one is drift.
func TestPoolDriftOnlyWhenKnown(t *testing.T) {
	h := newHarness(t, nil)
	unknown := teamVM("01", "dc", 10101)
	unknown.Pool = ""
	wrong := teamVM("02", "dc", 10201)
	wrong.Pool = "pool-01"
	h.api.add(unknown, cleanConfig("01"), "initial")
	h.api.add(wrong, cleanConfig("02"), "initial")
	h.poll(t)
	g := h.p.Grid()
	if c := cell(t, g, "01", "dc"); c.State != StateRunning || c.Drift != nil {
		t.Errorf("unknown pool: %s %+v, want running with no drift", c.State, c.Drift)
	}
	want := []Drift{{Kind: DriftPool, Reason: `in pool "pool-01", expected "pool-02"`}}
	if c := cell(t, g, "02", "dc"); !reflect.DeepEqual(c.Drift, want) {
		t.Errorf("wrong pool drift = %+v, want %+v", c.Drift, want)
	}
}
