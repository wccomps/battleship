package web

import (
	"context"
	"encoding/json"
	"html"
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

	// The family page has Edit beside the Details heading, which opens
	// Edit notes; the form isn't folded into the page.
	fam := h.get(&sess, "/images/family/win2022").Body.String()
	inOrder(t, "the family page", fam, append(append([]string{`<div class="sech"><h2 class="sub">Details</h2><span class="sp"></span>` +
		`<a class="btn sm" href="/images/details?family=win2022" data-panel-link aria-label="Edit notes of win2022">` + string(icon("edit")) + `Edit</a></div>`}, fields...),
		`<h2 class="sub">Notes</h2><div class="notes box">Patched monthly.</div>`)...)
	lacks(t, "the family page", fam, "Details unreadable", "- OS:", "battleship:", "<details class=\"dedit\">", "detail_user", "disabled", "Editing notes needs")

	odd := h.get(&sess, "/images/family/odd").Body.String()
	contains(t, "an unreadable block", odd, `<p class="meta dunread">Details unreadable: the notes are shown as they are.</p>`,
		`<div class="notes box">**Details**`+"\n- OS: Debian 12\n- Colour: blue\n\nfree words</div>", `<a class="btn sm" href="/images/details?family=odd" data-panel-link aria-label="Edit notes of odd">`)
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

// detailsForm is details alone, as the register form posts them, or an
// Edit notes form from before it posted free text.
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

// notesForm is Edit notes's form as a browser posts it: the details, and
// the free text with CRLF line ends.
func notesForm(family string, d pods.ImageDetails, notes string) url.Values {
	f := detailsForm(family, d)
	f.Set(fieldDetails, detailsWithNotes)
	f.Set(fieldNotes, strings.ReplaceAll(notes, "\n", "\r\n"))
	return f
}

var (
	textareaRE    = regexp.MustCompile(`(?s)<textarea class="inp" id="notes" name="notes"[^>]*>(.*?)</textarea>`)
	detailInputRE = regexp.MustCompile(`<input class="inp" type="text" name="(detail_[a-z]+)" value="([^"]*)"`)
	formRE        = regexp.MustCompile(`(?s)<form class="sheet" method="post" action="(/images/details/preview)"[^>]*>(.*?)</form>`)
)

// browserText is text as a browser posts it from a page: the HTML parser
// reads CR and CRLF as LF, and a form sends each LF as CRLF.
func browserText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// browserForm is the Edit notes form on page as a browser posts it
// unchanged: where it posts, and its hidden fields, details fields and
// notes, without the CSRF token, which post sends.
func browserForm(t interface{ Fatalf(string, ...any) }, page string) (string, url.Values) {
	m := formRE.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no Edit notes form in:\n%s", page)
	}
	form := url.Values{}
	for _, f := range hiddenRE.FindAllStringSubmatch(m[2], -1) {
		if f[1] != "csrf" {
			form.Add(f[1], browserText(html.UnescapeString(f[2])))
		}
	}
	for _, f := range detailInputRE.FindAllStringSubmatch(m[2], -1) {
		form.Add(f[1], browserText(html.UnescapeString(f[2])))
	}
	if ta := textareaRE.FindStringSubmatch(m[2]); ta != nil {
		form.Add(fieldNotes, browserText(strings.TrimPrefix(html.UnescapeString(ta[1]), "\n")))
	}
	return m[1], form
}

// notesFormValues reads Edit notes's form as a browser would: its details
// fields and its free text.
func notesFormValues(t *testing.T, body string) (pods.ImageDetails, string) {
	t.Helper()
	m := textareaRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no notes textarea in:\n%s", body)
	}
	vals := map[string]string{}
	for _, in := range detailInputRE.FindAllStringSubmatch(body, -1) {
		vals[in[1]] = html.UnescapeString(in[2])
	}
	if len(vals) != 7 {
		t.Fatalf("details fields = %v", vals)
	}
	d := pods.ImageDetails{OS: vals[fieldOS], User: vals[fieldUser], Password: vals[fieldPassword], Built: vals[fieldBuilt],
		Generalized: vals[fieldGeneralized], Installed: vals[fieldInstalled], Licence: vals[fieldLicence]}
	return d, html.UnescapeString(m[1])
}

