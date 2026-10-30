package web

import (
	"context"
	"encoding/json"
	"html"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// post submits form as sess, with its CSRF token (via sess.Apply).
func (h *harness) post(sess *authtest.Session, target string, form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(sess, http.MethodPost, target, strings.NewReader(form.Encode()))
}

var (
	confirmFormRE = regexp.MustCompile(`(?s)<form class="sheet" method="post" action="(/[a-z0-9./-]+/confirm)"[^>]*>(.*?)</form>`)
	hiddenRE      = regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`)
)

// confirmOf reads the confirm form of a preview page: where it posts, and
// its hidden fields (without the CSRF token, which post sends).
func confirmOf(t *testing.T, body string) (string, url.Values) {
	t.Helper()
	m := confirmFormRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no confirm form in:\n%s", body)
	}
	form := url.Values{}
	for _, f := range hiddenRE.FindAllStringSubmatch(m[2], -1) {
		if f[1] != "csrf" {
			form.Add(f[1], html.UnescapeString(f[2]))
		}
	}
	return m[1], form
}

// preview posts a preview and returns its confirm form, failing without one.
func (h *harness) preview(sess *authtest.Session, path string, form url.Values) (string, url.Values, string) {
	h.t.Helper()
	rec := h.post(sess, path+"/preview", form)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("POST %s/preview = %d\n%s", path, rec.Code, rec.Body)
	}
	body := rec.Body.String()
	action, fields := confirmOf(h.t, body)
	return action, fields, body
}

// jobsInStore lists every stored job.
func (h *harness) jobsInStore() []store.Job {
	h.t.Helper()
	js, err := h.st.Jobs(context.Background(), 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	return js
}

// noJobs fails the test if any job was stored.
func (h *harness) noJobs(what string) {
	h.t.Helper()
	if js := h.jobsInStore(); len(js) != 0 {
		h.t.Errorf("%s: %d jobs stored, want none", what, len(js))
	}
}

func TestEveryKindHasOperation(t *testing.T) {
	if len(operations) != len(pods.Kinds) {
		t.Errorf("operations has %d rows, pods.Kinds has %d", len(operations), len(pods.Kinds))
	}
	for _, k := range pods.Kinds {
		op, ok := operationFor(k)
		if !ok {
			t.Errorf("%s has no operation row", k)
			continue
		}
		if op.Path == "" || op.Title == "" || op.Icon == "" || op.Noun == "" {
			t.Errorf("%s operation row is incomplete: %+v", k, op)
		}
		if k.Teamless() && (op.Section == "" || op.review == nil || op.home == nil) {
			t.Errorf("%s teamless operation row lacks Section, review or home", k)
		}
		if (op.Pick != "") != (op.step != nil) {
			t.Errorf("%s Pick=%q step=%t", k, op.Pick, op.step != nil)
		}
		if (op.review != nil) != (op.Verb != "") {
			t.Errorf("%s review=%t Verb=%q", k, op.review != nil, op.Verb)
		}
	}
}

func TestOperationForms(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	lead := h.login(asLead)

	for _, tc := range []struct {
		path  string
		wants []string
	}{
		{"/power", []string{
			"<title>Power · battleship</title>",
			`<form class="sheet" method="post" action="/power/preview" aria-labelledby="form-title">`,
			`<input class="inp" type="text" id="teams" name="teams" value="" required`,
			`<span class="hint" id="teams-hint">1-3 · 3,7 · 12-14</span>`,
			`<legend>Hosts</legend><button type="button" class="btn sm ghost" data-hosts-toggle hidden>Deselect all</button>`,
			`<label class="opt"><input type="checkbox" name="hosts" value="dc" checked><span>dc</span></label>`,
			`<input type="radio" name="action" value="stop" required><span>`,
			`Force stop</span></label>`,
			`<button type="submit" class="btn pri">Preview</button>`,
		}},
		{"/reset", []string{
			`<form method="get" action="/reset" class="stack" id="reset-which">`,
			`<button type="submit" class="btn pri" form="reset-which">Next</button>`,
		}},
		{"/teardown", []string{
			`<form method="get" action="/teardown" class="stack" id="teardown-which">`,
			`<label class="lbl" for="teams">Pods</label>`,
			`<legend>Hosts</legend><button type="button" class="btn sm ghost" data-hosts-toggle hidden>Deselect all</button>`,
			`<button type="submit" class="btn pri" form="teardown-which">Next</button>`,
		}},
		{"/deploy", []string{
			`<form class="sheet" method="post" action="/deploy/preview" aria-labelledby="form-title">`,
			`<input class="inp" type="text" name="pattern" value="" required`,
			`<label class="sw">Take baseline snapshot<input type="checkbox" name="baseline" value="yes" checked></label>`,
			`<label class="sw">Rebuild templates<input type="checkbox" name="rebuild" value="yes"></label>`,
			"No master VMs tagged dev were found.",
			`<button type="submit" class="btn pri">Preview</button>`,
		}},
	} {
		rec := h.get(&lead, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("lead GET %s = %d", tc.path, rec.Code)
			continue
		}
		contains(t, "lead's "+tc.path, rec.Body.String(), tc.wants...)
	}

	// Operators lack VM.Clone and VM.Allocate, so no Deploy or Teardown
	// buttons (the forms open, but previews block every VM).
	body := h.get(&op, "/power").Body.String()
	contains(t, "operator's power form", body, `<form class="sheet" method="post" action="/power/preview"`)
	lacks(t, "operator's power form", body, `href="/deploy"`, `href="/teardown"`)
	// The form fills in from the query, as change links and retries use.
	body = h.get(&lead, "/deploy?teams=2&hosts=dc&pattern=*.kilo.alpha&baseline=no&rebuild=yes").Body.String()
	contains(t, "prefilled deploy form", body,
		`name="teams" value="2"`, `<input type="checkbox" name="hosts" value="dc" checked>`, `name="pattern" value="*.kilo.alpha"`,
		`<input type="checkbox" name="baseline" value="yes"></label>`,
		`<input type="checkbox" name="rebuild" value="yes" checked></label>`)
	body = h.get(&op, "/power?teams=1&action=reboot").Body.String()
	contains(t, "prefilled power form", body, `value="reboot" required checked>`)
}

func TestDeployFormListsTemplateSets(t *testing.T) {
	h := newHarness(t)
	addMasters(h)
	lead := h.login(asLead)
	h.poll() // the form offers the sets of the viewer's grid
	body := h.get(&lead, "/deploy").Body.String()
	contains(t, "deploy form", body,
		`<label class="opt"><input type="radio" name="pattern" value="*.kilo.alpha" required data-hosts="dc web" checked><span>kilo.alpha</span></label>`,
		`<legend>Hosts</legend><button type="button" class="btn sm ghost" data-hosts-toggle hidden>Deselect all</button>`,
		`<label class="opt"><input type="checkbox" name="hosts" value="dc" checked><span>dc</span></label>`,
		`<label class="opt"><input type="checkbox" name="hosts" value="web" checked><span>web</span></label>`)
}

func TestResetFormPicksSnapshots(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	body := h.get(&op, "/reset?teams=2&hosts=web").Body.String()
	contains(t, "reset step 2", body,
		`<form method="post" action="/reset/preview" class="stack" id="reset-pick">`,
		`<input type="hidden" name="teams" value="2">`,
		`<input type="hidden" name="hosts" value="web">`,
		`<input type="radio" name="snapshot" value="initial" required checked><span>initial</span>`,
		`<input type="radio" name="snapshot" value="before-scoring" required><span>before-scoring</span>`,
		`<button type="submit" class="btn pri" form="reset-pick">Preview</button>`,
	)
	lacks(t, "reset step 2", body, `value=""`, "baseline")

	// A VM without the baseline: it is still offered, and the note says so.
	h.api.setSnapshots(10101, "golden")
	body = h.get(&op, "/reset?teams=1").Body.String()
	contains(t, "reset of a VM without the baseline", body,
		`<input type="radio" name="snapshot" value="initial" required checked><span>initial</span>`,
		`<input type="radio" name="snapshot" value="golden" required><span>golden</span>`,
		"<span class=\"name\">team01-dc</span> has no initial snapshot.")

	// No VM matches: only initial.
	body = h.get(&op, "/reset?teams=1&hosts=nope").Body.String()
	contains(t, "reset matching nothing", body, "only initial is offered")

	// A snapshot read that fails says why in words, as other Proxmox
	// errors on the pages do.
	h.api.setReadErr(10101, &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/10101, VM.Audit)"})
	body = h.get(&op, "/reset?teams=1&hosts=dc").Body.String()
	contains(t, "reset with an unreadable VM", body,
		"Couldn&#39;t read the snapshots of <span class=\"name\">team01-dc</span>: reading <span class=\"name\">team01-dc</span> (VMID 10101 on n1): not permitted: you don&#39;t have <span class=\"name\">VM.Audit</span> on /vms/10101. Only initial is offered.")
}

func TestPreviewShowsThePlan(t *testing.T) {
	h := newHarness(t)
	h.poll()
	h.api.setSnapshots(10102, "other")
	op := h.login(asOperator)

	action, form, body := h.preview(&op, "/reset", url.Values{"teams": {"1"}})
	contains(t, "reset preview", body,
		"<title>Reset to snapshot · battleship</title>",
		`Reset to snapshot <span class="vm">initial</span></h1>`,
		"1 blocked</span>",
		`<dt>Pods</dt><dd><span class="vm">01</span></dd>`,
		"<tr><td><span class=\"vm\">team01-dc</span></td><td><span class=\"vm\">n1</span></td><td>stop · rollback to initial · start</td><td></td></tr>",
		`<tr class="blk"><td><span class="vm">team01-web</span></td>`,
		`no snapshot &#34;initial&#34; (has: other)`,
		"1 VM rolls back to snapshot <span class=\"name\">initial</span> and starts. Work since then is lost.",
		`<button type="submit" class="btn pri bad">`, "Reset 1 VM</button>",
		`<a class="btn first" href="/reset?snapshot=initial&amp;teams=1" data-panel-link>Back</a>`,
	)
	if strings.Index(body, "team01-web") > strings.Index(body, "team01-dc</span></td><td><span") {
		t.Error("the blocked VM isn't listed first")
	}
	lacks(t, "reset preview", body, `name="typed"`, "<dt>Snapshot</dt>")
	if action != "/reset/confirm" {
		t.Errorf("confirm posts to %q", action)
	}
	if form.Get("teams") != "1" || form.Get("snapshot") != "initial" || len(form.Get("nonce")) != 43 || len(form.Get("fingerprint")) != 64 {
		t.Errorf("confirm fields = %v", form)
	}
	h.noJobs("after a preview")
}

