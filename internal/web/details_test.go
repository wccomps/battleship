package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// winProse is a base image's notes as people wrote them before the
// details block: one paragraph.
const winProse = "Base image build: Windows Server 2022 Standard Evaluation (Desktop Experience), built 2026-10-14 from windows_server_2022.iso. " +
	"Generalized (sysprep /oobe). Login: Administrator / Template-Password-1. QEMU guest agent + VirtIO drivers installed. " +
	"Eval licence (servers): 180 days from first boot of each clone."

// winBlock is its details as a block.
const winBlock = "**Details**\n" +
	"- OS: Windows Server 2022 Standard Evaluation (Desktop Experience)\n" +
	"- Login: Administrator / Template-Password-1\n" +
	"- Built: 2026-10-14 from windows_server_2022.iso\n" +
	"- Generalized: sysprep /oobe\n" +
	"- Installed: QEMU guest agent, VirtIO drivers\n" +
	"- Licence: 180-day evaluation from each clone's first boot"

// winDesc is win2022-v1's notes: the block, a line people wrote, and the
// battleship line.
const winDesc = winBlock + "\n\nPatched monthly.\nbattleship: source=160"

// passwordOf is the markup of a password behind its toggle.
func passwordOf(pw string) string {
	return `<span class="pw"><details><summary class="disc"><span class="show">Show password</span><span class="hide">Hide password</span></summary><code>` +
		pw + `</code></details><button type="button" class="btn sm" data-copy hidden>`
}

// addDetailed puts base images with details on the harness's cluster:
//   - win2022 (windows): v1 with a details block (winDesc), registered
//     from win2022-build (160);
//   - odd (linux): v1 whose block has a key Battleship doesn't know;
//   - hx (other): v1 whose details are markup;
//   - build VMs: alder-build (prose notes), birch-build (a block) and
//     cedar-build (an unreadable block);
//   - mike.alpha in dev-05: dc, whose notes carry the block; old, made
//     before details, whose base image has them; bad, unreadable.
func addDetailed(h *harness) {
	for _, im := range []struct {
		vmid       int
		name, tags string
		desc       string
	}{
		{8010, "win2022-v1", "base;windows", winDesc},
		{8011, "odd-v1", "base;linux", "**Details**\n- OS: Debian 12\n- Colour: blue\n\nfree words"},
		{8012, "hx-v1", "base;other", "**Details**\n- OS: <script>alert(1)</script>\n- Login: <b>x</b> / \"><img src=x onerror=alert(2)>"},
	} {
		h.api.Add(proxmox.VM{VMID: im.vmid, Name: im.name, Node: "n1", Template: true, Tags: im.tags},
			map[string]string{"description": im.desc, "agent": "1", "net0": "virtio=BC:24:11:00:80:10,bridge=vmbr0"})
	}
	pool := h.cfg.Images.Pool
	for _, vm := range []struct {
		vmid int
		name string
		desc string
	}{
		{160, "win2022-build", winProse},
		{161, "alder-build", winProse},
		{162, "birch-build", winBlock + "\n\nbirch notes"},
		{163, "cedar-build", "**Details**\n- Login: root\n- OS: Debian 12"},
	} {
		h.api.Add(proxmox.VM{VMID: vm.vmid, Name: vm.name, Node: "n1", Pool: pool, Status: "stopped"},
			map[string]string{"ostype": "win11", "agent": "1", "description": vm.desc})
	}
	for _, vm := range []struct {
		vmid       int
		name, desc string
	}{
		{2500, "dc.mike.alpha", winDesc[:strings.Index(winDesc, "\nbattleship:")] + "\nbattleship: ip=192.168.100.10 os=windows base=win2022-v1 dns=router"},
		{2501, "old.mike.alpha", "old notes\nbattleship: ip=192.168.100.11 os=windows base=win2022-v1 dns=router"},
		{2502, "bad.mike.alpha", "**Details**\n- Login: root\nrun on\nbattleship: ip=192.168.100.12 os=linux base=odd-v1 dns=router"},
	} {
		h.api.Add(proxmox.VM{VMID: vm.vmid, Name: vm.name, Node: "n1", Pool: "dev-05", Tags: "dev;mike.alpha", Status: "running"},
			map[string]string{"description": vm.desc, "net0": "virtio=BC:24:11:00:25:00,bridge=devint05"})
	}
}

