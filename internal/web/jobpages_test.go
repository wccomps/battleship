package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

// submitJob plans in and stores it as by's job, as a confirm does.
func (h *harness) submitJob(in jobs.Inputs, by jobs.Submitter) int64 {
	h.t.Helper()
	plan, err := jobs.BuildPlan(context.Background(), pods.NewPlanner(h.api, h.cfg), in)
	if err != nil {
		h.t.Fatal(err)
	}
	if !by.Credential.Usable() {
		// lena@example.org acts as Proxmox user lena@auth.example.org.
		name, _, _ := strings.Cut(strings.TrimPrefix(by.User, "cli:"), "@")
		by.Credential, by.Seal = h.ticket(name), h.creds
		if strings.HasPrefix(by.User, "cli:") { // their own API token
			by.Credential = proxmox.TokenCredential(name+"@auth.example.org!cli", "secret")
		}
	}
	id, err := jobs.Submit(context.Background(), h.st, in, plan, by)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

var (
	byOperator = jobs.Submitter{User: "olive@example.org"}
	byLead     = jobs.Submitter{User: "lena@example.org"}
)

// start claims job id for worker w1.
func (h *harness) start(id int64) {
	h.t.Helper()
	j, err := h.st.ClaimJob(context.Background(), id, "w1")
	if err != nil || j == nil {
		h.t.Fatalf("claiming job %d: %v %v", id, j, err)
	}
}

// finish records how job id, started with start, ended.
func (h *harness) finish(id int64, o store.Outcome) {
	h.t.Helper()
	if err := h.st.Finish(context.Background(), id, "w1", o); err != nil {
		h.t.Fatalf("finishing job %d: %v", id, err)
	}
}

// event records a log line of job id.
func (h *harness) event(id int64, item, step, status, msg string) {
	h.t.Helper()
	at := time.Date(2026, 10, 3, 9, 0, 5, 0, time.UTC)
	if err := h.st.AddEvent(context.Background(), id, store.Event{At: at, Item: item, Step: step, Status: status, Message: msg}); err != nil {
		h.t.Fatal(err)
	}
}

func summaryJSON(t *testing.T, s jobs.Summary) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestJobsPage(t *testing.T) {
	h := newHarness(t)
	h.poll()
	first := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "stop"}, byOperator)
	waiting := h.submitJob(jobs.Inputs{Kind: pods.KindReset, Teams: "1", Hosts: []string{"dc"}, Snapshot: "initial"}, byOperator)
	done := h.submitJob(jobs.Inputs{Kind: pods.KindTeardown, Teams: "2"}, byLead)
	h.start(done)
	h.finish(done, store.Outcome{Status: store.StatusSucceeded})
	op := h.login(asOperator)

	rec := h.get(&op, "/logs")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /logs = %d", rec.Code)
	}
	body := rec.Body.String()
	contains(t, "job list", body,
		"<title>Logs · battleship</title>", `<a href="/logs" aria-current="page">Logs</a>`,
		`<div id="jobs-live" data-events="/events/jobs">`,
		`<a class="job" href="/logs/`+itoa(done)+`">`, `<span class="j-id vm">#`+itoa(done)+`</span>`,
		`<span class="st s-ok">`, "succeeded</span>",
		"Teardown</span>",
		`<span class="j-by" title="lena@example.org">lena <span class="role">lena@auth.example.org</span></span>`,
		`Reset to snapshot <span class="vm">initial</span></span>`,
		"Force stop</span>",
		`<small>after #`+itoa(first)+`</small>`,
		`<span class="j-teams"><span class="vm">01</span><small class="vm">dc</small></span>`, `<span class="j-teams"><span class="vm">01</span></span>`,
	)
	// Newest first.
	if strings.Index(body, `href="/logs/`+itoa(done)+`"`) > strings.Index(body, `href="/logs/`+itoa(waiting)+`"`) {
		t.Error("the list isn't newest first")
	}
	lacks(t, "job list", body, "Older</a>", "Newest</a>")

	lead := h.login(asLead)
	if rec := h.get(&lead, "/logs"); rec.Code != http.StatusOK {
		t.Errorf("lead GET /logs = %d", rec.Code)
	}
	// What ran is open to everyone signed in, like the grid.
	student := h.loginStudent()
	if rec := h.get(&student, "/logs"); rec.Code != http.StatusOK {
		t.Errorf("student GET /logs = %d, want 200", rec.Code)
	}
	if rec := h.get(&op, "/logs?before=x"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /logs?before=x = %d, want 404", rec.Code)
	}
}

