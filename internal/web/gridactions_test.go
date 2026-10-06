package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

// selection is what the grid's form posts: from=grid, one vms value per
// ticked box, and the pressed button's name and value.
func selection(vms []string, button ...string) url.Values {
	f := url.Values{"from": {"grid"}, "vms": vms}
	for i := 0; i+1 < len(button); i += 2 {
		f.Add(button[i], button[i+1])
	}
	return f
}

// panelPost posts as the script does for the side panel: the same form,
// asking for the page's content without the layout.
func (h *harness) panelPost(sess *authtest.Session, target string, form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Battleship-Panel", "1")
	sess.Apply(req)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

// panelGet is a GET for the side panel, with cookies from an earlier
// response (the flash a confirm leaves).
func (h *harness) panelGet(sess *authtest.Session, target string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("X-Battleship-Panel", "1")
	sess.Apply(req)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

// cellHTML is the markup of one grid cell in body.
func cellHTML(t *testing.T, body, id string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<div class="gc" role="cell" id="` + id + `">.*?</div>`).FindString(body)
	if m == "" {
		t.Fatalf("no cell %s in:\n%s", id, body)
	}
	return m
}

// The grid is a form: each VM that exists has a box to tick, missing ones
// have none, and the action bar's buttons post the ticked VMs to the
// operations' previews. Rows, columns and the whole grid can be selected
// without the script, by links that tick the boxes server-side.
func TestGridIsASelectionForm(t *testing.T) {
	h := newHarness(t)
	h.api.add(proxmox.VM{VMID: 10303, Name: "team03-ftp", Node: "n2", Status: "running", Pool: "pool-03"}, cleanConfig("03"), "initial")
	h.poll()
	lead := h.login(asLead)

	body := h.get(&lead, "/").Body.String()
	contains(t, "grid form", body,
		`<form id="grid-form" class="gform" method="post" action="/power/preview" data-panel>`,
		`<input type="hidden" name="csrf" value="`+lead.CSRF+`">`,
		`<input type="hidden" name="from" value="grid">`,
		`<input type="checkbox" name="vms" value="team01-dc"`,
		`<input type="checkbox" name="vms" value="team03-ftp"`,
		// Row and column selection work as links; the whole grid's box
		// needs the script.
		`href="/?select=team:01"`, `href="/?select=host:dc"`, `<input type="checkbox" data-select="all">`,
		// The action bar.
		`id="actionbar"`,
		`formaction="/power/preview" name="action" value="start"`,
		`formaction="/power/preview" name="action" value="shutdown"`,
		`formaction="/power/preview" name="action" value="stop"`,
		`formaction="/power/preview" name="action" value="reboot"`,
		`formaction="/reset"`, "Reset to snapshot…",
		// The panel the script shows previews in.
		`<dialog class="panel" id="panel"`,
	)
	lacks(t, "grid form", body, `id="ab-details" href="/vm/`)
	if n := strings.Count(body, `name="vms"`); n != 7 {
		t.Errorf("grid has %d boxes, want 7 (9 cells, 2 missing)", n)
	}
	lacks(t, "grid form", cellHTML(t, body, "cell-01-ftp"), `type="checkbox"`)
	lacks(t, "unselected grid", body, " checked>")

	// ?select ticks boxes: a team's row, a host's column, or everything.
	body = h.get(&lead, "/?select=team:01").Body.String()
	contains(t, "row selected", body, `value="team01-dc" data-team="01" data-host="dc" checked>`, `value="team01-web" data-team="01" data-host="web" checked>`)
	lacks(t, "row selected", body, `value="team02-dc" data-team="02" data-host="dc" checked>`)
	body = h.get(&lead, "/?select=host:web").Body.String()
	contains(t, "column selected", body, `value="team01-web" data-team="01" data-host="web" checked>`, `value="team03-web" data-team="03" data-host="web" checked>`)
	lacks(t, "column selected", body, `value="team01-dc" data-team="01" data-host="dc" checked>`)
	body = h.get(&lead, "/?select=all").Body.String()
	if n := strings.Count(body, ` checked>`); n != 7 {
		t.Errorf("select=all ticked %d boxes, want 7", n)
	}
	lacks(t, "all selected", cellHTML(t, body, "cell-01-ftp"), `type="checkbox"`)
	// One VM ticked: Details links to its page.
	h.api.setStatus(10102, "stopped")
	body = h.get(&lead, "/?select=host:ftp").Body.String()
	contains(t, "one VM selected", body, `<a class="btn" id="ab-details" href="/vm/03/ftp" data-panel-link>Details</a>`)

	// The script handles shift-click ranges, Esc and the panel.
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "app.js", string(js), "shiftKey", `"Escape"`, "X-Battleship-Panel")
}

// The grid's form, posted without the script, makes an ordinary preview of
// exactly the ticked VMs: the teams are worked out from them.
func TestGridSelectionPreviewsExactVMs(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	action, form, body := h.preview(&op, "/power", selection([]string{"team02-web", "team01-dc"}, "action", "stop"))
	contains(t, "grid preview", body,
		"<tr><td><span class=\"vm\">team01-dc</span></td>", "<tr><td><span class=\"vm\">team02-web</span></td>",
		`<a class="btn" href="/" data-close>Cancel</a>`)
	lacks(t, "grid preview", body, "<span class=\"vm\">team01-web</span>", "<span class=\"vm\">team02-dc</span>", "<dt>Teams</dt>")
	if form.Get("teams") != "1-2" || form.Get("vms") != "team01-dc,team02-web" {
		t.Errorf("confirm fields = %v", form)
	}

	if rec := h.post(&op, action, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 {
		t.Fatalf("jobs = %d, want 1", len(js))
	}
	var in jobs.Inputs
	_ = json.Unmarshal(js[0].Inputs, &in)
	if in.Teams != "1-2" || !reflect.DeepEqual(in.VMs, []string{"team01-dc", "team02-web"}) || in.Action != "stop" {
		t.Errorf("job inputs = %+v", in)
	}
	items, _ := h.st.Items(context.Background(), js[0].ID)
	var names []string
	for _, it := range items {
		names = append(names, it.Name)
	}
	if !reflect.DeepEqual(names, []string{"team01-dc", "team02-web"}) {
		t.Errorf("items = %v, want exactly the ticked VMs", names)
	}
}

// Nothing ticked: the volunteer is told to tick VMs first.
func TestGridSelectionNeedsAVM(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	for _, target := range []string{"/power/preview", "/reset"} {
		rec := h.post(&op, target, selection(nil, "action", "start"))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("POST %s with nothing ticked = %d, want 422", target, rec.Code)
		}
		contains(t, target, rec.Body.String(), "Tick at least one VM on the grid")
		lacks(t, target, rec.Body.String(), `class="confirm"`)
	}
	h.noJobs("with nothing ticked")
}

// What each user is offered is decided on the server from their Proxmox
// privileges: each action button names the privilege it needs, and each VM
// box the grid's privileges the viewer lacks on it. Over every team, power
// asks to type the range (a safety catch), whoever previews it.
func TestGridSelectionPrivilegeGating(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	lead := h.login(asLead)

	body := h.get(&op, "/").Body.String()
	contains(t, "operator's grid", body, `value="start" data-needs="VM.PowerMgmt">`, `formaction="/snapshot" data-needs="VM.Snapshot">`,
		`formaction="/reset" data-needs="VM.Snapshot VM.Snapshot.Rollback">`)
	lacks(t, "operator's grid", body, `lead-ops`, `href="/deploy"`, `href="/teardown"`, `data-lacks`, `data-lead-all`)

	body = h.get(&lead, "/").Body.String()
	contains(t, "lead's grid", body, `<nav class="lead-ops" aria-label="Team operations">`, `<a class="btn" href="/deploy" data-panel-link>`, `<a class="btn bad" href="/teardown" data-panel-link>`)

	// Someone who may only power team 01's dc: every other box says so.
	cred := h.ticketWith("test-powerer", []string{"VM.Audit"})
	h.pve.Grant(cred.User, "/vms/10101", "VM.Audit", "VM.PowerMgmt")
	pw := authtest.Login(t, h.st, h.cfg, authtest.User{Subject: "test-powerer", At: h.clock.Now(), Proxmox: cred})
	h.openView(cred)
	body = h.get(&pw, "/").Body.String()
	contains(t, "powerer's dc", cellHTML(t, body, "cell-01-dc"), `data-lacks="VM.Snapshot VM.Snapshot.Rollback"`)
	contains(t, "powerer's web", cellHTML(t, body, "cell-01-web"), `data-lacks="VM.PowerMgmt VM.Snapshot VM.Snapshot.Rollback"`)

	every := []string{"team01-dc", "team02-dc", "team03-web"}
	for _, panel := range []bool{false, true} {
		var rec *httptest.ResponseRecorder
		if panel {
			rec = h.panelPost(&op, "/power/preview", selection(every, "action", "start"))
		} else {
			rec = h.post(&op, "/power/preview", selection(every, "action", "start"))
		}
		if rec.Code != http.StatusOK {
			t.Errorf("operator's all-teams power (panel %v) = %d, want 200", panel, rec.Code)
		}
		contains(t, "operator's all-teams power", rec.Body.String(), "This covers every team (01-03).")
	}
	h.noJobs("after previews")
}

// A selection edited by hand to reach VMs it shouldn't is refused by the
// same checks as any preview: exact VMs must be team VMs of the teams
// asked for, and the grid only works out teams that have VMs.
func TestGridSelectionTamper(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"another team's VM", func() url.Values {
			f := selection([]string{"team01-dc", "team02-dc"}, "action", "start")
			f.Set("teams", "1")
			return f
		}(), "team02-dc is not in teams 1"},
		{"a VM of a team with no VMs", selection([]string{"team01-dc", "team09-dc"}, "action", "start"), "team09-dc is not in teams 1"},
		{"not a team VM", selection([]string{"team01-dc", "dc.kilo.alpha"}, "action", "start"), "is not a team VM name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.post(&op, "/power/preview", tc.form)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("preview = %d, want 422", rec.Code)
			}
			contains(t, tc.name, rec.Body.String(), tc.want)
			lacks(t, tc.name, rec.Body.String(), `class="confirm"`)
		})
	}

	// Without privileges, the grid's form gets nowhere: Proxmox shows
	// them no VMs, so there is nothing to plan.
	for _, who := range []string{"student", "none"} {
		sess := h.loginStudent()
		if who == "none" {
			sess = h.login(asNobody)
		}
		for _, target := range []string{"/power/preview", "/reset"} {
			if rec := h.post(&sess, target, selection([]string{"team01-dc"}, "action", "start")); confirmFormRE.MatchString(rec.Body.String()) {
				t.Errorf("POST %s by %s offers a confirm", target, who)
			}
		}
	}
	h.noJobs("after tampered selections")
}

