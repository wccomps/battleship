package status

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

var ctx = context.Background()

type harness struct {
	api   *fakeAPI
	hist  *fakeHistory
	clock *fakeClock
	hub   *Hub
	lim   *apply.Limits
	cfg   config.Config
	p     *Poller
}

// newHarness builds a poller over a fake cluster. edit, if set, changes the
// config first.
func newHarness(t *testing.T, edit func(*config.Config, *Options)) *harness {
	t.Helper()
	h := &harness{api: newFakeAPI(), clock: newFakeClock(), cfg: testConfig()}
	h.hist = &fakeHistory{clock: h.clock}
	h.hub = NewHub(nil, HubOptions{Clock: h.clock, Logf: t.Logf})
	opts := Options{Clock: h.clock, Hub: h.hub, Logf: t.Logf}
	if edit != nil {
		edit(&h.cfg, &opts)
	}
	h.lim = apply.NewLimits(h.cfg.Concurrency)
	p, err := NewPoller(h.api, h.hist, h.lim, h.cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	h.p = p
	return h
}

func (h *harness) poll(t *testing.T) {
	t.Helper()
	if err := h.p.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
}

func (h *harness) scan(t *testing.T) {
	t.Helper()
	if err := h.p.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
}

func cell(t *testing.T, g Grid, team, host string) Cell {
	t.Helper()
	c, ok := g.Cell(team, host)
	if !ok {
		t.Fatalf("no cell %s/%s in grid with teams %v hosts %v", team, host, g.Teams, g.Hosts)
	}
	return c
}

func kinds(c Cell) []DriftKind {
	var out []DriftKind
	for _, d := range c.Drift {
		out = append(out, d.Kind)
	}
	return out
}

func TestGridBeforeFirstPoll(t *testing.T) {
	h := newHarness(t, nil)
	g := h.p.Grid()
	if !g.Stale || g.Err == "" || g.Version != 0 || len(g.Teams) != 0 || len(g.Hosts) != 0 {
		t.Errorf("grid before the first poll = %+v, want stale with no teams and no hosts", g)
	}
	if !h.p.Grid().PolledAt.IsZero() {
		t.Errorf("LastOK before a poll = %v, want zero", h.p.Grid().PolledAt)
	}
}

func TestGridStates(t *testing.T) {
	h := newHarness(t, nil)
	stopped := teamVM("01", "web", 10102)
	stopped.Status = "stopped"
	wrongPool := teamVM("02", "dc", 10201)
	wrongPool.Pool = "pool-01"
	noPool := teamVM("03", "web", 10302)
	noPool.Pool = ""
	for _, vm := range []proxmox.VM{
		teamVM("01", "dc", 10101),
		stopped,
		wrongPool,
		teamVM("03", "dc", 10399), // two VMs share a name
		teamVM("03", "dc", 10301),
		noPool,
		teamVM("04", "mail", 10401),
		{VMID: 9005, Name: "dc.kilo.alpha.tpl", Node: "n1", Template: true, Tags: "dev"},
		{VMID: 105, Name: "ftp.kilo.alpha", Node: "n2", Status: "running", Tags: "dev"}, // a master, but web.templates is empty: no column
		{VMID: 106, Name: "zzz.scratch", Node: "n2", Tags: "other"},                     // untagged: not a master
		{VMID: 10150, Name: "team01-old", Node: "n1", Template: true},                   // a template, not a team VM
	} {
		h.api.add(vm, cleanConfig("01"), "initial")
	}
	h.poll(t)
	g := h.p.Grid()

	if want := []string{"dc", "mail", "web"}; !reflect.DeepEqual(g.Hosts, want) {
		t.Errorf("hosts = %v, want %v", g.Hosts, want)
	}
	if g.Stale || g.Err != "" || g.Version != 1 || !g.PolledAt.Equal(h.clock.Now()) || !h.p.Grid().PolledAt.Equal(h.clock.Now()) {
		t.Errorf("grid = stale %v err %q version %d polled %v; want fresh version 1 polled now", g.Stale, g.Err, g.Version, g.PolledAt)
	}
	if len(g.Rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(g.Rows))
	}
	for i, row := range g.Rows {
		if row.Team != g.Teams[i] || len(row.Cells) != len(g.Hosts) {
			t.Errorf("row %d = team %s with %d cells", i, row.Team, len(row.Cells))
		}
		for j, c := range row.Cells {
			if c.Team != row.Team || c.Host != g.Hosts[j] {
				t.Errorf("row %s cell %d is %s/%s", row.Team, j, c.Team, c.Host)
			}
		}
	}

	tests := []struct {
		team, host string
		want       Cell
	}{
		{"01", "dc", Cell{Team: "01", Host: "dc", Name: "team01-dc", State: StateRunning, Power: "running", VMID: 10101, Node: "n1", Pool: "pool-01"}},
		{"01", "web", Cell{Team: "01", Host: "web", Name: "team01-web", State: StateStopped, Power: "stopped", VMID: 10102, Node: "n1", Pool: "pool-01"}},
		{"02", "web", Cell{Team: "02", Host: "web", Name: "team02-web", State: StateMissing}},
		{"02", "dc", Cell{Team: "02", Host: "dc", Name: "team02-dc", State: StateDrifted, Power: "running", VMID: 10201, Node: "n1", Pool: "pool-01",
			Drift: []Drift{{Kind: DriftPool, Reason: `in pool "pool-01", expected "pool-02"`}}}},
		{"03", "dc", Cell{Team: "03", Host: "dc", Name: "team03-dc", State: StateDrifted, Power: "running", VMID: 10301, Node: "n1", Pool: "pool-03",
			Drift: []Drift{{Kind: DriftDuplicate, Reason: "several VMs are named team03-dc (VMIDs 10301, 10399); remove the extras"}}}},
		// No pool means the token can't read pools: unknown, not drift.
		{"03", "web", Cell{Team: "03", Host: "web", Name: "team03-web", State: StateRunning, Power: "running", VMID: 10302, Node: "n1"}},
	}
	for _, tt := range tests {
		if got := cell(t, g, tt.team, tt.host); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("cell %s/%s =\n %+v\nwant\n %+v", tt.team, tt.host, got, tt.want)
		}
	}
	if _, ok := g.Cell("04", "mail"); !ok {
		t.Error("team 04 has VMs, and no cell")
	}
	if _, ok := g.Cell("01", "ftp"); ok {
		t.Error("host ftp is only a master's, with web.templates empty, but has a column")
	}
}