func TestJobsPagePages(t *testing.T) {
	h := newHarness(t)
	var ids []int64
	for range jobsPerPage + 1 {
		id, err := h.st.CreateJob(context.Background(), store.NewJob{
			Kind: "power", Inputs: json.RawMessage(`{"kind":"power","teams":"1","action":"start"}`),
			Plan: json.RawMessage(`{"kind":"power"}`), Fingerprint: "fp", LockKeys: []string{"team:01"}, CreatedBy: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	op := h.login(asOperator)
	body := h.get(&op, "/logs").Body.String()
	older := "/logs?before=" + itoa(ids[1])
	contains(t, "first page", body, `<a class="btn" href="`+older+`">Older</a>`, `href="/logs/`+itoa(ids[jobsPerPage])+`"`)
	lacks(t, "first page", body, `href="/logs/`+itoa(ids[0])+`"`, "Newest</a>")

	body = h.get(&op, older).Body.String()
	contains(t, "second page", body, `href="/logs/`+itoa(ids[0])+`"`, `<a class="btn" href="/logs">Newest</a>`)
	lacks(t, "second page", body, "Older</a>", "data-events")
}

func TestJobPage(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindReset, Teams: "1", Snapshot: "initial"}, byOperator)
	op := h.login(asOperator)
	lead := h.login(asLead)

	rec := h.get(&op, "/logs/"+itoa(id))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /logs/%d = %d", id, rec.Code)
	}
	body := rec.Body.String()
	contains(t, "pending job page", body,
		"<title>Log #"+itoa(id)+" · battleship</title>",
		`Reset to snapshot <span class="vm">initial</span> <span class="muted vm">#`+itoa(id)+`</span></h1>`,
		`<div class="page wide" id="job-live" data-events="/events/jobs/`+itoa(id)+`?after=0">`,
		`<span class="st b s-wait">`, "pending</span>",
		`<span title="olive@example.org">olive</span> · olive@auth.example.org`,
		`teams <span class="vm">01</span>`,
		`<div class="tally"><span><b>2</b> pending</span></div>`,
		`<div class="pips" role="img" aria-label="2 pending"><span class="pip wait"></span><span class="pip wait"></span></div>`,
		`<tr><td><span class="vm">team01-dc</span></td><td class="muted c-vmid">10101</td><td class="c-steps"><span class="steps" role="img" aria-label="stop: waiting, rollback: waiting, start: waiting"><span class="pip wait" title="stop: waiting"></span><span class="pip wait" title="rollback: waiting"></span><span class="pip wait" title="start: waiting"></span></span></td><td><span class="st s-wait">`,
		`<ol class="log" id="job-log" aria-live="polite">`,
	)
	// Whoever may reset VMs may cancel; others see why not.
	contains(t, "operator's job page", body, "/cancel")
	nobody := h.login(asNobody)
	body = h.get(&nobody, "/logs/"+itoa(id)).Body.String()
	lacks(t, "nobody's job page", body, "/cancel")
	contains(t, "nobody's job page", body, "Cancel · not permitted</span>")
	body = h.get(&lead, "/logs/"+itoa(id)).Body.String()
	contains(t, "lead's job page", body,
		`<form method="post" action="/logs/`+itoa(id)+`/cancel">`,
		`<input type="hidden" name="csrf" value="`+lead.CSRF+`">`,
		`<button type="submit" class="btn bad">`, "Cancel job</button>")

	for _, path := range []string{"/logs/999", "/logs/x", "/logs/0"} {
		if rec := h.get(&op, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// A job's page recaps the inputs its title, teams and hosts don't show
// (askedFacts).
func TestJobPageRecapsWhatWasAsked(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	for _, c := range []struct {
		in   jobs.Inputs
		want string
	}{
		{jobs.Inputs{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "round2", Description: "after lunch", VMState: true},
			` · Description <span class="vm">after lunch</span> · Include RAM <span class="vm">on</span>`},
		{jobs.Inputs{Kind: pods.KindReset, Teams: "1"}, ` · Snapshot <span class="vm">baseline</span>`},
	} {
		id := h.submitJob(c.in, byOperator)
		contains(t, string(c.in.Kind)+" job page", h.get(&op, "/logs/"+itoa(id)).Body.String(), c.want)
	}
	id := h.submitJob(jobs.Inputs{Kind: pods.KindReset, Teams: "1", Snapshot: "initial"}, byOperator)
	lacks(t, "named reset's job page", h.get(&op, "/logs/"+itoa(id)).Body.String(), " · Snapshot ")
}

// Items of a job that ended before running them read "not run".
func TestJobPageShowsItemsThatNeverRan(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	stale := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	h.start(stale)
	h.finish(stale, store.Outcome{Status: store.StatusStale, Error: "the cluster changed since the preview"})
	body := h.get(&op, "/logs/"+itoa(stale)).Body.String()
	contains(t, "stale job", body,
		`<span class="st b s-stale">`,
		"the cluster changed since the preview",
		"<b>2</b> not run</span>",
		`<span class="st s-skip">`, "not run</span>",
		"Preview again</a>")
	lacks(t, "stale job", body, `<span class="st s-wait">`, "data-events", "Retry")

	cancelled := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Action: "start"}, byOperator)
	if err := h.st.RequestCancel(context.Background(), cancelled, "lena@example.org"); err != nil {
		t.Fatal(err)
	}
	body = h.get(&op, "/logs/"+itoa(cancelled)).Body.String()
	contains(t, "job cancelled before it started", body,
		`<span class="st b s-cancel">`, "<b>2</b> not run</span>",
		"cancelled by lena@example.org")
}

// A cancel that arrives after the job's work is done doesn't stop it, and
// the page says so.
func TestJobPageOfACancelThatCameTooLate(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	h.start(id)
	if err := h.st.RequestCancel(context.Background(), id, "lena@example.org"); err != nil {
		t.Fatal(err)
	}
	op := h.login(asOperator)
	contains(t, "running job", h.get(&op, "/logs/"+itoa(id)).Body.String(), "cancel requested by lena@example.org</span>")
	h.finish(id, store.Outcome{Status: store.StatusSucceeded, Items: map[string]store.ItemOutcome{
		"team01-dc": {Status: store.ItemDone}, "team01-web": {Status: store.ItemDone},
	}})
	contains(t, "finished job", h.get(&op, "/logs/"+itoa(id)).Body.String(),
		"cancel requested by lena@example.org came too late to stop anything")
}

// A VM a cancel cut off reads as interrupted in the log, not failed.
func TestJobLogShowsInterruptions(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	h.start(id)
	h.event(id, "team01-dc", "", "interrupted", "cancelled before delete")
	h.event(id, "team01-web", "stop", "interrupted", "cancelled during stop; a Proxmox task it started may still be running")
	op := h.login(asOperator)
	body := h.get(&op, "/logs/"+itoa(id)).Body.String()
	contains(t, "job log", body,
		`<span class="vm">team01-dc</span><span class="err">interrupted: cancelled before delete</span>`,
		`<span class="vm">team01-web</span><span class="err">stop interrupted: cancelled during stop; a Proxmox task it started may still be running</span>`)
}

func TestJobPageOfAFinishedJob(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	h.start(id)
	h.event(id, "team01-dc", "power", "done", "")
	h.event(id, "team01-web", "power", "skipped", "") // already on: not worth a line
	h.event(id, "team01-web", "power", "failed", "start: VM is locked (backup)")
	h.finish(id, store.Outcome{
		Status: store.StatusCompletedWithFailures,
		Items: map[string]store.ItemOutcome{
			"team01-dc":  {Status: store.ItemDone},
			"team01-web": {Status: store.ItemFailed, Error: "start: VM is locked (backup)"},
		},
		Summary: summaryJSON(t, jobs.Summary{
			Removed:       []string{"team01-ftp"},
			CleanupFailed: map[string]string{"team01-ftp": "team01-ftp (VMID 10103) is half-built; delete it by hand"},
		}),
	})
	op := h.login(asOperator)
	body := h.get(&op, "/logs/"+itoa(id)).Body.String()
	contains(t, "finished job", body,
		`<span class="st b s-part">`, "completed with failures</span>",
		`title="started `, `, finished `,
		`<div class="tally"><span class="bad"><b>1</b> failed</span><span><b>1</b> done</span></div>`,
		`<td class="err">start: VM is locked (backup)</td>`,
		"<span class=\"name\">team01-ftp</span> (VMID 10103) is <span class=\"name\">half-built</span>; delete it by hand",
		`Removed again after failing half-built: <span class="vm">team01-ftp</span>`,
		`<time title="09:00:05 UTC">09:00:05</time><span class="vm">team01-dc</span><span>power done</span></li>`,
		`<span class="vm">team01-web</span><span class="err">power failed: start: VM is locked (backup)</span></li>`,
		// An operator may retry a power job.
		`<form method="post" action="/logs/`+itoa(id)+`/retry">`,
		`Runs the job again for 1 VM, after a preview."`, "Retry 1 failed VM</button>",
	)
	lacks(t, "finished job", body, "data-events", "skipped", "/cancel")
}

func TestCancelJob(t *testing.T) {
	h := newHarness(t)
	h.poll()
	pending := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	running := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Action: "start"}, byOperator)
	h.start(running)
	op := h.login(asOperator)
	lead := h.login(asLead)

	// Nobody else can, even with a valid form.
	nobody := h.login(asNobody)
	if rec := h.post(&nobody, "/logs/"+itoa(pending)+"/cancel", url.Values{}); rec.Code != http.StatusForbidden {
		t.Errorf("nobody's cancel = %d, want 403", rec.Code)
	}
	if j, _ := h.st.Job(context.Background(), pending); j.CancelRequested || j.Status != store.StatusPending {
		t.Errorf("after nobody's cancel: %+v", j)
	}

	rec := h.post(&lead, "/logs/"+itoa(running)+"/cancel", url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/logs/"+itoa(running) {
		t.Fatalf("lead cancel = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	j, _ := h.st.Job(context.Background(), running)
	if !j.CancelRequested || j.CancelledBy != "test-lead@example.org" || j.Status != store.StatusRunning {
		t.Errorf("running job after cancel = %+v", j)
	}
	body := h.get(&op, "/logs/"+itoa(running)).Body.String()
	contains(t, "running job with a cancel requested", body,
		"cancel requested by test-lead@example.org", `<span class="st b ">`, "running</span>")
	// Asking twice does nothing more, so the button goes.
	lacks(t, "lead's page of a job being cancelled", h.get(&lead, "/logs/"+itoa(running)).Body.String(), "Cancel job")

	h.post(&lead, "/logs/"+itoa(pending)+"/cancel", url.Values{})
	if j, _ := h.st.Job(context.Background(), pending); j.Status != store.StatusCancelled {
		t.Errorf("pending job after cancel = %s, want cancelled", j.Status)
	}
	// Once finished, there is nothing to cancel.
	rec = h.post(&lead, "/logs/"+itoa(pending)+"/cancel", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("cancel of a finished job = %d, want 303", rec.Code)
	}
	if c := flashOf(rec); !strings.Contains(c, "had already finished") {
		t.Errorf("flash = %q", c)
	}
	if rec := h.post(&lead, "/logs/999/cancel", url.Values{}); rec.Code != http.StatusNotFound {
		t.Errorf("cancel of no job = %d, want 404", rec.Code)
	}
	if !strings.Contains(h.logs.String(), "cancel requested: job="+itoa(running)) {
		t.Errorf("cancel isn't logged:\n%s", h.logs)
	}
}

// flashOf decodes the flash message a response leaves.
func flashOf(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookie {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.AddCookie(c)
			if f := (&Server{}).takeFlash(httptest.NewRecorder(), r); f != nil {
				return f.Message
			}
		}
	}
	return ""
}

