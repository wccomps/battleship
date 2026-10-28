package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
)

// tagSets gives pods 01 and 02 a set each per host: dc kilo.alpha, web
// tango.delta. Pod 03's VMs stay untagged: deployed before sets.
func tagSets(h *harness) {
	h.api.Mu.Lock()
	defer h.api.Mu.Unlock()
	for _, vm := range h.api.VMs {
		switch vm.Name {
		case "team01-dc", "team02-dc":
			vm.Config["tags"] = "dev;set-kilo.alpha"
		case "team01-web", "team02-web":
			vm.Config["tags"] = "set-tango.delta"
		}
	}
}

func TestTeardownFormSteps(t *testing.T) {
	h := newHarness(t)
	tagSets(h)
	h.poll()
	lead := h.login(asLead)

	body := h.get(&lead, "/teardown").Body.String()
	contains(t, "step 1", body, `<form method="get" action="/teardown" class="stack" id="teardown-which">`,
		`<label class="lbl" for="teams">Pods</label>`, `<button type="submit" class="btn">Next</button>`)
	lacks(t, "step 1", body, `id="teardown-pick"`, "Preview teardown")

	body = h.get(&lead, "/teardown?teams=1-3").Body.String()
	contains(t, "step 2", body,
		`<input class="inp" type="text" id="teams" name="teams" value="1-3" required`,
		`<button type="submit" class="btn">Choose again</button>`,
		`<form method="post" action="/teardown/preview" class="stack" id="teardown-pick">`,
		`<input type="hidden" name="teams" value="1-3">`,
		`<h2 class="lbl">In pods 01-03</h2>`,
		`<dt><span class="vm">kilo.alpha</span></dt><dd>1 host in each of pods 01-02</dd>`,
		`<dt><span class="vm">tango.delta</span></dt><dd>1 host in each of pods 01-02</dd>`,
		`<dt><span class="muted">unknown set</span></dt><dd>2 hosts in pod 03</dd>`,
		`<input type="radio" name="set" value="kilo.alpha" required><span>kilo.alpha</span>`,
		`<input type="radio" name="set" value="tango.delta" required><span>tango.delta</span>`,
		`<label class="opt sans"><input type="radio" name="set" value="" required><span>Everything in these pods</span>`,
		`<span class="hint">Battleship can&#39;t tell the set of 2 VMs, so only Everything in these pods removes them.</span>`,
		`form="teardown-pick">`, "Preview teardown</button>")
	lacks(t, "step 2 with several sets", body, "required checked>")

	// Narrowed by hosts, the nameless choice says "Every matching VM in
	// these pods" instead of "Everything in these pods".
	body = h.get(&lead, "/teardown?teams=1-3&hosts=dc").Body.String()
	contains(t, "step 2 narrowed by host", body,
		`<label class="opt sans"><input type="radio" name="set" value="" required><span>Every matching VM in these pods</span>`,
		`<span class="hint">Battleship can&#39;t tell the set of 1 VM, so only Every matching VM in these pods removes it.</span>`)

	body = h.get(&lead, "/teardown?teams=1&hosts=web").Body.String()
	contains(t, "pod 01's web", body,
		`<h1 id="pv-title"><span class="eyebrow">Review ·</span> Tear down <span class="vm">tango.delta</span></h1>`,
		`Tears down <span class="name">tango.delta</span> in pod 01: 1 VM.`,
		`<a class="btn first" href="/teardown?hosts=web&amp;set=tango.delta&amp;teams=1"`)
	lacks(t, "pod 01's web", body, `id="teardown-pick"`, "Every matching VM in these pods")
	back := h.get(&lead, "/teardown?hosts=web&set=tango.delta&teams=1").Body.String()
	contains(t, "back from single-set review", back, `id="teardown-which"`, `value="1"`, `value="web" checked`)
	lacks(t, "back from single-set review", back, `id="teardown-pick"`, "Every matching VM in these pods")

	body = h.get(&lead, "/teardown?teams=1-3&set=").Body.String()
	contains(t, "everything chosen", body, `value="" required checked>`)
	body = h.get(&lead, "/teardown?teams=1-3&set=Kilo.Alpha").Body.String()
	contains(t, "kilo.alpha chosen", body, `value="kilo.alpha" required checked>`)
	body = h.get(&lead, "/teardown?teams=1-3&set=kilo.bravo").Body.String()
	contains(t, "a set the pods don't hold", body, `<input type="radio" name="set" value="kilo.bravo" required checked><span>kilo.bravo</span>`)
	body = h.get(&lead, "/teardown?teams=1&hosts=web&set=kilo.bravo").Body.String()
	contains(t, "single-set pod with an unheld set", body,
		`<input type="radio" name="set" value="tango.delta" required><span>tango.delta</span>`,
		`<input type="radio" name="set" value="kilo.bravo" required checked><span>kilo.bravo</span>`,
		`<input type="radio" name="set" value="" required><span>Every matching VM in these pods</span>`)
	body = h.get(&lead, "/teardown?teams=3").Body.String()
	contains(t, "a pod with only unknown-set VMs", body,
		`<h1 id="pv-title"><span class="eyebrow">Review ·</span> Tear down <span class="vm">pod 03</span></h1>`,
		`<a class="btn first" href="/teardown?set=&amp;teams=3"`)
	lacks(t, "a pod with only unknown-set VMs", body, `id="teardown-pick"`)
	body = h.get(&lead, "/teardown?teams=9").Body.String()
	contains(t, "an empty pod", body,
		`<h1 id="pv-title"><span class="eyebrow">Review ·</span> Tear down <span class="vm">pod 09</span></h1>`,
		"No VMs in these pods match these hosts.")
	lacks(t, "an empty pod", body, `id="teardown-pick"`)
}