// Rows are exactly the teams with team VMs; a team's row goes with its VMs.
func TestGridRowsAreTeamsWithVMs(t *testing.T) {
	h := newHarness(t, nil)
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	h.api.add(teamVM("00", "dc", 10001), cleanConfig("01"), "initial") // wired as team 01's: network drift
	h.api.add(teamVM("00", "mail", 10002), cleanConfig("00"), "initial")
	h.api.add(teamVM("40", "dc", 14001), cleanConfig("40"), "initial")
	h.api.add(proxmox.VM{VMID: 10701, Name: "team07-dc", Node: "n1", Template: true}, nil)   // a template: no row
	h.api.add(proxmox.VM{VMID: 10501, Name: "team5-dc", Node: "n1", Status: "running"}, nil) // not a team VM name: no row
	h.hist.set("team00-mail", store.ItemResult{JobID: 9, JobKind: "teardown", FinishedAt: h.clock.Now(), Status: store.ItemFailed, Step: "delete", Error: "locked"})
	h.poll(t)
	g := h.p.Grid()

	if want := []string{"00", "01", "40"}; !reflect.DeepEqual(g.Teams, want) {
		t.Fatalf("teams = %v, want %v", g.Teams, want)
	}
	if want := []string{"dc", "mail"}; !reflect.DeepEqual(g.Hosts, want) {
		t.Errorf("hosts = %v, want %v", g.Hosts, want)
	}
	for i, row := range g.Rows {
		if row.Team != g.Teams[i] || len(row.Cells) != len(g.Hosts) {
			t.Errorf("row %d = team %s with %d cells, want team %s", i, row.Team, len(row.Cells), g.Teams[i])
		}
	}
	if c := cell(t, g, "00", "dc"); c.State != StateRunning || c.VMID != 10001 {
		t.Errorf("team00-dc = %+v, want running VMID 10001", c)
	}
	if c := cell(t, g, "00", "mail"); c.State != StateDrifted || !reflect.DeepEqual(kinds(c), []DriftKind{DriftJob}) {
		t.Errorf("team00-mail = %+v, want drifted by its failed job", c)
	}
	if c := cell(t, g, "40", "mail"); c.State != StateMissing {
		t.Errorf("team40-mail = %+v, want missing", c)
	}

	// The deep scan reads every team's VMs.
	h.scan(t)
	if c := cell(t, h.p.Grid(), "00", "dc"); !reflect.DeepEqual(kinds(c), []DriftKind{DriftNetwork}) {
		t.Errorf("team00-dc after a scan = %+v, want network drift", c)
	}
	// So does a cell's detail.
	d, err := h.p.Detail(ctx, "40", "dc")
	if err != nil || d.Cell.VMID != 14001 || d.Config == nil {
		t.Errorf("Detail(40, dc) = %+v, %v; want team40-dc read", d, err)
	}

	// Torn down, team 00's row goes; team 40's stays while it has a VM.
	h.api.remove(10001)
	h.api.remove(10002)
	h.poll(t)
	g = h.p.Grid()
	if want := []string{"01", "40"}; !reflect.DeepEqual(g.Teams, want) {
		t.Errorf("teams after team 00's teardown = %v, want %v", g.Teams, want)
	}
	if _, ok := g.Cell("00", "dc"); ok {
		t.Error("team 00 has no VMs left but still has a cell")
	}
	// With no team VMs left, there are no rows.
	h.api.remove(10101)
	h.api.remove(14001)
	h.poll(t)
	if g := h.p.Grid(); len(g.Teams) != 0 || len(g.Rows) != 0 {
		t.Errorf("teams with no VMs = %v, %d rows; want none", g.Teams, len(g.Rows))
	}
}