// A retry targets exactly the unfinished VMs: the failed VMs' teams × hosts
// also cover two that succeeded, which a retried reset must not roll back.
func TestRetryFailed(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindReset, Teams: "1-3", Snapshot: "initial"}, byOperator)
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"team01-dc":  {Status: store.ItemFailed, Error: "rollback: timeout"},
		"team01-web": {Status: store.ItemDone},
		"team02-dc":  {Status: store.ItemDone},
		"team02-web": {Status: store.ItemFailed, Error: "stop: VM is locked"},
		"team03-dc":  {Status: store.ItemDone},
		"team03-web": {Status: store.ItemDone},
	}})
	op := h.login(asOperator)

	rec := h.post(&op, "/logs/"+itoa(id)+"/retry", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	contains(t, "retry preview", body,
		"<title>Reset to snapshot · battleship</title>",
		"<dt>Retry of</dt><dd>#"+itoa(id)+"</dd>",
		"2 will run</span>", `Reset to snapshot <span class="vm">initial</span></h1>`,
		`<dt>Teams</dt><dd><span class="vm">01-02</span></dd>`, "<dt>Hosts</dt><dd>dc, web</dd>",
		"<dt>Only these VMs</dt><dd><span class=\"name\">team01-dc</span>, <span class=\"name\">team02-web</span></dd>",
		"<tr><td><span class=\"vm\">team01-dc</span></td>", "<tr><td><span class=\"vm\">team02-web</span></td>",
	)
	lacks(t, "retry preview", body, "<span class=\"vm\">team01-web</span>", "<span class=\"vm\">team02-dc</span>", "team03", "<dt>Snapshot</dt>")
	action, form := confirmOf(t, body)
	if action != "/reset/confirm" || form.Get("teams") != "1-2" || form.Get("hosts") != "dc,web" ||
		form.Get("vms") != "team01-dc,team02-web" || form.Get("snapshot") != "initial" {
		t.Errorf("retry confirm = %s %v", action, form)
	}

	// The VM list is bound to the preview: a confirm that changes it is
	// refused.
	for _, vms := range []string{"team01-dc,team01-web,team02-dc,team02-web", ""} {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		f.Set("vms", vms)
		if vms == "" {
			f.Del("vms")
		}
		if rec := h.post(&op, action, f); rec.Code != http.StatusBadRequest {
			t.Errorf("confirm with vms %q = %d, want 400", vms, rec.Code)
		}
	}
	if n := len(h.jobsInStore()); n != 1 {
		t.Fatalf("jobs after tampered confirms = %d, want 1", n)
	}

	// Confirming it is an ordinary confirm, and the job has just those VMs.
	if rec := h.post(&op, action, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("retry confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 2 {
		t.Fatalf("jobs = %d, want 2", len(js))
	}
	var in jobs.Inputs
	_ = json.Unmarshal(js[0].Inputs, &in)
	if strings.Join(in.VMs, ",") != "team01-dc,team02-web" || js[0].CreatedAs != "test-operator@auth.example.org" {
		t.Errorf("retry job = %+v, inputs %+v", js[0], in)
	}
	items, _ := h.st.Items(context.Background(), js[0].ID)
	var names []string
	for _, it := range items {
		names = append(names, it.Name)
	}
	if strings.Join(names, ",") != "team01-dc,team02-web" {
		t.Errorf("retry job's VMs = %v, want exactly team01-dc and team02-web", names)
	}
}

// A named VM that no longer exists is left out, with a note.
func TestPreviewOfNamedVMs(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	_, form, body := h.preview(&op, "/power", url.Values{"teams": {"1-2"}, "action": {"start"}, "vms": {"team02-dc, team01-ftp"}})
	contains(t, "preview of named VMs", body,
		"1 will run</span>", `<dt>Teams</dt><dd><span class="vm">01-02</span></dd>`,
		"<dt>Only these VMs</dt><dd><span class=\"name\">team02-dc</span>, <span class=\"name\">team01-ftp</span></dd>",
		"1 of the 2 VMs asked for by name no longer exist or match the teams and hosts, so they are left out: <span class=\"name\">team01-ftp</span>.")
	if form.Get("vms") != "team02-dc,team01-ftp" {
		t.Errorf("confirm vms = %q", form.Get("vms"))
	}
	// Names outside the teams are refused on the form.
	rec := h.post(&op, "/power/preview", url.Values{"teams": {"1"}, "action": {"start"}, "vms": {"team02-dc"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("VM outside the teams = %d, want 422", rec.Code)
	}
	contains(t, "VM outside the teams", rec.Body.String(), "team02-dc is not in teams 1", `name="vms" value="team02-dc"`)
}

// Retry is offered to whoever could run the operation: an operator gets a
// lock on a deploy's retry, and posting it anyway blocks every VM.
func TestRetryFollowsPrivileges(t *testing.T) {
	h := newHarness(t)
	h.poll()
	addMasters(h)
	id := h.submitJob(jobs.Inputs{Kind: pods.KindDeploy, Teams: "1", Pattern: "*.kilo.alpha"}, byLead)
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusInterrupted, Items: map[string]store.ItemOutcome{
		"team01-dc": {Status: store.ItemDone},
	}})
	op := h.login(asOperator)
	lead := h.login(asLead)

	body := h.get(&op, "/logs/"+itoa(id)).Body.String()
	lacks(t, "operator's page of a deploy job", body, "/retry")
	contains(t, "operator's page of a deploy job", body, "Retry · not permitted", "You don&#39;t have VM.Clone in Proxmox.")
	rec := h.post(&op, "/logs/"+itoa(id)+"/retry", url.Values{})
	if confirmFormRE.MatchString(rec.Body.String()) {
		t.Errorf("operator's retry of a deploy offers a confirm:\n%s", rec.Body)
	}
	contains(t, "operator's retry of a deploy", rec.Body.String(), `you don&#39;t have <span class="name">VM.Clone</span> on /vms/`, "Every VM is blocked")

	// A lead gets a deploy preview for the unfinished VM.
	rec = h.post(&lead, "/logs/"+itoa(id)+"/retry", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("lead's retry = %d\n%s", rec.Code, rec.Body)
	}
	contains(t, "lead's retry", rec.Body.String(), "<span class=\"vm\">team01-web</span>", "<dt>Rebuild templates</dt><dd>off</dd>")
	lacks(t, "lead's retry", rec.Body.String(), "<span class=\"vm\">team01-dc</span>")

	// A job that succeeded has nothing to retry.
	ok := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Action: "start"}, byOperator)
	h.start(ok)
	h.finish(ok, store.Outcome{Status: store.StatusSucceeded})
	rec = h.post(&op, "/logs/"+itoa(ok)+"/retry", url.Values{})
	if rec.Code != http.StatusSeeOther || !strings.Contains(flashOf(rec), "can be retried") {
		t.Errorf("retry of a succeeded job = %d, flash %q", rec.Code, flashOf(rec))
	}
	if n := len(h.jobsInStore()); n != 2 {
		t.Errorf("jobs = %d, want 2", n)
	}
}

