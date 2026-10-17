package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// addFormImages puts a router, Linux, Windows and other base images on the
// cluster, and a deprecated Windows one, which no form offers.
func addFormImages(h *harness) {
	addImages(h)
	for _, im := range []struct {
		vmid       int
		name, tags string
	}{
		{8002, "win2022-v2", "base;windows"}, {8003, "win2022-v3", "base;windows"},
		{8004, "win10-v1", "base;windows;deprecated"}, {8005, "appliance-v2", "base;other"}, {8006, "win2019-v1", "base;windows"},
	} {
		h.api.Add(proxmox.VM{VMID: im.vmid, Name: im.name, Node: "n1", Template: true, Tags: im.tags},
			map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:80:09,bridge=vmbr0"})
	}
}

// formRow is a host row as a form shows it: its values, its error and the
// input marked invalid.
type formRow struct{ name, image, octet, err, bad string }

var (
	hostRowRE  = regexp.MustCompile(`(?s)<div class="mrow" data-row>(.*?)\n</div>`)
	valueRE    = regexp.MustCompile(`name="host_(name|octet)" value="([^"]*)"`)
	selectedRE = regexp.MustCompile(`<option value="([^"]*)" selected`)
	ferrRE     = regexp.MustCompile(`<span class="ferr" id="([a-z0-9-]+)">([^<]*)</span>`)
	invalidRE  = regexp.MustCompile(`name="(?:host|router)_(name|image|octet)"[^>]*aria-invalid="true" aria-describedby="([a-z0-9-]+)"`)
)

// formRows reads the host rows of a form.
func formRows(body string) []formRow {
	var rows []formRow
	for _, m := range hostRowRE.FindAllStringSubmatch(body, -1) {
		var r formRow
		for _, v := range valueRE.FindAllStringSubmatch(m[1], -1) {
			if v[1] == "name" {
				r.name = html.UnescapeString(v[2])
			} else {
				r.octet = html.UnescapeString(v[2])
			}
		}
		if s := selectedRE.FindStringSubmatch(m[1]); s != nil {
			r.image = html.UnescapeString(s[1])
		}
		if e := ferrRE.FindStringSubmatch(m[1]); e != nil {
			r.err = html.UnescapeString(e[2])
			if inv := invalidRE.FindStringSubmatch(m[1]); inv != nil && inv[2] == e[1] {
				r.bad = inv[1]
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// rowsForm is a form posting rows (name, image, octet each) and a router.
func rowsForm(routerName, routerImage string, rows ...[3]string) url.Values {
	f := url.Values{"router_name": {routerName}, "router_image": {routerImage}}
	for _, r := range rows {
		f.Add("host_name", r[0])
		f.Add("host_image", r[1])
		f.Add("host_octet", r[2])
	}
	return f
}

// selectOf is the markup of the select named name.
func selectOf(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<select [^>]*name="` + name + `".*?</select>`).FindString(body)
	if m == "" {
		t.Fatalf("no select %s in:\n%s", name, body)
	}
	return m
}

// Create environment says the LAN, router and host addresses from config,
// offers the free slots (not those a queued job will fill) with the first
// chosen, then the router row, fixed at .2 with its one image chosen, and
// four empty host rows whose images are grouped by kind, each family's
// newest usable version. Enter reviews: the first submit button does.
func TestCreateEnvironmentForm(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Environments.Subnet = "10.50.7" })
	addFormImages(h)
	addEnv(h, true) // dev-01
	h.submitJob(jobs.Inputs{Kind: pods.KindEnvCreate, Domain: "kilo.bravo", Router: "gw=gw"}, byLead)
	sess := h.login(asLead)
	body := h.get(&sess, "/environments/new").Body.String()
	contains(t, "the form", body,
		`<p class="facts"><span>LAN <b>10.50.7.0/24</b></span><span>router <b>.2</b> · hosts <b>.4–.254</b></span></p>`,
		`<input class="inp n" type="text" name="router_name" value="gw" aria-label="Router name"`,
		`<div class="addr fixed a"><span class="pre l">10.50.7.</span><span class="pre s">.</span><b>2</b></div>`,
		`<legend class="lbl">Machines</legend>`, `<button type="submit" class="btn pri">Review</button>`,
		`<span class="pre l">10.50.7.</span><span class="pre s">.</span><input type="text" name="host_octet" value="" inputmode="numeric" placeholder="auto" aria-label="Host 1 last octet"`)
	slot := selectOf(t, body, "slot")
	contains(t, "the slot choice", slot, `<option value="03" selected>dev-03 · first free</option><option value="04">dev-04</option>`, `<option value="10">dev-10</option>`)
	lacks(t, "the slot choice", slot, `value="01"`, `value="02"`)
	contains(t, "the router image", selectOf(t, body, "router_image"), `<option value="gw" selected>gw · v1</option>`)
	if rows := formRows(body); len(rows) != blankRows || rows[0] != (formRow{}) || rows[3] != (formRow{}) {
		t.Errorf("host rows = %+v, want %d empty ones", rows, blankRows)
	}
	images := selectOf(t, body, "host_image")
	contains(t, "a host image choice", images, `<option value="">Choose image</option><optgroup label="Windows"><option value="win2019">win2019 · v1</option><option value="win2022">win2022 · v3</option></optgroup>`+
		`<optgroup label="Linux"><option value="debian-12">debian-12 · v1</option></optgroup><optgroup label="Other"><option value="appliance">appliance · v2</option></optgroup>`)
	lacks(t, "a host image choice", images, "win10", "v2</option></optgroup><optgroup label=\"Linux\"")
	if review, more := strings.Index(body, `<button type="submit" class="sr" tabindex="-1" aria-hidden="true">Review</button>`), strings.Index(body, `name="more"`); review < 0 || review > more {
		t.Errorf("the first submit button isn't Review (at %d; Add host at %d)", review, more)
	}

	// With two router images, none is chosen for the user.
	h.api.Add(proxmox.VM{VMID: 8010, Name: "opnsense-v1", Node: "n1", Template: true, Tags: "base;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:80:10,bridge=vmbr0", "net1": "virtio=BC:24:11:00:80:11,bridge=vmbr0"})
	router := selectOf(t, h.get(&sess, "/environments/new").Body.String(), "router_image")
	contains(t, "two router images", router, `<option value="">Choose image</option><option value="gw">gw · v1</option><option value="opnsense">opnsense · v1</option>`)
	lacks(t, "two router images", router, "selected")
}

// With no slot free, Create says so; with the free ones all taken by
// queued jobs, it says which those are.
func TestCreateFormWithoutAFreeSlot(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Environments.Slots = "1-2" })
	addFormImages(h)
	addEnv(h, true) // dev-01
	sess := h.login(asLead)
	h.submitJob(jobs.Inputs{Kind: pods.KindEnvCreate, Domain: "kilo.bravo", Router: "gw=gw"}, byLead)
	contains(t, "the form", h.get(&sess, "/environments/new").Body.String(), `<span class="hint">Queued jobs will fill every free dev slot (dev-02).</span>`)
	h.api.Add(proxmox.VM{VMID: 2200, Name: "gw.kilo.bravo", Node: "n1", Pool: "dev-02", Tags: "dev;kilo.bravo;router"}, nil)
	contains(t, "the form", h.get(&sess, "/environments/new").Body.String(), `<span class="hint">Every dev slot is in use.</span>`)
}

// A chosen slot goes through the preview and confirm into the job, which
// is planned there; one no longer free is marked on the field.
func TestCreateEnvironmentInAChosenSlot(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	form := rowsForm("gw", "gw", [3]string{"teak", "debian-12", "10"}, [3]string{}, [3]string{}, [3]string{})
	form.Set("domain", "kilo.bravo")
	form.Set("slot", "04")
	action, fields, body := h.preview(&sess, "/environments/new", form)
	contains(t, "the preview", body, "into <span class=\"name\">dev-04</span>", `<td class="r wide meta">2400 · n1</td>`)
	if fields.Get("slot") != "04" || fields.Get("env_hosts") != "teak=debian-12:10" || fields.Get("router") != "gw=gw" {
		t.Errorf("confirm fields = %v", fields)
	}
	rec := h.post(&sess, action, fields)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	contains(t, "the job page", h.get(&sess, rec.Header().Get("Location")).Body.String(), ` · Slot <span class="vm">dev-04</span>`)
	js := h.jobsInStore()
	items, _ := h.st.Items(context.Background(), js[0].ID)
	if in := readInputs(js[0]); in.Slot != "04" || len(items) != 2 || items[0].VMID/100 != 24 || items[1].VMID/100 != 24 {
		t.Errorf("job inputs %+v, items %+v; want slot 04", in, items)
	}

	// dev-01 holds kilo.alpha.
	form.Set("slot", "01")
	rec = h.post(&sess, "/environments/new/preview", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a slot in use = %d", rec.Code)
	}
	contains(t, "a slot in use", rec.Body.String(), `<span class="ferr" id="slot-err">Dev slot 01 is in use; choose a free one.</span>`,
		`name="slot" aria-invalid="true" aria-describedby="slot-err"`, `<option value="01" selected>01 · not free</option>`,
		`value="teak"`, `value="kilo.bravo"`)
}

// After Review, each machine row says what is wrong with it on the input
// it is about; the rows keep what was typed, blank ones dropped. Nothing
// is previewed.
func TestMachineFieldErrors(t *testing.T) {
	h := newHarness(t)
	addFormImages(h)
	sess := h.login(asLead)
	form := rowsForm("gw", "",
		[3]string{"dc", "win2022", "10"},
		[3]string{"web", "debian-12", ""},
		[3]string{"dc", "debian-12", ""},
		[3]string{"", "debian-12", ""},
		[3]string{"wks1", "", ""},
		[3]string{"files", "debian-12", ".10"},
		[3]string{"db", "debian-12", "300"},
		[3]string{"", "", ""},
		[3]string{"Bad_Name", "debian-12", ""},
		[3]string{"averyveryverylongname", "win2022", ""},
		[3]string{"old", "win10", ""},
		[3]string{"", "", "12"},
		[3]string{"gw", "appliance", "3"},
	)
	form.Set("domain", "Kilo")
	rec := h.post(&sess, "/environments/new/preview", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("preview = %d", rec.Code)
	}
	body := rec.Body.String()
	contains(t, "the form", body, `<p class="warn" role="alert">`, "Fix the marked fields, then review again.",
		`<span class="ferr" id="domain-err">Domain &#34;Kilo&#34; must be two or more labels like kilo.alpha.</span>`,
		`id="domain" name="domain" value="Kilo" required autocomplete="off" spellcheck="false" aria-describedby="domain-hint domain-err" aria-invalid="true"`,
		`name="router_image" aria-label="Router image" aria-invalid="true" aria-describedby="r-err"`,
		`<span class="ferr" id="r-err">Choose the router&#39;s image.</span>`)
	lacks(t, "the form", body, "confirm")
	pick := "Leave it blank to have one picked."
	want := []formRow{
		{name: "dc", image: "win2022", octet: "10"},
		{name: "web", image: "debian-12"},
		{name: "dc", image: "debian-12", err: "Named twice.", bad: "name"},
		{image: "debian-12", err: "Name this host, or clear its row.", bad: "name"},
		{name: "wks1", err: "Choose an image for wks1, or clear its row.", bad: "image"},
		{name: "files", image: "debian-12", octet: "10", err: "Address .10 is already taken in Kilo. " + pick, bad: "octet"},
		{name: "db", image: "debian-12", octet: "300", err: "Hosts take .4 to .254. " + pick, bad: "octet"},
		{name: "Bad_Name", image: "debian-12", err: "Use lower-case letters, digits and dashes, like dc or wks-1.", bad: "name"},
		{name: "averyveryverylongname", image: "win2022", err: "Windows host names are at most 15 characters.", bad: "name"},
		{name: "old", image: "win10", err: `No usable base image of family "win10".`, bad: "image"},
		{octet: "12", err: "Name this host, or clear its row.", bad: "name"},
		{name: "gw", image: "appliance", octet: "3", err: "Hosts take .4 to .254. " + pick, bad: "octet"},
		{},
	}
	if got := formRows(body); !slices.Equal(got, want) {
		t.Errorf("rows:\n got %+v\nwant %+v", got, want)
	}
	if n := strings.Count(body, `aria-invalid="true"`); n != 12 {
		t.Errorf("%d inputs marked invalid, want 12", n)
	}
	h.noJobs("a refused review")

	// Fixed, it reviews.
	form = rowsForm("gw", "gw", [3]string{"dc", "win2022", "10"}, [3]string{"web", "debian-12", ""}, [3]string{}, [3]string{"db", "debian-12", "11"})
	form.Set("domain", "kilo.bravo")
	_, fields, _ := h.preview(&sess, "/environments/new", form)
	if fields.Get("env_hosts") != "dc=win2022:10\nweb=debian-12\ndb=debian-12:11" {
		t.Errorf("env_hosts = %q", fields.Get("env_hosts"))
	}
	// An existing domain is marked on its field.
	addEnv(h, true)
	form.Set("domain", "kilo.alpha")
	contains(t, "an existing domain", h.post(&sess, "/environments/new/preview", form).Body.String(),
		`<span class="ferr" id="domain-err">kilo.alpha is already in dev slot 01; add hosts to it instead.</span>`)
}

// Without JavaScript, Add host asks for the form again with four more
// rows, keeping what was typed and checking nothing yet.
func TestAddHostWithoutJS(t *testing.T) {
	h := newHarness(t)
	addFormImages(h)
	sess := h.login(asLead)
	form := rowsForm("gw", "gw", [3]string{"dc", "win2022", "10"}, [3]string{}, [3]string{"Bad", "", ""}, [3]string{})
	form.Set("domain", "kilo.bravo")
	form.Set("slot", "03")
	form.Set("more", "yes")
	rec := h.post(&sess, "/environments/new/preview", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("Add host = %d", rec.Code)
	}
	body := rec.Body.String()
	rows := formRows(body)
	if len(rows) != 8 || rows[0] != (formRow{name: "dc", image: "win2022", octet: "10"}) || rows[1] != (formRow{name: "Bad"}) || rows[2] != (formRow{}) {
		t.Errorf("rows = %+v, want dc, Bad, then 6 empty", rows)
	}
	contains(t, "the form again", body, `value="kilo.bravo"`, `<option value="03" selected>dev-03</option>`,
		`name="router_name" value="gw"`, `<option value="gw" selected>gw · v1</option>`)
	lacks(t, "the form again", body, `role="alert"`, "ferr", "aria-invalid")
	// Again: four more than it had.
	for i := range 4 {
		form.Add("host_name", "")
		form.Add("host_image", "")
		form.Add("host_octet", strings.Repeat(" ", i%2))
	}
	if rows := formRows(h.post(&sess, "/environments/new/preview", form).Body.String()); len(rows) != 12 {
		t.Errorf("Add host on 8 rows gives %d", len(rows))
	}
	h.noJobs("Add host")
}

// Add hosts names its environment in the title, shows what is already
// there (names and addresses, router first), and has no router row when
// the environment has a router. After Review, addresses already used are
// marked; a host already there is resumed.
func TestAddHostsForm(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnvs(h)
	sess := h.login(asLead)
	body := h.get(&sess, "/environments/kilo.alpha/add").Body.String()
	contains(t, "the form", body, `Add hosts to <span class="vm">kilo.alpha</span></h1>`,
		`<div class="sec"><span class="hint">Already here · dev-01</span><div class="here"><span>gw .2</span><span>yew .5</span><span>teak .10</span><span>elm</span></div></div>`,
		`<legend class="lbl">New hosts</legend>`, `action="/environments/kilo.alpha/add/preview"`)
	lacks(t, "the form", body, `name="router_name"`, `name="domain"`, `name="slot"`)
	if rows := formRows(body); len(rows) != blankRows {
		t.Errorf("%d host rows", len(rows))
	}
	form := rowsForm("", "", [3]string{"teak", "debian-12", ""}, [3]string{"oak", "debian-12", "10"}, [3]string{"ash", "debian-12", "5"}, [3]string{"fir", "debian-12", "11"})
	form.Del("router_name")
	form.Del("router_image")
	rec := h.post(&sess, "/environments/kilo.alpha/add/preview", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("preview = %d", rec.Code)
	}
	pick := "Leave it blank to have one picked."
	want := []formRow{
		{name: "teak", image: "debian-12"},
		{name: "oak", image: "debian-12", octet: "10", err: "Address .10 is already taken in kilo.alpha. " + pick, bad: "octet"},
		{name: "ash", image: "debian-12", octet: "5", err: "Address .5 is already taken in kilo.alpha. " + pick, bad: "octet"},
		{name: "fir", image: "debian-12", octet: "11"},
	}
	if got := formRows(rec.Body.String()); !slices.Equal(got, want) {
		t.Errorf("rows:\n got %+v\nwant %+v", got, want)
	}
	contains(t, "the form again", rec.Body.String(), `action="/environments/kilo.alpha/add/preview"`)
}

// An environment without a router gets a router row on Add hosts, filled
// in with its one image; choosing none, or removing the row (which posts
// no router fields), adds hosts only.
func TestAddHostsOffersARouterOnlyWithoutOne(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, false)
	sess := h.login(asLead)
	body := h.get(&sess, "/environments/kilo.alpha/add").Body.String()
	contains(t, "the form", body, `name="router_name" value="gw"`, `<b>2</b></div><button type="button" class="btn ghost sq x" data-remove-row aria-label="Remove the router" hidden>`)
	contains(t, "the router image", selectOf(t, body, "router_image"), `<option value="">No router</option><option value="gw" selected>gw · v1</option>`)
	lacks(t, "the form", body, "Already here · dev-01</span><div class=\"here\"><span>gw")

	for what, form := range map[string]url.Values{
		"none chosen": rowsForm("gw", "", [3]string{"oak", "debian-12", ""}),
		"removed":     {"host_name": {"oak"}, "host_image": {"debian-12"}, "host_octet": {""}},
	} {
		_, fields, _ := h.preview(&sess, "/environments/kilo.alpha/add", form)
		if fields.Get("router") != "" || fields.Get("env_hosts") != "oak=debian-12" {
			t.Errorf("%s: confirm fields %v", what, fields)
		}
	}
	// A removed router row stays removed when the form comes back.
	back := h.post(&sess, "/environments/kilo.alpha/add/preview", url.Values{"host_name": {"Oak"}, "host_image": {"debian-12"}, "host_octet": {""}}).Body.String()
	lacks(t, "the form after a removed router", back, `name="router_name"`)
	// Chosen, it is planned.
	_, fields, preview := h.preview(&sess, "/environments/kilo.alpha/add", rowsForm("fw", "gw", [3]string{"oak", "debian-12", ""}))
	if fields.Get("router") != "fw=gw" {
		t.Errorf("router field = %q", fields.Get("router"))
	}
	contains(t, "the preview", preview, "fw.kilo.alpha", "oak.kilo.alpha")
}

// Typed rows of any content either review, posting exactly the rows typed,
// with nothing blocked, or come back with exactly the rows a user would
// call wrong marked: half-filled, a name that isn't a DNS label or is
// used twice, an image with no usable version, a Windows name too long, an
// address out of range or used. The router row is checked the same.
func TestPropMachineRows(t *testing.T) {
	h := newHarness(t)
	addFormImages(h)
	sess := h.login(asLead)
	names := []string{"", "dc", "web", "dc", "gw", "a-b", "Bad", "x y", "averyveryverylongname", "<b>", "ü", " web ", "-x", "fw"}
	images := []string{"", "debian-12", "win2022", "appliance", "win10", "nope", "<x>", " debian-12"}
	octets := []string{"", "10", "4", "254", "3", "255", "010", ".11", "x", " 12 ", "-5", "10", "1e1", "١٢"}
	usable := map[string]string{"debian-12": pods.ImageLinux, "win2022": pods.ImageWindows, "appliance": pods.ImageOther}
	n := 0
	rapid.Check(t, func(t *rapid.T) {
		n++
		routerName := rapid.SampledFrom([]string{"gw", "", "Gw", "fw", "dc", "g w"}).Draw(t, "router name")
		routerImage := rapid.SampledFrom([]string{"gw", "", "debian-12", "nope"}).Draw(t, "router image")
		var rows [][3]string
		for range rapid.IntRange(0, 6).Draw(t, "rows") {
			rows = append(rows, [3]string{rapid.SampledFrom(names).Draw(t, "name"), rapid.SampledFrom(images).Draw(t, "image"), rapid.SampledFrom(octets).Draw(t, "octet")})
		}
		form := rowsForm(routerName, routerImage, rows...)
		form.Set("domain", "env"+strconv.Itoa(n)+".kilo") // a new domain each time

		// The oracle. A row is filled in as a host when it has a DNS-label
		// name, an image and, if any, a host's address; the planner's
		// rules then hold among those rows and the router: a name once, a
		// usable image, a Windows name short enough, a Windows or Linux
		// host's address not claimed first by another.
		shapeOK := func(name, octet string) bool {
			o, err := strconv.Atoi(octet)
			return name != "" && pods.CheckHostname(name) == nil && (octet == "" || err == nil && strconv.Itoa(o) == octet && o >= 4 && o <= 254)
		}
		seen, claimed := map[string]bool{}, map[int]string{}
		bad := !shapeOK(strings.TrimSpace(routerName), "") || strings.TrimSpace(routerImage) != "gw"
		if !bad {
			seen[strings.TrimSpace(routerName)] = true
		}
		var kept []string // the rows' specs, blank ones dropped
		var wantBad []bool
		type host struct {
			name, image string
			octet       int
		}
		var hosts []host
		for _, r := range rows {
			name, image, octet := strings.TrimSpace(r[0]), strings.TrimSpace(r[1]), strings.TrimPrefix(strings.TrimSpace(r[2]), ".")
			if name == "" && image == "" && octet == "" {
				continue
			}
			spec := name + "=" + image
			if octet != "" {
				spec += ":" + octet
			}
			kept = append(kept, spec)
			wantBad = append(wantBad, !shapeOK(name, octet) || image == "")
			if !wantBad[len(wantBad)-1] {
				o, _ := strconv.Atoi(octet)
				hosts = append(hosts, host{name, image, o})
				if o != 0 && claimed[o] == "" {
					claimed[o] = name
				}
			} else {
				hosts = append(hosts, host{})
			}
		}
		for i, h := range hosts {
			if h.name == "" {
				continue
			}
			kind := usable[h.image]
			wantBad[i] = seen[h.name] || kind == "" || kind == pods.ImageWindows && len(h.name) > 15 ||
				(kind == pods.ImageWindows || kind == pods.ImageLinux) && h.octet != 0 && claimed[h.octet] != h.name
			seen[h.name] = true
		}
		for _, b := range wantBad {
			bad = bad || b
		}

		rec := h.post(&sess, "/environments/new/preview", form)
		body := rec.Body.String()
		if !bad {
			if rec.Code != http.StatusOK {
				t.Fatalf("rows %q, router %q=%q: %d, want a review\n%s", rows, routerName, routerImage, rec.Code, body)
			}
			_, fields := confirmOf(h.t, body)
			if got := fields.Get("env_hosts"); got != strings.Join(kept, "\n") {
				t.Fatalf("env_hosts %q, want %q", got, strings.Join(kept, "\n"))
			}
			if strings.Contains(body, `class="blk"`) || !strings.Contains(body, "Create "+countVMs(len(kept)+1)+"</button>") {
				t.Fatalf("rows %q: the review blocks some\n%s", rows, body)
			}
			return
		}
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("rows %q, router %q=%q: %d, want the form marked\n%s", rows, routerName, routerImage, rec.Code, body)
		}
		got := formRows(body)
		for i, b := range wantBad {
			if i >= len(got) || (got[i].err != "") != b || (got[i].bad != "") != b {
				t.Fatalf("rows %q: row %d marked %+v, want marked %v", rows, i+1, got, b)
			}
		}
		for _, r := range got[len(wantBad):] {
			if r != (formRow{}) {
				t.Fatalf("rows %q: extra row %+v", rows, r)
			}
		}
	})
}

// A typed slot is read as typed, trimmed; Validate takes it just when it
// is a slot's name (pods.ParseSlotName): "02", not "2".
func TestPropSlotInput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		typed := rapid.OneOf(
			rapid.StringMatching(`[ ]?[0+-]?[0-9]{1,4}[ ]?`),
			rapid.SampledFrom([]string{"dev-02", "0x2", "٢", "02\n", "", " ", "1e1", "2.0", "99999999999999999999", "2", "002", " 03 "}),
			rapid.String(),
		).Draw(t, "slot")
		in := formInputs(pods.KindEnvCreate, url.Values{"domain": {"kilo.alpha"}, "router": {"gw=gw"}, "slot": {typed}})
		trimmed := strings.TrimSpace(typed)
		n, name := pods.ParseSlotName(trimmed)
		if in.Slot != trimmed {
			t.Fatalf("slot %q reads %q", typed, in.Slot)
		}
		switch verr := in.Validate(); {
		case trimmed == "":
			if verr != nil {
				t.Fatalf("no slot: %v", verr)
			}
		case name:
			if verr != nil {
				t.Fatalf("slot %q refused: %v", typed, verr)
			}
			if req, _ := in.EnvRequest(); req.Slot != n {
				t.Fatalf("slot %q plans slot %d", typed, req.Slot)
			}
		case verr == nil:
			t.Fatalf("slot %q (read %q) is valid", typed, in.Slot)
		}
		if again := Normalize(in); again.Slot != in.Slot {
			t.Fatalf("slot %q doesn't read back: %q, then %q", typed, in.Slot, again.Slot)
		}
	})
}