// notesOf is VM vmid's notes on the fake cluster.
func (h *harness) notesOf(vmid int) string {
	h.api.Mu.Lock()
	defer h.api.Mu.Unlock()
	return h.api.VMs[vmid].Config["description"]
}

// Edit notes is a job: its form opens with the newest version's details
// and free text; its review shows each detail and the free text before and
// after; its confirm stores them; running it writes the block, the free
// text and the battleship line as it was. The form then reads back what
// was posted, and the job page shows what it wrote.
func TestEditNotesJob(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	sess := h.login(asLead)

	form := h.get(&sess, notesHref("win2022")).Body.String()
	contains(t, "the Edit notes form", form, `Edit notes of <span class="vm">win2022-v1</span></h1>`, `action="/images/details/preview"`,
		`<input type="hidden" name="family" value="win2022">`, "Written into the notes of <code>win2022-v1</code>",
		`<a class="btn" href="/images/family/win2022" data-panel-link>Cancel</a>`)
	lacks(t, "the Edit notes form", form, "battleship:", "**Details**", "unreadable")
	if d, notes := notesFormValues(t, form); d != winDetails || notes != "Patched monthly." {
		t.Fatalf("the form opens with %+v, %q", d, notes)
	}

	edited := winDetails
	edited.Password, edited.Installed, edited.Generalized = "Template-Password-2", "", "sysprep /generalize /oobe"
	notes := "Patched monthly.\nRebooted after <KB5034439> & checked."
	action, fields, body := h.preview(&sess, "/images/details", notesForm("win2022", edited, notes+"\n\n"))
	contains(t, "the review", body, `<span class="eyebrow">Review ·</span> Edit notes of <span class="vm">win2022-v1</span>`,
		"Writes these details and notes into the notes of <span class=\"name\">win2022-v1</span>, which new hosts copy.",
		`<tr class="chg"><th scope="row">Password <span class="chip">changed</span></th><td>`+passwordOf("Template-Password-1"),
		`<td>`+passwordOf("Template-Password-2"),
		`<tr><th scope="row">User</th><td>Administrator</td><td>Administrator</td></tr>`,
		`<tr class="chg"><th scope="row">Generalized <span class="chip">changed</span></th><td>sysprep /oobe</td><td>sysprep /generalize /oobe</td></tr>`,
		`<tr class="chg"><th scope="row">Installed <span class="chip">changed</span></th><td>QEMU guest agent, VirtIO drivers</td><td><span class="meta">removed</span></td></tr>`,
		`<div class="sech"><h2 class="sub">Notes</h2><span class="chip">changed</span></div>`,
		`<div class="meta">Before</div><div class="notes box">Patched monthly.</div>`,
		`<div class="meta">After</div><div class="notes box">Patched monthly.`+"\n"+`Rebooted after &lt;KB5034439&gt; &amp; checked.</div>`,
		`<a class="btn first" href="/images/family/win2022" data-panel-link>Back</a>`, "Save notes of win2022-v1</button>")
	lacks(t, "the review", body, "battleship:", "<KB5034439>")
	if action != "/images/details/confirm" || fields.Get(fieldPassword) != "Template-Password-2" || fields.Get(fieldDetails) != detailsWithNotes ||
		fields.Has(fieldInstalled) || fields.Get(fieldNotes) != notes {
		t.Fatalf("confirm %s with %v", action, fields)
	}
	if rec := h.post(&sess, action, fields); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 || js[0].Kind != string(pods.KindImageDetails) {
		t.Fatalf("jobs = %+v", js)
	}
	if in := readInputs(js[0]); in.Details == nil || in.Details.ImageDetails != edited || in.Family != "win2022" || in.Details.Free == nil ||
		*in.Details.Free != notes {
		t.Errorf("job inputs = %+v", in)
	}
	if res := h.runPlan(js[0]); len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	block := strings.NewReplacer("Template-Password-1", "Template-Password-2", "- Installed: QEMU guest agent, VirtIO drivers\n", "",
		"sysprep /oobe", "sysprep /generalize /oobe").Replace(winBlock)
	if got, want := h.notesOf(8010), block+"\n\n"+notes+"\n\nbattleship: source=160"; got != want {
		t.Errorf("notes after the job =\n%s\nwant\n%s", got, want)
	}
	page := h.get(&sess, "/logs/"+itoa(js[0].ID)).Body.String()
	contains(t, "the job page", page, `<h2 class="sub">Details</h2>`, passwordOf("Template-Password-2"), "Edit notes", `href="/images/family/win2022"`,
		`<h2 class="sub">Notes</h2><div class="notes box">Patched monthly.`+"\n"+`Rebooted after &lt;KB5034439&gt; &amp; checked.</div>`)

	// Round trip: the form reads back what was posted.
	if d, got := notesFormValues(t, h.get(&sess, notesHref("win2022")).Body.String()); d != edited || got != notes {
		t.Errorf("the form reads back %+v, %q; want %+v, %q", d, got, edited, notes)
	}

	// The same again: nothing to do.
	rec := h.post(&sess, "/images/details/preview", notesForm("win2022", edited, notes))
	contains(t, "an unchanged review", rec.Body.String(), "The notes of <span class=\"name\">win2022-v1</span> already say this.")
	lacks(t, "an unchanged review", rec.Body.String(), "/images/details/confirm")
	// Clearing the free text alone is a change, which the review says.
	_, _, body = h.preview(&sess, "/images/details", notesForm("win2022", edited, ""))
	contains(t, "a review removing the notes", body, `<span class="chip">changed</span>`, `<div class="meta">After</div><p class="meta">None: removed.</p>`)

	// Details or notes that would break the notes: the form again, saying
	// why, with what was typed.
	for _, bad := range []struct {
		d     pods.ImageDetails
		notes string
	}{
		{pods.ImageDetails{OS: "a\nbattleship: ip=10.50.0.1"}, "x"},
		{pods.ImageDetails{User: "a / b", Password: "c"}, "x"},
		{pods.ImageDetails{OS: strings.Repeat("x", pods.MaxDetail+1)}, "x"},
		{winDetails, "ok\nbattleship: ip=10.50.0.9 os=linux"},
		{winDetails, "ok\n  **Details**"},
		{winDetails, "bell\x07here"},
		{winDetails, "bidi \u202eevil"},
		{winDetails, "isolate \u2066evil"},
		{winDetails, strings.Repeat("é", pods.MaxFreeNotes+1)},
	} {
		rec := h.post(&sess, "/images/details/preview", notesForm("win2022", bad.d, bad.notes))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `role="alert"`) {
			t.Errorf("%+v %q: preview = %d\n%s", bad.d, bad.notes, rec.Code, rec.Body)
		}
	}
	if n := len(h.jobsInStore()); n != 1 {
		t.Errorf("%d jobs stored, want 1", n)
	}
	if q := formQuery(operation{Kind: pods.KindImageDetails, Path: "/images/details"}, jobs.Inputs{Kind: pods.KindImageDetails,
		ImageRequest: pods.ImageRequest{Family: "win2022", Details: &pods.ImageNotes{ImageDetails: edited, Free: &notes}}}); strings.Contains(q, "Template") || strings.Contains(q, "Rebooted") {
		t.Errorf("formQuery = %s", q)
	}
}