// An unreadable stored plan is battleship's fault: retrying is a server
// error, and the page says why retry isn't offered.
func TestRetryOfAnUnreadablePlan(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id, err := h.st.CreateJob(context.Background(), store.NewJob{
		Kind: string(pods.KindReset), Inputs: json.RawMessage(`{"kind":"reset","teams":"1"}`),
		Plan: json.RawMessage(`{"kind":"reset","teams":"01"}`), Fingerprint: "fp", LockKeys: []string{"team:01"},
		CreatedBy: byLead.User,
		Items:     []store.NewItem{{Name: "team01-dc", Team: "01", VMID: 10105}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"team01-dc": {Status: store.ItemFailed},
	}})
	lead := h.login(asLead)
	rec := h.post(&lead, "/logs/"+itoa(id)+"/retry", url.Values{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("retry of a job with an unreadable plan = %d, want 500", rec.Code)
	}
	contains(t, "retry of an unreadable plan", rec.Body.String(), "had a problem working out job "+itoa(id)+"&#39;s retry.")
	body := h.get(&lead, "/logs/"+itoa(id)).Body.String()
	lacks(t, "page of a job with an unreadable plan", body, "/retry")
	contains(t, "page of a job with an unreadable plan", body, "Retry · unavailable", "Reading job "+itoa(id)+"&#39;s plan")
}

func TestJobEvents(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "reboot"}, byOperator)
	h.start(id)
	h.event(id, "team01-dc", "power", "done", "")
	op := h.login(asOperator)

	c := h.openSSE(&op, "/events/jobs/"+itoa(id)+"?after=0")
	if c.resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events/jobs/%d = %d", id, c.resp.StatusCode)
	}
	// First the catch-up: log so far (with its last ID), header, VMs.
	ev := c.next()
	if ev.Event != "log" || !strings.Contains(ev.Data, `<span class="vm">team01-dc</span><span>power done</span></li>`) || ev.ID == "" {
		t.Fatalf("first event = %+v, want the log line", ev)
	}
	firstID := ev.ID
	ev = c.next()
	if ev.Event != "patch" {
		t.Fatalf("second event = %+v, want patch", ev)
	}
	contains(t, "first patch", ev.Data, `<div class="sec" id="job-head">`, "running</span>", `<div class="pf-acts" id="job-actions">`, `<div class="sec" id="job-items">`)

	// A new line reaches the stream, held back by the stream's gap.
	h.clock.BlockUntil(t, 2) // heartbeat and stream end
	h.event(id, "team01-web", "power", "failed", "reboot: VM is locked")
	h.hub.Publish(status.Msg{Topic: status.JobTopic(id)})
	h.clock.BlockUntil(t, 3)
	h.clock.Advance(time.Second)
	ev = c.next()
	if ev.Event != "log" || ev.ID == firstID || !strings.Contains(ev.Data, "reboot: VM is locked") || strings.Contains(ev.Data, "team01-dc") {
		t.Fatalf("event after a new line = %+v, want just the new line", ev)
	}
	secondID := ev.ID
	ev = c.next()
	if ev.Event != "patch" || !strings.Contains(ev.Data, `<div class="sec" id="job-items">`) || strings.Contains(ev.Data, `<div class="sec" id="job-head">`) {
		t.Fatalf("event after a new line = %+v, want a patch of the VMs only", ev)
	}

	// A Resync (the hub may have missed notices) resends everything shown.
	h.clock.BlockUntil(t, 2)
	h.hub.Publish(status.Msg{Topic: status.JobTopic(id), Resync: true})
	h.clock.BlockUntil(t, 3)
	h.clock.Advance(time.Second)
	ev = c.next()
	if ev.Event != "patch" || !strings.Contains(ev.Data, `<div class="sec" id="job-head">`) || !strings.Contains(ev.Data, `<div class="sec" id="job-items">`) {
		t.Fatalf("event after a resync = %+v, want a patch of everything", ev)
	}

	// A reconnect resumes after its Last-Event-ID, overriding ?after=.
	c3 := h.openSSEWith(&op, "/events/jobs/"+itoa(id)+"?after=0", http.Header{"Last-Event-ID": {firstID}})
	ev = c3.next()
	if ev.Event != "log" || ev.ID != secondID || strings.Contains(ev.Data, "team01-dc") || !strings.Contains(ev.Data, "team01-web") {
		t.Fatalf("resumed stream's first event = %+v, want only the line after %s", ev, firstID)
	}

	// When the job finishes, the stream sends the final state and ends.
	h.finish(id, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"team01-dc": {Status: store.ItemDone}, "team01-web": {Status: store.ItemFailed, Error: "reboot: VM is locked"},
	}})
	h.clock.BlockUntil(t, 2)
	h.hub.Publish(status.Msg{Topic: status.JobTopic(id)})
	h.clock.Advance(time.Second)
	ev = c.next()
	if ev.Event != "patch" || !strings.Contains(ev.Data, "completed with failures") {
		t.Fatalf("event after the finish = %+v, want the final state", ev)
	}
	if ev = c.next(); ev.Event != "end" {
		t.Fatalf("event after the final state = %+v, want end", ev)
	}
	c.ended()
}

