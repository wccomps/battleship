package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// Power on every team says so, a safety catch, but is confirmed with the
// button like the CLI's yes (only a teardown is typed); who may do it is
// Proxmox's business.
func TestAllTeamsPowerSaysSo(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	_, fields, body := h.preview(&op, "/power", selection([]string{"team01-dc", "team02-dc", "team03-dc"}, "action", "start"))
	contains(t, "all-teams power", body, "This covers every team (01-03).")
	lacks(t, "all-teams power", body, `name="typed"`)
	if fields.Get("teams") != "1-3" {
		t.Errorf("confirm fields = %v", fields)
	}
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	lacks(t, "app.js", string(js), "lead", "untick a team")
	contains(t, "app.js", string(js), "data-needs", "data-lacks")
}

// A reset from the grid stays a grid reset: the snapshot picker, the
// preview and every preview shown again by the confirm keep from=grid, so
// changing it goes back to the grid (or the picker), not to the reset form.
func TestGridResetKeepsFromGrid(t *testing.T) {
	h := newHarness(t, longSessions)
	h.poll()
	op := h.login(asOperator)
	fromGrid := `<input type="hidden" name="from" value="grid">`
	back := `<a class="btn" href="/" data-close>Cancel</a>`

	picker := h.panelPost(&op, "/reset", selection([]string{"team02-dc"})).Body.String()
	contains(t, "picker", picker, `<form method="post" action="/reset/preview" class="stack" id="reset-pick">`, fromGrid,
		`<span class="hint">The snapshots of <span class="name">team02-dc</span>.</span>`)

	action, form, body := h.preview(&op, "/reset", url.Values{
		"from": {"grid"}, "teams": {"2"}, "hosts": {"dc"}, "vms": {"team02-dc"}, "snapshot": {"initial"}})
	contains(t, "grid reset preview", body, back,
		`<button type="submit" class="btn first" formaction="/reset" formnovalidate>Change snapshot</button>`)
	lacks(t, "grid reset preview", body, `href="/reset?`)
	if form.Get("from") != "grid" {
		t.Errorf("confirm fields = %v, want from=grid", form)
	}

	h.clock.Advance(previewTTL)
	rec := h.post(&op, action, form)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expired confirm = %d, want 409", rec.Code)
	}
	contains(t, "expired grid reset", rec.Body.String(), "This preview had expired", back, fromGrid)
	h.noJobs("after an expired preview")

	h.noJobs("after an expired preview")
}

// At competition scale with every VM busy, the whole grid still fits the
// fragment budget.
func TestGridFragmentSizeAllBusy(t *testing.T) {
	h := newHarness(t)
	busy := map[string]int64{}
	for team := 1; team <= 32; team++ {
		for host := range 10 {
			tm := fmt.Sprintf("%02d", team)
			vm := teamVM(tm, fmt.Sprintf("host%d", host), 10000+team*100+host+10)
			h.api.add(vm, cleanConfig(tm))
			busy[vm.Name] = 12345
		}
	}
	h.poll()
	g := h.poller.Grid()
	if len(g.Hosts) != 12 || len(g.Rows) != 32 {
		t.Fatalf("grid is %d×%d, want 32×12", len(g.Rows), len(g.Hosts))
	}
	for _, row := range g.Rows {
		for _, c := range row.Cells {
			busy[c.Name] = 12345
		}
	}
	v := h.srv.newGridView(g, h.clock.Now(), nil)
	v.markBusy(busy)
	frag, err := gridFragment(v)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(frag, `<a class="cell is-busy"`); n < 32*12 {
		t.Errorf("fragment has %d busy marks, want %d", n, 32*12)
	}
	if len(frag) > 128<<10 {
		t.Errorf("all-busy grid fragment is %d bytes, want at most 128 KiB", len(frag))
	}
	t.Logf("32×12 all-busy grid: %d bytes", len(frag))
}

// The live dot's pop-over says how often the grid is read, and has its
// words for a dropped connection ready for the script.
func TestGridLiveWordingNeedsScript(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	body := h.get(&op, "/").Body.String()
	contains(t, "grid", body, "reads Proxmox every 5 seconds",
		`<div class="off"><b>Disconnected</b><span class="muted" data-retry>Reconnecting</span><a class="btn" href="">Reload</a></div>`)
	lacks(t, "grid", body, "updates this page by itself", "banner")
}