func TestPreviewOfDeploy(t *testing.T) {
	h := newHarness(t)
	addMasters(h)
	lead := h.login(asLead)
	_, form, body := h.preview(&lead, "/deploy", url.Values{"teams": {"1"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}})
	contains(t, "deploy preview", body,
		`<h1 id="pv-title">`, `Deploy <span class="vm">kilo.alpha</span></h1>`,
		`<dt>Pods</dt><dd><span class="vm">01</span></dd>`,
		`<summary class="disc"><h2 class="sub">Templates</h2><span class="meta">· 2 to create</span></summary>`,
		`<summary class="disc"><h2 class="sub">VMs</h2><span class="meta">· 2 VMs</span></summary>`,
		"<tr><td><span class=\"vm\">dc.kilo.alpha.tpl</span></td><td><span class=\"vm\">n1</span></td><td>create · stops its master</td><td></td></tr>",
		"Building the templates stops 2 running masters, each until its copy finishes.",
		"2 VMs will be created or repaired from <span class=\"name\">kilo.alpha</span> on n1.",
		"<td>network · disk-limits · cdrom · clear-notes · tag-set · snapshot · start</td>",
		"Deploy 2 VMs</button>",
	)
	lacks(t, "deploy preview", body, `name="typed"`, "will run</span>", `<p class="say bad">`, "<dt>Hosts</dt>", "<dt>Rebuild templates</dt>", "<dt>Baseline snapshot</dt>")
	if form.Get("baseline") != "yes" || form.Get("pattern") != "*.kilo.alpha" || form.Has("rebuild") {
		t.Errorf("confirm fields = %v", form)
	}

	// When one template is rebuilt (stopped master) and another is newly created (running master),
	// the rebuild and stop-master warnings stay separate so the rebuilt template isn't blamed for stopping the other master.
	h.noTeamVMs()
	h.api.add(proxmox.VM{VMID: 9001, Name: "dc.kilo.alpha.tpl", Node: "n1", Template: true}, nil)
	h.api.setStatus(5001, "stopped")
	_, _, mixedBody := h.preview(&lead, "/deploy", url.Values{"teams": {"1"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}, "rebuild": {"yes"}})
	contains(t, "deploy mixed create+rebuild preview", mixedBody,
		"Template <span class=\"name\">dc.kilo.alpha.tpl</span> will be deleted and built again from its master.",
		"Building its template stops master <span class=\"name\">web.kilo.alpha</span>, which is running, until the copy finishes.",
	)
	lacks(t, "deploy mixed create+rebuild preview", mixedBody, "Rebuilding <span class=\"name\">dc.kilo.alpha.tpl</span> deletes the old template and stops")

	// Rebuilding existing templates combines the rebuild and stop-master notice into one line.
	h.api.setStatus(5001, "running")
	h.api.add(proxmox.VM{VMID: 9002, Name: "web.kilo.alpha.tpl", Node: "n1", Template: true}, nil)
	_, _, rebuildBody := h.preview(&lead, "/deploy", url.Values{"teams": {"1-3"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}, "rebuild": {"yes"}})
	contains(t, "deploy rebuild preview", rebuildBody,
		"<dt>Rebuild templates</dt><dd>on</dd>",
		`<summary class="disc"><h2 class="sub">Templates</h2><span class="meta">· 2 to rebuild</span></summary>`,
		"Rebuilding 2 templates deletes the old ones and stops 2 running masters, each until its copy finishes.",
	)
	lacks(t, "deploy rebuild preview", rebuildBody, "will be deleted and built again", "This covers every pod")

	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"))
	h.api.add(teamVM("01", "web", 10102), cleanConfig("01"))
	_, _, recreateBody := h.preview(&lead, "/deploy", url.Values{"teams": {"1"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}, "rebuild": {"yes"}})
	contains(t, "deploy recreate preview", recreateBody,
		"<td>stop · delete · clone · network · disk-limits · cdrom · clear-notes · tag-set · snapshot · start</td>",
		"2 VMs will be torn down and recreated from <span class=\"name\">kilo.alpha</span> on n1.",
		`<button type="submit" class="btn pri bad">`, "Recreate 2 VMs</button>",
	)
	lacks(t, "deploy recreate preview", recreateBody, `<summary class="disc"><h2 class="sub">VMs</h2>`)

	_, _, partialRecreateBody := h.preview(&lead, "/deploy", url.Values{"teams": {"1-2"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}, "rebuild": {"yes"}})
	contains(t, "deploy partial recreate preview", partialRecreateBody,
		"4 VMs (2 VMs torn down and recreated) will be created or repaired from <span class=\"name\">kilo.alpha</span> on n1, n2.",
		`<button type="submit" class="btn pri bad">`, "Deploy 4 VMs</button>",
	)
	lacks(t, "deploy partial recreate preview", partialRecreateBody, `<summary class="disc"><h2 class="sub">VMs</h2>`, "Recreate 4 VMs")
}

