package status

import (
	"reflect"
	"testing"
)

// A fresh_clone_<timestamp> baseline (no "initial") counts as a baseline:
// no drift, and Detail names it.
func TestPatternBaselineIsNotDrift(t *testing.T) {
	h := newHarness(t, nil)
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "fresh_clone_20261002034615")
	h.poll(t)
	h.scan(t)
	if c := cell(t, h.p.Grid(), "01", "dc"); c.State != StateRunning || c.Drift != nil {
		t.Errorf("cell = %s %+v, want running with no drift", c.State, c.Drift)
	}
	d, err := h.p.Detail(ctx, "01", "dc")
	if err != nil {
		t.Fatal(err)
	}
	if d.Baseline != "fresh_clone_20261002034615" || d.Cell.Drift != nil {
		t.Errorf("detail baseline %q drift %+v", d.Baseline, d.Cell.Drift)
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
