package web

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// eventAt records a log line of job id at a given time.
func (h *harness) eventAt(id int64, at time.Time, item, step, status, msg string) {
	h.t.Helper()
	if err := h.st.AddEvent(context.Background(), id, store.Event{At: at, Item: item, Step: step, Status: status, Message: msg}); err != nil {
		h.t.Fatal(err)
	}
}

// startAt starts job id and sets the clock to its start plus d, so its
// running time reads d.
func (h *harness) startAt(id int64, d time.Duration) store.Job {
	h.t.Helper()
	h.start(id)
	j, err := h.st.Job(context.Background(), id)
	if err != nil || j.StartedAt == nil {
		h.t.Fatalf("job %d: %+v, %v", id, j, err)
	}
	h.clock.Advance(j.StartedAt.Add(d).Sub(h.clock.Now()))
	return j
}

// steps declared in package pods, read from its source.
func podsSteps(t *testing.T) []pods.Step {
	t.Helper()
	files, err := filepath.Glob("../pods/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("pods sources: %v %v", files, err)
	}
	var steps []pods.Step
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Step" {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: a Step that isn't a string literal", fset.Position(v.Pos()))
					}
					s, _ := strconv.Unquote(lit.Value)
					steps = append(steps, pods.Step(s))
				}
			}
		}
	}
	return steps
}

// Every step a plan can hold reads in words as a VM's current step.
func TestEveryStepHasWords(t *testing.T) {
	steps := podsSteps(t)
	if len(steps) < 17 {
		t.Fatalf("found %d steps in package pods: %v", len(steps), steps)
	}
	for _, st := range steps {
		w, ok := stepDoing[st]
		if !ok || w == "" || w == string(st) {
			t.Errorf("step %q has no words for the job page (stepDoing)", st)
		}
	}
	for action, want := range powerDoing {
		if got := doingOf(pods.StepPower, pods.Item{Action: action}); got != want {
			t.Errorf("power %s reads %q, want %q", action, got, want)
		}
	}
}