// The side panel asks for the same pages without the layout: the preview
// is the same plan, its confirm makes one job, and the job's page follows
// it live with a link to the full page.
func TestPanelFragments(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	sel := selection([]string{"team01-dc", "team01-web"}, "action", "reboot")

	full := h.post(&op, "/power/preview", sel)
	frag := h.panelPost(&op, "/power/preview", sel)
	if frag.Code != http.StatusOK {
		t.Fatalf("panel preview = %d\n%s", frag.Code, frag.Body)
	}
	if got := frag.Header().Get("X-Battleship-Panel"); got != "1" {
		t.Errorf("panel response marker = %q, want 1", got)
	}
	fb := frag.Body.String()
	lacks(t, "panel preview", fb, "<!doctype", "<header", `<nav class="nav"`, "<script")
	_, fullForm := confirmOf(t, full.Body.String())
	action, fragForm := confirmOf(t, fb)
	if fullForm.Get("fingerprint") != fragForm.Get("fingerprint") {
		t.Errorf("fragment's plan %s differs from the page's %s", fragForm.Get("fingerprint"), fullForm.Get("fingerprint"))
	}
	table := regexp.MustCompile(`(?s)<table class="tbl">.*?</table>`)
	if a, b := table.FindString(full.Body.String()), table.FindString(fb); a == "" || a != b {
		t.Errorf("VM tables differ:\npage:\n%s\npanel:\n%s", a, b)
	}

	rec := h.panelPost(&op, action, fragForm)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("panel confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 {
		t.Fatalf("jobs = %d, want 1", len(js))
	}
	id := itoa(js[0].ID)
	if loc := rec.Header().Get("Location"); loc != "/logs/"+id {
		t.Errorf("panel confirm redirects to %q", loc)
	}
	page := h.panelGet(&op, "/logs/"+id, rec.Result().Cookies())
	pb := page.Body.String()
	lacks(t, "panel job page", pb, "<!doctype", "<header")
	contains(t, "panel job page", pb,
		`<div class="sheet" id="job-live" aria-labelledby="job-title" data-events="/events/jobs/`+id+`?after=0">`,
		`<a class="btn ghost first" href="/logs/`+id+`">Log</a>`)

	// A second click of the panel's confirm lands on the same job.
	if rec := h.panelPost(&op, action, fragForm); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/logs/"+id {
		t.Errorf("second panel confirm = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if n := len(h.jobsInStore()); n != 1 {
		t.Errorf("jobs = %d after a second confirm, want 1", n)
	}
}