// inOrder fails unless each of wants is in body, each after the one
// before.
func inOrder(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	at := 0
	for _, w := range wants {
		i := strings.Index(body[at:], w)
		if i < 0 {
			t.Errorf("%s lacks %q after byte %d:\n%s", what, w, at, body)
			return
		}
		at += i + len(w)
	}
}

// hostRowOf is the Environments page's row of host name.
func hostRowOf(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<tr><td class="hn"><span class="hname">` + regexp.QuoteMeta(name) + `</span>.*?</tr>`).FindString(body)
	if m == "" {
		t.Fatalf("no row for %s in:\n%s", name, body)
	}
	return m
}

// Details show as values wherever notes do: the login first, its password
// behind a toggle, the other fields in order, the rest of the notes below.
// A block Battleship can't read shows as the raw notes with a quiet note,
// and everything is text.
func TestDetailsShowAsValues(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	sess := h.login(asLead)
	login := `<dt>Login</dt><dd class="login"><code>Administrator</code> <span class="meta">/</span> ` + passwordOf("Template-Password-1")
	fields := []string{login, "<dt>OS</dt><dd>Windows Server 2022 Standard Evaluation (Desktop Experience)</dd>",
		"<dt>Built</dt><dd>2026-10-14 from windows_server_2022.iso</dd>", "<dt>Generalized</dt><dd>sysprep /oobe</dd>",
		"<dt>Installed</dt><dd>QEMU guest agent, VirtIO drivers</dd>", "<dt>Licence</dt><dd>180-day evaluation from each clone&#39;s first boot</dd>"}

	// The catalogue says what each image is: the OS, not the login.
	images := h.get(&sess, "/images").Body.String()
	contains(t, "the catalogue", images, `<td class="nl">Windows Server 2022 Standard Evaluation (Desktop Experience)</td>`)
	lacks(t, "the catalogue", images, "Template-Password-1", "**Details**")

	fam := h.get(&sess, "/images/family/win2022").Body.String()
	inOrder(t, "the family page", fam, append(append([]string{`<h2 class="sub">Details</h2>`}, fields...),
		`<summary class="disc">Edit details</summary>`, `<h2 class="sub">Notes</h2><div class="notes box">Patched monthly.</div>`)...)
	contains(t, "the family's form", fam, `<form method="post" action="/images/details/preview">`, `<input type="hidden" name="family" value="win2022">`,
		`<input type="hidden" name="details" value="yes">`, `name="detail_user" value="Administrator"`, `name="detail_password" value="Template-Password-1"`,
		`name="detail_os" value="Windows Server 2022 Standard Evaluation (Desktop Experience)"`, `name="detail_licence" value="180-day evaluation from each clone&#39;s first boot"`,
		`<button class="btn" type="submit">Review details</button>`)
	lacks(t, "the family page", fam, "Details unreadable", "- OS:", "battleship:")

	odd := h.get(&sess, "/images/family/odd").Body.String()
	contains(t, "an unreadable block", odd, `<p class="meta dunread">Details unreadable: the notes are shown as they are.</p>`,
		`<div class="notes box">**Details**`+"\n- OS: Debian 12\n- Colour: blue\n\nfree words</div>", `name="detail_os" value=""`)
	lacks(t, "an unreadable block", odd, "<dt>OS</dt>")

	hx := h.get(&sess, "/images/family/hx").Body.String()
	contains(t, "markup details", hx, "<dt>OS</dt><dd>&lt;script&gt;alert(1)&lt;/script&gt;</dd>", "<code>&lt;b&gt;x&lt;/b&gt;</code>",
		"<code>&#34;&gt;&lt;img src=x onerror=alert(2)&gt;</code>")
	lacks(t, "markup details", hx, "<script>alert", "<img src=x", "<b>x</b>")

	envs := h.get(&sess, "/environments").Body.String()
	dc := hostRowOf(t, envs, "dc")
	inOrder(t, "dc's row", dc, append(slices.Clone(fields), `<div class="notes">Patched monthly.</div>`)...)
	old := hostRowOf(t, envs, "old")
	inOrder(t, "old's row, from its base image", old, append(slices.Clone(fields), `<div class="notes">old notes</div>`)...)
	lacks(t, "old's row", old, "Patched monthly.")
	bad := hostRowOf(t, envs, "bad")
	contains(t, "bad's row", bad, "Details unreadable", `<div class="notes">**Details**`+"\n- Login: root\nrun on</div>")
	lacks(t, "bad's row", bad, "<dt>")
	lacks(t, "the Environments page", envs, "battleship:", "Colour")

	form := h.get(&sess, "/environments/new").Body.String()
	contains(t, "the environment form's image notes", form, fields...)
}

// detailsForm is Edit details's fields, as the family page posts them.
func detailsForm(family string, d pods.ImageDetails) url.Values {
	return url.Values{fieldFamily: {family}, fieldDetails: {yes}, fieldUser: {d.User}, fieldPassword: {d.Password}, fieldOS: {d.OS},
		fieldBuilt: {d.Built}, fieldGeneralized: {d.Generalized}, fieldInstalled: {d.Installed}, fieldLicence: {d.Licence}}
}

var winDetails = pods.ImageDetails{OS: "Windows Server 2022 Standard Evaluation (Desktop Experience)", User: "Administrator",
	Password: "Template-Password-1", Built: "2026-10-14 from windows_server_2022.iso", Generalized: "sysprep /oobe",
	Installed: "QEMU guest agent, VirtIO drivers", Licence: "180-day evaluation from each clone's first boot"}

// runPlan runs a stored job's plan on the fake cluster, as a worker would.
func (h *harness) runPlan(j store.Job) apply.Result {
	h.t.Helper()
	var plan pods.Plan
	if err := json.Unmarshal(j.Plan, &plan); err != nil {
		h.t.Fatal(err)
	}
	h.start(j.ID)
	exec := &apply.Executor{API: h.api, Cfg: h.cfg, Sleep: func(context.Context, time.Duration) error { return nil }}
	res := exec.Run(context.Background(), &plan)
	h.finish(j.ID, store.Outcome{Status: store.StatusSucceeded})
	return res
}

// Edit details is a job: its review shows each detail before and after,
// its confirm stores the details, and running it rewrites only the block
// in the newest version's notes. The job page shows what it wrote.
func TestEditDetailsJob(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	sess := h.login(asLead)
	edited := winDetails
	edited.Password, edited.Installed, edited.Generalized = "Template-Password-2", "", "sysprep /generalize /oobe"
	action, fields, body := h.preview(&sess, "/images/details", detailsForm("win2022", edited))
	contains(t, "the review", body, `<span class="eyebrow">Review ·</span> Edit details of <span class="vm">win2022-v1</span>`,
		"The rest of its notes, and older versions, stay as they are.",
		`<tr class="chg"><th scope="row">Password <span class="chip">changed</span></th><td>`+passwordOf("Template-Password-1"),
		`<td>`+passwordOf("Template-Password-2"),
		`<tr><th scope="row">User</th><td>Administrator</td><td>Administrator</td></tr>`,
		`<tr class="chg"><th scope="row">Generalized <span class="chip">changed</span></th><td>sysprep /oobe</td><td>sysprep /generalize /oobe</td></tr>`,
		`<tr class="chg"><th scope="row">Installed <span class="chip">changed</span></th><td>QEMU guest agent, VirtIO drivers</td><td><span class="meta">removed</span></td></tr>`,
		`<a class="btn first" href="/images/family/win2022" data-panel-link>Back</a>`, "Save details of win2022-v1</button>")
	if action != "/images/details/confirm" || fields.Get(fieldPassword) != "Template-Password-2" || fields.Get(fieldDetails) != yes || fields.Has(fieldInstalled) {
		t.Fatalf("confirm %s with %v", action, fields)
	}
	if rec := h.post(&sess, action, fields); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 || js[0].Kind != string(pods.KindImageDetails) {
		t.Fatalf("jobs = %+v", js)
	}
	if in := readInputs(js[0]); in.Details == nil || *in.Details != edited || in.Family != "win2022" {
		t.Errorf("job inputs = %+v", in)
	}
	if res := h.runPlan(js[0]); len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	want := strings.NewReplacer("Template-Password-1", "Template-Password-2", "- Installed: QEMU guest agent, VirtIO drivers\n", "",
		"sysprep /oobe", "sysprep /generalize /oobe").Replace(winDesc)
	h.api.Mu.Lock()
	got := h.api.VMs[8010].Config["description"]
	h.api.Mu.Unlock()
	if got != want {
		t.Errorf("notes after the job =\n%s\nwant\n%s", got, want)
	}
	page := h.get(&sess, "/logs/"+itoa(js[0].ID)).Body.String()
	contains(t, "the job page", page, `<h2 class="sub">Details</h2>`, passwordOf("Template-Password-2"), "Edit details", `href="/images/family/win2022"`)

	// The same details again: nothing to do.
	rec := h.post(&sess, "/images/details/preview", detailsForm("win2022", edited))
	contains(t, "an unchanged review", rec.Body.String(), "The details of <span class=\"name\">win2022-v1</span> are already these.")
	lacks(t, "an unchanged review", rec.Body.String(), "/images/details/confirm")

	// Details that would break the block: the form again, saying why.
	for _, bad := range []pods.ImageDetails{{OS: "a\nbattleship: ip=10.0.0.1"}, {User: "a / b", Password: "c"}, {OS: strings.Repeat("x", pods.MaxDetail+1)}} {
		rec := h.post(&sess, "/images/details/preview", detailsForm("win2022", bad))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `role="alert"`) {
			t.Errorf("%+v: preview = %d\n%s", bad, rec.Code, rec.Body)
		}
	}
	if n := len(h.jobsInStore()); n != 1 {
		t.Errorf("%d jobs stored, want 1", n)
	}

	// Its form opens with the newest version's details, the password out of
	// the address.
	form := h.get(&sess, "/images/details?family=win2022").Body.String()
	contains(t, "the Edit details form", form, `action="/images/details/preview"`, `name="detail_password" value="Template-Password-2"`)
	if q := formQuery(operation{Kind: pods.KindImageDetails, Path: "/images/details"}, jobs.Inputs{Kind: pods.KindImageDetails, Family: "win2022", Details: &edited}); strings.Contains(q, "Template") {
		t.Errorf("formQuery = %s", q)
	}
}

// Edit details takes VM.Config.Options on the newest version: without it
// the family page's button is off and says so, and the review is blocked.
func TestEditDetailsNeedsConfigOptions(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	viewer := h.loginWith("details-viewer", []int{8010})
	fam := h.get(&viewer, "/images/family/win2022").Body.String()
	contains(t, "the family page", fam, `<button class="btn" type="submit" disabled>Review details</button>`,
		"Editing details needs VM.Config.Options on win2022-v1, which you don&#39;t have.")
	rec := h.post(&viewer, "/images/details/preview", detailsForm("win2022", pods.ImageDetails{OS: "Debian 12"}))
	contains(t, "the review", rec.Body.String(), `<p class="say bad">`, `You don&#39;t have <span class="name">VM.Config.Options</span> on /vms/8010.`)
	lacks(t, "the review", rec.Body.String(), "/images/details/confirm")

	editor := h.loginWith("details-editor", []int{8010}, "VM.Config.Options")
	lacks(t, "the family page with the privilege", h.get(&editor, "/images/family/win2022").Body.String(), "Review details</button> disabled", "Editing details needs")
	h.preview(&editor, "/images/details", detailsForm("win2022", pods.ImageDetails{OS: "Debian 12"}))
}