// itemNamed is job id's item called name.
func (h *harness) itemNamed(id int64, name string) store.Item {
	h.t.Helper()
	items, err := h.st.Items(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, it := range items {
		if it.Name == name {
			return it
		}
	}
	h.t.Fatalf("job %d has no item %s: %+v", id, name, items)
	return store.Item{}
}

// vmRowOf is the table row of VM name in a job page.
func vmRowOf(t *testing.T, body, name string) string {
	t.Helper()
	i := strings.Index(body, `<td><span class="vm">`+name+`</span></td>`)
	if i < 0 {
		t.Fatalf("no row for %s:\n%s", name, body)
	}
	start := strings.LastIndex(body[:i], "<tr")
	end := strings.Index(body[i:], "</tr>")
	return body[start : i+end]
}

// A running VM's row names its step in words, what it last logged on that
// step, and how long it has been on it; a done VM says it's ready and where.
// The job's status says how long it has run. One spinner: the job's.
func TestJobRowsSayStepAndTime(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindEnvAdd, Domain: "kilo.alpha", EnvHosts: []string{"oak=debian-12", "ash=debian-12", "elm=debian-12"}}, byLead)
	j := h.startAt(id, 3*time.Minute+12*time.Second)
	var plan pods.Plan
	if err := json.Unmarshal(j.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	steps := map[string][]pods.Step{}
	ips := map[string]string{}
	for _, it := range plan.Items {
		steps[it.Name] = it.Steps
		ips[it.Name] = addressOf(it)
	}
	oak := steps["oak.kilo.alpha"]
	wait := -1
	for i, st := range oak {
		if st == pods.StepWaitAgent {
			wait = i
		}
	}
	if wait < 1 || ips["ash.kilo.alpha"] == "" {
		t.Fatalf("plan: oak's steps %v, ash's address %q; want a wait for the agent after the clone, and an address", oak, ips["ash.kilo.alpha"])
	}
	now := h.clock.Now()
	h.eventAt(id, now.Add(-5*time.Minute), "oak.kilo.alpha", string(oak[0]), "started", "")
	for i, st := range oak[:wait] {
		ev := "done"
		if i == 1 {
			ev = "skipped"
		}
		h.eventAt(id, now.Add(-151*time.Second), "oak.kilo.alpha", string(st), ev, "")
	}
	h.eventAt(id, now.Add(-30*time.Second), "oak.kilo.alpha", string(pods.StepWaitAgent), "info", "first boot <b>slow</b>")
	// ash is done; elm started its clone 12 s ago.
	for _, st := range steps["ash.kilo.alpha"] {
		h.eventAt(id, now.Add(-time.Minute), "ash.kilo.alpha", string(st), "done", "")
	}
	h.eventAt(id, now.Add(-time.Minute), "ash.kilo.alpha", "", string(apply.EventSucceeded), "")
	h.eventAt(id, now.Add(-12*time.Second), "elm.kilo.alpha", string(steps["elm.kilo.alpha"][0]), "started", "")
	if it := h.itemNamed(id, "oak.kilo.alpha"); !it.StepAt.Equal(now.Add(-151 * time.Second)) {
		t.Fatalf("oak's step began %v, want %v (the info line must leave it)", it.StepAt, now.Add(-151*time.Second))
	}

	sess := h.login(asLead)
	body := h.get(&sess, "/logs/"+itoa(id)).Body.String()
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	contains(t, "job head", body, `running · <time data-tick="`+ms(*j.StartedAt)+`">3:12</time></span>`, h.dataNow())
	contains(t, "oak's row", vmRowOf(t, body, "oak.kilo.alpha"),
		`<span class="pip run" title="wait-agent: running">`,
		`<span class="step">Waiting for the guest agent</span> <span class="meta">· first boot &lt;b&gt;slow&lt;/b&gt;</span>`,
		`<td class="c2 r"><time data-tick="`+ms(now.Add(-151*time.Second))+`">2:31</time></td>`)
	contains(t, "elm's row", vmRowOf(t, body, "elm.kilo.alpha"),
		`<span class="step">Cloning</span></td><td class="c2 r"><time data-tick="`+ms(now.Add(-12*time.Second))+`">0:12</time></td>`)
	contains(t, "ash's row", vmRowOf(t, body, "ash.kilo.alpha"),
		`<span class="st s-ok">`, `Ready · `+ips["ash.kilo.alpha"]+`</span></td><td class="c2 r"></td>`)
	if n := strings.Count(body, `class="spin"`); n != 1 {
		t.Errorf("%d spinners, want the job status's only", n)
	}

	// A step's end clears its note.
	h.eventAt(id, now, "oak.kilo.alpha", string(pods.StepWaitAgent), "done", "")
	row := vmRowOf(t, h.get(&sess, "/logs/"+itoa(id)).Body.String(), "oak.kilo.alpha")
	contains(t, "oak's row after the wait", row, `<span class="step">`+stepDoing[oak[wait+1]]+`</span></td>`, `">0:00</time>`)
	lacks(t, "oak's row after the wait", row, "first boot")

	// elm's clone fails: it reads failed until a retry round starts it
	// again.
	h.eventAt(id, now, "elm.kilo.alpha", string(steps["elm.kilo.alpha"][0]), "failed", "clone: storage full")
	row = vmRowOf(t, h.get(&sess, "/logs/"+itoa(id)).Body.String(), "elm.kilo.alpha")
	contains(t, "elm's row after its clone failed", row, `<tr class="bad">`, `Failed</span><span class="err">clone: storage full</span></td><td class="c2 r"></td>`)
	lacks(t, "elm's row after its clone failed", row, "pip run")
	h.eventAt(id, now, "elm.kilo.alpha", string(steps["elm.kilo.alpha"][0]), "started", "")
	row = vmRowOf(t, h.get(&sess, "/logs/"+itoa(id)).Body.String(), "elm.kilo.alpha")
	contains(t, "elm's row in a retry round", row, `<span class="pip run" title="clone: running">`, `<span class="step">Cloning</span>`)

	// A worker from before items had a succeeded event (a rolling update)
	// leaves a VM whose steps are all done running until the job ends: it
	// reads running, with no step under way.
	for _, st := range oak[wait:] {
		h.eventAt(id, now, "oak.kilo.alpha", string(st), "done", "")
	}
	row = vmRowOf(t, h.get(&sess, "/logs/"+itoa(id)).Body.String(), "oak.kilo.alpha")
	contains(t, "oak's row with every step done", row, `Running</span>`)
	lacks(t, "oak's row with every step done", row, "pip run", `class="step"`, "Ready")

	// Finished: no times tick; done rows read as their outcome.
	h.finish(id, store.Outcome{Status: store.StatusSucceeded, Items: map[string]store.ItemOutcome{
		"oak.kilo.alpha": {Status: store.ItemDone}, "ash.kilo.alpha": {Status: store.ItemDone}, "elm.kilo.alpha": {Status: store.ItemDone},
	}})
	body = h.get(&sess, "/logs/"+itoa(id)).Body.String()
	lacks(t, "finished job", body, "data-tick", `class="step"`)
	contains(t, "finished job", body, `succeeded · <time>`, `Ready · `+ips["oak.kilo.alpha"]+`</span>`)
}

