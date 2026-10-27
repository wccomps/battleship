package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// addCatalog puts base images and build VMs on the harness's cluster:
//   - gw (router): v1, registered from gw-build (140), which still exists;
//   - debian-12 (linux): v1 deprecated, v2 current, and an edit copy of v2
//     open (debian-12-edit, 141);
//   - win10 (windows): v1, deprecated: a retired family;
//   - build VMs: teak-build (windows by OS type), elm-build (OS type other,
//     so its kind is asked), oak-build (running, so blocked) and Odd_Name
//     (no family in its name).
func addCatalog(h *harness) {
	pool := h.cfg.Images.Pool
	for _, im := range []struct {
		vmid       int
		name, tags string
		desc       string
	}{
		{8000, "gw-v1", "base;router", "admin / gwpass\nWAN dhcp\nbattleship: source=140"},
		{8001, "debian-12-v1", "base;linux;deprecated", "old login"},
		{8002, "debian-12-v2", "base;linux", "debian / Changeme1!\nsudo, cloud-init"},
		{8003, "win10-v1", "base;windows;deprecated", "user / retired"},
	} {
		h.api.Add(proxmox.VM{VMID: im.vmid, Name: im.name, Node: "n1", Template: true, Tags: im.tags},
			map[string]string{"description": im.desc, "agent": "1", "net0": "virtio=BC:24:11:00:80:00,bridge=vmbr0"})
	}
	for _, vm := range []struct {
		vmid         int
		name, status string
		cfg          map[string]string
	}{
		{140, "gw-build", "stopped", map[string]string{"ostype": "other"}},
		{141, "debian-12-edit", "stopped", map[string]string{"ostype": "l26", "agent": "1", "description": "patched\nbattleship: base=debian-12-v2"}},
		{150, "teak-build", "stopped", map[string]string{"ostype": "win11", "agent": "1", "description": "Login: admin"}},
		{151, "oak-build", "running", map[string]string{"ostype": "l26"}},
		{152, "elm-build", "stopped", map[string]string{"ostype": "other", "agent": "1", "description": "root / opnsense\nLAN .2"}},
		{153, "Odd_Name", "stopped", map[string]string{"ostype": "l26", "agent": "1"}},
	} {
		h.api.Add(proxmox.VM{VMID: vm.vmid, Name: vm.name, Node: "n1", Pool: pool, Status: vm.status}, vm.cfg)
	}
}

// rowOfVM is the ready-to-register row of the VM named name.
func rowOfVM(t *testing.T, body, name string) string {
	t.Helper()
	parts := strings.Split(body, `class="rrow`)
	for _, p := range parts[1:] {
		if strings.Contains(p, `<div class="nm">`+name+`</div>`) {
			row, _, _ := strings.Cut(p, `</section>`)
			row = strings.TrimSuffix(strings.TrimSuffix(row, "<form "), "<div ")
			return `class="rrow` + row
		}
	}
	t.Fatalf("no row for %s:\n%s", name, body)
	return ""
}

// Ready to register lists each build VM once with what it becomes: its
// kind as a chip when known, asked with required radios when not, the
// reason in place of the button when blocked, and a link to the form when
// its name says no family. A VM an image was registered from isn't listed.
func TestImagesPageReadyRows(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	sess := h.login(asLead)
	body := h.get(&sess, "/images").Body.String()
	if !strings.Contains(body, "Ready to register") || strings.Index(body, "Ready to register") > strings.Index(body, "Base images") {
		t.Errorf("Ready to register isn't first:\n%s", body)
	}

	teak := rowOfVM(t, body, "teak-build")
	contains(t, "teak-build's row", teak, `class="rrow" method="post" action="/images/register/preview">`,
		`name="vmid" value="150"`, `name="family" value="teak"`, `<code>teak-v1</code> <span class="meta">· new family</span>`,
		`<span class="chip">windows</span><input type="hidden" name="image_kind" value="windows">`, `>Register</button>`)
	lacks(t, "teak-build's row", teak, `type="radio"`, "disabled")

	elm := rowOfVM(t, body, "elm-build")
	contains(t, "elm-build's row", elm, `Its OS type is "other". Pick a kind:`, `<code>elm-v1</code>`)
	for _, k := range pods.ImageKinds {
		contains(t, "elm-build's row", elm, `<input type="radio" name="image_kind" value="`+k+`" required>`)
	}
	lacks(t, "elm-build's row", elm, `type="hidden" name="image_kind"`)

	edit := rowOfVM(t, body, "debian-12-edit")
	contains(t, "the edit copy's row", edit, `<code>debian-12-v3</code> <span class="meta">· edit copy of v2</span>`,
		`name="family" value="debian-12"`, `<span class="chip">linux</span>`)

	oak := rowOfVM(t, body, "oak-build")
	contains(t, "oak-build's row", oak, `class="rrow blocked"`, "is running; shut it down first")
	lacks(t, "oak-build's row", oak, "<form", "<button")

	odd := rowOfVM(t, body, "Odd_Name")
	contains(t, "Odd_Name's row", odd, `href="/images/register?vmid=153&amp;image_kind=linux"`)

	// gw-build is gw-v1's source: its family offers it, not this list.
	lacks(t, "the page", body, `<div class="nm">gw-build</div>`)
	contains(t, "the page", body, `<a class="btn" href="/images/register">`, "Register image</a>", `<a class="btn bad" href="/images/delete">`, "Delete image</a>")
}