func TestPreviewRefusesBadInputs(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	for _, tc := range []struct {
		path string
		form url.Values
		want string
	}{
		{"/reset", url.Values{"teams": {"3-1"}}, "Pods: range &#34;3-1&#34; is backwards"},
		{"/reset", url.Values{}, "Pods: no pods given"},
		{"/power", url.Values{"teams": {"1"}}, "Power action must be start, shutdown, stop or reboot"},
		{"/power", url.Values{"teams": {"1"}, "action": {"explode"}}, "not &#34;explode&#34;"},
	} {
		rec := h.post(&op, tc.path+"/preview", tc.form)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("POST %s/preview %v = %d, want 422", tc.path, tc.form, rec.Code)
		}
		// The form comes back, filled in as sent.
		contains(t, "refused preview", rec.Body.String(), `<p class="warn" role="alert">`, tc.want,
			`action="`+tc.path)
	}
	// Planning errors come back on the form too.
	lead := h.login(asLead)
	rec := h.post(&lead, "/deploy/preview", url.Values{"teams": {"1"}, "pattern": {"*.nope"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("deploy of an unknown template set = %d, want 422", rec.Code)
	}
	contains(t, "unknown template set", rec.Body.String(), "Couldn&#39;t make a plan: No master VMs match &#34;*.nope&#34;", `value="*.nope"`)
	h.noJobs("after refused previews")
}

// A VM with a team name outside the team's VMIDs isn't acted on, and the
// preview says so even when nothing else matches.
func TestPreviewNoticesTeamNamedVMsOutsideTheBlock(t *testing.T) {
	h := newHarness(t)
	h.api.add(proxmox.VM{VMID: 8005, Name: "team01-scratch", Node: "n1", Status: "running", Pool: "dev-05"}, nil)
	h.poll()
	op := h.login(asOperator)
	body := h.post(&op, "/teardown/preview", url.Values{"teams": {"1"}, "hosts": {"scratch"}}).Body.String()
	contains(t, "teardown preview", body, "No VMs in these pods match these hosts.",
		`Left alone: <span class="name">team01-scratch</span> has pod 01&#39;s name but is VMID 8005, outside the pod&#39;s VMIDs, so it isn&#39;t a pod VM; rename or remove it in Proxmox.`)
	lacks(t, "teardown preview", body, `name="nonce"`)
}

func TestPreviewWithNothingToRun(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	rec := h.post(&op, "/reset/preview", url.Values{"teams": {"1"}, "hosts": {"nope"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty preview = %d", rec.Code)
	}
	body := rec.Body.String()
	contains(t, "empty preview", body, "No VMs in these pods match these hosts.")
	lacks(t, "empty preview", body, `name="nonce"`, "Something went wrong", `class="say caution"`)

	h.api.setSnapshots(10101)
	h.api.setSnapshots(10102)
	body = h.post(&op, "/reset/preview", url.Values{"teams": {"1"}}).Body.String()
	contains(t, "all-blocked preview", body, "Every VM is blocked: nothing can run.", `class="blk"`)
	lacks(t, "all-blocked preview", body, `name="nonce"`)
}

func TestPreviewThenConfirmMakesOneJob(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	action, form, _ := h.preview(&op, "/reset", url.Values{"teams": {"1"}, "hosts": {"dc"}, "snapshot": {"before-scoring"}})

	rec := h.post(&op, action, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 {
		t.Fatalf("jobs = %d, want 1", len(js))
	}
	j := js[0]
	if loc := rec.Header().Get("Location"); loc != "/logs/"+itoa(j.ID) {
		t.Errorf("redirect to %q, want the job", loc)
	}
	if j.Kind != "reset" || j.Status != store.StatusPending || j.CreatedBy != "test-operator@example.org" || j.CreatedAs != "test-operator@auth.example.org" {
		t.Errorf("job = %+v", j)
	}
	if want := []string{"team:01", "vmid:10101"}; !reflect.DeepEqual(j.LockKeys, want) {
		t.Errorf("lock keys = %v, want %v", j.LockKeys, want)
	}
	var in jobs.Inputs
	_ = json.Unmarshal(j.Inputs, &in)
	want := jobs.Inputs{Kind: pods.KindReset, Teams: "1", Hosts: []string{"dc"}, Snapshot: "before-scoring"}
	if !reflect.DeepEqual(in, want) {
		t.Errorf("inputs = %+v, want %+v", in, want)
	}
	if j.Fingerprint != form.Get("fingerprint") {
		t.Errorf("job fingerprint %s, want the previewed %s", j.Fingerprint, form.Get("fingerprint"))
	}
	items, _ := h.st.Items(context.Background(), j.ID)
	if len(items) != 1 || items[0].Name != "team01-dc" {
		t.Errorf("items = %+v, want team01-dc only", items)
	}
	// The job page shows it, waiting to start.
	cookie := rec.Result().Cookies()
	req := httptest.NewRequest(http.MethodGet, "/logs/"+itoa(j.ID), nil)
	op.Apply(req)
	for _, c := range cookie {
		req.AddCookie(c)
	}
	page := httptest.NewRecorder()
	h.h.ServeHTTP(page, req)
	contains(t, "job page after submit", page.Body.String(), `<span class="muted vm">#`+itoa(j.ID)+`</span>`, `<span class="st b s-wait">`)

	// Submitting the same confirm again (a double click, or the back
	// button) lands on the same job and makes no other.
	rec = h.post(&op, action, form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/logs/"+itoa(j.ID) {
		t.Errorf("second confirm = %d to %q, want 303 to the first job", rec.Code, rec.Header().Get("Location"))
	}
	if n := len(h.jobsInStore()); n != 1 {
		t.Errorf("jobs after a second confirm = %d, want 1", n)
	}
}

func TestDoubleClickedConfirmMakesOneJob(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	action, form, _ := h.preview(&op, "/power", url.Values{"teams": {"2"}, "action": {"reboot"}})

	const clicks = 6
	codes := make([]int, clicks)
	locs := make([]string, clicks)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range clicks {
		wg.Go(func() {
			<-start
			req := httptest.NewRequest(http.MethodPost, action, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			op.Apply(req)
			rec := httptest.NewRecorder()
			h.h.ServeHTTP(rec, req)
			codes[i], locs[i] = rec.Code, rec.Header().Get("Location")
		})
	}
	close(start)
	wg.Wait()

	js := h.jobsInStore()
	if len(js) != 1 {
		t.Fatalf("jobs = %d, want 1", len(js))
	}
	for i := range clicks {
		if codes[i] != http.StatusSeeOther || locs[i] != "/logs/"+itoa(js[0].ID) {
			t.Errorf("click %d = %d to %q, want 303 to job %d", i, codes[i], locs[i], js[0].ID)
		}
	}
}

func TestConfirmAfterTheClusterChanged(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	action, form, _ := h.preview(&op, "/reset", url.Values{"teams": {"1"}})

	// Someone deletes team01-web's baseline: that VM would now be blocked.
	h.api.setSnapshots(10102, "before-scoring")
	rec := h.post(&op, action, form)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm after a change = %d, want 409", rec.Code)
	}
	body := rec.Body.String()
	contains(t, "changed preview", body,
		"The cluster changed since your preview; check it again",
		`<tr class="blk"><td><span class="vm">team01-web</span></td>`,
		"Reset 1 VM</button>")
	h.noJobs("after a confirm of a changed cluster")

	// A fresh, confirmable preview with its own nonce.
	action2, form2 := confirmOf(t, body)
	if form2.Get("nonce") == form.Get("nonce") || form2.Get("fingerprint") == form.Get("fingerprint") {
		t.Errorf("the new preview reuses the old nonce or fingerprint")
	}
	if rec := h.post(&op, action2, form2); rec.Code != http.StatusSeeOther {
		t.Errorf("confirm of the new preview = %d, want 303", rec.Code)
	}
	if n := len(h.jobsInStore()); n != 1 {
		t.Errorf("jobs = %d, want 1", n)
	}
	// The old preview still doesn't match the cluster.
	if rec := h.post(&op, action, form); rec.Code != http.StatusConflict {
		t.Errorf("confirm of the old preview = %d, want 409", rec.Code)
	}
}

func TestConfirmOfAnExpiredPreview(t *testing.T) {
	h := newHarness(t, longSessions)
	h.poll()
	op := h.login(asOperator)
	action, form, _ := h.preview(&op, "/reset", url.Values{"teams": {"1"}})
	h.clock.Advance(previewTTL)
	rec := h.post(&op, action, form)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm after %s = %d, want 409", previewTTL, rec.Code)
	}
	contains(t, "expired preview", rec.Body.String(), "This preview had expired", "Previews can be confirmed for 30 minutes.")
	h.noJobs("after an expired preview")
}

// A confirm repeated long after (back button, expired previews cleared)
// still lands on its job.
func TestLateRepeatedConfirmFindsItsJob(t *testing.T) {
	h := newHarness(t, longSessions)
	h.poll()
	op := h.login(asOperator)
	action, form, _ := h.preview(&op, "/reset", url.Values{"teams": {"1"}})
	rec := h.post(&op, action, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d", rec.Code)
	}
	job := rec.Header().Get("Location")
	h.clock.Advance(previewTTL + time.Minute)
	h.preview(&op, "/power", url.Values{"teams": {"2"}, "action": {"start"}}) // clears expired previews
	rec = h.post(&op, action, form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != job {
		t.Errorf("late repeat = %d to %q, want 303 to %s", rec.Code, rec.Header().Get("Location"), job)
	}
	if n := len(h.jobsInStore()); n != 1 {
		t.Errorf("jobs = %d, want 1", n)
	}
}

// A preview expiring between the confirm's check and the store makes no
// job: the store checks again.
func TestConfirmOfAPreviewExpiringDuringSubmit(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	action, form, _ := h.preview(&op, "/reset", url.Values{"teams": {"1"}})
	h.srv.beforeSubmit = func() { h.clock.Advance(previewTTL) }
	rec := h.post(&op, action, form)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm = %d, want 409\n%s", rec.Code, rec.Body)
	}
	contains(t, "preview expired during submit", rec.Body.String(), "This preview had expired")
	h.noJobs("after a preview expired during its submit")
}

// A confirm needs its session's preview nonce on the previewed operation,
// and ignores edits to the form's hidden input fields.
func TestConfirmRefusesEditedForms(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	other := h.login(asOperator)
	action, form, _ := h.preview(&op, "/reset", url.Values{"teams": {"1"}, "hosts": {"dc"}})

	edit := func(mut func(url.Values)) url.Values {
		f := url.Values{}
		for k, v := range form {
			f[k] = append([]string(nil), v...)
		}
		mut(f)
		return f
	}
	for _, tc := range []struct {
		name string
		form url.Values
		code int
		want string
	}{
		{"no nonce", edit(func(f url.Values) { f.Del("nonce") }), http.StatusConflict, "Preview it again"},
		{"made-up nonce", edit(func(f url.Values) { f.Set("nonce", strings.Repeat("A", 43)) }), http.StatusConflict, "Preview it again"},
	} {
		rec := h.post(&op, action, tc.form)
		if rec.Code != tc.code {
			t.Errorf("%s: confirm = %d, want %d", tc.name, rec.Code, tc.code)
		}
		contains(t, tc.name, rec.Body.String(), tc.want)
	}
	// Another login can't confirm this browser's preview.
	if rec := h.post(&other, action, form); rec.Code != http.StatusConflict {
		t.Errorf("another session's confirm = %d, want 409", rec.Code)
	}
	// Nor can the preview be sent to another operation's confirm.
	if rec := h.post(&op, "/power/confirm", edit(func(f url.Values) { f.Set("action", "stop") })); rec.Code != http.StatusBadRequest {
		t.Errorf("reset preview sent to power confirm = %d, want 400", rec.Code)
	}
	h.noJobs("after refused confirms")
	if !strings.Contains(h.logs.String(), "confirm refused") {
		t.Errorf("refusals aren't logged:\n%s", h.logs)
	}

	// Editing the hidden input fields on the confirm POST has no effect:
	// the job runs the stored preview's inputs and fingerprint.
	tampered := edit(func(f url.Values) {
		f.Set("teams", "1-32")
		f.Del("hosts")
		f.Set("snapshot", "before-scoring")
		f.Set("fingerprint", strings.Repeat("0", 64))
	})
	if rec := h.post(&op, action, tampered); rec.Code != http.StatusSeeOther {
		t.Fatalf("tampered confirm = %d, want 303", rec.Code)
	}
	j := h.jobsInStore()[0]
	want := jobs.Inputs{Kind: pods.KindReset, Teams: "1", Hosts: []string{"dc"}, Snapshot: "initial"}
	if got := readInputs(j); !reflect.DeepEqual(got, want) || j.Fingerprint != form.Get("fingerprint") {
		t.Errorf("job inputs = %#v (fp %s), want %#v (fp %s)", got, j.Fingerprint, want, form.Get("fingerprint"))
	}
}

// Someone who previews power on one pod and edits the confirm form to cover
// every pod runs only the previewed pod: confirm reads the stored preview.
func TestConfirmRefusesEditedTeams(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	action, form, _ := h.preview(&op, "/power", url.Values{"teams": {"1"}, "action": {"stop"}})
	form.Set("teams", "1-3")
	form.Set("action", "start")
	form.Set("typed", "1-3")
	rec := h.post(&op, action, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("widened confirm = %d, want 303", rec.Code)
	}
	want := jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "stop"}
	if got := readInputs(h.jobsInStore()[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("job inputs = %#v, want %#v", got, want)
	}
}

func TestOperatorCannotRunWhatTheyLackPrivilegesFor(t *testing.T) {
	h := newHarness(t)
	h.poll()
	addMasters(h)
	op := h.login(asOperator)
	lead := h.login(asLead)

	// Hand-made forms with a valid CSRF token get nowhere: previews block
	// every VM and confirms match no preview.
	for _, target := range []string{"/deploy/preview", "/deploy/confirm", "/teardown/preview", "/teardown/confirm"} {
		form := url.Values{"teams": {"1"}, "pattern": {"*.kilo.alpha"}, "typed": {"1"}, "nonce": {"x"}, "fingerprint": {"y"}}
		rec := h.post(&op, target, form)
		if confirmFormRE.MatchString(rec.Body.String()) {
			t.Errorf("operator POST %s offers a confirm", target)
		}
		if strings.HasSuffix(target, "/preview") {
			contains(t, "operator's "+target, rec.Body.String(), "Every VM is blocked", "you don&#39;t have <span class=\"name\">VM.")
		}
	}
	// Power on every team is the operator's to do.
	_, _, body := h.preview(&op, "/power", url.Values{"teams": {"1-3"}, "action": {"start"}})
	contains(t, "operator's all-teams power", body, "This covers every pod (01-03).", `action="/power/confirm"`)
	h.noJobs("after the operator's attempts")

	// A lead may do all of it.
	action, form, _ := h.preview(&lead, "/teardown", url.Values{"teams": {"1"}})
	form.Set("typed", "1")
	if rec := h.post(&lead, action, form); rec.Code != http.StatusSeeOther {
		t.Errorf("lead's teardown confirm = %d, want 303", rec.Code)
	}
	action, form, _ = h.preview(&lead, "/deploy", url.Values{"teams": {"2"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}})
	form.Set("typed", "2")
	if rec := h.post(&lead, action, form); rec.Code != http.StatusSeeOther {
		t.Errorf("lead's deploy confirm = %d, want 303", rec.Code)
	}
	if n := len(h.jobsInStore()); n != 2 {
		t.Errorf("jobs = %d, want 2", n)
	}
}

func TestTypedConfirmation(t *testing.T) {
	h := newHarness(t)
	h.poll()
	lead := h.login(asLead)

	for _, tc := range []struct {
		name  string
		path  string
		form  url.Values
		typed string // what to type; "" for no typing needed
	}{
		{"teardown", "/teardown", url.Values{"teams": {"1-2"}}, "1-2"},
		{"all-teams power", "/power", url.Values{"teams": {"1-3"}, "action": {"shutdown"}}, ""},
		{"power on some teams", "/power", url.Values{"teams": {"1-2"}, "action": {"shutdown"}}, ""},
		{"reset of every team", "/reset", url.Values{"teams": {"1-3"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(h.jobsInStore())
			action, form, body := h.preview(&lead, tc.path, tc.form)
			if tc.typed == "" {
				lacks(t, "preview", body, `name="typed"`)
				if rec := h.post(&lead, action, form); rec.Code != http.StatusSeeOther {
					t.Errorf("confirm = %d, want 303", rec.Code)
				}
				return
			}
			contains(t, "preview", body, `Type <code>`+tc.typed+`</code> to confirm`)
			for _, wrong := range []string{"", "1", "1-32", tc.typed + ",", "01-0" + tc.typed[len(tc.typed)-1:]} {
				f := url.Values{}
				for k, v := range form {
					f[k] = v
				}
				if wrong != "" {
					f.Set("typed", wrong)
				}
				rec := h.post(&lead, action, f)
				if rec.Code != http.StatusUnprocessableEntity {
					t.Errorf("typed %q: confirm = %d, want 422", wrong, rec.Code)
				}
				contains(t, "wrongly typed", rec.Body.String(), "Type the pod range to confirm", "exactly as shown: "+tc.typed)
				// The page can be confirmed again: same preview.
				if _, again := confirmOf(t, rec.Body.String()); again.Get("nonce") != form.Get("nonce") {
					t.Errorf("typed %q: the page has another preview", wrong)
				}
			}
			if n := len(h.jobsInStore()); n != before {
				t.Fatalf("jobs = %d after wrong typing, want %d", n, before)
			}
			form.Set("typed", " "+tc.typed+" ") // stray spaces are fine
			if rec := h.post(&lead, action, form); rec.Code != http.StatusSeeOther {
				t.Errorf("typed right: confirm = %d, want 303\n%s", rec.Code, rec.Body)
			}
			if n := len(h.jobsInStore()); n != before+1 {
				t.Errorf("jobs = %d, want %d", n, before+1)
			}
		})
	}
}

func TestOperationPostsNeedCSRF(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	action, form, _ := h.preview(&op, "/reset", url.Values{"teams": {"1"}})

	send := func(what, token, origin string) {
		f := maps.Clone(form)
		if token != "" {
			f.Set(auth.CSRFField, token)
		}
		req := httptest.NewRequest(http.MethodPost, action, strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(op.Cookie)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: confirm = %d, want 403", what, rec.Code)
		}
	}
	send("no token", "", "")
	send("wrong token", strings.Repeat("x", 43), "")
	send("another site", op.CSRF, "https://evil.example")
	for _, target := range []string{"/power/preview", "/logs/1/cancel", "/logs/1/retry"} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader("teams=1&action=stop"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(op.Cookie)
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without a token = %d, want 403", target, rec.Code)
		}
	}
	h.noJobs("after forged posts")
}

// formValues are field values that stress the hidden-field and link
// encodings: edge and inner spaces, list separators, line breaks, signs and
// zeros on numbers, and the box values.
var formValues = []string{
	"", " ", "a", " a , b ", "a,b", ",", "a,,b", "a\nb", "\r\n x \r\n", "a\r\nb\n\nc", " a \n b ", "\u00a0a\u00a0", "\u0085a\u2028",
	"+5", "-5", " 12 ", "007", "-0", "0", "100", "7000", "99999999999999999999", "1e3", "0x10",
	"yes", " yes", "YES", "no", detailsWithNotes, "dc=windows", "gw=opnsense:1", "a=b\nc=d:20", "debian-12-v1", "*.kilo.alpha", "1-3",
}

// drawFormValues draws url.Values over every field, 0-2 values each, mostly
// from formValues.
func drawFormValues(t *rapid.T) url.Values {
	value := rapid.OneOf(rapid.SampledFrom(formValues), rapid.SampledFrom(formValues), rapid.SampledFrom(formValues),
		rapid.Custom(func(t *rapid.T) string {
			return strings.Join(rapid.SliceOfN(rapid.SampledFrom(formValues), 2, 3).Draw(t, "pieces"), "")
		}),
		rapid.String())
	form := url.Values{}
	for _, name := range confirmFields {
		if vs := rapid.SliceOfN(value, 0, 2).Draw(t, name); len(vs) > 0 {
			form[name] = vs
		}
	}
	return form
}

// Inputs read from any form, of any kind, read back the same from the
// preview's hidden fields (except an Edit notes form's free text and
// opening mark, which only its preview stores) and from the "change" link.
func TestInputFieldsRoundTrip(t *testing.T) {
	roundTrip := func(t interface{ Fatalf(string, ...any) }, kind pods.Kind, form url.Values) {
		in := formInputs(kind, form)
		hidden := url.Values{}
		for _, f := range inputFields(in) {
			hidden.Add(f.Name, f.Value)
		}
		if in.Details != nil {
			in.Details = &pods.ImageNotes{ImageDetails: in.Details.ImageDetails}
		}
		if got := formInputs(kind, hidden); !reflect.DeepEqual(got, in) {
			t.Fatalf("%s: formInputs(inputFields(%#v)) = %#v", kind, in, got)
		}
		op, _ := operationFor(kind)
		link, err := url.Parse(formQuery(op, in))
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		q := link.Query()
		if op.inEnv() {
			// The domain is in the path; without a valid one, the link is
			// the Environments page.
			if pods.CheckDomain(in.Domain) != nil {
				if link.String() != envsPath {
					t.Fatalf("%s: formQuery(%#v) = %s", kind, in, link)
				}
				return
			}
			q.Set(fieldDomain, strings.Split(link.Path, "/")[2])
		}
		in.Details = nil // a password and long text stay out of links: the form reads them again
		if got := formInputs(kind, q); !reflect.DeepEqual(got, in) {
			t.Fatalf("%s: formInputs(formQuery(%#v)) = %#v", kind, in, got)
		}
	}
	for _, op := range operations {
		for _, v := range formValues {
			form := url.Values{}
			for _, name := range confirmFields {
				form.Set(name, v)
			}
			roundTrip(t, op.Kind, form)
			roundTrip(t, op.Kind, url.Values{})
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		roundTrip(t, rapid.SampledFrom(operations).Draw(t, "op").Kind, drawFormValues(t))
	})
}

// addMasters adds tagged masters dc.kilo.alpha and web.kilo.alpha, whose
// template-to-be VMIDs (9001, 9002) give the harness's team VMs' VMIDs.
func addMasters(h *harness) {
	for i, host := range []string{"dc", "web"} {
		vm := proxmox.VM{VMID: 5001 + i, Name: host + ".kilo.alpha", Node: "n1", Status: "running", Tags: "dev"}
		h.api.add(vm, map[string]string{"net0": "virtio=BC:24:11:00:00:0" + itoa(int64(i)) + ",bridge=vmbr0"})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Every confirm runs the stored preview's inputs, ignoring edits to the
// form's hidden input fields.
func TestConfirmRefusesEditedPowerAndDeployForms(t *testing.T) {
	h := newHarness(t)
	h.poll()
	addMasters(h)
	lead := h.login(asLead)
	for _, tc := range []struct {
		name, path string
		preview    url.Values
		mut        func(url.Values)
	}{
		{"power: vms added", "/power", url.Values{"teams": {"1"}, "action": {"start"}}, func(f url.Values) { f.Set("vms", "team01-dc") }},
		{"power: vms changed", "/power", url.Values{"teams": {"1"}, "action": {"start"}, "vms": {"team01-dc"}}, func(f url.Values) { f.Set("vms", "team01-web") }},
		{"power: action changed", "/power", url.Values{"teams": {"1"}, "action": {"start"}}, func(f url.Values) { f.Set("action", "stop") }},
		{"deploy: pattern changed", "/deploy", url.Values{"teams": {"1"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}}, func(f url.Values) { f.Set("pattern", "dc.kilo.alpha") }},
		{"deploy: rebuild added", "/deploy", url.Values{"teams": {"1"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}}, func(f url.Values) { f.Set("rebuild", "yes") }},
		{"deploy: baseline dropped", "/deploy", url.Values{"teams": {"1"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}}, func(f url.Values) { f.Del("baseline") }},
		{"deploy: baseline added", "/deploy", url.Values{"teams": {"1"}, "pattern": {"*.kilo.alpha"}}, func(f url.Values) { f.Set("baseline", "yes") }},
		{"snapshot: name changed", "/snapshot", url.Values{"teams": {"1"}, "snapshot": {"round2"}}, func(f url.Values) { f.Set("snapshot", "round3") }},
		{"snapshot: description changed", "/snapshot", url.Values{"teams": {"1"}, "snapshot": {"round2"}, "description": {"a"}}, func(f url.Values) { f.Set("description", "b") }},
		{"snapshot: RAM added", "/snapshot", url.Values{"teams": {"1"}, "snapshot": {"round2"}}, func(f url.Values) { f.Set("vmstate", "yes") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			action, form, _ := h.preview(&lead, tc.path, tc.preview)
			op, _ := operationFor(pods.Kind(strings.TrimPrefix(tc.path, "/")))
			want := op.scope(formInputs(op.Kind, form))
			tc.mut(form)
			rec := h.post(&lead, action, form)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("confirm = %d, want 303", rec.Code)
			}
			j := h.jobsInStore()[0]
			if got := readInputs(j); !reflect.DeepEqual(got, want) {
				t.Errorf("job inputs = %#v, want %#v", got, want)
			}
			if err := h.st.RequestCancel(context.Background(), j.ID, "test"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A user with no Proxmox privileges (a competitor, or anyone else Authentik
// let in) sees no VMs and can't change anything or cancel others' jobs.
func TestOperationAndJobRoutesDoNothingWithoutPrivileges(t *testing.T) {
	h := newHarness(t)
	h.poll()
	opID := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	job := "/logs/" + itoa(opID)
	routes := []struct{ method, path string }{
		{http.MethodGet, "/power"}, {http.MethodPost, "/power/preview"}, {http.MethodPost, "/power/confirm"},
		{http.MethodGet, "/reset"}, {http.MethodPost, "/reset/preview"}, {http.MethodPost, "/reset/confirm"},
		{http.MethodGet, "/snapshot"}, {http.MethodPost, "/snapshot"}, {http.MethodPost, "/snapshot/preview"}, {http.MethodPost, "/snapshot/confirm"},
		{http.MethodGet, "/deploy"}, {http.MethodPost, "/deploy/preview"}, {http.MethodPost, "/deploy/confirm"},
		{http.MethodGet, "/teardown"}, {http.MethodPost, "/teardown/preview"}, {http.MethodPost, "/teardown/confirm"},
		{http.MethodGet, "/logs"}, {http.MethodGet, job}, {http.MethodPost, job + "/cancel"}, {http.MethodPost, job + "/retry"},
	}
	form := url.Values{"teams": {"1"}, "action": {"start"}, "snapshot": {"initial"}, "pattern": {"*.kilo.alpha"}, "typed": {"1"}}
	for _, who := range []string{"student", "nobody"} {
		sess := h.loginStudent()
		if who == "nobody" {
			sess = h.login(asNobody)
		}
		for _, rt := range routes {
			var rec *httptest.ResponseRecorder
			if rt.method == http.MethodPost {
				rec = h.post(&sess, rt.path, form)
			} else {
				rec = h.get(&sess, rt.path)
			}
			if strings.HasSuffix(rt.path, "/preview") && confirmFormRE.MatchString(rec.Body.String()) {
				t.Errorf("%s %s %s offers a confirm", who, rt.method, rt.path)
			}
		}
	}
	if js := h.jobsInStore(); len(js) != 1 {
		t.Errorf("jobs = %d, want only the operator's", len(js))
	}
	if j, _ := h.st.Job(context.Background(), opID); j.CancelRequested {
		t.Error("a user without privileges cancelled the job")
	}
}

// cliValues are a command line's values for one operation: each string
// field as a flag carries it, by form field name, and the boxes.
type cliValues struct {
	strs                                 map[string]string
	envHosts                             []string // -host, repeated
	vmid, octet                          int
	rebuild, noSnap, vmstate, ejectCDROM bool
	details                              *pods.ImageNotes
}

// raw is the inputs a command line sets before Normalize.
func (v cliValues) raw(kind pods.Kind) jobs.Inputs {
	s := v.strs
	return jobs.Inputs{Kind: kind, Teams: s[fieldTeams], Hosts: jobs.SplitList(s[fieldHosts]), VMs: jobs.SplitList(s[fieldVMs]),
		Pattern: s[fieldPattern], Rebuild: v.rebuild, NoSnapshot: v.noSnap, Snapshot: s[fieldSnapshot], Description: s[fieldDescription],
		VMState: v.vmstate, Action: s[fieldAction], Domain: s[fieldDomain], Router: s[fieldRouter], EnvHosts: v.envHosts,
		EnvHost: s[fieldEnvHost], Octet: v.octet, EjectCDROM: v.ejectCDROM, CPU: s[fieldCPU],
		ImageRequest: pods.ImageRequest{VMID: v.vmid, Family: s[fieldFamily], ImageKind: s[fieldImageKind], Image: s[fieldImage], Details: v.details}}
}

// posted is the form a page posts with the same values: boxes as "yes",
// hosts one per line of the textarea, which browsers send with CRLF.
func (v cliValues) posted() url.Values {
	form := url.Values{}
	for k, s := range v.strs {
		form.Set(k, s)
	}
	form.Set(fieldEnvHosts, strings.Join(v.envHosts, "\r\n"))
	form.Set(fieldVMID, strconv.Itoa(v.vmid))
	if v.octet == pods.ClearOctet {
		form.Set(fieldOctet, clearOctetValue)
	} else if v.octet != 0 {
		form.Set(fieldOctet, strconv.Itoa(v.octet))
	}
	for k, on := range map[string]bool{fieldRebuild: v.rebuild, fieldBaseline: !v.noSnap, fieldVMState: v.vmstate, fieldEjectCDROM: v.ejectCDROM} {
		if on {
			form.Set(k, yes)
		}
	}
	if d := v.details; d != nil {
		form.Set(fieldDetails, yes)
		if d.Free != nil {
			form.Set(fieldDetails, detailsWithNotes)
			form.Set(fieldNotes, *d.Free)
		}
		form.Set(fieldOS, d.OS)
		form.Set(fieldUser, d.User)
		form.Set(fieldPassword, d.Password)
		form.Set(fieldBuilt, d.Built)
		form.Set(fieldGeneralized, d.Generalized)
		form.Set(fieldInstalled, d.Installed)
		form.Set(fieldLicence, d.Licence)
		form.Set(fieldNotesFrom, d.From)
	}
	return form
}

// What the CLI asks for, through Normalize, is what the form reads from a
// page posting the same values.
func TestPropNormalizeReadsAsTheForm(t *testing.T) {
	strFields := []string{fieldTeams, fieldHosts, fieldVMs, fieldPattern, fieldSnapshot, fieldDescription, fieldAction, fieldDomain,
		fieldRouter, fieldEnvHost, fieldCPU, fieldFamily, fieldImageKind, fieldImage}
	check := func(t interface{ Fatalf(string, ...any) }, kind pods.Kind, v cliValues) {
		got, want := v.raw(kind).Normalize(), formInputs(kind, v.posted())
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: Normalize(%#v) = %#v, the form reads %#v", kind, v.raw(kind), got, want)
		}
		if kind.Teamless() && v.details != nil {
			if got.Details == nil || got.Details.From != strings.TrimSpace(v.details.From) {
				t.Fatalf("%s: Normalize dropped Details.From: got %#v", kind, got.Details)
			}
			if (got.Details.Free == nil) != (v.details.Free == nil) ||
				(v.details.Free != nil && *got.Details.Free != pods.CleanFreeNotes(*v.details.Free)) {
				t.Fatalf("%s: Normalize dropped Details.Free: got %#v", kind, got.Details)
			}
		}
	}
	for _, op := range operations {
		for _, s := range formValues {
			v := cliValues{strs: map[string]string{}, envHosts: []string{s, "dc=x"}, vmid: 7000, octet: 10}
			for _, f := range strFields {
				v.strs[f] = s
			}
			check(t, op.Kind, v)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		value := rapid.OneOf(rapid.SampledFrom(formValues), rapid.SampledFrom(formValues), rapid.String())
		v := cliValues{strs: map[string]string{}, envHosts: rapid.SliceOfN(value, 0, 3).Draw(t, "env hosts"),
			vmid:       rapid.SampledFrom([]int{0, 7000, -5, 100}).Draw(t, "vmid"),
			octet:      rapid.SampledFrom([]int{0, -1, 4, 254}).Draw(t, "octet"),
			rebuild:    rapid.Bool().Draw(t, "rebuild"),
			noSnap:     rapid.Bool().Draw(t, "no snapshot"),
			vmstate:    rapid.Bool().Draw(t, "vmstate"),
			ejectCDROM: rapid.Bool().Draw(t, "eject cdrom")}
		if rapid.Bool().Draw(t, "details") {
			n := pods.ImageNotes{
				ImageDetails: pods.ImageDetails{OS: value.Draw(t, "os"), User: value.Draw(t, "user"), Password: value.Draw(t, "pw")},
				From:         value.Draw(t, "from"),
			}
			if rapid.Bool().Draw(t, "free") {
				free := value.Draw(t, "notes")
				n.Free = &free
			}
			v.details = &n
		}
		for _, f := range strFields {
			v.strs[f] = value.Draw(t, f)
		}
		check(t, rapid.SampledFrom(operations).Draw(t, "op").Kind, v)
	})
}