// While a deploy builds a template, the pending VMs waiting to clone from it
// show what the template build is doing and sort above idle pending VMs.
func TestDeployRowsShowTemplateBuildProgress(t *testing.T) {
	h := newHarness(t)
	addMasters(h)
	h.noTeamVMs()
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindDeploy, Teams: "1", Pattern: "*.kilo.alpha"}, byLead)
	h.startAt(id, 45*time.Second)
	now := h.clock.Now()
	// web's template starts building while dc's waits for a slot.
	h.eventAt(id, now.Add(-20*time.Second), "web.kilo.alpha.tpl", string(pods.StepClone), string(apply.EventInfo), "cloning master web.kilo.alpha")
	sess := h.login(asLead)
	body := h.get(&sess, "/logs/"+itoa(id)).Body.String()
	webRow := vmRowOf(t, body, "team01-web")
	dcRow := vmRowOf(t, body, "team01-dc")
	contains(t, "web's row while its template builds", webRow,
		`Pending</span> <span class="meta">· cloning master <span class="name">web.kilo.alpha</span></span>`)
	lacks(t, "dc's row while its template waits", dcRow, `class="meta"`)
	if strings.Index(body, `>team01-web<`) > strings.Index(body, `>team01-dc<`) {
		t.Errorf("team01-web (building its template) sorted below idle team01-dc")
	}

	// Once the template is created, the note clears.
	h.eventAt(id, now, "web.kilo.alpha.tpl", string(pods.StepClone), string(apply.EventDone), "template created")
	webRow = vmRowOf(t, h.get(&sess, "/logs/"+itoa(id)).Body.String(), "team01-web")
	lacks(t, "web's row after its template finishes", webRow, "cloning master")
}

// Done rows read as what the job's kind makes of them.
func TestDoneRowsByKind(t *testing.T) {
	h := newHarness(t)
	h.poll()
	sess := h.login(asOperator)
	for _, c := range []struct {
		in   jobs.Inputs
		want string
	}{
		{jobs.Inputs{Kind: pods.KindTeardown, Teams: "1", Hosts: []string{"dc"}}, `Deleted</span>`},
		{jobs.Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "start"}, `Done</span>`},
	} {
		id := h.submitJob(c.in, byLead)
		h.start(id)
		h.finish(id, store.Outcome{Status: store.StatusSucceeded, Items: map[string]store.ItemOutcome{"team01-dc": {Status: store.ItemDone}}})
		contains(t, string(c.in.Kind), vmRowOf(t, h.get(&sess, "/logs/"+itoa(id)).Body.String(), "team01-dc"), c.want)
	}
}

