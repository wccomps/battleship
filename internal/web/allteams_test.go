package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/proxmox"
)

// withTeam00 is the harness (teams 01-03) with team 00, the test team,
// deployed too: the VMs a deploy over "0-3" leaves.
func withTeam00(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	for j, host := range []string{"dc", "web"} {
		h.api.add(teamVM("00", host, 10001+j), cleanConfig("00"), "initial", "before-scoring")
	}
	h.poll()
	return h
}

// The grid's rows are the teams with VMs: team 00 gets a row like any
// other, whose cells can be ticked and opened; the live grid adds and drops
// it as its VMs come and go.
func TestGridRowsAreTeamsWithVMs(t *testing.T) {
	h := withTeam00(t)
	op := h.login(asOperator)
	body := h.get(&op, "/").Body.String()
	contains(t, "grid with team 00", body,
		`<a class="hb" href="/?select=team:00" data-select="team:00" role="button" aria-pressed="false"><span class="sr">Select team </span>00</a>`,
		`<input type="checkbox" name="vms" value="team00-dc" data-team="00" data-host="dc">`,
		`<a class="hb" href="/?select=team:01" data-select="team:01"`)
	// Four teams are one group: no gap.
	lacks(t, "grid with team 00", body, `class="gr grp"`)
	// Selecting its row works without the script.
	sel := h.get(&op, "/?select=team:00").Body.String()
	contains(t, "team 00 selected", sel, `value="team00-dc" data-team="00" data-host="dc" checked>`, `value="team00-web" data-team="00" data-host="web" checked>`)

	// Its VM's page.
	if rec := h.get(&op, "/vm/00/dc"); rec.Code != http.StatusOK {
		t.Fatalf("GET /vm/00/dc = %d\n%s", rec.Code, rec.Body)
	} else {
		contains(t, "team00-dc's page", rec.Body.String(), "team00-dc", "before-scoring")
	}

	// An operator may act on it from the grid: its team is worked out from
	// the VM like any other's.
	_, fields, preview := h.preview(&op, "/power", selection([]string{"team00-dc"}, "action", "start"))
	if fields.Get("teams") != "0" || fields.Get("vms") != "team00-dc" {
		t.Errorf("confirm fields = %v, want team 0's VM", fields)
	}
	lacks(t, "power preview of team 00", preview, "covers every team")

	// Live: tearing team 00 down drops its row from the stream.
	ev := h.openSSE(&op, "/events/grid")
	first := ev.next()
	contains(t, "first grid event", first.Data, `id="cell-00-dc"`)
	h.api.remove("team00-dc", "team00-web")
	h.poll()
	next := ev.next()
	if next.Event != "grid" {
		t.Fatalf("event after team 00 went = %q, want a whole grid", next.Event)
	}
	lacks(t, "grid event after team 00 went", next.Data, `cell-00-`)
	lacks(t, "grid after team 00 went", h.get(&op, "/").Body.String(), `data-select="team:00"`)
}