// The catalogue lists each family once, by its current version, grouped by
// kind in the kinds' order, each linking to its page; a family with every
// version deprecated is folded away as retired.
func TestImagesPageCatalogue(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	sess := h.login(asLead)
	body := h.get(&sess, "/images").Body.String()
	cat, rest, ok := strings.Cut(body[strings.Index(body, `<section class="sec" id="cat" aria-label="Base images">`):], `<details class="olds">`)
	if !ok {
		t.Fatalf("no catalogue:\n%s", body)
	}
	router, linux := strings.Index(cat, `<h2 id="kind-router-h">Router</h2>`), strings.Index(cat, `<h2 id="kind-linux-h">Linux / BSD</h2>`)
	if router < 0 || linux < router || strings.Contains(cat, ">Windows</h2>") {
		t.Errorf("groups wrong (router %d, linux %d):\n%s", router, linux, cat)
	}
	contains(t, "the catalogue", cat,
		`<details open>`, `<summary class="envh disc">`, `<span>1 family</span>`,
		`<a class="fam" href="/images/family/gw" data-panel-link>gw</a></td><td class="cur"><code>v1</code> <span class="meta">· 8000</span></td><td class="nl">admin / gwpass</td><td class="act"><div class="acts"><a class="btn sm" href="/images/details?family=gw" data-panel-link aria-label="Edit notes of gw">`,
		`<a class="fam" href="/images/family/debian-12" data-panel-link>debian-12</a></td><td class="cur"><code>v2</code> <span class="meta">· 8002</span></td><td class="nl">debian / Changeme1!</td><td class="act"><div class="acts"><a class="btn sm" href="/images/details?family=debian-12" data-panel-link aria-label="Edit notes of debian-12">`)
	lacks(t, "the catalogue", cat, "win10", "WAN dhcp", "old login", "debian-12-v1")
	if strings.Count(cat, `class="fam"`) != 2 {
		t.Errorf("want 2 families in the catalogue:\n%s", cat)
	}
	fold, _, _ := strings.Cut(rest, "</details>")
	contains(t, "the retired fold", fold, `<summary class="disc">Retired families (1)</summary>`, "<code>win10</code>",
		"windows · every version deprecated · v1 is the newest", `<a class="btn sm" href="/images/family/win10" data-panel-link>Open</a>`)
	contains(t, "the page", body, `<dialog class="panel" id="panel"`)
}

// Deploys build their templates into the images pool: neither a converted
// one, even tagged like a base image, nor one still being built shows on
// the Images page.
func TestImagesPageLeavesOutDeployTemplates(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	pool := h.cfg.Images.Pool
	h.api.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta" + h.cfg.Naming.TemplateSuffix, Node: "n1", Pool: pool,
		Template: true, Tags: "base;linux"}, map[string]string{"agent": "1", "ostype": "l26"})
	h.api.Add(proxmox.VM{VMID: 9022, Name: "oak.tango.delta" + h.cfg.Naming.TemplateSuffix, Node: "n1", Pool: pool,
		Status: "stopped"}, map[string]string{"agent": "1", "ostype": "l26"})
	sess := h.login(asLead)
	body := h.get(&sess, "/images").Body.String()
	contains(t, "the page", body, `<div class="nm">teak-build</div>`)
	lacks(t, "the page", body, "tango.delta", "9021", "9022")
}