// A cell some active job still has to work on says so, with the job; one
// viewer's grid rendering, busy marks included, is shared by their tabs.
func TestGridBusyIndicator(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	h.hub.Publish(status.Msg{Topic: status.TopicJobs})
	op := h.login(asOperator)
	lead := h.login(asLead)
	busy := `<a class="cell is-busy" href="/logs/` + itoa(id) + `" data-panel-link`

	body := h.get(&op, "/").Body.String()
	contains(t, "busy cell", cellHTML(t, body, "cell-01-dc"), busy, "team01-dc · busy — job "+itoa(id))
	contains(t, "busy cell", cellHTML(t, body, "cell-01-web"), busy)
	lacks(t, "busy cell", cellHTML(t, body, "cell-01-dc"), `type="checkbox"`)
	lacks(t, "idle cell", cellHTML(t, body, "cell-02-dc"), `is-busy`)
	contains(t, "legend", body, "<b>2</b>busy</li>")

	renders := h.srv.gridRenders.Load()
	a := h.openSSE(&op, "/events/grid").next()
	a2 := h.openSSE(&op, "/events/grid").next() // a second tab
	if n := h.srv.gridRenders.Load() - renders; n != 1 || a.Data != a2.Data {
		t.Errorf("one viewer's two tabs cost %d renderings, want 1", n)
	}
	// Each viewer has their own grid; the same VMs and privileges render
	// the same.
	b := h.openSSE(&lead, "/events/grid").next()
	if a.Data != b.Data {
		t.Errorf("operator and lead got different grids:\n%s\n---\n%s", a.Data, b.Data)
	}
	contains(t, "shared grid", a.Data, busy)
	lacks(t, "shared grid", a.Data, "lead-ops", `name="csrf"`, "needs a lead", "checked")

	// The job finishing clears the marks, through the jobs notice.
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusSucceeded})
	c := h.openSSE(&op, "/events/grid")
	c.next()
	h.hub.Publish(status.Msg{Topic: status.TopicJobs})
	ev := c.next()
	if ev.Event != "patch" {
		t.Fatalf("after the job finished: %+v, want a patch", ev)
	}
	contains(t, "patch", ev.Data, `id="cell-01-dc"><label class="cell is-running"`)
	lacks(t, "patch", ev.Data, `class="cell is-busy"`)
}