// An image whose details block is unreadable is still editable: the form
// starts with empty fields and the text outside the block, showing the
// block as it is. Saving with that text as it was writes a block
// Battleship reads in the old one's place; saving other text writes the
// notes afresh.
func TestEditNotesOverUnreadableBlock(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	const odd = "intro\n\n**Details**\n- OS: Debian 12\n- Colour: blue\n\nfree words\nbattleship: source=99"
	h.api.Mu.Lock()
	h.api.VMs[8011].Config["description"] = odd
	h.api.Mu.Unlock()
	sess := h.login(asLead)

	fam := h.get(&sess, "/images/family/odd").Body.String()
	contains(t, "the family page", fam, "Details unreadable", `<a class="btn sm" href="/images/details?family=odd" data-panel-link aria-label="Edit notes of odd">`)
	form := h.get(&sess, notesHref("odd")).Body.String()
	contains(t, "the form", form, "Its details block is unreadable, so the fields start empty.",
		`<div class="meta">The block as it is:</div><div class="notes box">**Details**`+"\n- OS: Debian 12\n- Colour: blue</div>")
	if d, notes := notesFormValues(t, form); !d.Empty() || notes != "intro\n\nfree words" {
		t.Fatalf("the form opens with %+v, %q", d, notes)
	}
	for _, c := range []struct{ notes, want string }{
		{"intro\n\nfree words", "intro\n\n**Details**\n- OS: Debian 12\n\nfree words\nbattleship: source=99"},
		{"intro\n\nfree words, edited", "**Details**\n- OS: Debian 12\n\nintro\n\nfree words, edited\n\nbattleship: source=99"},
	} {
		h.api.Mu.Lock()
		h.api.VMs[8011].Config["description"] = odd
		h.api.Mu.Unlock()
		action, fields, body := h.preview(&sess, "/images/details", notesForm("odd", pods.ImageDetails{OS: "Debian 12"}, c.notes))
		contains(t, "the review", body, "Its details block is unreadable: these replace it.",
			`<div class="notes box">**Details**`+"\n- OS: Debian 12\n- Colour: blue</div>")
		if rec := h.post(&sess, action, fields); rec.Code != http.StatusSeeOther {
			t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
		}
		h.runPlan(h.jobsInStore()[0])
		got := h.notesOf(8011)
		if got != c.want {
			t.Errorf("notes after the job =\n%s\nwant\n%s", got, c.want)
		}
		if d, _, err := pods.ParseDetails(got); err != nil || d.OS != "Debian 12" {
			t.Errorf("read back %+v, %v", d, err)
		}
	}
	contains(t, "the form", form, "Saving replaces it with the details below, or removes it if they are left empty.")
	// Left empty, the fields write no block: the review says the block goes.
	h.api.Mu.Lock()
	h.api.VMs[8011].Config["description"] = odd
	h.api.Mu.Unlock()
	action, fields, body := h.preview(&sess, "/images/details", notesForm("odd", pods.ImageDetails{}, "intro\n\nfree words"))
	contains(t, "the review", body, "Its details block is unreadable: it is removed, and no details are written.")
	lacks(t, "the review", body, "these replace it")
	if rec := h.post(&sess, action, fields); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	h.runPlan(slices.MaxFunc(js, func(a, b store.Job) int { return int(a.ID - b.ID) }))
	if got, want := h.notesOf(8011), "intro\n\nfree words\nbattleship: source=99"; got != want {
		t.Errorf("notes after an empty write =\n%s\nwant\n%s", got, want)
	}
}