// The empty grid has no legend: there is nothing to count.
func TestGridEmptyHidesTotalsAndLegend(t *testing.T) {
	h := emptyWithMasters(t)
	op := h.login(asOperator)
	body := h.get(&op, "/").Body.String()
	contains(t, "empty grid", body, `<ul class="legend" id="grid-counts" aria-label="VM states" hidden>`)

	h = newHarness(t)
	h.poll()
	op = h.login(asOperator)
	body = h.get(&op, "/").Body.String()
	contains(t, "grid", body, `<ul class="legend" id="grid-counts" aria-label="VM states">`)
}

// Teams read as the grid shows them, two digits, wherever the UI names
// them; only the range to type stays as it must be typed.
func TestTeamsShownWithTwoDigits(t *testing.T) {
	h := newHarness(t)
	h.poll()
	lead := h.login(asLead)
	_, _, body := h.preview(&lead, "/power", url.Values{"teams": {"1-3"}, "action": {"start"}})
	contains(t, "preview", body, `<dt>Teams</dt><dd><span class="vm">01-03</span></dd>`, "This covers every team (01-03).")
	_, _, body = h.preview(&lead, "/teardown", url.Values{"teams": {"1-3"}})
	contains(t, "teardown preview", body, `Type <code>1-3</code>`)
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1,3", Action: "start"}, byOperator)
	contains(t, "jobs", h.get(&lead, "/logs").Body.String(), `<span class="j-teams"><span class="vm">01, 03</span>`)
	contains(t, "job", h.get(&lead, "/logs/"+itoa(id)).Body.String(), `teams <span class="vm">01, 03</span>`)
}

// Starting VMs that already run, or stopping stopped ones, does nothing to
// them, and the preview says so.
func TestPowerPreviewSaysWhatIsAlreadyDone(t *testing.T) {
	h := newHarness(t)
	h.api.setStatus(10102, "stopped") // team01-web
	h.poll()
	op := h.login(asOperator)
	_, _, body := h.preview(&op, "/power", selection([]string{"team01-dc", "team01-web", "team02-dc"}, "action", "start"))
	contains(t, "start preview", body, "3 will run</span>", "2 already running</span>")
	_, _, body = h.preview(&op, "/power", selection([]string{"team01-web"}, "action", "shutdown"))
	contains(t, "shutdown preview", body, "1 already stopped</span>")
	_, _, body = h.preview(&op, "/power", selection([]string{"team01-web"}, "action", "start"))
	lacks(t, "start of a stopped VM", body, "already")
}

// What the help page said, where it's needed now: as controls, locks and
// the one consequence line.
func TestHelpKnowledgeInPlace(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	nobody := h.login(asNobody)

	running := h.submitJob(jobs.Inputs{Kind: pods.KindTeardown, Teams: "1"}, byLead)
	h.start(running)
	// Cancelling needs the job's privilege (VM.Allocate for a teardown)
	// somewhere, unless you started it.
	contains(t, "operator's view of a teardown", h.get(&op, "/logs/"+itoa(running)).Body.String(), "Cancel · not permitted</span>")
	contains(t, "nobody's view of a teardown", h.get(&nobody, "/logs/"+itoa(running)).Body.String(), "Cancel · not permitted</span>")
	power := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Action: "start"}, byLead)
	body := h.get(&op, "/logs/"+itoa(power)).Body.String()
	contains(t, "operator's view of a power job", body, `<button type="submit" class="btn bad">`, "Cancel job</button>")
	lacks(t, "operator's view of a power job", body, "not permitted")

	_, _, body = h.preview(&op, "/reset", url.Values{"teams": {"1"}})
	contains(t, "reset preview", body, "Work since then is lost.")

	h.finish(running, store.Outcome{Status: store.StatusInterrupted})
	contains(t, "interrupted job", h.get(&op, "/logs/"+itoa(running)).Body.String(), `<span class="st b s-int">`, "interrupted</span>")

	h.start(power)
	h.finish(power, store.Outcome{Status: store.StatusSucceeded})
	failed := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Action: "start"}, byOperator)
	h.start(failed)
	h.finish(failed, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"team02-dc": {Status: store.ItemDone}, "team02-web": {Status: store.ItemFailed, Error: "locked"}}})
	contains(t, "failed job", h.get(&op, "/logs/"+itoa(failed)).Body.String(),
		`title="Battleship already tried them again by itself (1 automatic round), so fix the cause first. Runs the job again for 1 VM, after a preview."`,
		"Retry 1 failed VM</button>")
}