// The help page is gone: its old address leads to the grid, and nothing
// links to it.
func TestHelpRedirectsToGrid(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	for _, sess := range []*authtest.Session{&op, nil} {
		rec := h.get(sess, "/help")
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
			t.Errorf("GET /help = %d to %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
		}
	}
	lead := h.login(asLead)
	for _, path := range []string{"/", "/vm/01/dc", "/logs", "/power", "/deploy"} {
		lacks(t, path, h.get(&lead, path).Body.String(), `href="/help`)
	}
	// Power and reset are on the grid now, not in the menu.
	lacks(t, "nav", h.get(&lead, "/").Body.String(), `<a href="/power">`, `<a href="/reset">`)
}

// What the help page said is where it is needed, in few words: the grid's
// legend, the preview's one consequence line, and each job status's "what
// now" as a control.
func TestGuidanceInPlace(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	body := h.get(&op, "/").Body.String()
	contains(t, "legend", body, `<ul class="legend" id="grid-counts" aria-label="VM states">`,
		`<li class="is-running"><i class="g" aria-hidden="true"></i><b>6</b>running</li></ul>`)
	if strings.Contains(body, "<b>0</b>") {
		t.Error("the legend lists a state no VM is in")
	}

	h.api.setSnapshots(10102, "other")
	_, _, body = h.preview(&op, "/reset", url.Values{"teams": {"1"}})
	contains(t, "reset preview", body,
		`<p class="warn">`, "1 VM rolls back to its baseline snapshot and starts. Work since then is lost.",
		`<span class="chip">`, "1 blocked</span>", `<tr class="blk">`)
	_, _, body = h.preview(&op, "/power", url.Values{"teams": {"2"}, "action": {"stop"}})
	contains(t, "force stop preview", body, "2 VMs lose power at once. Unsaved work is lost.",
		`<button type="submit" class="btn pri bad">`, "Force stop 2 VMs</button>")

	stale := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "3", Action: "start"}, byOperator)
	h.start(stale)
	h.finish(stale, store.Outcome{Status: store.StatusStale})
	body = h.get(&op, "/logs/"+itoa(stale)).Body.String()
	contains(t, "stale job", body, `<span class="st b s-stale">`, "Preview again</a>", `href="/power?action=start&amp;teams=3"`, "not run</span>")

	// Empty states say so in few words.
	empty := newHarness(t)
	eop := empty.login(asOperator)
	contains(t, "no jobs", empty.get(&eop, "/logs").Body.String(), "Nothing has run yet.")
}

// From the grid, Reset to snapshot asks for the snapshot first, offering
// only those every ticked VM has, the baseline first.
func TestGridResetPicksCommonSnapshots(t *testing.T) {
	h := newHarness(t)
	h.poll()
	h.api.setSnapshots(10102, "initial") // team01-web has no before-scoring
	op := h.login(asOperator)

	rec := h.post(&op, "/reset", selection([]string{"team01-dc", "team01-web"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /reset = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	contains(t, "picker", body,
		`<form method="post" action="/reset/preview" class="stack" id="reset-pick">`,
		`<input type="hidden" name="vms" value="team01-dc,team01-web">`,
		`<input type="hidden" name="teams" value="1">`,
		`<input type="radio" name="snapshot" value="" checked><span>baseline (initial)</span>`,
		`<ul class="hosts" aria-label="VMs"><li>team01-dc</li><li>team01-web</li></ul>`)
	lacks(t, "picker", body, `value="before-scoring"`, `value="initial"`, `<form method="get" action="/reset"`)

	body = h.post(&op, "/reset", selection([]string{"team01-dc"})).Body.String()
	contains(t, "picker of one VM", body, `<input type="radio" name="snapshot" value="before-scoring"><span>before-scoring</span>`)
}