func TestJobEventsOfAFinishedJobEnd(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "reboot"}, byOperator)
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusSucceeded})
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/jobs/"+itoa(id))
	if ev := c.next(); ev.Event != "patch" {
		t.Fatalf("first event = %+v, want patch", ev)
	}
	if ev := c.next(); ev.Event != "end" {
		t.Fatalf("second event = %+v, want end", ev)
	}
	c.ended()
	if rec := h.get(&op, "/events/jobs/999"); rec.Code != http.StatusNotFound {
		t.Errorf("events of no job = %d, want 404", rec.Code)
	}
}

func TestJobEventsSameAsPage(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "reboot"}, byOperator)
	lead := h.login(asLead)
	page := h.get(&lead, "/logs/"+itoa(id)).Body.String()
	ev := h.openSSE(&lead, "/events/jobs/"+itoa(id)).next()
	for _, piece := range pieces(t, ev.Data) {
		if !strings.Contains(page, piece) {
			t.Errorf("the page doesn't contain the patch's piece verbatim; the script would swap it for nothing:\n%s", piece)
		}
	}
}

func TestJobsEvents(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/jobs")
	ev := c.next()
	if ev.Event != "patch" || !strings.Contains(ev.Data, `<div class="jobs" id="jobs-table"`) || !strings.Contains(ev.Data, "Nothing has run yet.") {
		t.Fatalf("first event = %+v, want the table", ev)
	}
	h.clock.BlockUntil(t, 2)

	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	h.hub.Publish(status.Msg{Topic: status.TopicJobs})
	ev = c.next()
	if ev.Event != "patch" || !strings.Contains(ev.Data, `href="/logs/`+itoa(id)+`"`) {
		t.Fatalf("event after a new job = %+v, want the table with it", ev)
	}

	// A notice that changes nothing shown sends nothing.
	h.clock.BlockUntil(t, 2)
	h.hub.Publish(status.Msg{Topic: status.TopicJobs})
	h.clock.Advance(heartbeatEvery)
	if ev := c.next(); ev.Comment != "heartbeat" {
		t.Fatalf("got %+v, want the heartbeat", ev)
	}
}