func TestGridIsACopy(t *testing.T) {
	h := newHarness(t, nil)
	vm := teamVM("01", "dc", 10101)
	vm.Pool = "elsewhere" // pool drift, so the cell has a drift to change
	h.api.add(vm, cleanConfig("01"), "initial")
	h.poll(t)
	g := h.p.Grid()
	g.Teams[0] = "99"
	g.Hosts[0] = "x"
	g.Rows[0].Cells[0].State = StateMissing
	g.Rows[0].Cells[0].Drift[0].Reason = "changed"
	again := h.p.Grid()
	c := cell(t, again, "01", "dc")
	if again.Teams[0] != "01" || c.State != StateDrifted || c.Drift[0].Reason == "changed" {
		t.Errorf("changing a returned grid changed the poller's: %+v", c)
	}
}

func TestGridLastJobDrift(t *testing.T) {
	h := newHarness(t, nil)
	for _, vm := range []proxmox.VM{teamVM("01", "dc", 10101), teamVM("01", "web", 10102), teamVM("02", "dc", 10201), teamVM("02", "web", 10202)} {
		h.api.add(vm, cleanConfig(vm.Name[4:6]), "initial")
	}
	at := h.clock.Now().Add(-time.Minute)
	h.hist.set("team01-dc", store.ItemResult{JobID: 7, JobKind: "deploy", FinishedAt: at, Status: store.ItemFailed, Step: "network", Error: "network: bridge ext01 missing"})
	h.hist.set("team01-web", store.ItemResult{JobID: 8, JobKind: "reset", FinishedAt: at, Status: store.ItemInterrupted, Step: "rollback"})
	// Cut off once only its start was left: its config is as the deploy leaves it.
	h.hist.set("team02-dc", store.ItemResult{JobID: 9, JobKind: "deploy", FinishedAt: at, Status: store.ItemInterrupted, Step: "start", LeftConfig: store.LeftConverged})
	h.hist.set("team03-dc", store.ItemResult{JobID: 10, JobKind: "deploy", FinishedAt: at, Status: store.ItemFailed, Error: "clone: timeout"})
	h.poll(t)
	g := h.p.Grid()

	want := map[string][]Drift{
		"01/dc":  {{Kind: DriftJob, JobID: 7, Reason: "failed in deploy job 7: network: bridge ext01 missing"}},
		"01/web": {{Kind: DriftJob, JobID: 8, Reason: "reset job 8 was interrupted; its last step was rollback"}},
		"02/dc":  nil,
		"02/web": nil,
	}
	for key, drift := range want {
		team, host, _ := strings.Cut(key, "/")
		c := cell(t, g, team, host)
		if !reflect.DeepEqual(c.Drift, drift) {
			t.Errorf("%s drift = %+v, want %+v", key, c.Drift, drift)
		}
		if drift != nil && c.State != StateDrifted {
			t.Errorf("%s state = %s, want drifted", key, c.State)
		}
	}
	if _, ok := g.Cell("03", "dc"); ok {
		t.Error("team 03 has a job's history but no VMs, and a cell")
	}
	asked := h.hist.asked[len(h.hist.asked)-1]
	if want := []string{"team01-dc", "team01-web", "team02-dc", "team02-web"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("history asked for %v, want the team VMs %v", asked, want)
	}
}