// storeJob stores a job of in's kind with in as its inputs and no items,
// ended with status (or left pending if "").
func (h *harness) storeJob(in jobs.Inputs, status string) int64 {
	h.t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		h.t.Fatal(err)
	}
	id, err := h.st.CreateJob(context.Background(), store.NewJob{
		Kind: string(in.Kind), Inputs: raw, Plan: json.RawMessage(`{"kind":"` + string(in.Kind) + `"}`), Fingerprint: "fp",
		LockKeys: []string{"x:" + strconv.Itoa(int(time.Now().UnixNano()))}, CreatedBy: "lena@example.org", CreatedAs: "lena@auth.example.org",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if status != "" {
		h.start(id)
		if status != store.StatusRunning {
			h.finish(id, store.Outcome{Status: status})
		}
	}
	return id
}

// A finished environment job opens its environment, an image job its
// family; a teardown or team job opens nothing. Only once it succeeded,
// at least in part.
func TestFinishedJobOpensItsResult(t *testing.T) {
	h := newHarness(t)
	sess := h.login(asLead)
	for _, c := range []struct {
		in   jobs.Inputs
		want string // "" for no button
	}{
		{jobs.Inputs{Kind: pods.KindEnvCreate, Domain: "kilo.bravo"}, `<a class="btn pri" href="/environments#kilo.bravo">Open kilo.bravo</a>`},
		{jobs.Inputs{Kind: pods.KindEnvAdd, Domain: "kilo.alpha"}, `<a class="btn pri" href="/environments#kilo.alpha">Open kilo.alpha</a>`},
		{jobs.Inputs{Kind: pods.KindImageRegister, ImageRequest: pods.ImageRequest{Family: "debian-12", VMID: 150}}, `<a class="btn pri" href="/images/family/debian-12">Open debian-12</a>`},
		{jobs.Inputs{Kind: pods.KindImageEdit, ImageRequest: pods.ImageRequest{Family: "win2022"}}, `<a class="btn pri" href="/images/family/win2022">Open win2022</a>`},
		{jobs.Inputs{Kind: pods.KindImageDeprecate, ImageRequest: pods.ImageRequest{Image: "debian-12-v1"}}, `<a class="btn pri" href="/images/family/debian-12">Open debian-12</a>`},
		{jobs.Inputs{Kind: pods.KindImageDelete, ImageRequest: pods.ImageRequest{Image: "debian-12-v1"}}, `<a class="btn pri" href="/images/family/debian-12">Open debian-12</a>`},
		{jobs.Inputs{Kind: pods.KindEnvTeardown, Domain: "kilo.alpha"}, ""},
		{jobs.Inputs{Kind: pods.KindTeardown, Teams: "1"}, ""},
		{jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, ""},
	} {
		for _, status := range []string{store.StatusSucceeded, store.StatusCompletedWithFailures, store.StatusFailed, store.StatusRunning} {
			id := h.storeJob(c.in, status)
			body := h.get(&sess, "/logs/"+itoa(id)).Body.String()
			actions := pieceOf(t, body, `<div class="pf-acts" id="job-actions">`)
			want := c.want
			if status == store.StatusFailed || status == store.StatusRunning {
				want = ""
			}
			if want == "" && strings.Contains(actions, ">Open ") || want != "" && !strings.Contains(actions, want) {
				t.Errorf("%s %s: actions %s, want %q", c.in.Kind, status, actions, want)
			}
		}
	}
}

// pieceOf is the element of body starting at start, up to its </div>.
func pieceOf(t *testing.T, body, start string) string {
	t.Helper()
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("no %s in:\n%s", start, body)
	}
	end := strings.Index(body[i:], "</div>")
	return body[i : i+end]
}

// The log is folded, with its line count, unless the job or a line failed.
func TestJobLogFolds(t *testing.T) {
	h := newHarness(t)
	h.poll()
	sess := h.login(asOperator)
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	h.start(id)
	h.event(id, "team01-dc", "power", "done", "")
	body := h.get(&sess, "/logs/"+itoa(id)).Body.String()
	contains(t, "running job", body, `<details class="logbox" id="job-logbox">`+"\n"+`<summary class="disc" id="job-log-n">Log · 1 line</summary>`)
	h.event(id, "team01-web", "power", "failed", "start: VM is locked")
	body = h.get(&sess, "/logs/"+itoa(id)).Body.String()
	contains(t, "running job with a failed line", body, `<details class="logbox" id="job-logbox" open>`+"\n"+`<summary class="disc" id="job-log-n">Log · 2 lines</summary>`)
	panel := h.panelGet(&sess, "/logs/"+itoa(id), nil).Body.String()
	contains(t, "panel with a failed line", panel, `<details class="logbox" id="job-logbox" open>`)

	// A job that failed without a line about it.
	failed := h.storeJob(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Action: "start"}, store.StatusFailed)
	contains(t, "failed job", h.get(&sess, "/logs/"+itoa(failed)).Body.String(), `<details class="logbox" id="job-logbox" open>`, "Log · 0 lines")
	done := h.storeJob(jobs.Inputs{Kind: pods.KindPower, Teams: "3", Action: "start"}, store.StatusSucceeded)
	contains(t, "succeeded job", h.get(&sess, "/logs/"+itoa(done)).Body.String(), `<details class="logbox" id="job-logbox">`)
}