// The review names the set and the pods, lists the VMs it takes and says
// which it leaves; the confirm is typed and the job keeps the set.
func TestTeardownReviewOfASet(t *testing.T) {
	h := newHarness(t)
	tagSets(h)
	h.api.Mu.Lock()
	h.api.VMs[10302].Config["tags"] = "set-tango.delta;set-kilo.alpha"
	h.api.Mu.Unlock()
	h.poll()
	lead := h.login(asLead)

	action, form, body := h.preview(&lead, "/teardown", url.Values{"teams": {"1-3"}, "set": {"Kilo.Alpha"}})
	contains(t, "set review", body,
		`<h1 id="pv-title"><span class="eyebrow">Review ·</span> Tear down <span class="vm">kilo.alpha</span></h1>`,
		`Tears down <span class="name">kilo.alpha</span> in pods 01-03: 2 VMs. Each is stopped and deleted with its disks. This can&#39;t be undone.`,
		`<span class="vm">team01-dc</span>`, `<span class="vm">team02-dc</span>`,
		`<h2 class="sub">Not touched</h2>`,
		`<span class="name">team01-web</span> and <span class="name">team02-web</span> are of <span class="name">tango.delta</span>.`,
		`<span class="name">team03-dc</span> and <span class="name">team03-web</span> are of an unknown set, with no single set tag, which only a teardown that names no set removes.`,
		`Type <code>1-3</code> to confirm`, "Delete 2 VMs</button>")
	lacks(t, "set review", body, `<span class="vm">team01-web</span></td>`, `<th scope="col">Set</th>`, "<dt>Pods</dt>", "<dt>Hosts</dt>")
	if form.Get("set") != "kilo.alpha" || form.Has("pattern") {
		t.Errorf("confirm fields = %v", form)
	}
	form.Set("typed", "1-3")
	if rec := h.post(&lead, action, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 {
		t.Fatalf("jobs = %d", len(js))
	}
	var in jobs.Inputs
	if err := json.Unmarshal(js[0].Inputs, &in); err != nil || in.Set != "kilo.alpha" || in.Pattern != "" {
		t.Errorf("stored inputs = %s (%v)", js[0].Inputs, err)
	}
	job := h.get(&lead, "/logs/"+itoa(js[0].ID)).Body.String()
	contains(t, "job page", job, `Teardown <span class="vm">kilo.alpha</span>`)

	// Everything: each VM's set, distinguishing "not recorded" (no set tag)
	// from "unknown set" (several set tags).
	_, _, body = h.preview(&lead, "/teardown", url.Values{"teams": {"1-3"}, "set": {""}})
	contains(t, "everything review", body,
		`Tear down <span class="vm">pods 01-03</span></h1>`,
		"Tears down everything in pods 01-03: 6 VMs.",
		`<th scope="col">Set</th>`, `<td class="c2"><code>kilo.alpha</code></td>`,
		`<td class="c2"><code>not recorded</code></td>`, `<td class="c2"><code>unknown set</code></td>`)
	lacks(t, "everything review", body, "Not touched")

	// Narrowed by hosts with no set, the review says "every matching VM".
	_, _, body = h.preview(&lead, "/teardown", url.Values{"teams": {"1-3"}, "hosts": {"dc"}, "set": {""}})
	contains(t, "narrowed review", body,
		`Tear down <span class="vm">pods 01-03</span></h1>`,
		"Tears down every matching VM in pods 01-03: 3 VMs.",
		`<dt>Hosts</dt><dd>dc</dd>`)

	// A set the pods don't hold leaves nothing to confirm.
	body = h.post(&lead, "/teardown/preview", url.Values{"teams": {"3"}, "set": {"tango.delta"}}).Body.String()
	contains(t, "no VMs of the set", body, `No VMs of <span class="name">tango.delta</span> are in these pods and hosts.`)
	lacks(t, "no VMs of the set", body, `name="nonce"`)
	// alpha is a set of its own, not kilo.alpha's end; a pattern is refused.
	body = h.post(&lead, "/teardown/preview", url.Values{"teams": {"1-3"}, "set": {"alpha"}}).Body.String()
	contains(t, "alpha", body, `No VMs of alpha are in these pods and hosts.`)
	lacks(t, "alpha", body, `name="nonce"`)
	rec := h.post(&lead, "/teardown/preview", url.Values{"teams": {"1-3"}, "set": {"*.kilo.alpha"}})
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "is a pattern") || strings.Contains(body, `name="nonce"`) {
		t.Errorf("a pattern for the set: %d\n%s", rec.Code, body)
	}
}

