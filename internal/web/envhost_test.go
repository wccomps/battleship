package web

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/status"
)

func TestEnvHostSheet(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	addEnvs(h)
	h.api.CPUModels = []string{"lab-baseline", "lab-baseline-nested"}
	h.api.VMs[2101].Config["ide0"] = "local:iso/debian.iso,media=cdrom"
	sess := h.login(asLead)

	rec := h.get(&sess, "/environments/host?vmid=2101")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /environments/host?vmid=2101 = %d\n%s", rec.Code, rec.Body)
	}
	if got := h.api.Called("config:2400"); got != 0 {
		t.Errorf("GET /environments/host?vmid=2101 read 2400's config in dev-04 %d times, want 0", got)
	}
	body := rec.Body.String()
	contains(t, "teak's sheet", body,
		`<h1 id="host-title"><span class="vm">teak.kilo.alpha</span> <span class="chip">linux</span></h1>`,
		`<dt>Environment</dt><dd><span class="vm">kilo.alpha</span> · <span class="vm">dev-01</span></dd>`,
		`<dt>VM</dt><dd>VMID <span class="vm">2101</span> · <span class="vm">n1</span></dd>`,
		`<dt>Address</dt><dd id="host-addr"><code>192.168.100.10</code> <span class="meta">· DNS router</span></dd>`,
		`<dt>Base image</dt><dd><code>debian-12-v2</code> · <a href="/images/family/debian-12">open debian-12</a></dd>`,
		`<dt>CPU</dt><dd><code>default (kvm64)</code></dd>`,
		`<span class="name">CD-ROM</span> attached`,
		`<a class="btn" href="/environments/edit?vmid=2101" data-panel-link>`,
		`<div class="notes box">debian / Changeme1!</div>`,
		`Eject CD-ROM</button>`,
		`<input type="hidden" name="cpu" value="lab-baseline">`,
		`Set CPU lab-baseline</button>`,
		`action="/environments/power/preview"`,
		`action="/environments/delete/preview"`,
		`<input type="hidden" name="env_host" value="teak.kilo.alpha">`,
	)

	// Router sheet shows gateway address and router chip.
	gw := h.get(&sess, "/environments/host?vmid=2100").Body.String()
	contains(t, "gw's sheet", gw,
		`<span class="chip">router</span>`,
		`<code>192.168.100.2</code>`,
	)

	if code := h.get(&sess, "/environments/host?vmid=9999").Code; code != http.StatusNotFound {
		t.Errorf("unknown VMID = %d, want 404", code)
	}
	if code := h.get(&sess, "/environments/host?vmid=abc").Code; code != http.StatusNotFound {
		t.Errorf("bad VMID = %d, want 404", code)
	}
}

func TestEnvEditFormAndConfirm(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	addEnvs(h)
	h.api.CPUModels = []string{"lab-baseline", "lab-baseline-nested"}
	h.api.VMs[2101].Config["agent"] = "1"
	h.api.VMs[2101].Config["ide0"] = "local:iso/debian.iso,media=cdrom"
	sess := h.login(asLead)

	before := h.api.Called("config:2101")
	form := h.get(&sess, "/environments/edit?vmid=2101").Body.String()
	if got := h.api.Called("config:2101") - before; got != 1 {
		t.Errorf("GET /environments/edit read 2101's config %d times, want 1", got)
	}
	if got := h.api.Called("config:2400"); got != 0 {
		t.Errorf("GET /environments/edit?vmid=2101 read 2400's config in dev-04 %d times, want 0", got)
	}
	contains(t, "edit form", form,
		`name="vmid" value="2101"`,
		`name="env_host" value="teak"`,
		`<input type="radio" name="image_kind" value="linux" checked>`,
		`name="octet" value="10"`,
		`<option value="debian-12-v2" selected>debian-12-v2 (linux)</option>`,
		`<input type="radio" name="cpu" value="lab-baseline"><span>lab-baseline</span>`,
		`<input type="radio" name="cpu" value="lab-baseline-nested"><span>lab-baseline-nested</span>`,
		`<input type="radio" name="cpu" value="" checked><span>keep default (kvm64)</span>`,
		`name="eject_cdrom" value="yes"`,
		`debian / Changeme1!`,
	)

	action, fields, review := h.preview(&sess, "/environments/edit", url.Values{
		"vmid":        {"2101"},
		"env_host":    {"oak"},
		"image_kind":  {"linux"},
		"octet":       {"25"},
		"image":       {"debian-12-v1"},
		"cpu":         {"lab-baseline"},
		"eject_cdrom": {"yes"},
		"details":     {"with-notes"},
		"detail_os":   {"Debian 12"},
		"notes":       {"updated notes"},
	})
	if action != "/environments/edit/confirm" {
		t.Fatalf("confirm action = %q\n%s", action, review)
	}
	contains(t, "edit review", review,
		"oak.kilo.alpha",
		"192.168.100.25",
		"debian-12-v1",
		"lab-baseline",
		"ejects attached install media",
		"Debian 12",
		"updated notes",
	)
	rec := h.post(&sess, action, fields)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 || js[0].Kind != string(pods.KindEnvEdit) {
		t.Fatalf("jobs = %+v, want one env-edit", js)
	}
	in := readInputs(js[0])
	if in.VMID != 2101 || in.EnvHost != "oak" || in.Octet != 25 || in.Image != "debian-12-v1" || in.CPU != "lab-baseline" || !in.EjectCDROM {
		t.Errorf("inputs = %+v", in)
	}
	page := h.get(&sess, rec.Header().Get("Location")).Body.String()
	contains(t, "job page", page, "Edit host", "kilo.alpha", "2101")
}