// A retry's preview, re-shown by its confirm, still names the retried job.
func TestReshownRetryPreviewKeepsItsJob(t *testing.T) {
	h := newHarness(t, longSessions)
	h.poll()
	addMasters(h)
	id := h.submitJob(jobs.Inputs{Kind: pods.KindDeploy, Teams: "1", Pattern: "*.kilo.alpha"}, byLead)
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusInterrupted, Items: map[string]store.ItemOutcome{
		"team01-dc": {Status: store.ItemDone},
	}})
	lead := h.login(asLead)
	retrying := "<dt>Retry of</dt><dd>#" + itoa(id) + "</dd>"

	rec := h.post(&lead, "/logs/"+itoa(id)+"/retry", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry = %d\n%s", rec.Code, rec.Body)
	}
	action, form := confirmOf(t, rec.Body.String())

	// Expired: a new preview, still of the retry.
	h.clock.Advance(previewTTL)
	rec = h.post(&lead, action, form)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expired confirm = %d", rec.Code)
	}
	contains(t, "retry preview after expiry", rec.Body.String(), "This preview had expired", retrying)

	// And its confirm still works.
	_, form = confirmOf(t, rec.Body.String())
	if rec := h.post(&lead, action, form); rec.Code != http.StatusSeeOther {
		t.Errorf("confirm of the re-shown retry = %d\n%s", rec.Code, rec.Body)
	}
}

