package web

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
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

	for _, badOctet := range []string{"abc", "0", "-1", "none"} {
		bad := h.post(&sess, "/environments/edit/preview", url.Values{
			"vmid":       {"2101"},
			"env_host":   {"oak"},
			"image_kind": {"linux"},
			"octet":      {badOctet},
		})
		if bad.Code != http.StatusUnprocessableEntity {
			t.Errorf("preview with octet=%q = %d, want 422", badOctet, bad.Code)
		}
	}
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

// The Edit host review lists what changes, and its sentence says what
// confirming does: configure the guest, only eject the CD-ROM, only set the
// CPU, or update the host; a blocked edit says why.
func TestEnvEditReview(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	addEnvs(h)
	h.api.CPUModels = []string{"lab-baseline", "lab-baseline-nested"}
	h.api.VMs[2101].Config["agent"] = "1"
	h.api.VMs[2101].Config["ide0"] = "local:iso/debian.iso,media=cdrom"
	h.api.VMs[2102].Config["ide0"] = "local:iso/debian.iso,media=cdrom"
	h.api.VMs[2103].Config["net0"] = "virtio=BC:24:11:00:21:00,bridge=vmbr0" // off dev-01's bridges
	sess := h.login(asLead)
	review := func(form url.Values) string {
		_, _, body := h.preview(&sess, "/environments/edit", form)
		return body
	}
	elm := func(kv ...string) url.Values {
		f := url.Values{"vmid": {"2102"}, "env_host": {"elm"}, "image_kind": {"other"}, "image": {"win10-v1"}}
		for i := 0; i+1 < len(kv); i += 2 {
			f.Set(kv[i], kv[i+1])
		}
		return f
	}

	body := review(url.Values{"vmid": {"2101"}, "env_host": {"oak"}, "image_kind": {"linux"}, "octet": {"25"}, "image": {"debian-12-v1"},
		"cpu": {"lab-baseline"}, "eject_cdrom": {"yes"}, "details": {"with-notes"}, "detail_os": {"Debian 12"}, "notes": {"updated notes"}})
	contains(t, "teak renamed and readdressed", body,
		`<span>Updates <span class="name">teak.kilo.alpha</span> in <span class="name">dev-01</span> and configures its hostname and address through the guest agent.</span>`,
		`<dt>Rename</dt><dd><code>teak.kilo.alpha -&gt; oak.kilo.alpha</code> · VMID 2101 · n1</dd>`,
		`<dt>Kind</dt><dd>linux</dd>`, `<dt>Address</dt><dd><code>.25</code></dd>`, `<dt>Base image</dt><dd><code>debian-12-v1</code></dd>`,
		`<dt>CD-ROM</dt><dd>ejects attached install media</dd>`, `<dt>CPU</dt><dd><code>lab-baseline</code></dd>`,
		`<dt>Guest agent</dt><dd>sets hostname oak and address 192.168.100.25 inside the guest</dd>`,
		`<tr class="chg"><th scope="row">OS <span class="chip">changed</span></th><td>—</td><td>Debian 12</td></tr>`,
		`<div class="meta">Before</div><div class="notes box">debian / Changeme1!</div>`,
		`<div class="meta">After</div><div class="notes box">updated notes</div>`)
	lacks(t, "teak renamed and readdressed", body, "<dt>Host</dt>", "<dt>Network</dt>")

	body = review(elm("eject_cdrom", "yes"))
	contains(t, "elm's CD-ROM ejected", body,
		`<span>Ejects the attached <span class="name">CD-ROM</span> from <span class="name">elm.kilo.alpha</span>.</span>`,
		`<dt>Host</dt><dd><code>elm.kilo.alpha</code> · VMID 2102 · n1</dd>`, `<dt>CD-ROM</dt><dd>ejects attached install media</dd>`)
	lacks(t, "elm's CD-ROM ejected", body, "<dt>Kind</dt>", "<dt>Address</dt>", "<dt>Guest agent</dt>")

	body = review(elm("cpu", "lab-baseline"))
	contains(t, "elm's CPU set", body,
		`<span>Sets the CPU of <span class="name">elm.kilo.alpha</span> to <span class="name">lab-baseline</span>.</span>`,
		`<dt>CPU</dt><dd><code>lab-baseline</code></dd>`)

	body = review(elm("octet", "30"))
	contains(t, "elm given an address", body,
		`<span>Updates <span class="name">elm.kilo.alpha</span> in <span class="name">dev-01</span>.</span>`,
		`<dt>Kind</dt><dd>other</dd>`, `<dt>Address</dt><dd><code>.30</code></dd>`, `<dt>Base image</dt><dd><code>win10-v1</code></dd>`)

	body = review(elm("env_host", "ash"))
	contains(t, "elm renamed", body, `<dt>Rename</dt><dd><code>elm.kilo.alpha -&gt; ash.kilo.alpha</code> · VMID 2102 · n1</dd>`,
		`<dt>Address</dt><dd><code>none</code></dd>`)

	body = review(url.Values{"vmid": {"2100"}, "env_host": {"gw2"}, "image_kind": {"router"}, "image": {"gw-v1"}})
	contains(t, "the router renamed", body, `<dt>Kind</dt><dd>router</dd>`, `<dt>Address</dt><dd><code>.2</code></dd>`)

	body = review(url.Values{"vmid": {"2103"}, "env_host": {"yew"}, "image_kind": {"linux"}, "octet": {"5"}, "image": {"debian-12-v1"}})
	contains(t, "yew off its bridges", body, `<dt>Network</dt><dd>moves NICs onto dev-01&#39;s bridges</dd>`,
		`<span>Updates <span class="name">yew.kilo.alpha</span> in <span class="name">dev-01</span>.</span>`)

	// Renamed onto a name in use: why, and nothing to confirm.
	body = h.post(&sess, "/environments/edit/preview", url.Values{"vmid": {"2101"}, "env_host": {"elm"}, "image_kind": {"linux"}, "octet": {"10"}, "image": {"debian-12-v2"}}).Body.String()
	contains(t, "teak renamed onto elm", body, `<p class="say bad">`, `<span>A VM named <span class="name">elm.kilo.alpha</span> already exists.</span>`)
	lacks(t, "teak renamed onto elm", body, "/confirm", "Updates")
}