func TestPollFailureKeepsStaleGrid(t *testing.T) {
	h := newHarness(t, nil)
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	sub, cancel := h.hub.SubscribeTopics(TopicGrid)
	defer cancel()
	h.poll(t)
	good := h.p.Grid()
	okAt := h.clock.Now()
	recv(t, sub)

	h.clock.Advance(5 * time.Second)
	h.api.setListErr(&proxmox.APIError{Status: 595, Message: "no route to host"})
	h.api.setStatus(10101, "stopped") // not seen: the poll fails
	if err := h.p.Poll(ctx); err == nil {
		t.Fatal("Poll with Proxmox down = nil, want an error")
	}
	g := h.p.Grid()
	if !g.Stale || !strings.Contains(g.Err, "no route to host") {
		t.Errorf("after a failed poll: stale %v err %q, want stale with the reason", g.Stale, g.Err)
	}
	if !reflect.DeepEqual(g.Rows, good.Rows) || !reflect.DeepEqual(g.Hosts, good.Hosts) {
		t.Errorf("a failed poll changed the cells: %+v", g.Rows)
	}
	if !g.PolledAt.Equal(okAt) || !h.p.Grid().PolledAt.Equal(okAt) {
		t.Errorf("PolledAt %v LastOK %v, want the last good poll %v", g.PolledAt, h.p.Grid().PolledAt, okAt)
	}
	if m := recv(t, sub); m.Topic != TopicGrid || g.Version != good.Version+1 {
		t.Errorf("stale grid published %+v (grid version %d), want version %d", m, g.Version, good.Version+1)
	}

	// The job history failing makes the poll fail the same way.
	h.clock.Advance(5 * time.Second)
	h.api.setListErr(nil)
	h.hist.setErr(errors.New("connection refused"))
	if err := h.p.Poll(ctx); err == nil {
		t.Fatal("Poll with the database down = nil, want an error")
	}
	if g := h.p.Grid(); !g.Stale || !strings.Contains(g.Err, "job history") || !strings.Contains(g.Err, "connection refused") {
		t.Errorf("after a failed history read: stale %v err %q", g.Stale, g.Err)
	}

	h.clock.Advance(5 * time.Second)
	h.hist.setErr(nil)
	h.poll(t)
	g = h.p.Grid()
	if g.Stale || g.Err != "" || cell(t, g, "01", "dc").State != StateStopped || !h.p.Grid().PolledAt.Equal(h.clock.Now()) {
		t.Errorf("after recovering: stale %v err %q cell %+v", g.Stale, g.Err, cell(t, g, "01", "dc"))
	}
}

func TestPollPublishesOnlyChanges(t *testing.T) {
	h := newHarness(t, nil)
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	sub, cancel := h.hub.SubscribeTopics(TopicGrid)
	defer cancel()

	h.poll(t)
	if m := recv(t, sub); m.Topic != TopicGrid || m.Resync {
		t.Errorf("first poll published %+v, want a grid message", m)
	}
	h.clock.Advance(5 * time.Second)
	h.poll(t)
	none(t, sub)
	if g := h.p.Grid(); g.Version != 1 || !g.PolledAt.Equal(h.clock.Now()) {
		t.Errorf("unchanged poll: version %d polled %v, want version 1 polled now", g.Version, g.PolledAt)
	}

	h.api.setStatus(10101, "stopped")
	h.poll(t)
	recv(t, sub)
	if g := h.p.Grid(); g.Version != 2 || cell(t, g, "01", "dc").State != StateStopped {
		t.Errorf("grid version %d cell %+v", g.Version, cell(t, g, "01", "dc"))
	}
}

func TestPollerWithoutHub(t *testing.T) {
	h := newHarness(t, func(_ *config.Config, o *Options) { o.Hub = nil })
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	h.poll(t)
	h.scan(t)
	if g := h.p.Grid(); g.Version != 2 {
		t.Errorf("version = %d, want 2", g.Version)
	}
}