// "All teams" is every team with VMs: the forms' hint and the preview's
// note name team 00 while it has VMs, and a teardown that leaves it out is
// not of every team.
func TestAllTeamsIsTeamsWithVMs(t *testing.T) {
	h := withTeam00(t)
	lead := h.login(asLead)
	for _, path := range []string{"/teardown", "/power", "/reset", "/snapshot", "/deploy"} {
		contains(t, path+" form", h.get(&lead, path).Body.String(), `<span class="hint" id="teams-hint">0-3 · 3,7 · 12-14</span>`)
	}

	_, _, body := h.preview(&lead, "/teardown", url.Values{"teams": {"0-3"}})
	contains(t, "teardown of 0-3", body, "This covers every team (00-03).")

	_, _, body = h.preview(&lead, "/teardown", url.Values{"teams": {"1-3"}})
	lacks(t, "teardown of 1-3", body, "This covers every team", "counts as every team")

	// Without team 00's VMs, all teams are 01-03.
	h.api.remove("team00-dc", "team00-web")
	h.poll()
	contains(t, "teardown form", h.get(&lead, "/teardown").Body.String(), `<span class="hint" id="teams-hint">1-3 · 3,7 · 12-14</span>`)
	_, _, body = h.preview(&lead, "/teardown", url.Values{"teams": {"1-3"}})
	contains(t, "teardown of 1-3", body, "This covers every team (01-03).")

	// With no team VMs on the grid, the hint has no teams to name, and any
	// range counts as every team, so the catch asks: here a VM made since
	// the grid's last read.
	h.noTeamVMs()
	h.poll()
	contains(t, "teardown form", h.get(&lead, "/teardown").Body.String(), `<span class="hint" id="teams-hint">3,7 · 12-14</span>`)
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	op := h.login(asOperator)
	_, fields, body := h.preview(&op, "/power", url.Values{"teams": {"1"}, "action": {"start"}})
	contains(t, "power of 1 with no team VMs on the grid", body, "No team has VMs on the grid, so this counts as every team.")
	lacks(t, "power of 1 with no team VMs on the grid", body, `name="typed"`) // only a teardown is typed
	if fields.Get("teams") != "1" {
		t.Errorf("confirm fields = %v", fields)
	}
	h.noJobs("after previews")
}

// A power or snapshot over every team with VMs is flagged as such; one that
// leaves out any of them, team 00 included, isn't. Neither is typed: only a
// teardown is (pods.Kind.TypedConfirm).
func TestAllTeamsRuleCoversEveryTeamWithVMs(t *testing.T) {
	h := withTeam00(t)
	op := h.login(asOperator)
	for _, tc := range []struct {
		teams string
		all   bool
	}{
		{"0-3", true},
		{"0,1,2,3", true},
		{"0-9", true},
		{"1-3", false},
		{"0", false},
		{"0-2", false},
		{"0,3", false},
	} {
		for _, kind := range []string{"power", "snapshot"} {
			form := url.Values{"teams": {tc.teams}, "action": {"start"}}
			if kind == "snapshot" {
				form = url.Values{"teams": {tc.teams}, "snapshot": {"midday"}}
			}
			body := h.post(&op, "/"+kind+"/preview", form).Body.String()
			if got := strings.Contains(body, "This covers every team"); got != tc.all {
				t.Errorf("%s of teams %s says it covers every team: %v, want %v", kind, tc.teams, got, tc.all)
			}
			lacks(t, kind+" of teams "+tc.teams, body, `name="typed"`)
		}
	}
	// From the grid too: every team's VMs, or all but team 00's.
	body := h.post(&op, "/power/preview", selection([]string{"team00-dc", "team01-dc", "team02-dc", "team03-dc"}, "action", "start")).Body.String()
	contains(t, "grid power of teams 0-3", body, "This covers every team")
	body = h.post(&op, "/power/preview", selection([]string{"team01-dc", "team02-dc", "team03-dc"}, "action", "start")).Body.String()
	lacks(t, "grid power of teams 1-3 with team 00 deployed", body, "This covers every team")
	h.noJobs("after previews")
}

// A VM's page of any team with VMs offers what any other's does, and a
// missing VM of a team past the others opens too.
func TestTeamWithVMsCellPage(t *testing.T) {
	h := withTeam00(t)
	op := h.login(asOperator)
	body := h.get(&op, "/vm/00/web").Body.String()
	if !strings.Contains(body, "team00-web") {
		t.Fatalf("team00-web's page:\n%s", body)
	}
	contains(t, "team00-web's page", body, `action="/power/preview"`)
	h.api.add(proxmox.VM{VMID: 14001, Name: "team40-dc", Node: "n1", Status: "stopped", Pool: "pool-40"}, cleanConfig("40"), "initial")
	h.poll()
	if rec := h.get(&op, "/vm/40/web"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "missing") {
		t.Errorf("GET /vm/40/web (missing) = %d", rec.Code)
	}
}