// A create that failed partway offers Retry on its page; the retry
// previews the same create in its slot for just the VMs that didn't
// finish, resuming one that exists, and confirms as an ordinary job.
func TestRetryAFailedCreate(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true) // kilo.alpha in dev-01: the create goes in dev-02
	in := jobs.Inputs{Kind: pods.KindEnvCreate, Domain: "kilo.bravo", Router: "gw=gw", EnvHosts: []string{"teak=debian-12", "oak=debian-12"}}
	id := h.submitJob(in, byLead)
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"gw.kilo.bravo":   {Status: store.ItemDone},
		"teak.kilo.bravo": {Status: store.ItemFailed, Error: "wait-agent: timeout"},
		"oak.kilo.bravo":  {Status: store.ItemNotRun},
	}})
	// What it left: the router, and teak at the address picked for it,
	// without its guest settings.
	h.api.Add(proxmox.VM{VMID: 2200, Name: "gw.kilo.bravo", Node: "n1", Pool: "dev-02", Tags: "dev;kilo.bravo;router", Status: "running"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	h.api.Add(proxmox.VM{VMID: 2201, Name: "teak.kilo.bravo", Node: "n1", Pool: "dev-02", Tags: "dev;kilo.bravo", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=router", "net0": "virtio=BC:24:11:00:22:01,bridge=devint02"})
	sess := h.login(asLead)

	page := h.get(&sess, "/logs/"+itoa(id)).Body.String()
	contains(t, "the job page", page, `<form method="post" action="/logs/`+itoa(id)+`/retry">`, "Retry 2 failed VMs</button>")

	rec := h.post(&sess, "/logs/"+itoa(id)+"/retry", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	contains(t, "the retry preview", body, `<p class="meta">Retry of #`+itoa(id)+": only the VMs that didn&#39;t finish.</p>",
		"<span>Clones oak into <span class=\"name\">dev-02</span> and starts it. Picks up teak where it stopped. Then teak and oak get their name,",
		`<tr><td><span class="vm">teak.kilo.bravo</span></td><td class="wide"><code>debian-12-v1</code></td><td class="c2"><code>.10</code></td><td class="r wide meta">2201 · n1</td></tr>`,
		`<tr><td><span class="vm">oak.kilo.bravo</span></td>`, "Create 2 VMs</button>")
	lacks(t, "the retry preview", body, `<span class="vm">gw.kilo.bravo</span></td>`)
	action, form := confirmOf(t, body)
	if action != "/environments/new/confirm" || form.Get("vms") != "oak.kilo.bravo,teak.kilo.bravo" || form.Get("slot") != "02" ||
		form.Get("router") != "gw=gw" || form.Get("env_hosts") != "teak=debian-12\noak=debian-12" {
		t.Errorf("retry confirm = %s %v", action, form)
	}
	if rec := h.post(&sess, action, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("retry confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	items, _ := h.st.Items(context.Background(), js[0].ID)
	var plan pods.Plan
	_ = json.Unmarshal(js[0].Plan, &plan)
	got := map[string]string{}
	for _, it := range plan.Items {
		got[it.Name] = strconv.Itoa(it.VMID) + " " + stepsText(it)
	}
	if len(js) != 2 || len(items) != 2 || len(got) != 2 || !strings.HasPrefix(got["teak.kilo.bravo"], "2201 label") ||
		!strings.HasPrefix(got["oak.kilo.bravo"], "22") || !strings.Contains(got["oak.kilo.bravo"], " clone") {
		t.Errorf("retry job items %+v, plan %v; want oak cloned and teak resumed, in dev-02", items, got)
	}
}

var (
	backRE   = regexp.MustCompile(`<a class="btn first" href="([^"]*)" data-panel-link>Back</a>`)
	domainRE = regexp.MustCompile(`id="domain" name="domain" value="([^"]*)"`)
	routerRE = regexp.MustCompile(`name="router_name" value="([^"]*)"`)
)

// reviewAgain opens a review's Back (the form, filled in) and presses
// Review on it as it is, returning the response.
func (h *harness) reviewAgain(sess *authtest.Session, review string) *httptest.ResponseRecorder {
	h.t.Helper()
	m := backRE.FindStringSubmatch(review)
	if m == nil {
		h.t.Fatalf("no Back in:\n%s", review)
	}
	back := html.UnescapeString(m[1])
	page := h.get(sess, back).Body.String()
	form := url.Values{}
	if d := domainRE.FindStringSubmatch(page); d != nil {
		form.Set("domain", html.UnescapeString(d[1]))
	}
	if strings.Contains(page, `name="slot"`) {
		if s := selectedRE.FindStringSubmatch(selectOf(h.t, page, "slot")); s != nil {
			form.Set("slot", s[1])
		}
	}
	if r := routerRE.FindStringSubmatch(page); r != nil {
		form.Set("router_name", html.UnescapeString(r[1]))
		form.Set("router_image", "")
		if s := selectedRE.FindStringSubmatch(selectOf(h.t, page, "router_image")); s != nil {
			form.Set("router_image", html.UnescapeString(s[1]))
		}
	}
	for _, r := range formRows(page) {
		form.Add("host_name", r.name)
		form.Add("host_image", r.image)
		form.Add("host_octet", r.octet)
	}
	path, _, _ := strings.Cut(back, "?")
	return h.post(sess, path+"/preview", form)
}

// A retry's Back opens the form filled in with the job's machines, and
// Review on it reviews: a domain, slot and hosts already there are
// resumed, not marked.
func TestRetryChangedReviews(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	create := h.submitJob(jobs.Inputs{Kind: pods.KindEnvCreate, Domain: "kilo.bravo", Slot: "02", Router: "gw=gw", EnvHosts: []string{"teak=debian-12:10"}}, byLead)
	add := h.submitJob(jobs.Inputs{Kind: pods.KindEnvAdd, Domain: "kilo.alpha", EnvHosts: []string{"oak=debian-12", "ash=debian-12"}}, byLead)
	for _, id := range []int64{create, add} {
		h.start(id)
	}
	h.finish(create, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"gw.kilo.bravo": {Status: store.ItemDone}, "teak.kilo.bravo": {Status: store.ItemFailed, Error: "wait-agent: timeout"}}})
	h.finish(add, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"oak.kilo.alpha": {Status: store.ItemDone}, "ash.kilo.alpha": {Status: store.ItemFailed, Error: "clone: storage full"}}})
	h.api.Add(proxmox.VM{VMID: 2200, Name: "gw.kilo.bravo", Node: "n1", Pool: "dev-02", Tags: "dev;kilo.bravo;router", Status: "running"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	h.api.Add(proxmox.VM{VMID: 2201, Name: "teak.kilo.bravo", Node: "n1", Pool: "dev-02", Tags: "dev;kilo.bravo", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=router", "net0": "virtio=BC:24:11:00:22:01,bridge=devint02"})
	h.api.Add(proxmox.VM{VMID: 2104, Name: "oak.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.20 os=linux base=debian-12-v1 dns=router", "net0": "virtio=BC:24:11:00:21:04,bridge=devint01"})
	for _, c := range []struct {
		id      int64
		confirm string
		want    []string
	}{
		{create, "/environments/new/confirm", []string{"Picks up gw and teak where they stopped.", "Create 2 VMs</button>"}},
		{add, "/environments/kilo.alpha/add/confirm", []string{"Clones ash into", "Picks up oak where it stopped.", "Add 2 hosts</button>"}},
	} {
		retry := h.post(&sess, "/logs/"+itoa(c.id)+"/retry", url.Values{})
		if retry.Code != http.StatusOK {
			t.Fatalf("retry of #%d = %d\n%s", c.id, retry.Code, retry.Body)
		}
		rec := h.reviewAgain(&sess, retry.Body.String())
		if rec.Code != http.StatusOK {
			t.Fatalf("job #%d's retry changed: Review = %d\n%s", c.id, rec.Code, rec.Body)
		}
		contains(t, "the review", rec.Body.String(), c.want...)
		if action, _ := confirmOf(t, rec.Body.String()); action != c.confirm {
			t.Errorf("confirms at %s, want %s", action, c.confirm)
		}
	}
}

// What only planning finds, such as a slot holding VMs the user can't
// see, is marked on its field too.
func TestPlanRefusalMarksItsField(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	h.api.Mu.Lock()
	h.api.Unseen = map[int]bool{2305: true}
	h.api.Mu.Unlock()
	sess := h.login(asLead)
	form := rowsForm("gw", "gw", [3]string{"teak", "debian-12", ""})
	form.Set("domain", "kilo.bravo")
	form.Set("slot", "03")
	rec := h.post(&sess, "/environments/new/preview", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("preview = %d\n%s", rec.Code, rec.Body)
	}
	contains(t, "the form", rec.Body.String(), `<span class="ferr" id="slot-err">Dev slot 03 holds VMs you can&#39;t see; choose another.</span>`,
		"Fix the marked fields, then review again.", `value="teak"`)
	lacks(t, "the form", rec.Body.String(), "Couldn&#39;t make a plan")
}

// A failed add's retry opens Add hosts at its environment's path.
func TestRetryAFailedAdd(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	id := h.submitJob(jobs.Inputs{Kind: pods.KindEnvAdd, Domain: "kilo.alpha", EnvHosts: []string{"oak=debian-12", "ash=debian-12"}}, byLead)
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"oak.kilo.alpha": {Status: store.ItemDone},
		"ash.kilo.alpha": {Status: store.ItemFailed, Error: "clone: storage full"},
	}})
	sess := h.login(asLead)
	rec := h.post(&sess, "/logs/"+itoa(id)+"/retry", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry = %d\n%s", rec.Code, rec.Body)
	}
	action, form := confirmOf(t, rec.Body.String())
	if action != "/environments/kilo.alpha/add/confirm" || form.Get("vms") != "ash.kilo.alpha" {
		t.Errorf("retry confirm = %s %v", action, form)
	}
	if rec := h.post(&sess, action, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("retry confirm = %d\n%s", rec.Code, rec.Body)
	}
}

// The environment forms fit a phone, light and dark, with field errors
// too. With the script, Add host adds a row in place and × removes one,
// renumbering the rest; Enter in a field reviews rather than adding rows.
func TestBrowserEnvironmentForms(t *testing.T) {
	h := browserHarness(t)
	addFormImages(h)
	addEnvs(h) // oscar.lima has no router
	lead := h.login(asLead)
	rows := `document.querySelectorAll("[data-row]").length`
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/environments/new")
			b.noSideways("create")
			b.shot("env-new-" + size.name)
			if !b.is(rows + ` === 4 && [...document.querySelectorAll("[data-row] [data-remove-row]")].every(x => !x.hidden)`) {
				t.Errorf("the create form doesn't start with 4 removable host rows")
			}

			b.run("filling the form", chromedp.SendKeys("#domain", "kilo.bravo"),
				chromedp.SendKeys(`input[aria-label="Host 1 name"]`, "teak", chromedp.ByQuery),
				chromedp.SetValue(`select[aria-label="Host 1 image"]`, "debian-12", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 1 last octet"]`, "10", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 2 name"]`, "oak", chromedp.ByQuery))
			b.click(`[data-add-row]`)
			b.waitFor("a fifth row", rows+` === 5`)
			if !b.is(`location.pathname === "/environments/new" && document.activeElement.getAttribute("aria-label") === "Host 5 name" && document.activeElement.value === ""`) {
				t.Errorf("Add host didn't add an empty row in place and focus it: %s", b.where())
			}
			b.click(`button[aria-label="Remove host 2"]`)
			b.waitFor("four rows", rows+` === 4`)
			var names []string
			b.eval(`[...document.querySelectorAll("[data-row] input[name=host_name]")].map(i => i.getAttribute("aria-label") + "=" + i.value)`, &names)
			if strings.Join(names, ",") != "Host 1 name=teak,Host 2 name=,Host 3 name=,Host 4 name=" {
				t.Errorf("after removing oak's row, rows %v", names)
			}
			b.noSideways("create with rows changed")
			b.shot("env-new-rows-" + size.name)

			// Enter in a name field reviews.
			b.run("pressing Enter", chromedp.SendKeys(`input[aria-label="Host 1 name"]`, kb.Enter, chromedp.ByQuery))
			b.waitFor("the review", `location.pathname === "/environments/new/preview" && document.querySelector('form[action="/environments/new/confirm"]') !== null`)
			b.noSideways("create review")
			b.shot("env-new-review-" + size.name)

			// Field errors.
			b.clean()
			b.open("/environments/new")
			b.run("filling the form wrongly", chromedp.SendKeys("#domain", "kilo.bravo"),
				chromedp.SendKeys(`input[aria-label="Host 1 name"]`, "averyveryverylongname", chromedp.ByQuery),
				chromedp.SetValue(`select[aria-label="Host 1 image"]`, "win2022", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 2 name"]`, "web", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 2 last octet"]`, "300", chromedp.ByQuery),
				chromedp.SetValue(`select[aria-label="Host 2 image"]`, "debian-12", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 3 name"]`, "wks1", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 4 name"]`, "db", chromedp.ByQuery))
			b.click(`.pf button[type=submit]`)
			b.waitFor("the form marked", `document.querySelectorAll(".mrow .ferr").length === 4`)
			b.expect("status of 422")
			b.noSideways("create with errors")
			b.shot("env-new-errors-" + size.name)
			// A row added after one marked comes clean.
			b.click(`[data-add-row]`)
			b.waitFor("a fifth row", rows+` === 5`)
			if !b.is(`(r => !r.querySelector(".ferr, [aria-invalid], [aria-describedby]") && r.querySelector("input").value === "")(document.querySelectorAll("[data-row]")[4])`) {
				t.Errorf("a row added after a marked one is marked or filled: %s", b.where())
			}
			// On a phone a row's image is under its name and address; on a
			// laptop, beside them.
			under := b.is(`(r => r.querySelector(".im").getBoundingClientRect().top >= r.querySelector(".n").getBoundingClientRect().bottom)(document.querySelector("[data-row]"))`)
			if under != b.phone {
				t.Errorf("phone %v: a row's image is under its name: %v", b.phone, under)
			}

			// Add hosts, with a router row for oscar.lima, which has none.
			b.open("/environments/kilo.alpha/add")
			b.noSideways("add hosts")
			b.shot("env-add-" + size.name)
			b.open("/environments/oscar.lima/add")
			b.noSideways("add hosts with a router row")
			b.shot("env-add-router-" + size.name)
			b.click(`button[aria-label="Remove the router"]`)
			b.waitFor("the router row gone", `document.querySelector(".mrow.router") === null`)
			b.clean()
		})
	}
}