// Without the script, an active job's page reloads itself every 5 s; a
// finished job's doesn't, nor does the panel, which the script fills.
func TestActiveJobPageReloadsWithoutScript(t *testing.T) {
	h := newHarness(t)
	sess := h.login(asLead)
	reload := `<noscript><meta http-equiv="refresh" content="5"></noscript>`
	for status, want := range map[string]bool{
		"": true, store.StatusRunning: true,
		store.StatusSucceeded: false, store.StatusFailed: false, store.StatusCompletedWithFailures: false, store.StatusInterrupted: false,
	} {
		id := h.storeJob(jobs.Inputs{Kind: pods.KindEnvAdd, Domain: "kilo.alpha"}, status)
		page := h.get(&sess, "/logs/"+itoa(id)).Body.String()
		if got := strings.Contains(page, reload); got != want {
			t.Errorf("job %q: page reloads %v, want %v", status, got, want)
		}
		if strings.Contains(h.panelGet(&sess, "/logs/"+itoa(id), nil).Body.String(), "refresh") {
			t.Errorf("job %q: the panel refreshes", status)
		}
	}
	if strings.Contains(h.get(&sess, "/logs").Body.String(), "refresh") {
		t.Error("the job list refreshes")
	}
}

// The job's head names what it acts on: its teams, environment and slot,
// or image family.
func TestJobHeadNamesItsSubject(t *testing.T) {
	h := newHarness(t)
	sess := h.login(asLead)
	for _, c := range []struct {
		in   jobs.Inputs
		want []string
	}{
		{jobs.Inputs{Kind: pods.KindPower, Teams: "1-3", Action: "start", Hosts: []string{"dc"}}, []string{`pods <span class="vm">01-03</span> · <span class="vm">dc</span>`}},
		{jobs.Inputs{Kind: pods.KindEnvCreate, Domain: "kilo.bravo", Slot: "02"}, []string{`Domain <span class="vm">kilo.bravo</span>`, `Slot <span class="vm">` + pods.ExpandSlot(h.cfg.Environments.Pool, 2) + `</span>`}},
		{jobs.Inputs{Kind: pods.KindImageRegister, ImageRequest: pods.ImageRequest{Family: "debian-12", VMID: 150}}, []string{`Family <span class="vm">debian-12</span>`}},
		{jobs.Inputs{Kind: pods.KindImageDeprecate, ImageRequest: pods.ImageRequest{Image: "debian-12-v1"}}, []string{`Image <span class="vm">debian-12-v1</span>`}},
	} {
		id := h.storeJob(c.in, store.StatusRunning)
		contains(t, string(c.in.Kind)+" head", pieceOf(t, h.get(&sess, "/logs/"+itoa(id)).Body.String(), `<div class="sec" id="job-head">`), c.want...)
	}
}

func TestClockText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0:00", 0: "0:00", 7 * time.Second: "0:07", 999 * time.Millisecond: "0:00", 3*time.Minute + 12*time.Second: "3:12",
		59*time.Minute + 59*time.Second: "59:59", time.Hour: "1:00:00", time.Hour + 2*time.Minute + 9*time.Second: "1:02:09",
	} {
		if got := clockText(d); got != want {
			t.Errorf("clockText(%v) = %q, want %q", d, got, want)
		}
	}
}

// runEnvAdd submits an environment add of oak, ash and elm and starts it
// 3:12 ago: oak waits for its agent, ash is done, elm clones. It returns
// the job.
func runEnvAdd(h *harness) int64 {
	h.t.Helper()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindEnvAdd, Domain: "kilo.alpha", EnvHosts: []string{"oak=debian-12", "ash=debian-12", "elm=debian-12"}}, byLead)
	j := h.startAt(id, 3*time.Minute+12*time.Second)
	var plan pods.Plan
	if err := json.Unmarshal(j.Plan, &plan); err != nil {
		h.t.Fatal(err)
	}
	now := h.clock.Now()
	for _, it := range plan.Items {
		switch it.Name {
		case "oak.kilo.alpha":
			for _, st := range it.Steps {
				if st == pods.StepWaitAgent {
					h.eventAt(id, now.Add(-2*time.Minute), it.Name, string(st), "info", "first boot")
					break
				}
				h.eventAt(id, now.Add(-151*time.Second), it.Name, string(st), "done", "")
			}
		case "ash.kilo.alpha":
			for _, st := range it.Steps {
				h.eventAt(id, now.Add(-time.Minute), it.Name, string(st), "done", "")
			}
			h.eventAt(id, now.Add(-time.Minute), it.Name, "", string(apply.EventSucceeded), "")
		case "elm.kilo.alpha":
			h.eventAt(id, now.Add(-12*time.Second), it.Name, string(it.Steps[0]), "started", "")
		}
	}
	return id
}

