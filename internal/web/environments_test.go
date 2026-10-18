package web

import (
	"bytes"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// addEnvs puts kilo.alpha in dev-01, its hosts' names and addresses in
// different orders, one stopped and one without an address, and oscar.lima
// in dev-04.
func addEnvs(h *harness) {
	for _, vm := range []struct {
		vmid             int
		name, pool, tags string
		status, desc     string
	}{
		{2101, "teak.kilo.alpha", "dev-01", "dev;kilo.alpha;linux", "running", "debian / Changeme1!\nbattleship: ip=192.168.100.10 os=linux base=debian-12-v2"},
		{2102, "elm.kilo.alpha", "dev-01", "dev;kilo.alpha", "stopped", "battleship: base=win10-v1"},
		{2103, "yew.kilo.alpha", "dev-01", "dev;kilo.alpha;linux", "stopped", "battleship: ip=192.168.100.5 os=linux base=debian-12-v2"},
		{2100, "gw.kilo.alpha", "dev-01", "dev;kilo.alpha;router", "running", "admin / gwpass\nbattleship: base=gw-v1 dns=router"},
		{2400, "ad.oscar.lima", "dev-04", "dev;oscar.lima;windows", "running", "battleship: ip=192.168.100.10 os=windows base=win10-v1 dns=ad"},
	} {
		cfg := map[string]string{"description": vm.desc, "net0": "virtio=BC:24:11:00:21:00,bridge=devint" + vm.pool[4:]}
		if vm.vmid == 2100 {
			cfg["net0"] = "virtio=BC:24:11:00:21:00,bridge=devext01"
			cfg["net1"] = "virtio=BC:24:11:00:21:01,bridge=devint01"
		}
		h.api.Add(proxmox.VM{VMID: vm.vmid, Name: vm.name, Node: "n1", Pool: vm.pool, Tags: vm.tags, Status: vm.status}, cfg)
	}
}

// envSection is the markup of the block with anchor id.
func envSection(t *testing.T, body, id string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<section class="env" id="` + regexp.QuoteMeta(id) + `".*?</section>`).FindString(body)
	if m == "" {
		t.Fatalf("no block %s in:\n%s", id, body)
	}
	return m
}

var hnameRE = regexp.MustCompile(`<span class="hname">([^<]*)</span>`)

// hostNames are the host names a block lists, in order.
func hostNames(section string) []string {
	var out []string
	for _, m := range hnameRE.FindAllStringSubmatch(section, -1) {
		out = append(out, m[1])
	}
	return out
}

// Each used slot is a block anchored at its domain, with its slot, router
// and DNS, its actions at its own paths, and its hosts router first, then
// by address, with their status, image and notes. The free slots are one
// line of runs, naming the one Create takes.
func TestEnvironmentsPage(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	addEnvs(h)
	sess := h.login(asLead)
	body := h.get(&sess, "/environments").Body.String()
	contains(t, "the page", body,
		`<a class="btn pri" href="/environments/new">Create environment</a>`,
		`<p class="free"><span class="sub">Free</span><span class="vm">dev-02, dev-03, dev-05 – dev-10</span><span class="meta">· Create uses dev-02</span></p>`)
	kilo := envSection(t, body, "kilo.alpha")
	contains(t, "kilo.alpha", kilo,
		`<h2 id="kilo.alpha-h">kilo.alpha</h2>`,
		`<span>dev-01</span><span>router <b>gw</b></span><span>DNS <b>router</b></span>`,
		`<a class="btn sm" href="/environments/kilo.alpha/add">Add hosts</a>`,
		`<a class="btn sm" href="/environments/kilo.alpha/dns">Set DNS</a>`,
		`<span class="hname">gw</span> <span class="chip">router</span>`,
		`<code>192.168.100.5</code>`,
		`<span class="gl is-running"><i class="g" aria-hidden="true"></i>running</span> <span class="meta">· gw-v1</span>`,
		`<div class="notes">admin / gwpass</div>`,
		`<span class="gl is-stopped"><i class="g" aria-hidden="true"></i>stopped</span> <span class="meta">· debian-12-v2</span>`,
		`<span class="meta">no address</span>`)
	lacks(t, "kilo.alpha", kilo, "battleship:")
	if got := strings.Join(hostNames(kilo), " "); got != "gw yew teak elm" {
		t.Errorf("hosts in order %q, want gw yew teak elm", got)
	}
	contains(t, "oscar.lima", envSection(t, body, "oscar.lima"), `<span>dev-04</span><span>router <b>none</b></span><span>DNS <b>ad</b></span>`)
	if n := strings.Count(body, `<section class="env"`); n != 2 {
		t.Errorf("%d blocks, want 2", n)
	}
}

// A free slot a queued job will fill isn't offered: Create uses the next,
// and the line says which slot is spoken for.
func TestEnvironmentsPageSkipsAReservedSlot(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnvs(h)
	h.submitJob(jobs.Inputs{Kind: pods.KindEnvCreate, Domain: "kilo.bravo", Router: "gw=gw"}, byLead)
	sess := h.login(asLead)
	body := h.get(&sess, "/environments").Body.String()
	contains(t, "the page", body,
		`<span class="vm">dev-03, dev-05 – dev-10</span><span class="meta">· Create uses dev-03</span><span class="meta">· a queued job will fill dev-02</span>`)
}

// Slots that can't be acted on by one domain still show, with why: VMs of
// several domains, a domain an earlier slot holds too, and VMs from outside
// on a slot's networks. Only one block is anchored at a domain, and only
// it offers actions.
func TestEnvironmentsPageOddSlots(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	addEnvs(h)
	for _, vm := range []struct {
		vmid       int
		name, pool string
		bridge     string
	}{
		{2200, "a.kilo.bravo", "dev-02", "devint02"},
		{2201, "b.other.zone", "dev-02", "devint02"},
		{2500, "oak.kilo.alpha", "dev-05", "devint05"},
		{150, "stray-one", "", "devint03"},
		{151, "stray-two", "", "devint01"},
	} {
		h.api.Add(proxmox.VM{VMID: vm.vmid, Name: vm.name, Node: "n1", Pool: vm.pool, Status: "running"},
			map[string]string{"net0": "virtio=BC:24:11:00:22:00,bridge=" + vm.bridge})
	}
	sess := h.login(asLead)
	body := h.get(&sess, "/environments").Body.String()

	mixed := envSection(t, body, "slot-02")
	contains(t, "the mixed slot", mixed, `<h2 id="slot-02-h">dev-02</h2>`, `<span class="chip">mixed domains</span>`,
		"don't share one domain", `<span class="hname">a.kilo.bravo</span>`, `<span class="hname">b.other.zone</span>`)
	lacks(t, "the mixed slot", mixed, "Add hosts</a>", "Set DNS</a>", "/teardown")

	twin := envSection(t, body, "slot-05")
	contains(t, "the second kilo.alpha", twin, `<h2 id="slot-05-h">kilo.alpha</h2>`, "kilo.alpha is also in dev-01: Add hosts, Set DNS and Tear down act on that one.")
	lacks(t, "the second kilo.alpha", twin, "Add hosts</a>", "/teardown")
	if n := strings.Count(body, `id="kilo.alpha"`); n != 1 {
		t.Errorf("%d blocks anchored at kilo.alpha", n)
	}

	users := envSection(t, body, "slot-03")
	contains(t, "the slot used from outside", users, `<h2 id="slot-03-h">dev-03</h2>`, `<span class="chip">in use</span>`,
		"Nothing is in dev-03, but VMs outside it are on its networks, so Create skips it: <code>stray-one</code>.")
	lacks(t, "the slot used from outside", users, "<table")
	contains(t, "kilo.alpha", envSection(t, body, "kilo.alpha"), "Also on its networks, from outside dev-01: <code>stray-two</code>.",
		`<a class="btn sm" href="/environments/kilo.alpha/add">Add hosts</a>`,
		`<a class="btn sm bad" href="/environments/kilo.alpha/teardown">Tear down kilo.alpha</a>`)

	contains(t, "the free line", body, `<span class="vm">dev-06 – dev-10</span><span class="meta">· Create uses dev-06</span>`)
}

// Without a privilege, its action shows disabled and one line names it.
func TestEnvironmentsPageLacksPrivileges(t *testing.T) {
	h := newHarness(t)
	addEnvs(h)
	op := h.login(asOperator)
	body := h.get(&op, "/environments").Body.String()
	contains(t, "as an operator", body,
		`<button class="btn pri" type="button" disabled>Create environment</button>`,
		`<button class="btn sm" type="button" disabled>Add hosts</button>`,
		`<button class="btn sm" type="button" disabled>Set DNS</button>`,
		`<button class="btn sm bad" type="button" disabled>Tear down kilo.alpha</button>`,
		"Creating an environment needs VM.Clone, which you don&#39;t have.",
		"Adding hosts needs VM.Clone, which you don&#39;t have.",
		"Setting DNS needs VM.GuestAgent.Unrestricted, which you don&#39;t have.",
		"Tearing down an environment needs VM.Allocate, which you don&#39;t have.")
	lead := h.login(asLead)
	lacks(t, "as a lead", h.get(&lead, "/environments").Body.String(), "disabled", "which you don&#39;t have")
}

// Add hosts and Set DNS live at their environment's path, which fixes the
// domain whatever the form posts; no or an unknown environment goes back
// to Environments, saying why, and a path that can't name one does so
// without reading the cluster.
func TestEnvironmentRoutes(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	for _, sub := range []string{"add", "dns"} {
		if rec := h.get(&sess, "/environments/kilo.alpha/"+sub); rec.Code != http.StatusOK ||
			!strings.Contains(rec.Body.String(), `action="/environments/kilo.alpha/`+sub+`/preview"`) {
			t.Errorf("GET %s = %d", sub, rec.Code)
		}
	}
	goesBack := func(rec *httptest.ResponseRecorder, what, flash string) {
		t.Helper()
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/environments" || flashOf(rec) != flash {
			t.Errorf("%s = %d to %q, flash %q; want back to /environments saying %q", what, rec.Code, rec.Header().Get("Location"), flashOf(rec), flash)
		}
	}
	gone := "There's no environment kilo.bravo. It may have been torn down or renamed; pick one below."
	pick := "Add hosts, Set DNS and Tear down start from an environment: pick one below."
	goesBack(h.get(&sess, "/environments/kilo.bravo/add"), "an unknown environment", gone)
	goesBack(h.post(&sess, "/environments/kilo.bravo/dns/preview", url.Values{"dns": {"router"}}), "an unknown environment's preview", gone)
	before, _, _ := h.api.readCounts()
	goesBack(h.get(&sess, "/environments/Kilo.Alpha/add"), "a path that isn't a domain", pick)
	goesBack(h.post(&sess, "/environments/x/dns/preview", nil), "a preview at a path that isn't a domain", pick)
	if after, _, _ := h.api.readCounts(); after != before {
		t.Errorf("the cluster was read for paths that aren't domains")
	}
	// The flash shows on the page it lands on.
	rec := h.get(&sess, "/environments/kilo.bravo/add")
	page := h.panelGet(&sess, "/environments", rec.Result().Cookies()).Body.String()
	contains(t, "Environments after an unknown environment", page, "There&#39;s no environment <span class=\"name\">kilo.bravo</span>")

	// The path fixes the domain: one posted is ignored.
	_, fields, body := h.preview(&sess, "/environments/kilo.alpha/add", url.Values{"domain": {"kilo.bravo"}, "env_hosts": {"oak=debian-12"}})
	contains(t, "a preview posted with another domain", body, "oak.kilo.alpha")
	if fields.Get("domain") != "kilo.alpha" {
		t.Errorf("the confirm carries domain %q", fields.Get("domain"))
	}
}

// The old addresses, with the environment in a domain field, lead to its
// path, keeping the rest of the query, or of a posted body; without a
// domain, back to Environments.
func TestOldEnvironmentAddresses(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	for from, to := range map[string]string{
		"/environments/add?domain=kilo.alpha":                    "/environments/kilo.alpha/add",
		"/environments/add?domain=kilo.alpha&router=gw%3Dgw":     "/environments/kilo.alpha/add?router=gw%3Dgw",
		"/environments/dns?domain=kilo.alpha&dns=teak":           "/environments/kilo.alpha/dns?dns=teak",
		"/environments/dns?domain=kilo.bravo":                    "/environments/kilo.bravo/dns",
		"/environments/add":                                      "/environments",
		"/environments/dns?domain=%3Cscript%3E":                  "/environments",
		"/environments/add?domain=kilo.alpha%2F..%2F..%2Fdeploy": "/environments",
	} {
		rec := h.get(&sess, from)
		if got := rec.Header().Get("Location"); got != to || rec.Code/100 != 3 {
			t.Errorf("GET %s = %d to %q, want %q", from, rec.Code, got, to)
		}
	}
	for _, step := range []string{"preview", "confirm"} {
		rec := h.post(&sess, "/environments/add/"+step, url.Values{"domain": {"kilo.alpha"}, "env_hosts": {"oak=debian-12"}})
		if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/environments/kilo.alpha/add/"+step {
			t.Errorf("POST %s = %d to %q", step, rec.Code, rec.Header().Get("Location"))
		}
		if rec := h.post(&sess, "/environments/dns/"+step, url.Values{"dns": {"router"}}); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/environments" {
			t.Errorf("POST dns %s without a domain = %d to %q", step, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// Every way out of Add hosts and Set DNS goes back to the environment's
// block: the form's close and Cancel, the preview's close and Back, and
// the job's; Change, and a failed job's Start again, go to the form at its
// path, filled in; the confirm posts to its path.
func TestEnvironmentBackTargets(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	for _, c := range []struct {
		sub  string
		form url.Values
		edit string
	}{
		{"add", url.Values{"env_hosts": {"oak=debian-12"}}, "/environments/kilo.alpha/add?env_hosts=oak%3Ddebian-12"},
		{"dns", url.Values{"dns": {"router"}}, "/environments/kilo.alpha/dns?dns=router"},
	} {
		form := h.get(&sess, "/environments/kilo.alpha/"+c.sub).Body.String()
		contains(t, c.sub+" form", form, `<a class="btn ghost sq" href="/environments#kilo.alpha" data-close aria-label="Close">`,
			`<a class="btn" href="/environments#kilo.alpha">Cancel</a>`)
		action, fields, preview := h.preview(&sess, "/environments/kilo.alpha/"+c.sub, c.form)
		contains(t, c.sub+" preview", preview, `<a class="btn ghost sq" href="/environments#kilo.alpha" data-close aria-label="Close">`,
			`href="`+template.HTMLEscapeString(c.edit)+`"`)
		if action != "/environments/kilo.alpha/"+c.sub+"/confirm" {
			t.Errorf("%s confirms at %q", c.sub, action)
		}
		rec := h.post(&sess, action, fields)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s confirm = %d\n%s", c.sub, rec.Code, rec.Body)
		}
		job := h.get(&sess, rec.Header().Get("Location")).Body.String()
		contains(t, c.sub+" job", job, `<a class="btn ghost back" href="/environments#kilo.alpha">`)
		id := h.jobsInStore()[0].ID
		h.start(id)
		h.finish(id, store.Outcome{Status: store.StatusFailed})
		job = h.get(&sess, logHref(id)).Body.String()
		contains(t, c.sub+" failed job", job, `href="`+template.HTMLEscapeString(c.edit)+`"`)
	}
}

// Any path segment either names an environment that exists, whose form it
// serves, or goes back to Environments: never an error page, never another
// environment's form, and the cluster is read only for a valid domain.
func TestPropEnvironmentPath(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	rapid.Check(t, func(t *rapid.T) {
		seg := rapid.OneOf(rapid.SampledFrom([]string{"kilo.alpha", "kilo.bravo", "kilo", "kilo.alpha.", "KILO.alpha"}),
			rapid.SampledFrom(hostileStrings), rapid.String(), rapid.StringMatching(`[a-z0-9.-]{1,16}`)).Draw(t, "domain")
		if seg == "" || seg == "." || seg == ".." {
			return // the path would be /environments/add, or above it
		}
		sub := rapid.SampledFrom([]string{"add", "dns"}).Draw(t, "sub")
		before, _, _ := h.api.readCounts()
		rec := h.get(&sess, "/environments/"+url.PathEscape(seg)+"/"+sub)
		after, _, _ := h.api.readCounts()
		valid := pods.CheckDomain(seg) == nil
		switch {
		case !valid && after != before:
			t.Fatalf("%q isn't a domain, but the cluster was read for it", seg)
		case seg == "kilo.alpha" && (rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `action="/environments/kilo.alpha/`+sub+`/preview"`)):
			t.Fatalf("kilo.alpha = %d", rec.Code)
		case strings.Contains(seg, "/") && rec.Code == http.StatusNotFound:
			// An escaped slash the mux may not route.
		case seg != "kilo.alpha" && (rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/environments"):
			t.Fatalf("%q = %d to %q, want back to /environments", seg, rec.Code, rec.Header().Get("Location"))
		case seg != "kilo.alpha" && valid != strings.Contains(flashOf(rec), "There's no environment "+seg+"."):
			t.Fatalf("%q: the flash %q names it = %v", seg, flashOf(rec), !valid)
		}
		if _, err := checkMarkup(rec.Body.String()); err != nil {
			t.Fatalf("%q: %v", seg, err)
		}
	})
}

// VM names and notes, which anyone with the right privileges can set in
// Proxmox, show on Environments as text, each host once, in well-formed
// markup.
func TestPropEnvironmentsPageShowsVMsAsText(t *testing.T) {
	h := newHarness(t)
	sess := h.login(asLead)
	esc := template.Must(template.New("").Parse("{{.}}"))
	escaped := func(s string) string {
		var b bytes.Buffer
		_ = esc.Execute(&b, s)
		return b.String()
	}
	var names []string
	rapid.Check(t, func(t *rapid.T) {
		h.api.remove(names...)
		names = nil
		domain := rapid.SampledFrom([]string{"kilo.alpha", "oscar.lima"}).Draw(t, "domain")
		n := rapid.IntRange(1, 4).Draw(t, "hosts")
		var notes []string
		for i := range n {
			// Unique by i; mostly host.domain, sometimes anything.
			host := rapid.OneOf(rapid.StringMatching(`[a-z][a-z0-9-]{0,8}`), rapid.SampledFrom(hostileStrings)).Draw(t, "host")
			name := host + strconv.Itoa(i) + "." + domain
			if rapid.IntRange(0, 3).Draw(t, "odd name") == 0 {
				name = hostile(t, "name") + strconv.Itoa(i)
			}
			note := hostile(t, "notes")
			desc := note + "\nbattleship: ip=192.168.100." + strconv.Itoa(10+i) + " os=linux base=debian-12-v1"
			h.api.Add(proxmox.VM{VMID: 2100 + i, Name: name, Node: "n1", Pool: "dev-01", Status: "running"}, map[string]string{"description": desc})
			names = append(names, name)
			notes = append(notes, pods.UserNotes(desc))
		}
		body := h.get(&sess, "/environments").Body.String()
		if _, err := checkMarkup(body); err != nil {
			t.Fatal(err)
		}
		section := regexp.MustCompile(`(?s)<section class="env".*?</section>`).FindString(body)
		shown := hostNames(section)
		if len(shown) != n {
			t.Fatalf("%d hosts shown, want %d:\n%s", len(shown), n, section)
		}
		mixed := false
		for _, name := range names {
			d := pods.Domain(name)
			mixed = mixed || d == "" || d != pods.Domain(names[0])
		}
		if mixed != strings.Contains(section, "mixed domains") {
			t.Fatalf("mixed = %v, but the block says otherwise:\n%s", mixed, section)
		}
		for i, name := range names {
			want := name
			if !mixed {
				want = pods.Hostname(name)
			}
			if !strings.Contains(section, `<span class="hname">`+escaped(want)+`</span>`) {
				t.Fatalf("%q isn't shown as text:\n%s", want, section)
			}
			if notes[i] != "" && !strings.Contains(section, `<div class="notes">`+escaped(notes[i])+`</div>`) {
				t.Fatalf("notes %q aren't shown as text:\n%s", notes[i], section)
			}
		}
	})
}

// slotRuns names every slot once, in order, writing a run of three or more
// in a row as its ends.
func TestPropSlotRuns(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var slots []pods.Slot
		for n := range 30 {
			if rapid.Bool().Draw(t, "free") {
				slots = append(slots, pods.Slot{N: n + 1, Pool: "dev-" + strconv.Itoa(100 + n + 1)[1:]})
			}
		}
		var back []string
		for _, part := range strings.Split(slotRuns(slots), ", ") {
			first, last, run := strings.Cut(part, " – ")
			if !run {
				back = append(back, part)
				continue
			}
			a, _ := strconv.Atoi(strings.TrimPrefix(first, "dev-"))
			b, _ := strconv.Atoi(strings.TrimPrefix(last, "dev-"))
			if b-a < 2 {
				t.Fatalf("run %q is shorter than three", part)
			}
			for n := a; n <= b; n++ {
				back = append(back, "dev-"+strconv.Itoa(100 + n)[1:])
			}
		}
		var want []string
		for _, s := range slots {
			want = append(want, s.Pool)
		}
		if len(want) == 0 {
			want = []string{""}
		}
		if strings.Join(back, ",") != strings.Join(want, ",") {
			t.Fatalf("slotRuns = %q, which names %v, want %v", slotRuns(slots), back, want)
		}
		for i := 0; i+2 < len(slots); i++ {
			if slots[i+2].N == slots[i].N+2 && strings.Contains(slotRuns(slots), slots[i].Pool+", "+slots[i+1].Pool+", "+slots[i+2].Pool) {
				t.Fatalf("slotRuns = %q lists three in a row", slotRuns(slots))
			}
		}
	})
}

func TestEnvironmentsPageHostWarnings(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Deploy.CPUTypes = []string{"baseline"}
	})
	h.api.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "", Status: "running"},
		map[string]string{
			"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v2",
			"net0":        "virtio=BC:24:11:00:21:01,bridge=devint02",
			"cpu":         "x86-64-v2-AES",
			"agent":       "0",
			"ide2":        "local:iso/debian.iso,media=cdrom",
		})
	sess := h.login(asLead)
	body := h.get(&sess, "/environments").Body.String()
	contains(t, "the environment row", body,
		`<span class="chip is-drifted">missing tags dev, kilo.alpha</span>`,
		`<span class="chip is-drifted">net0 on devint02, expected devint01</span>`,
		`<span class="chip is-drifted">CPU x86-64-v2-AES, expected baseline</span>`,
		`<span class="chip is-drifted">guest agent off</span>`,
		`<span class="chip is-drifted">CD-ROM attached</span>`)
}