// The family page knows its state: an open edit copy is the primary way to
// the next version, the build VM a second; without an edit copy it offers
// to open one. Deprecate is only on the current version, Delete only on
// deprecated ones.
func TestFamilyPageStates(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	sess := h.login(asLead)

	deb := h.get(&sess, "/images/family/debian-12").Body.String()
	contains(t, "debian-12", deb, `<dt>Current</dt><dd><code>debian-12-v2</code> · VMID 8002 · n1</dd>`,
		`<div class="notes box">debian / Changeme1!`+"\n"+`sudo, cloud-init</div>`, "Next version · v3",
		`<code>debian-12-edit</code> is open for editing`,
		`<input type="hidden" name="vmid" value="141"><input type="hidden" name="family" value="debian-12">`+"\n"+`<button class="btn pri" type="submit">Register debian-12-edit as v3</button>`,
		`<a class="btn ghost sq" href="/images" data-close aria-label="Close">`, `<a class="btn" href="/images" data-close>Back to Images</a>`)
	lacks(t, "debian-12", deb, "Open for editing", "Built from", "disabled", "which you don&#39;t have")
	versions := deb[strings.Index(deb, `<ul class="vers">`):]
	v2, v1 := strings.Index(versions, "<code>v2</code>"), strings.Index(versions, "<code>v1</code>")
	if v2 < 0 || v1 < v2 {
		t.Errorf("versions aren't newest first:\n%s", versions)
	}
	if strings.Count(versions, "Deprecate</button>") != 1 || strings.Count(versions, "Delete</button>") != 1 ||
		strings.Index(versions, "Deprecate</button>") > v1 || strings.Index(versions, "Delete</button>") < v1 {
		t.Errorf("want Deprecate on v2 only and Delete on v1 only:\n%s", versions)
	}
	contains(t, "debian-12's versions", versions, `action="/images/deprecate/preview"`, `name="image" value="debian-12-v2"`,
		`action="/images/delete/preview"`, `name="image" value="debian-12-v1"`)

	gw := h.get(&sess, "/images/family/gw").Body.String()
	contains(t, "gw", gw, `<dt>Built from</dt><dd><code>gw-build</code> · VMID 140 · stopped</dd>`,
		`<input type="hidden" name="vmid" value="140"><input type="hidden" name="family" value="gw">`+"\n"+`<button class="btn pri" type="submit">New version from gw-build</button>`,
		`action="/images/edit/preview"`, `<button class="btn" type="submit">Open for editing</button>`, "Next version · v2")
	lacks(t, "gw", gw, "is open for editing", "Delete</button>")

	// When an open edit copy is running (blocked) and the source build VM exists,
	// "New version from ..." becomes the primary button.
	h.api.Add(proxmox.VM{VMID: 142, Name: "gw-edit", Node: "n1", Pool: h.cfg.Images.Pool, Status: "running"}, map[string]string{"ostype": "other", "description": "battleship: base=gw-v1"})
	gwBlockedEdit := h.get(&sess, "/images/family/gw").Body.String()
	contains(t, "gw with running edit copy", gwBlockedEdit,
		`<code>gw-edit</code> is open for editing`,
		`<button class="btn pri" type="submit">New version from gw-build</button>`)
	lacks(t, "gw with running edit copy", gwBlockedEdit, "Register gw-edit")

	// An older version that isn't deprecated has neither action.
	h.api.Add(proxmox.VM{VMID: 8006, Name: "alder-v1", Node: "n1", Template: true, Tags: "base;linux"}, nil)
	h.api.Add(proxmox.VM{VMID: 8007, Name: "alder-v2", Node: "n1", Template: true, Tags: "base;linux"}, nil)
	alder := h.get(&sess, "/images/family/alder").Body.String()
	if strings.Count(alder, "Deprecate</button>") != 1 || strings.Contains(alder, "Delete</button>") {
		t.Errorf("alder-v1, neither current nor deprecated, has an action:\n%s", alder)
	}

	win := h.get(&sess, "/images/family/win10").Body.String()
	contains(t, "win10", win, "none <span class=\"meta\">· every version is deprecated</span>", "Delete</button>", "register a new build VM")
	lacks(t, "win10", win, "Deprecate</button>", "Open for editing", "New version from")
}