// A VM's page names its set, or says it has none recorded; several set
// tags or a bare "set-" tag are drift and show "unknown set".
func TestVMPageShowsItsSet(t *testing.T) {
	h := newHarness(t)
	tagSets(h)
	h.api.Mu.Lock()
	h.api.VMs[10202].Config["tags"] = "set-tango.delta;set-kilo.alpha"
	h.api.VMs[10302].Config["tags"] = "set-"
	h.api.Mu.Unlock()
	h.poll()
	lead := h.login(asLead)
	contains(t, "pod 01's dc", h.get(&lead, "/vm/01/dc").Body.String(), `<dt>Set</dt><dd><span class="vm">kilo.alpha</span></dd>`)
	body := h.get(&lead, "/vm/03/dc").Body.String()
	contains(t, "pod 03's dc", body, `<dt>Set</dt><dd><span class="muted" title="a teardown of one set leaves it; only a teardown that names no set removes it">not recorded</span></dd>`)
	lacks(t, "pod 03's dc", body, "set tags")
	contains(t, "pod 02's web", h.get(&lead, "/vm/02/web").Body.String(), `>unknown set</span></dd>`,
		`has several set tags (<span class="name">set-kilo.alpha</span>, <span class="name">set-tango.delta</span>), so no teardown of one set takes it`)
	contains(t, "pod 03's web", h.get(&lead, "/vm/03/web").Body.String(), `>unknown set</span></dd>`,
		`has an empty set tag (set-), so no teardown of one set takes it`)
}

// A mixed-case pattern in a stored job (or URL query) still selects its
// lower-case template set radio on the deploy form instead of adding a
// duplicate mixed-case option.
func TestDeployFormMatchesMixedCaseStoredPattern(t *testing.T) {
	h := newHarness(t)
	addMasters(h)
	h.api.add(proxmox.VM{VMID: 5003, Name: "dc.tango.delta", Node: "n1", Status: "running", Tags: "dev"}, nil)
	lead := h.login(asLead)
	h.poll()
	body := h.get(&lead, "/deploy?teams=1&pattern=%2A.Tango.Delta").Body.String()
	contains(t, "deploy form with mixed-case pattern", body, `value="*.tango.delta" required data-hosts="dc" checked><span>tango.delta</span>`)
	lacks(t, "deploy form with mixed-case pattern", body, "*.Tango.Delta")
}

// When the cluster restricts tag access and the set tag is not in the
// user-tag allowlist, deploy preview checks Sys.Modify on / and blocks
// operators who lack it.
func TestDeployPreviewRespectsUserTagAccess(t *testing.T) {
	h := newHarness(t)
	addMasters(h)
	lead := h.login(asLead)
	user := "test-lead@" + pvetest.Realm
	h.pve.SetTagAccess(proxmox.TagAccess{Mode: "existing", Allowed: []string{"prod"}})
	h.pve.Revoke(user, "/")
	h.pve.Grant(user, "/", slices.DeleteFunc(slices.Clone(allPrivileges), func(p string) bool { return p == "Sys.Modify" })...)
	body := h.post(&lead, "/deploy/preview", url.Values{"teams": {"9"}, "hosts": {"dc"}, "pattern": {"*.kilo.alpha"}}).Body.String()
	contains(t, "deploy blocked by tag access", body, `you don&#39;t have <span class="name">Sys.Modify</span> on /`)
	lacks(t, "deploy blocked by tag access", body, `name="nonce"`)
}