// Edit notes takes VM.Config.Options on the newest version: without it the
// family page's and the Images rows' Edit are off, a line saying why, and
// the review is blocked.
func TestEditNotesNeedsConfigOptions(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	viewer := h.loginWith("details-viewer", []int{8010})
	why := "Editing notes needs VM.Config.Options on win2022-v1, which you don&#39;t have."
	fam := h.get(&viewer, "/images/family/win2022").Body.String()
	contains(t, "the family page", fam, `<h2 class="sub">Details</h2><span class="sp"></span><button class="btn sm" type="button" disabled aria-label="Edit notes of win2022">`,
		`<p class="lock">`+string(icon("lock"))+why+`</p>`)
	lacks(t, "the family page", fam, `href="/images/details`)
	images := h.get(&viewer, "/images").Body.String()
	contains(t, "the Images page", images, `<td class="act"><div class="acts"><button class="btn sm" type="button" disabled aria-label="Edit notes of win2022">`,
		"Editing notes needs VM.Config.Options on the base images, which you don&#39;t have.")
	lacks(t, "the Images page", images, `href="/images/details`)
	rec := h.post(&viewer, "/images/details/preview", notesForm("win2022", pods.ImageDetails{OS: "Debian 12"}, "x"))
	contains(t, "the review", rec.Body.String(), `<p class="say bad">`, `You don&#39;t have <span class="name">VM.Config.Options</span> on /vms/8010.`)
	lacks(t, "the review", rec.Body.String(), "/images/details/confirm")

	// With it on win2022-v1 alone, only its Edit is on.
	editor := h.loginWith("details-editor", []int{8010}, "VM.Config.Options")
	fam = h.get(&editor, "/images/family/win2022").Body.String()
	contains(t, "the family page with the privilege", fam, `<a class="btn sm" href="/images/details?family=win2022" data-panel-link aria-label="Edit notes of win2022">`)
	lacks(t, "the family page with the privilege", fam, `<button class="btn sm" type="button" disabled>`, "Editing notes needs")
	images = h.get(&editor, "/images").Body.String()
	contains(t, "the Images page with the privilege", images,
		`<a class="btn sm" href="/images/details?family=win2022" data-panel-link aria-label="Edit notes of win2022">`,
		`<button class="btn sm" type="button" disabled aria-label="Edit notes of odd">`,
		"Editing notes needs VM.Config.Options on odd-v1 and hx-v1, which you don&#39;t have.")
	lacks(t, "the Images page with the privilege", images, `disabled aria-label="Edit notes of win2022"`)
	h.preview(&editor, "/images/details", notesForm("win2022", pods.ImageDetails{OS: "Debian 12"}, "x"))
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
	if !slices.Equal(it.Steps, []pods.Step{pods.StepClone, pods.StepLabel, pods.StepDetails, pods.StepCDROM, pods.StepConvert}) || it.Details == nil || it.Details.ImageDetails != d {
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

// The catalogue row of an image whose details block is unreadable says so,
// rather than showing a line of the broken block or nothing.
func TestCatalogueSaysDetailsUnreadable(t *testing.T) {
	h := newHarness(t)
	h.api.Add(proxmox.VM{VMID: 8003, Name: "odd-v1", Node: "n1", Template: true, Tags: "base;linux"},
		map[string]string{"agent": "1", "description": "**Details**\n- Login: root\nrun on"})
	sess := h.login(asLead)
	body := h.get(&sess, "/images").Body.String()
	if !strings.Contains(body, "Details unreadable: open the family to see its notes.") || strings.Contains(body, "Login: root") {
		t.Errorf("the catalogue row doesn't say the details are unreadable:\n%s", body)
	}
}

// Edit notes is one click from each Images row and from the family page's
// Details: over Images it opens in the side panel, its review there too;
// from the family page, as pages. The form and review fit phones and
// laptops, light and dark; an unreadable block's form shows the block as
// it is, and a form with nothing to edit offers no save.
func TestBrowserEditNotes(t *testing.T) {
	h := browserHarness(t)
	addDetailed(h)
	lead := h.login(asLead)
	const typed = `"Patched monthly.\nRebooted after the October updates."`
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/images")
			b.noSideways("the Images page")
			b.shot("editnotes-images-" + size.name)
			b.click(`a[aria-label="Edit notes of win2022"]`)
			b.waitFor("the form in the panel", `!!document.querySelector("#panel textarea[name=notes]")`)
			if got := b.text("#panel h1"); got != "Edit notes of win2022-v1" {
				t.Errorf("the panel's heading is %q", got)
			}
			b.shot("editnotes-form-panel-" + size.name)
			b.eval(`document.querySelector("#panel textarea[name=notes]").value = `+typed, nil)
			b.eval(`document.querySelector("#panel [name=detail_password]").value = "Template-Password-2"`, nil)
			b.click("#panel .pf button.pri")
			b.waitFor("the review in the panel", `!!document.querySelector("#panel .nchg")`)
			if got := b.text("#panel .nchg > div:last-child .notes"); got != "Patched monthly. Rebooted after the October updates." {
				t.Errorf("the review's notes after: %q", got)
			}
			b.shot("editnotes-review-panel-" + size.name)

			b.open("/images/family/win2022")
			b.noSideways("the family page")
			b.shot("editnotes-family-" + size.name)
			b.click(".sech a.btn")
			b.waitFor("the form as a page", `location.pathname === "/images/details" && !!document.querySelector("textarea[name=notes]")`)
			b.noSideways("the Edit notes form")
			b.fullShot("editnotes-form-" + size.name)
			b.eval(`document.querySelector("textarea[name=notes]").value = `+typed, nil)
			b.click(".pf button.pri")
			b.waitFor("the review as a page", `!!document.querySelector(".nchg")`)
			b.noSideways("the Edit notes review")
			b.fullShot("editnotes-review-" + size.name)

			b.open(notesHref("odd"))
			b.noSideways("the form over an unreadable block")
			b.fullShot("editnotes-unreadable-" + size.name)

			b.open("/images/details")
			if b.is(`!!document.querySelector("textarea, button.pri")`) {
				t.Error("the form without a family offers a save")
			}
			b.noSideways("the form without a family")
			b.shot("editnotes-nosave-" + size.name)
			b.clean()
		})
	}
}