// With no team VMs and no web.templates, the grid has no rows or columns,
// however many tagged masters the cluster holds from other template sets.
func TestGridNoTeamVMsNoTemplates(t *testing.T) {
	h := newHarness(t, nil)
	for i, name := range []string{"bugs.looney.tunes", "dc.kilo.alpha", "tern.y"} {
		h.api.add(proxmox.VM{VMID: 500 + i, Name: name, Node: "n1", Status: "running", Tags: "dev"}, nil)
	}
	h.poll(t)
	g := h.p.Grid()
	if len(g.Hosts) != 0 {
		t.Errorf("hosts = %v, want none", g.Hosts)
	}
	if len(g.Rows) != 0 || len(g.Teams) != 0 {
		t.Errorf("rows = %d, teams %v; want none", len(g.Rows), g.Teams)
	}
}

// Masters of other template sets don't add columns next to the team VMs'.
func TestGridIgnoresUnrelatedMasters(t *testing.T) {
	h := newHarness(t, nil)
	for _, vm := range []proxmox.VM{
		teamVM("01", "dc", 10101), teamVM("01", "web", 10102), teamVM("02", "dc", 10201),
		{VMID: 501, Name: "bugs.x", Node: "n1", Status: "running", Tags: "dev"},
		{VMID: 502, Name: "tern.y", Node: "n1", Status: "running", Tags: "dev"},
	} {
		h.api.add(vm, cleanConfig("01"), "initial")
	}
	h.poll(t)
	g := h.p.Grid()
	if want := []string{"dc", "web"}; !reflect.DeepEqual(g.Hosts, want) {
		t.Errorf("hosts = %v, want %v", g.Hosts, want)
	}
	if c := cell(t, g, "02", "web"); c.State != StateMissing {
		t.Errorf("02/web = %s, want missing", c.State)
	}
}

// web.templates names the competition's template set: its masters' hosts
// are columns before any team VM exists, and other sets' masters are not.
func TestGridTemplatesShowsSetHosts(t *testing.T) {
	h := newHarness(t, func(c *config.Config, _ *Options) { c.Web.Templates = "*.kilo.alpha" })
	for i, name := range []string{"dc.kilo.alpha", "web.kilo.alpha", "bugs.looney.tunes"} {
		h.api.add(proxmox.VM{VMID: 500 + i, Name: name, Node: "n1", Status: "running", Tags: "dev"}, nil)
	}
	h.poll(t)
	g := h.p.Grid()
	if want := []string{"dc", "web"}; !reflect.DeepEqual(g.Hosts, want) {
		t.Fatalf("hosts = %v, want %v", g.Hosts, want)
	}
	for _, row := range g.Rows {
		for _, c := range row.Cells {
			if c.State != StateMissing {
				t.Errorf("%s/%s = %s, want missing before a deploy", c.Team, c.Host, c.State)
			}
		}
	}
}

// With web.templates set, drift still judges team VMs, the scan never reads
// masters, and a host column without a team VM is missing (Detail reads
// nothing).
func TestGridTemplatesDriftScanAndDetail(t *testing.T) {
	h := newHarness(t, func(c *config.Config, _ *Options) { c.Web.Templates = "*.kilo.alpha" })
	wrongPool := teamVM("01", "dc", 10101)
	wrongPool.Pool = "pool-02"
	noSnap := teamVM("02", "dc", 10201)
	h.api.add(wrongPool, cleanConfig("01"), "initial")
	h.api.add(noSnap, cleanConfig("02"))
	h.api.add(teamVM("03", "mail", 10301), cleanConfig("03"), "initial") // a host outside the set still shows
	for i, name := range []string{"dc.kilo.alpha", "web.kilo.alpha", "bugs.looney.tunes"} {
		h.api.add(proxmox.VM{VMID: 500 + i, Name: name, Node: "n1", Status: "running", Tags: "dev"}, cleanConfig("01"), "initial")
	}
	h.poll(t)
	h.scan(t)
	g := h.p.Grid()
	if want := []string{"dc", "mail", "web"}; !reflect.DeepEqual(g.Hosts, want) {
		t.Fatalf("hosts = %v, want %v", g.Hosts, want)
	}
	if reads, _, _ := h.api.counts(); reads != 6 {
		t.Errorf("the scan made %d reads, want 6: config and snapshots of the 3 team VMs only", reads)
	}
	if got := kinds(cell(t, g, "01", "dc")); !reflect.DeepEqual(got, []DriftKind{DriftPool}) {
		t.Errorf("01/dc drift = %v, want pool", got)
	}
	if got := kinds(cell(t, g, "02", "dc")); !reflect.DeepEqual(got, []DriftKind{DriftSnapshot}) {
		t.Errorf("02/dc drift = %v, want snapshot", got)
	}
	if c := cell(t, g, "03", "mail"); c.State != StateRunning {
		t.Errorf("03/mail = %s, want running", c.State)
	}

	d, err := h.p.Detail(ctx, "01", "web")
	if err != nil || d.Cell.State != StateMissing || d.Config != nil {
		t.Errorf("Detail(01, web) = %+v, %v; want a missing cell and no read", d, err)
	}
	if _, err := h.p.Detail(ctx, "01", "bugs"); !errors.Is(err, ErrUnknownCell) {
		t.Errorf("Detail(01, bugs) = %v, want ErrUnknownCell: another set's host has no column", err)
	}
	if d, err := h.p.Detail(ctx, "01", "dc"); err != nil || d.Config == nil || !reflect.DeepEqual(kinds(d.Cell), []DriftKind{DriftPool}) {
		t.Errorf("Detail(01, dc) = %+v, %v; want a live read with the pool drift", d, err)
	}
	if reads, _, _ := h.api.counts(); reads != 8 {
		t.Errorf("reads after Detail = %d, want 8: one more config and snapshot read, of team01-dc", reads)
	}
}