// Without the privileges, the family page's buttons stay, disabled, and a
// line each names what is missing.
func TestFamilyPageLacksPrivileges(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	sess := h.login(asOperator)
	deb := h.get(&sess, "/images/family/debian-12").Body.String()
	contains(t, "debian-12 as an operator", deb,
		`<button class="btn pri" type="submit" disabled>Register debian-12-edit as v3</button>`,
		`<button class="btn sm" type="submit" disabled>Deprecate</button>`, `<button class="btn sm bad" type="submit" disabled>Delete</button>`,
		"Making a new version needs VM.Clone, which you don&#39;t have.",
		"Deprecating needs VM.Config.Options on debian-12-v2, which you don&#39;t have.",
		"Deleting needs VM.Allocate on its versions, which you don&#39;t have.")
	images := h.get(&sess, "/images").Body.String()
	contains(t, "images as an operator", images, `<button class="btn sm" type="submit" disabled>Register</button>`,
		"Registering needs VM.Clone, which you don&#39;t have.")

	lead := h.login(asLead)
	contains(t, "images as a lead", h.get(&lead, "/images").Body.String(), `<button class="btn sm" type="submit">Register</button>`)
}

// The family page is a page, or the panel's sheet without the layout; a
// family that doesn't exist, or a path that isn't a family, is not found.
func TestFamilyPagePaths(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	sess := h.login(asLead)
	page := h.get(&sess, "/images/family/gw")
	panel := h.panelGet(&sess, "/images/family/gw", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "<html") {
		t.Errorf("page = %d", page.Code)
	}
	if panel.Code != http.StatusOK || strings.Contains(panel.Body.String(), "<html") || !strings.Contains(panel.Body.String(), `<div class="sheet family" aria-labelledby="fam-title">`) ||
		panel.Header().Get(panelHeader) != "1" {
		t.Errorf("panel = %d %s", panel.Code, panel.Body)
	}
	for _, path := range []string{"/images/family/nope", "/images/family/GW", "/images/family/gw-v1", "/images/family/-x", "/images/family/%3Cscript%3E",
		"/images/GW", "/images/-x", "/images/%3Cscript%3E"} {
		if rec := h.get(&sess, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	// The operations' forms keep their paths.
	if body := h.get(&sess, "/images/register?vmid=152&family=elm").Body.String(); !strings.Contains(body, `name="vmid" value="152"`) {
		t.Errorf("the register form is gone:\n%s", body)
	}
	// A family's old address goes to its page.
	if rec := h.get(&sess, "/images/gw"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/images/family/gw" {
		t.Errorf("GET /images/gw = %d to %q, want 301 to /images/family/gw", rec.Code, rec.Header().Get("Location"))
	}
	// A family may be named like an operation, or like the families' path:
	// its page is apart from them.
	for i, f := range []string{"register", "delete", "family"} {
		h.api.Add(proxmox.VM{VMID: 8100 + i, Name: f + "-v1", Node: "n1", Template: true, Tags: "base;linux"},
			map[string]string{"description": "login " + f, "agent": "1"})
		if body := h.get(&sess, "/images/family/"+f).Body.String(); !strings.Contains(body, `<span class="vm">`+f+`</span>`) || !strings.Contains(body, "login "+f) {
			t.Errorf("family %s's page:\n%s", f, body)
		}
	}
}

// Any family path segment is either a family that exists, served, or not
// found: never an error page, and never one family's page for another.
func TestPropFamilyPath(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	sess := h.login(asLead)
	families := []string{"gw", "debian-12", "win10"}
	rapid.Check(t, func(t *rapid.T) {
		seg := rapid.OneOf(rapid.SampledFrom(families), rapid.SampledFrom(hostileStrings), rapid.String(),
			rapid.StringMatching(`[a-zA-Z0-9-]{1,12}`)).Draw(t, "family")
		if seg == "" || seg == "." || seg == ".." {
			return // the path would be /images or above it
		}
		before, _, _ := h.api.readCounts()
		rec := h.get(&sess, "/images/family/"+url.PathEscape(seg))
		if after, _, _ := h.api.readCounts(); pods.CheckFamily(seg) != nil && after != before {
			t.Fatalf("%q isn't a family name, but the cluster was read for it", seg)
		}
		known := false
		for _, f := range families {
			known = known || f == seg
		}
		switch {
		case known && rec.Code != http.StatusOK:
			t.Fatalf("family %q = %d", seg, rec.Code)
		case known && !strings.Contains(rec.Body.String(), `<span class="vm">`+seg+`</span>`):
			t.Fatalf("family %q shows another family", seg)
		case !known && rec.Code != http.StatusNotFound:
			t.Fatalf("%q = %d, want 404", seg, rec.Code)
		}
		if _, err := checkMarkup(rec.Body.String()); err != nil {
			t.Fatalf("%q: %v", seg, err)
		}
	})
}

// The register review says in one sentence what happens, lists where the
// copy comes from and what it becomes, with a link to change it, and its
// confirm names the image it makes. The hidden fields still confirm it.
// It reads the source's notes from the plan, not from Proxmox again.
func TestRegisterReview(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	sess := h.login(asLead)
	h.api.Mu.Lock()
	h.api.Calls = nil
	h.api.Mu.Unlock()
	action, fields, body := h.preview(&sess, "/images/register", url.Values{"vmid": {"152"}, "family": {"elm"}, "image_kind": {"router"}})
	h.api.Mu.Lock()
	if n := slices.Index(h.api.Calls, "config:152"); n < 0 || slices.Contains(h.api.Calls[n+1:], "config:152") {
		t.Errorf("elm-build's config read %d times (want once, by the plan): %v", strings.Count(strings.Join(h.api.Calls, " "), "config:152 "), h.api.Calls)
	}
	h.api.Mu.Unlock()
	contains(t, "the review", body, `<span class="eyebrow">Review ·</span> Register <span class="vm">elm-build</span>`,
		"Copies <span class=\"name\">elm-build</span> into a new template, <span class=\"name\">elm-v1</span>. The build VM stays as it is.",
		`<dt>From</dt><dd><code>elm-build</code> · VMID 152 · n1</dd>`,
		`<dt>Becomes</dt><dd><code>elm-v1</code> <span class="meta">· new family</span> <a class="btn sm" href="/images/register?family=elm&amp;image_kind=router&amp;vmid=152">Change</a></dd>`,
		`<dt>Kind</dt><dd>router <span class="meta">· offered as the router when creating environments</span></dd>`,
		`<dt>Template</dt><dd>VMID 8004 on n1</dd>`,
		`<h2 class="sub">Notes</h2><div class="notes box">root / opnsense`+"\nLAN .2</div>",
		`<a class="btn first" href="/images" data-close>Back</a>`, "Register elm-v1</button>")
	lacks(t, "the review", body, "will run", "<table")
	if rec := h.post(&sess, action, fields); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}

	// An edit copy says so, and that it replaces the current version.
	_, _, body = h.preview(&sess, "/images/register", url.Values{"vmid": {"141"}, "family": {"debian-12"}})
	contains(t, "the edit copy's review", body, `<code>debian-12-v3</code> <span class="meta">· edit copy of v2</span>`,
		`<dt>Replaces</dt><dd><code>debian-12-v2</code> <span class="meta">· deprecated once this is registered</span></dd>`, "Register debian-12-v3</button>")

	// Blocked: the reason, and nothing to confirm.
	rec := h.post(&sess, "/images/register/preview", url.Values{"vmid": {"151"}, "family": {"oak"}, "image_kind": {"linux"}})
	blocked := rec.Body.String()
	contains(t, "the blocked review", blocked, `<p class="say bad">`, "is running; shut it down first")
	lacks(t, "the blocked review", blocked, "/confirm", "Register oak-v1</button>", "Copies")
}

// Whatever kind a register posts, only one of the kinds makes a review to
// confirm; anything else returns the form saying why.
func TestPropRegisterKind(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	sess := h.login(asLead)
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.OneOf(rapid.SampledFrom(pods.ImageKinds), rapid.SampledFrom(hostileStrings), rapid.String()).Draw(t, "kind")
		rec := h.post(&sess, "/images/register/preview", url.Values{"vmid": {"152"}, "family": {"elm"}, "image_kind": {kind}})
		body := rec.Body.String()
		valid := false
		for _, k := range pods.ImageKinds {
			valid = valid || strings.TrimSpace(kind) == k
		}
		confirms := strings.Contains(body, `action="/images/register/confirm"`)
		if valid != confirms {
			t.Fatalf("kind %q: confirm offered = %v (status %d)", kind, confirms, rec.Code)
		}
		if !valid && (rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, `role="alert"`)) {
			t.Fatalf("kind %q: %d without a reason", kind, rec.Code)
		}
		if _, err := checkMarkup(body); err != nil {
			t.Fatalf("kind %q: %v", kind, err)
		}
	})
}
