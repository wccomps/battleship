package status

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// dbHarness is a poller over a fake cluster and a real job history.
type dbHarness struct {
	*harness
	st *store.Store
}

func newDBHarness(t *testing.T) *dbHarness {
	t.Helper()
	st := storetest.New(t)
	h := newHarness(t, nil)
	p, err := NewPoller(h.api, st, h.lim, h.cfg, Options{Clock: h.clock, Hub: h.hub, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	h.p = p
	return &dbHarness{harness: h, st: st}
}

// startJob creates a kind job over the named VMs, claims it and starts a
// step on each.
func (h *dbHarness) startJob(t *testing.T, kind string, names ...string) int64 {
	t.Helper()
	var items []store.NewItem
	for i, n := range names {
		items = append(items, store.NewItem{Name: n, Team: n[4:6], VMID: 10101 + i})
	}
	id, err := h.st.CreateJob(ctx, store.NewJob{
		Kind: kind, Inputs: json.RawMessage(`{}`), Plan: json.RawMessage(`{"kind":"` + kind + `"}`),
		Fingerprint: "fp", LockKeys: []string{"team:01"}, CreatedBy: "lead", Items: items,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ClaimJob(ctx, id, "w"); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := h.st.AddEvent(ctx, id, store.Event{At: time.Now(), Item: n, Step: "network", Status: "started"}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (h *dbHarness) finish(t *testing.T, id int64, status string, items map[string]store.ItemOutcome) {
	t.Helper()
	if err := h.st.Finish(ctx, id, "w", store.Outcome{Status: status, Items: items}); err != nil {
		t.Fatal(err)
	}
}

func done(names ...string) map[string]store.ItemOutcome {
	out := map[string]store.ItemOutcome{}
	for _, n := range names {
		out[n] = store.ItemOutcome{Status: store.ItemDone}
	}
	return out
}

// A scan that read a VM mid-deploy is dated by that deploy even when a power
// job finished after it.
func TestConvergeDeployThenPowerHidesMidDeployScan(t *testing.T) {
	h := newDBHarness(t)
	driftCluster(h.harness)
	h.poll(t)
	dep := h.startJob(t, string(pods.KindDeploy), "team01-web")
	h.scan(t) // reads team01-web's half-converged network
	if c := cell(t, h.p.Grid(), "01", "web"); !reflect.DeepEqual(kinds(c), []DriftKind{DriftNetwork}) {
		t.Fatalf("01/web mid-deploy drift %v, want network", kinds(c))
	}
	h.finish(t, dep, store.StatusSucceeded, done("team01-web"))
	pw := h.startJob(t, string(pods.KindPower), "team01-web")
	h.finish(t, pw, store.StatusSucceeded, done("team01-web"))
	h.poll(t)
	if c := cell(t, h.p.Grid(), "01", "web"); c.Drift != nil {
		t.Errorf("01/web after deploy then power: drift %+v, want none (the deploy dates the scan)", c.Drift)
	}
}

// A successful power job doesn't clear a failed deploy.
func TestConvergeFailedDeployThenPowerStaysDrifted(t *testing.T) {
	h := newDBHarness(t)
	driftCluster(h.harness)
	dep := h.startJob(t, string(pods.KindDeploy), "team01-dc")
	h.finish(t, dep, store.StatusCompletedWithFailures,
		map[string]store.ItemOutcome{"team01-dc": {Status: store.ItemFailed, Error: "network: bridge missing"}})
	pw := h.startJob(t, string(pods.KindPower), "team01-dc")
	h.finish(t, pw, store.StatusSucceeded, done("team01-dc"))
	h.poll(t)
	c := cell(t, h.p.Grid(), "01", "dc")
	if c.State != StateDrifted || len(c.Drift) != 1 || c.Drift[0].Kind != DriftJob || c.Drift[0].JobID != dep {
		t.Errorf("01/dc after a failed deploy then power = %s %+v, want job drift from deploy %d", c.State, c.Drift, dep)
	}

	// A successful reset converges it again.
	rst := h.startJob(t, string(pods.KindReset), "team01-dc")
	h.finish(t, rst, store.StatusSucceeded, done("team01-dc"))
	h.poll(t)
	if c := cell(t, h.p.Grid(), "01", "dc"); c.State != StateRunning || c.Drift != nil {
		t.Errorf("01/dc after a reset = %s %+v, want running", c.State, c.Drift)
	}
}

// A VM a teardown failed on is left drifted, and a teardown that did remove a
// VM clears an older failed deploy (here the VM's name is back, as if made
// again outside battleship). A teardown doesn't date a scan: only deploys and
// resets re-converge a VM's config.
func TestConvergeTeardownOutcomes(t *testing.T) {
	h := newDBHarness(t)
	driftCluster(h.harness)
	dep := h.startJob(t, string(pods.KindDeploy), "team01-dc")
	h.finish(t, dep, store.StatusCompletedWithFailures,
		map[string]store.ItemOutcome{"team01-dc": {Status: store.ItemFailed, Error: "network: bridge missing"}})
	h.poll(t)
	h.scan(t) // reads team01-web's drifted network
	td := h.startJob(t, string(pods.KindTeardown), "team01-dc", "team01-web")
	h.finish(t, td, store.StatusCompletedWithFailures, map[string]store.ItemOutcome{
		"team01-dc":  {Status: store.ItemDone},
		"team01-web": {Status: store.ItemFailed, Error: "destroy: locked"},
	})
	h.poll(t)
	if c := cell(t, h.p.Grid(), "01", "dc"); c.State != StateRunning || c.Drift != nil {
		t.Errorf("01/dc after a teardown removed it = %s %+v, want running with no drift", c.State, c.Drift)
	}
	c := cell(t, h.p.Grid(), "01", "web")
	if c.State != StateDrifted || !reflect.DeepEqual(kinds(c), []DriftKind{DriftJob, DriftNetwork}) || c.Drift[0].JobID != td {
		t.Errorf("01/web after a failed teardown = %s %+v, want job drift from teardown %d plus the scan's network drift", c.State, c.Drift, td)
	}
}