// A host's power and delete reviews say what happens, red when it cuts the
// power or deletes; a template in the slot can't be either, and says so.
func TestEnvHostPowerAndDeleteReviews(t *testing.T) {
	h := newHarness(t)
	addEnvs(h)
	h.api.add(proxmox.VM{VMID: 2150, Name: "base.kilo.alpha", Node: "n1", Pool: "dev-01", Template: true}, nil)
	sess := h.login(asLead)

	_, _, body := h.preview(&sess, "/environments/power", url.Values{"vmid": {"2101"}, "action": {"reboot"}})
	contains(t, "reboot", body, `<h1 id="pv-title"><span class="eyebrow">Review ·</span> Reboot <span class="vm">teak.kilo.alpha</span></h1>`,
		`<p class="say"><svg`, "<span>1 VM will be asked to restart by its operating system.</span>")
	lacks(t, "reboot", body, `<p class="say bad">`)
	_, _, body = h.preview(&sess, "/environments/power", url.Values{"vmid": {"2101"}, "action": {"stop"}})
	contains(t, "force stop", body, `<p class="say bad">`, "<span>1 VM loses power at once. Unsaved work is lost.</span>")

	_, _, body = h.preview(&sess, "/environments/delete", url.Values{"vmid": {"2101"}, "env_host": {"teak.kilo.alpha"}})
	contains(t, "delete", body, `<p class="say bad">`,
		`<span>Stops and deletes <span class="name">teak.kilo.alpha</span> in <span class="name">dev-01</span> with its disks. The other VMs in <span class="name">dev-01</span> stay as they are. This can&#39;t be undone.</span>`)

	body = h.post(&sess, "/environments/power/preview", url.Values{"vmid": {"2150"}, "action": {"start"}}).Body.String()
	contains(t, "a template started", body, `<p class="say bad">`, "<span>Is a template.</span>")
	lacks(t, "a template started", body, "/confirm")
	body = h.post(&sess, "/environments/delete/preview", url.Values{"vmid": {"2150"}, "env_host": {"base.kilo.alpha"}}).Body.String()
	contains(t, "a template deleted", body, `<p class="say bad">`,
		`<span>Is a template, not an environment host; move it out of <span class="name">dev-01</span> first.</span>`)
	lacks(t, "a template deleted", body, "/confirm", "Stops and deletes")
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