// stillButOne is whether only one thing on the page moves: the job's
// status spinner.
const stillButOne = `(function(){var a=[...document.querySelectorAll("main *")].filter(e => getComputedStyle(e).animationName !== "none");` +
	`return a.length === 1 && a[0].matches("#job-head .spin")})()`

// An environment job's page at phone and laptop, light and dark: one
// spinner, times that tick, the log folded until a line fails, and Open
// once it finishes.
func TestBrowserEnvJob(t *testing.T) {
	h := browserHarness(t)
	addImages(h)
	addEnv(h, true)
	h.poll()
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			id := runEnvAdd(h)
			lead := h.login(asLead) // after the clock moved to the job's start
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/logs/" + itoa(id))
			b.waitFor("the rows", `document.querySelectorAll("#job-items time").length === 2`)
			if !b.is(stillButOne) {
				t.Errorf("more than the job's status moves: %s", b.where())
			}
			if got := b.text(".jobt tr:has(.step) .step"); got == "" {
				t.Errorf("no step in words")
			}
			if b.is(`document.getElementById("job-logbox").open`) || !strings.HasPrefix(b.text("#job-log-n"), "Log · ") {
				t.Errorf("log: open %v, summary %q", b.is(`document.getElementById("job-logbox").open`), b.text("#job-log-n"))
			}
			b.eval(`window.was = document.querySelector("#job-items time").textContent`, nil)
			b.waitFor("the time on the step to tick", `document.querySelector("#job-items time").textContent !== window.was`)
			// Reckoned on the server's clock, 3:12 after the job started.
			if got := b.text("#job-head time"); !strings.HasPrefix(got, "3:") {
				t.Errorf("the job has run %q, want 3:1x", got)
			}
			if cards := b.is(`getComputedStyle(document.querySelector(".jobt tbody tr")).display === "grid"`); cards != b.phone {
				t.Errorf("phone %v: rows are cards: %v", b.phone, cards)
			}
			b.noSideways("running environment job")
			b.shot("job-env-running-" + size.name)

			// A failed line unfolds the log as it arrives.
			n := b.text("#job-log-n")
			h.event(id, "elm.kilo.alpha", "clone", "failed", "clone: storage full")
			publishJob(h, id)
			b.waitFor("the log to unfold", `document.getElementById("job-logbox").open`)
			if got := b.text("#job-log-n"); got == n {
				t.Errorf("the log still says %q", got)
			}
			h.finish(id, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
				"oak.kilo.alpha": {Status: store.ItemDone}, "ash.kilo.alpha": {Status: store.ItemDone},
				"elm.kilo.alpha": {Status: store.ItemFailed, Error: "clone: storage full"},
			}})
			publishJob(h, id)
			b.waitFor("Open once it finished", `!!document.querySelector('#job-actions a[href="/environments#kilo.alpha"]')`)
			if b.is(`!!document.querySelector("[data-tick]")`) || b.is(`!!document.querySelector("main .spin")`) {
				t.Errorf("a finished job still ticks or spins")
			}
			b.noSideways("finished environment job")
			b.shot("job-env-failures-" + size.name)

			// A clean finish, as a fresh page.
			ok := runEnvAdd(h)
			h.finish(ok, store.Outcome{Status: store.StatusSucceeded, Items: map[string]store.ItemOutcome{
				"oak.kilo.alpha": {Status: store.ItemDone}, "ash.kilo.alpha": {Status: store.ItemDone}, "elm.kilo.alpha": {Status: store.ItemDone},
			}})
			b.open("/logs/" + itoa(ok))
			if got := b.text("#job-actions"); got != "Open kilo.alpha" {
				t.Errorf("finished job's actions = %q", got)
			}
			if b.is(`document.getElementById("job-logbox").open`) {
				t.Errorf("a clean job's log is open")
			}
			b.noSideways("finished environment job")
			b.shot("job-env-done-" + size.name)
			b.clean()
		})
	}
}