// The job list marks CLI jobs and each web job's role; a stale or failed
// job's page links to its filled-in form.
func TestJobsShowOriginAndStartAgain(t *testing.T) {
	h := newHarness(t)
	h.poll()
	cli := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Action: "stop"}, jobs.Submitter{User: "cli:alice"})
	stale := h.submitJob(jobs.Inputs{Kind: pods.KindReset, Teams: "1", Hosts: []string{"dc"}, Snapshot: "initial"}, byOperator)
	h.start(stale)
	h.finish(stale, store.Outcome{Status: store.StatusStale, Error: "the cluster changed since the preview"})
	failed := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "3", Action: "reboot"}, byLead)
	h.start(failed)
	h.finish(failed, store.Outcome{Status: store.StatusFailed, Error: "planning failed"})
	done := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	h.start(done)
	h.finish(done, store.Outcome{Status: store.StatusSucceeded})
	op := h.login(asOperator)

	body := h.get(&op, "/logs").Body.String()
	contains(t, "job list", body,
		`<span class="j-by" title="cli:alice">cli:alice <span class="role">alice@auth.example.org!cli</span></span>`,
		`<span class="j-by" title="olive@example.org">olive <span class="role">olive@auth.example.org</span></span>`,
		`<span class="j-by" title="lena@example.org">lena <span class="role">lena@auth.example.org</span></span>`,
		// A stale job ran nothing, and says so.
		"stale</span><small>nothing ran</small>")
	if !regexp.MustCompile(`<span class="j-took">[0-9]+ s</span>`).MatchString(body) {
		t.Error("the finished job doesn't say how long it took")
	}

	body = h.get(&op, "/logs/"+itoa(stale)).Body.String()
	contains(t, "stale job", body, `<a class="btn pri" href="/reset?hosts=dc&amp;snapshot=initial&amp;teams=1" data-panel-link>`, "Preview again</a>")
	body = h.get(&op, "/logs/"+itoa(failed)).Body.String()
	contains(t, "failed job", body, `<a class="btn pri" href="/power?action=reboot&amp;teams=3" data-panel-link>`, "Preview again</a>")
	body = h.get(&op, "/logs/"+itoa(done)).Body.String()
	lacks(t, "succeeded job", body, "Preview again</a>")
	body = h.get(&op, "/logs/"+itoa(cli)).Body.String()
	lacks(t, "pending job", body, "Preview again</a>")

	// Only for an operation the user may start.
	addMasters(h)
	dep := h.submitJob(jobs.Inputs{Kind: pods.KindDeploy, Teams: "1", Pattern: "*.kilo.alpha"}, byLead)
	h.start(dep)
	h.finish(dep, store.Outcome{Status: store.StatusStale})
	lacks(t, "operator's page of a stale deploy", h.get(&op, "/logs/"+itoa(dep)).Body.String(), "Preview again</a>")
	lead := h.login(asLead)
	contains(t, "lead's page of a stale deploy", h.get(&lead, "/logs/"+itoa(dep)).Body.String(), "Preview again</a>")
}