func TestEnvHostPowerAndDelete(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	addEnvs(h)
	sess := h.login(asLead)

	action, fields, _ := h.preview(&sess, "/environments/power", url.Values{
		"vmid":   {"2101"},
		"action": {"reboot"},
	})
	if rec := h.post(&sess, action, fields); rec.Code != http.StatusSeeOther {
		t.Fatalf("power confirm = %d\n%s", rec.Code, rec.Body)
	}

	action, fields, review := h.preview(&sess, "/environments/delete", url.Values{
		"vmid":     {"2101"},
		"env_host": {"teak.kilo.alpha"},
	})
	contains(t, "delete review", review, "teak.kilo.alpha", `name="typed"`)
	// Refused without typed confirmation.
	if rec := h.post(&sess, action, fields); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("untyped delete confirm = %d, want 422", rec.Code)
	}

	action, fields, _ = h.preview(&sess, "/environments/delete", url.Values{
		"vmid":     {"2101"},
		"env_host": {"teak.kilo.alpha"},
	})
	fields.Set("typed", "teak.kilo.alpha")
	if rec := h.post(&sess, action, fields); rec.Code != http.StatusSeeOther {
		t.Fatalf("typed delete confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 2 || js[0].Kind != string(pods.KindEnvDeleteHost) || js[1].Kind != string(pods.KindEnvPower) {
		t.Fatalf("jobs = %+v", js)
	}
}

func TestBrowserEnvHostPanel(t *testing.T) {
	h := browserHarness(t)
	addCatalog(h)
	addEnvs(h)
	h.api.CPUModels = []string{"lab-baseline"}
	h.api.VMs[2101].Config["ide0"] = "local:iso/debian.iso,media=cdrom"
	lead := h.login(asLead)
	b := newBrowser(t, h, &lead, 1280, 800, false)
	defer b.clean()

	b.open("/environments")
	b.eval(`(function(){var d=document.querySelector('#kilo\\.alpha summary .acts');d.dispatchEvent(new MouseEvent("click",{bubbles:true,cancelable:true,button:0}))})()`, nil)
	if !b.is(`document.querySelector('#kilo\\.alpha > details').open`) {
		t.Errorf("clicking summary .acts folded its environment")
	}
	b.click(`a.fam[href="/environments/host?vmid=2101"]`)
	b.waitFor("the host sheet", `document.querySelector("#panel[open] #host-title") !== null`)
	if !b.is(`document.querySelector('#kilo\\.alpha > details').open && document.querySelector('#kilo\\.alpha table.envt').checkVisibility()`) {
		t.Errorf("clicking a host folded its environment")
	}
	b.noSideways("host sheet")
	b.shot("envhost-panel")

	b.click(`#panel a[href="/environments/edit?vmid=2101"]`)
	b.waitFor("the edit host form", `document.querySelector("#panel[open] input[name=env_host]") !== null`)
	b.noSideways("edit host form")

	// Deleting a host from the panel updates the Environments page behind it
	// without a manual reload.
	b.click(`#panel .pf a[href="/environments/host?vmid=2101"]`)
	b.waitFor("the host sheet again", `document.querySelector("#panel[open] #host-title") !== null`)
	b.click(`#panel form[action="/environments/delete/preview"] button`)
	b.waitFor("the delete review", `document.querySelector("#panel[open] input[name=typed]") !== null`)
	b.eval(`(function(){var i=document.querySelector("#panel input[name=typed]");i.value="teak.kilo.alpha";i.dispatchEvent(new Event("input",{bubbles:true}))})()`, nil)
	b.click(`#panel .pf button[type=submit]`)
	b.waitFor("the delete job in the panel", `document.querySelector("#panel[open] #job-live") !== null`)
	j := h.jobsInStore()[0]
	h.runPlan(j)
	h.hub.Publish(status.Msg{Topic: status.JobTopic(j.ID)})
	h.clock.Advance(time.Second)
	b.waitFor("teak removed from Environments", `document.querySelector('a.fam[href="/environments/host?vmid=2101"]') === null`)
}