// Register writes details into the new image: read from its source's
// notes (a block as it is; from prose, the login, build and OS), shown
// in its review, and changed by posting the review to the register form.
func TestRegisterPrefillsDetails(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	sess := h.login(asLead)
	_, fields, body := h.preview(&sess, "/images/register", url.Values{"vmid": {"161"}, "family": {"alder"}, "image_kind": {"windows"}})
	inOrder(t, "the review", body, `<h2 class="sub">Details</h2>`,
		`<dt>Login</dt><dd class="login"><code>Administrator</code> <span class="meta">/</span> `+passwordOf("Template-Password-1"),
		"<dt>OS</dt><dd>Windows Server 2022 Standard Evaluation (Desktop Experience)</dd>", "<dt>Built</dt><dd>2026-10-14 from windows_server_2022.iso</dd>",
		"Read from the notes of <span class=\"name\">alder-build</span>: check them.",
		`<button type="submit" class="btn sm" formaction="/images/register" formnovalidate>`, "Edit details</button>",
		`<h2 class="sub">Notes</h2><div class="notes box">`+strings.ReplaceAll(winProse, "+", "&#43;")+"</div>")
	lacks(t, "the review", body, "<dt>Generalized</dt>", "<dt>Licence</dt>")
	if fields.Has(fieldDetails) {
		t.Errorf("the review's inputs carry details: %v", fields)
	}

	// Edit details posts the review's inputs to the form, which reads the
	// details again.
	fields.Del("nonce")
	fields.Del("fingerprint")
	form := h.post(&sess, "/images/register", fields).Body.String()
	contains(t, "the register form", form, `action="/images/register/preview"`, `name="vmid" value="161"`, `<input type="hidden" name="details" value="yes">`,
		`name="detail_user" value="Administrator"`, `name="detail_password" value="Template-Password-1"`, `name="detail_generalized" value=""`,
		`name="detail_os" value="Windows Server 2022 Standard Evaluation (Desktop Experience)"`)

	d := pods.ImageDetails{OS: winDetails.OS, User: "Administrator", Password: "Template-Password-1", Built: winDetails.Built, Generalized: "sysprep /oobe"}
	post := detailsForm("alder", d)
	post.Set("vmid", "161")
	post.Set("image_kind", "windows")
	action, fields, body := h.preview(&sess, "/images/register", post)
	contains(t, "the review of edited details", body, "<dt>Generalized</dt><dd>sysprep /oobe</dd>")
	lacks(t, "the review of edited details", body, "Read from the notes")
	if rec := h.post(&sess, action, fields); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	j := h.jobsInStore()[0]
	var plan pods.Plan
	if err := json.Unmarshal(j.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	it := plan.Items[0]
	if !slices.Equal(it.Steps, []pods.Step{pods.StepClone, pods.StepLabel, pods.StepDetails, pods.StepCDROM, pods.StepConvert}) || it.Details == nil || *it.Details != d {
		t.Fatalf("planned %v with %+v", it.Steps, it.Details)
	}
	if res := h.runPlan(j); len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	h.api.Mu.Lock()
	got := h.api.VMs[it.VMID].Config["description"]
	h.api.Mu.Unlock()
	if want := "**Details**\n- OS: " + winDetails.OS + "\n- Login: Administrator / Template-Password-1\n- Built: " + winDetails.Built +
		"\n- Generalized: sysprep /oobe\n\n" + winProse + "\nbattleship: source=161"; got != want {
		t.Errorf("the new image's notes =\n%s\nwant\n%s", got, want)
	}

	// A block is copied as it is; an unreadable one is left to the copy.
	_, _, body = h.preview(&sess, "/images/register", url.Values{"vmid": {"162"}, "family": {"birch"}, "image_kind": {"windows"}})
	contains(t, "a block's review", body, "<dt>Licence</dt><dd>180-day evaluation from each clone&#39;s first boot</dd>",
		`<div class="notes box">birch notes</div>`)
	_, _, body = h.preview(&sess, "/images/register", url.Values{"vmid": {"163"}, "family": {"cedar"}, "image_kind": {"windows"}})
	contains(t, "an unreadable block's review", body, "The details in the notes of <span class=\"name\">cedar-build</span> are unreadable; the copy keeps them as they are.",
		`<div class="notes box">**Details**`)
	lacks(t, "an unreadable block's review", body, "<dt>Login</dt>")
}

// pwShown is whether the first password on the page shows.
const pwShown = `document.querySelector(".dkv .pw code").checkVisibility()`

// A password stays hidden until its toggle is opened; its Copy button
// copies it, shown or not. Without the script the toggle still works, and
// there is no Copy button that couldn't copy.
func TestBrowserPassword(t *testing.T) {
	h := browserHarness(t)
	addDetailed(h)
	lead := h.login(asLead)
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.run("granting the clipboard", cdpbrowser.GrantPermissions([]cdpbrowser.PermissionType{cdpbrowser.PermissionTypeClipboardReadWrite,
				cdpbrowser.PermissionTypeClipboardSanitizedWrite}).WithOrigin(b.srv.URL).WithBrowserContextID(chromedp.FromContext(b.ctx).BrowserContextID))
			b.open("/images/family/win2022")
			b.noSideways("the family page")
			if b.is(pwShown) || !b.is(`document.querySelector(".dkv .pw [data-copy]").checkVisibility()`) {
				t.Fatalf("on opening: password shown %v, Copy shown %v", b.is(pwShown), !b.is(`document.querySelector(".dkv .pw [data-copy]").hidden`))
			}
			b.shot("details-family-" + size.name)
			b.click(".dkv .pw [data-copy]")
			b.waitFor("Copied", `document.querySelector(".dkv .pw [data-copy]").textContent.trim() === "Copied"`)
			var clip string
			b.eval(`navigator.clipboard.readText()`, &clip)
			if clip != "Template-Password-1" || b.is(pwShown) {
				t.Errorf("Copy put %q on the clipboard, password shown %v", clip, b.is(pwShown))
			}
			b.click(".dkv .pw summary")
			b.waitFor("the password", pwShown)
			var got string
			if b.eval(`document.querySelector(".dkv .pw summary").innerText.trim()`, &got); got != "Hide password" {
				t.Errorf("the open toggle says %q", got)
			}
			b.shot("details-family-shown-" + size.name)
			b.click(".dkv .pw summary")
			b.waitFor("the password hidden", "!"+pwShown)

			b.click(".dedit summary")
			b.waitFor("the form", `document.querySelector(".dedit form").checkVisibility()`)
			b.noSideways("the Edit details form")
			b.shot("details-edit-form-" + size.name)
			b.eval(`document.querySelector('[name=detail_password]').value = "Template-Password-2"`, nil)
			b.click(".dedit button[type=submit]")
			b.waitFor("the review", `!!document.querySelector(".dchg")`)
			b.noSideways("the Edit details review")
			b.fullShot("details-review-" + size.name)

			b.open("/environments")
			b.noSideways("the Environments page")
			b.shot("details-environments-" + size.name)

			b.open("/images")
			b.click(`.rrow input[value="161"] ~ .act button`)
			b.waitFor("the register review", `!!document.querySelector("#panel .sheet .dkv")`)
			b.shot("details-register-review-" + size.name)
			b.clean()
		})
	}

	t.Run("no script", func(t *testing.T) {
		h.t = t
		b := newBrowser(t, h, &lead, 1366, 768, false)
		b.run("blocking the script", network.Enable(), network.SetBlockedURLs([]string{"*/static/app.js*"}))
		b.expect("ERR_BLOCKED_BY_CLIENT")
		b.open("/images/family/win2022")
		b.expect("ERR_BLOCKED_BY_CLIENT")
		if b.is(pwShown) || !b.is(`document.querySelector(".dkv .pw [data-copy]").hidden`) {
			t.Fatalf("without the script: password shown %v, Copy hidden %v", b.is(pwShown), b.is(`document.querySelector(".dkv .pw [data-copy]").hidden`))
		}
		b.click(".dkv .pw summary")
		b.waitFor("the password", pwShown)
		if got := b.text(".dkv .pw code"); got != "Template-Password-1" {
			t.Errorf("the password shows as %q", got)
		}
		b.shot("details-family-noscript")
		b.clean()
	})
}