// A deploy of 32 teams × 12 VMs reads well on its page and in the panel:
// summed up above the table, still but for the job's status, and the panel
// folds the done rows.
func TestBrowserBigDeployJob(t *testing.T) {
	h := browserHarness(t, func(c *config.Config) { c.Web.Templates = "*.kilo.alpha" })
	h.api.Mu.Lock()
	clear(h.api.VMs)
	h.api.Mu.Unlock()
	for i, host := range competitionHosts {
		h.api.add(proxmox.VM{VMID: 5001 + i, Name: host + ".kilo.alpha", Node: []string{"n1", "n2"}[i%2], Status: "running", Tags: "dev"},
			map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0"})
	}
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindDeploy, Teams: "1-32", Pattern: "*.kilo.alpha"}, byLead)
	j := h.startAt(id, 9*time.Minute+41*time.Second)
	var plan pods.Plan
	if err := json.Unmarshal(j.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 32*len(competitionHosts) {
		t.Fatalf("%d items, want %d", len(plan.Items), 32*len(competitionHosts))
	}
	now := h.clock.Now()
	for i, it := range plan.Items {
		team := i / len(competitionHosts)
		switch {
		case team < 12: // done
			for _, st := range it.Steps {
				h.eventAt(id, now.Add(-time.Minute), it.Name, string(st), "done", "")
			}
			h.eventAt(id, now.Add(-time.Minute), it.Name, "", string(apply.EventSucceeded), "")
		case team < 14: // running, at different steps
			k := i % len(it.Steps)
			h.eventAt(id, now.Add(-3*time.Minute), it.Name, string(it.Steps[0]), "started", "")
			for _, st := range it.Steps[:k] {
				h.eventAt(id, now.Add(-time.Duration(i)*time.Second), it.Name, string(st), "done", "")
			}
		case team == 14 && i%len(competitionHosts) == 0:
			h.eventAt(id, now.Add(-time.Minute), it.Name, string(it.Steps[0]), "failed", "clone: storage full on n1")
		}
	}
	lead := h.login(asLead)
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/logs/" + itoa(id))
			if !b.is(`!!document.querySelector("#job-items .pips") && !!document.querySelector("#job-items .tally")`) {
				t.Errorf("a 384-VM job isn't summed up")
			}
			if got := b.text("#job-items .tally"); !strings.Contains(got, "1 failed") || !strings.Contains(got, "144 done") {
				t.Errorf("tally = %q", got)
			}
			if !b.is(stillButOne) {
				t.Errorf("more than the job's status moves: %s", b.where())
			}
			if !b.is(`document.getElementById("job-logbox").open`) {
				t.Errorf("the log of a job with a failed line is folded")
			}
			if n := b.text("#job-items tbody tr:not(.fold-n)"); n == "" {
				t.Error("no rows")
			}
			b.noSideways("big deploy page")
			b.shot("job-big-" + size.name)

			// In the panel, done rows fold into one line.
			b.open("/")
			b.eval(`(function(){var a=document.createElement("a");a.href="/logs/`+itoa(id)+`";a.setAttribute("data-panel-link","");a.textContent="job";document.querySelector("main").appendChild(a);a.click()})()`, nil)
			b.waitFor("the job panel", `!!document.querySelector("#panel[open] #job-items")`)
			if !b.is(`[...document.querySelectorAll("#panel tr.fold")].every(r => getComputedStyle(r).display === "none") && getComputedStyle(document.querySelector("#panel .fold-n")).display !== "none"`) {
				var dbg []string
				b.eval(`[...document.querySelectorAll("#panel tr.fold, #panel .fold-n")].slice(0,3).map(r => r.className + ":" + getComputedStyle(r).display)`, &dbg)
				t.Errorf("the panel doesn't fold done rows: %v", dbg)
			}
			if got := b.text("#panel .fold-n"); got != "144 done" {
				t.Errorf("fold line = %q", got)
			}
			b.noSideways("big deploy panel")
			b.shot("job-big-panel-" + size.name)
			b.clean()
		})
	}
}