// The grid carries the template sets; a change to them bumps the version,
// so per-version caches follow.
func TestGridListsTemplateSets(t *testing.T) {
	h := newHarness(t, nil)
	for i, name := range []string{"web.kilo.alpha", "dc.kilo.alpha", "bugs.looney.tunes", "y"} {
		h.api.add(proxmox.VM{VMID: 500 + i, Name: name, Node: "n1", Status: "stopped", Tags: "dev"}, nil)
	}
	h.api.setStatus(501, "running")
	if g := h.p.Grid(); g.Sets != nil || g.OtherMasters != nil {
		t.Errorf("before the first poll: sets %v, others %v; want none", g.Sets, g.OtherMasters)
	}
	h.poll(t)
	g := h.p.Grid()
	want := []pods.MasterSet{
		{Name: "kilo.alpha", Hosts: []string{"dc", "web"}, Masters: 2, Running: 1},
		{Name: "looney.tunes", Hosts: []string{"bugs"}, Masters: 1},
	}
	if !reflect.DeepEqual(g.Sets, want) || !reflect.DeepEqual(g.OtherMasters, []string{"y"}) {
		t.Fatalf("sets %+v, others %v", g.Sets, g.OtherMasters)
	}
	// Grid hands out copies.
	g.Sets[0].Hosts[0] = "changed"
	if h.p.Grid().Sets[0].Hosts[0] != "dc" {
		t.Error("changing a Grid's sets changed the poller's")
	}

	h.poll(t)
	if v := h.p.Grid().Version; v != g.Version {
		t.Errorf("an unchanged poll moved the version from %d to %d", g.Version, v)
	}
	h.api.setStatus(500, "running") // web.kilo.alpha
	h.poll(t)
	if g2 := h.p.Grid(); g2.Version == g.Version || g2.Sets[0].Running != 2 {
		t.Errorf("after a master started: version %d (was %d), sets %+v", g2.Version, g.Version, g2.Sets)
	}
}

// Polling failure and recovery are each logged once; the first good poll
// isn't a recovery.
func TestPollLogsFailureAndRecoveryOnce(t *testing.T) {
	var logs logLines
	h := newHarness(t, func(_ *config.Config, o *Options) { o.Logf = logs.Logf })
	h.api.setListErr(errors.New("connection refused"))
	for range 2 {
		_ = h.p.Poll(ctx)
	}
	h.api.setListErr(nil)
	h.poll(t)
	h.poll(t)
	want := "status: polling failed: reading the cluster from Proxmox: connection refused; the grid is stale (no good poll yet)\n" +
		"status: polling the cluster works again"
	if got := logs.String(); got != want {
		t.Errorf("logs:\n%s\nwant:\n%s", got, want)
	}

	var fresh logLines
	h = newHarness(t, func(_ *config.Config, o *Options) { o.Logf = fresh.Logf })
	h.poll(t)
	if got := fresh.String(); got != "" {
		t.Errorf("a first good poll logged %q", got)
	}
}
