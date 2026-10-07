package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// The grid's Take snapshot… opens the snapshot form for the ticked VMs:
// a name, an optional description and the RAM switch, off.
func TestGridSnapshotForm(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	body := h.get(&op, "/").Body.String()
	contains(t, "grid", body, `<button type="submit" class="btn" formaction="/snapshot"`, "Take snapshot…</button>")

	rec := h.post(&op, "/snapshot", selection([]string{"team01-dc", "team01-web"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /snapshot = %d\n%s", rec.Code, rec.Body)
	}
	body = rec.Body.String()
	contains(t, "snapshot form", body,
		"<title>Take snapshot · battleship</title>",
		`<form class="sheet" method="post" action="/snapshot/preview" aria-labelledby="form-title">`,
		`<input type="hidden" name="from" value="grid">`,
		`<input type="hidden" name="teams" value="1">`,
		`<input type="hidden" name="vms" value="team01-dc,team01-web">`,
		`<ul class="hosts" aria-label="VMs"><li>team01-dc</li><li>team01-web</li></ul>`,
		`<input class="inp" type="text" id="snap-name" name="snapshot" value="" required maxlength="40" pattern="[A-Za-z][A-Za-z0-9_\-]{1,39}"`,
		`<input class="inp" type="text" id="snap-desc" name="description" value="" maxlength="500"`,
		`<label class="sw">Include RAM<input type="checkbox" name="vmstate" value="yes"></label>`,
		`<button type="submit" class="btn pri">Preview</button>`)
	lacks(t, "snapshot form", body, `id="teams"`, `checked></label>`)
	if _, err := checkMarkup(body); err != nil {
		t.Error(err)
	}

	// Without the grid, it asks for teams and hosts, and fills in from the
	// query as change links do.
	body = h.get(&op, "/snapshot?teams=2&hosts=web&snapshot=round2&description=after+lunch&vmstate=yes").Body.String()
	contains(t, "prefilled snapshot form", body,
		`<input class="inp" type="text" id="teams" name="teams" value="2" required`,
		`<input type="checkbox" name="hosts" value="web" checked>`,
		`name="snapshot" value="round2"`, `name="description" value="after lunch"`,
		`<input type="checkbox" name="vmstate" value="yes" checked>`)
}

func TestSnapshotPreviewAndConfirm(t *testing.T) {
	h := newHarness(t)
	h.poll()
	h.api.setSnapshots(10102, "initial", "round2") // team01-web has it already
	op := h.login(asOperator)

	form := selection([]string{"team01-dc", "team01-web"})
	form.Set("snapshot", "round2")
	form.Set("description", "after lunch")
	form.Set("vmstate", "yes")
	action, fields, body := h.preview(&op, "/snapshot", form)
	contains(t, "snapshot preview", body,
		"<title>Take snapshot · battleship</title>",
		`Take snapshot <span class="vm">round2</span></h1>`,
		"<dt>Description</dt><dd>after lunch</dd>", "<dt>Include RAM</dt><dd>on</dd>",
		"1 will run</span>", "1 blocked</span>",
		`<tr class="blk"><td><span class="vm">team01-web</span></td>`,
		"already has a snapshot named &#34;round2&#34;; choose another name",
		"<td>snapshot with RAM</td>",
		`<p class="warn note">`, "1 VM gets a new snapshot, round2. Its RAM is saved too if it is running, which takes longer.",
		`<button type="submit" class="btn first" formaction="/snapshot" formnovalidate>Change</button>`,
		`<button type="submit" class="btn pri">`, "Snapshot 1 VM</button>")
	lacks(t, "snapshot preview", body, `btn pri bad`, `name="typed"`)
	if action != "/snapshot/confirm" {
		t.Errorf("confirm posts to %q", action)
	}
	if fields.Get("snapshot") != "round2" || fields.Get("description") != "after lunch" || fields.Get("vmstate") != "yes" {
		t.Errorf("confirm fields = %v", fields)
	}

	rec := h.post(&op, action, fields)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 || js[0].Kind != "snapshot" || js[0].CreatedAs != "test-operator@auth.example.org" {
		t.Fatalf("jobs = %+v", js)
	}
	var in jobs.Inputs
	_ = json.Unmarshal(js[0].Inputs, &in)
	want := jobs.Inputs{Kind: pods.KindSnapshot, Teams: "1", Hosts: []string{"dc", "web"}, VMs: []string{"team01-dc", "team01-web"},
		Snapshot: "round2", Description: "after lunch", VMState: true}
	if !reflect.DeepEqual(in, want) {
		t.Errorf("inputs = %+v, want %+v", in, want)
	}
	if !reflect.DeepEqual(js[0].LockKeys, []string{"team:01"}) {
		t.Errorf("lock keys = %v", js[0].LockKeys)
	}

	// The job list and page name it.
	page := h.get(&op, "/logs/"+itoa(js[0].ID)).Body.String()
	contains(t, "job page", page, "Take snapshot", `<span class="vm">round2</span>`)
	contains(t, "job list", h.get(&op, "/logs").Body.String(), "Take snapshot <span class=\"vm\">round2</span>")
}

func TestSnapshotPreviewRefusesBadNames(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	for name, want := range map[string]string{
		"":              "A snapshot needs a name.",
		"2nd":           "must start with a letter",
		"current":       "is reserved by Proxmox",
		"initial":       "Couldn&#39;t make a plan: &#34;initial&#34; is the baseline snapshot a deploy takes (deploy.snapshot_name); choose another name.",
		"fresh_clone_1": "so a reset to the baseline could pick it",
	} {
		rec := h.post(&op, "/snapshot/preview", url.Values{"teams": {"1"}, "snapshot": {name}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("snapshot %q = %d, want 422", name, rec.Code)
		}
		contains(t, "refused snapshot "+name, rec.Body.String(), `<p class="warn" role="alert">`, want,
			`action="/snapshot/preview"`, `name="snapshot" value="`+name+`"`)
	}
	h.noJobs("after refused snapshots")
}

// Snapshotting every team needs only VM.Snapshot, like any snapshot; the
// preview flags it, and users without VM.Snapshot get no button.
func TestSnapshotOfAllTeamsSaysSo(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	nobody := h.login(asNobody)

	contains(t, "operator's grid", h.get(&op, "/").Body.String(), `formaction="/snapshot" data-needs="VM.Snapshot">`)
	lacks(t, "operator's grid", h.get(&op, "/").Body.String(), `data-lacks`)

	every := selection([]string{"team01-dc", "team02-dc", "team03-web"})
	every.Set("snapshot", "round2")
	_, _, body := h.preview(&op, "/snapshot", every)
	contains(t, "operator's all-teams snapshot", body, "This covers every team (01-03).", `action="/snapshot/confirm"`)
	lacks(t, "operator's all-teams snapshot", body, `name="typed"`) // only a teardown is typed
	_, _, body = h.preview(&op, "/snapshot", url.Values{"teams": {"1-2"}, "snapshot": {"round2"}})
	lacks(t, "operator's two-team snapshot", body, `name="typed"`)

	rec := h.post(&nobody, "/snapshot/preview", every)
	if confirmFormRE.MatchString(rec.Body.String()) {
		t.Fatalf("a user without privileges got a confirm:\n%s", rec.Body)
	}
	h.noJobs("after previews")
}

// The VM page offers Take snapshot… to whoever holds VM.Snapshot on it.
func TestCellPageOffersSnapshot(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	body := h.get(&op, "/vm/01/dc").Body.String()
	contains(t, "cell page", body,
		`<form method="post" action="/snapshot">`,
		`<button type="submit" class="btn">`, "Take snapshot…</button>")

	one := newHarness(t)
	one.api.remove("team02-dc", "team02-web", "team03-dc", "team03-web")
	one.poll()
	oop := one.login(asOperator)
	contains(t, "operator's page, one team", one.get(&oop, "/vm/01/dc").Body.String(), `action="/snapshot"`)
	nobody := one.login(asNobody)
	lacks(t, "nobody's page, one team", one.get(&nobody, "/vm/01/dc").Body.String(), `action="/snapshot"`)
	olead := one.login(asLead)
	contains(t, "lead's page, one team", one.get(&olead, "/vm/01/dc").Body.String(), `action="/snapshot"`)
}

// A finished snapshot shows on the VM page and in the reset picker, which
// read the VM live.
func TestNewSnapshotShowsOnVMPageAndResetPicker(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	id := h.submitJob(jobs.Inputs{Kind: pods.KindSnapshot, Teams: "1", Hosts: []string{"dc"}, Snapshot: "round2"}, byOperator)
	h.start(id)
	h.api.setSnapshots(10101, "initial", "before-scoring", "round2") // what the job did
	h.finish(id, store.Outcome{Status: store.StatusSucceeded, Items: map[string]store.ItemOutcome{"team01-dc": {Status: store.ItemDone}}})

	contains(t, "cell page", h.get(&op, "/vm/01/dc").Body.String(),
		`<tr><td><span class="vm">round2</span></td><td class="muted"></td></tr>`)
	contains(t, "reset picker", h.post(&op, "/reset", selection([]string{"team01-dc"})).Body.String(),
		`<input type="radio" name="snapshot" value="round2"><span>round2</span>`)
}