// All streams of a job share one read per change, so a crowd costs one
// database read per notice, not per browser.
func TestJobStreamsShareReads(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "reboot"}, byOperator)
	h.start(id)
	op, lead := h.login(asNobody), h.login(asLead) // op may not cancel: no privileges
	var cs []*sseClient
	for _, sess := range []*authtest.Session{&op, &lead, &op} {
		c := h.openSSE(sess, "/events/jobs/"+itoa(id)+"?after=0")
		if ev := c.next(); ev.Event != "patch" {
			t.Fatalf("first event = %+v, want patch", ev)
		}
		cs = append(cs, c)
	}
	h.clock.BlockUntil(t, 6) // each stream's heartbeat and end
	before := h.srv.jobReads.Load()
	h.event(id, "team01-dc", "power", "done", "")
	h.hub.Publish(status.Msg{Topic: status.JobTopic(id)})
	h.clock.BlockUntil(t, 9) // and each one's gap
	h.clock.Advance(time.Second)
	for i, c := range cs {
		if ev := c.next(); ev.Event != "log" || !strings.Contains(ev.Data, "team01-dc") {
			t.Fatalf("stream %d: %+v, want the new log line", i, ev)
		}
		if ev := c.next(); ev.Event != "patch" || !strings.Contains(ev.Data, "1 running") {
			t.Fatalf("stream %d: %+v, want the VMs patched", i, ev)
		}
	}
	if n := h.srv.jobReads.Load() - before; n != 1 {
		t.Errorf("three streams read the job %d times for one change, want 1", n)
	}
	// Buttons are still per viewer.
	h.clock.BlockUntil(t, 6)
	h.hub.Publish(status.Msg{Topic: status.JobTopic(id), Resync: true})
	h.clock.BlockUntil(t, 9)
	h.clock.Advance(time.Second)
	heads := make([]string, len(cs))
	for i, c := range cs {
		ev := c.next()
		if ev.Event != "patch" {
			t.Fatalf("stream %d after a resync: %+v, want patch", i, ev)
		}
		heads[i] = ev.Data
	}
	if !strings.Contains(heads[1], "Cancel job") || strings.Contains(heads[0], "Cancel job") {
		t.Errorf("cancel button: nobody %v, lead %v; want the lead's only",
			strings.Contains(heads[0], "Cancel job"), strings.Contains(heads[1], "Cancel job"))
	}
}

// A shared read is reused only if it started at or after the notice's
// sequence number.
func TestSharedJobSnapFreshness(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "reboot"}, byOperator)
	ctx := context.Background()
	read := func(need uint64) jobSnap {
		t.Helper()
		snap, err := h.srv.sharedJobSnap(ctx, id, need)
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	n := func() int64 { return h.srv.jobReads.Load() }
	if snap := read(h.hub.Seq()); snap.job.Status != store.StatusPending || snap.itemsHTML == "" {
		t.Fatalf("snap = %+v", snap)
	}
	read(h.hub.Seq())
	if n() != 1 {
		t.Errorf("two reads at one sequence number read %d times, want 1", n())
	}
	h.start(id) // the change, then its notice
	h.hub.Publish(status.Msg{Topic: status.JobTopic(id)})
	if snap := read(h.hub.Seq()); snap.job.Status != store.StatusRunning || n() != 2 {
		t.Errorf("after a notice: status %s after %d reads, want running after 2", snap.job.Status, n())
	}
}

// A job's shared read lives only while one of its streams is open.
func TestJobSnapDroppedWhenLastStreamCloses(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "reboot"}, byOperator)
	h.start(id)
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/jobs/"+itoa(id)+"?after=0")
	if ev := c.next(); ev.Event != "patch" {
		t.Fatalf("first event = %+v, want patch", ev)
	}
	if !jobSnapCached(h, id) {
		t.Fatal("an open stream's read isn't shared")
	}
	c.resp.Body.Close()
	for deadline := time.Now().Add(10 * time.Second); jobSnapCached(h, id); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a running job's read is still cached 10s after its last stream closed")
		}
	}
}

func jobSnapCached(h *harness, id int64) bool {
	h.srv.jobSnaps.mu.Lock()
	defer h.srv.jobSnaps.mu.Unlock()
	_, ok := h.srv.jobSnaps.m[id]
	return ok
}

// A pending job's page names the job it waits for, until that one ends.
func TestPendingJobPageFollowsItsBlocker(t *testing.T) {
	h := newListeningHarness(t)
	h.poll()
	first := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	h.start(first)
	next := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "stop"}, byOperator)
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/jobs/"+itoa(next)+"?after=0")
	waiting := `after <a href="/logs/` + itoa(first) + `">#` + itoa(first) + `</a>`
	ev := c.next()
	if ev.Event != "patch" {
		t.Fatalf("first event = %+v, want patch", ev)
	}
	contains(t, "pending job's first patch", ev.Data, waiting)

	h.clock.BlockUntil(t, 2) // heartbeat and stream end
	h.finish(first, store.Outcome{Status: store.StatusSucceeded})
	h.clock.BlockUntil(t, 3) // and the stream's gap
	h.clock.Advance(time.Second)
	ev = c.next()
	if ev.Event != "patch" {
		t.Fatalf("after the job it waited for ended: %+v, want patch", ev)
	}
	contains(t, "patch after the job it waited for ended", ev.Data, `<div class="sec" id="job-head">`)
	lacks(t, "patch after the job it waited for ended", ev.Data, waiting)
}

// An unreadable plan can't be retried by anyone, so the page doesn't
// claim "Retry · lead only".
func TestUnreadableJobOffersNoLeadOnlyRetry(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id, err := h.st.CreateJob(context.Background(), store.NewJob{
		Kind: string(pods.KindReset), Inputs: json.RawMessage(`{"kind":"reset","teams":"1"}`), Plan: json.RawMessage(`{"teams":5}`),
		Fingerprint: "fp", LockKeys: []string{"team:01"}, CreatedBy: "olive@example.org", CreatedAs: "olive@auth.example.org",
		Items: []store.NewItem{{Name: "team01-dc", Team: "01", VMID: 10101}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusInterrupted})
	op := h.login(asOperator)
	body := h.get(&op, "/logs/"+itoa(id)).Body.String()
	lacks(t, "operator's page of a job with an unreadable plan", body, "lead only")
}
