package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// addImages puts a router and a Linux base image on the harness's cluster.
func addImages(h *harness) {
	h.api.Add(proxmox.VM{VMID: 8000, Name: "gw-v1", Node: "n1", Template: true, Tags: "base;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:80:00,bridge=vmbr0", "net1": "virtio=BC:24:11:00:80:01,bridge=vmbr0"})
	h.api.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v1", Node: "n1", Template: true, Tags: "base;linux"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:80:02,bridge=vmbr0"})
}

func TestEnvironmentsPageListsSlots(t *testing.T) {
	h := newHarness(t)
	h.api.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	sess := h.login(asLead)
	body := h.get(&sess, "/environments").Body.String()
	for _, want := range []string{"kilo.alpha", "gw.kilo.alpha", "dev-02", "free", "/environments/new"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestNewEnvironmentPreviewShowsSlotAndAddresses(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	sess := h.login(asLead)
	body := h.post(&sess, "/environments/new/preview", url.Values{
		"domain": {"kilo.alpha"}, "router": {"gw=gw"}, "env_hosts": {"teak=debian-12:10"}}).Body.String()
	for _, want := range []string{"gw.kilo.alpha", "teak.kilo.alpha", "configure-guest", "Create environment"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	if strings.Contains(body, "you don't have") {
		t.Errorf("a lead is blocked:\n%s", body)
	}
}

func TestImagesPageListsFamilies(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	sess := h.login(asLead)
	body := h.get(&sess, "/images").Body.String()
	for _, want := range []string{"debian-12", "debian-12-v1", "linux", "/images/register"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// A new environment goes from preview to a stored job, its hosts (one per
// line) surviving the confirm's hidden fields, and the job pages name its
// domain where a team job names teams.
func TestNewEnvironmentConfirmStoresJob(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	sess := h.login(asLead)
	action, fields, _ := h.preview(&sess, "/environments/new", url.Values{
		"domain": {"kilo.alpha"}, "router": {"gw=gw"}, "env_hosts": {"teak=debian-12:10\r\noak=debian-12\n"}})
	if action != "/environments/new/confirm" {
		t.Fatalf("confirm posts to %q", action)
	}
	rec := h.post(&sess, action, fields)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 || js[0].Kind != string(pods.KindEnvCreate) {
		t.Fatalf("jobs = %+v, want one env-create", js)
	}
	in := readInputs(js[0])
	want := jobs.Inputs{Kind: pods.KindEnvCreate, Domain: "kilo.alpha", Router: "gw=gw", EnvHosts: []string{"teak=debian-12:10", "oak=debian-12"}}
	if in.Domain != want.Domain || in.Router != want.Router || strings.Join(in.EnvHosts, "|") != strings.Join(want.EnvHosts, "|") {
		t.Errorf("job inputs = %+v, want %+v", in, want)
	}

	page := h.get(&sess, rec.Header().Get("Location")).Body.String()
	for _, want := range []string{"Create environment", "kilo.alpha", "teak=debian-12:10"} {
		if !strings.Contains(page, want) {
			t.Errorf("job page lacks %q", want)
		}
	}
	if strings.Contains(page, "teams <span") {
		t.Errorf("job page shows teams for a teamless job")
	}
	logs := h.get(&sess, "/logs").Body.String()
	if !strings.Contains(logs, `<span class="vm">kilo.alpha</span>`) {
		t.Errorf("job list lacks the domain in the teams column")
	}
}

// The image forms open prefilled from the pages' links.
func TestImageFormsPrefill(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	for path, want := range map[string]string{
		"/images/deprecate?image=debian-12-v1": `value="debian-12-v1"`,
		"/images/edit?family=debian-12":        `value="debian-12"`,
		"/environments/dns?domain=kilo.alpha":  `<input type="hidden" name="domain" value="kilo.alpha">`,
		"/images/register":                     `name="vmid"`,
	} {
		rec := h.get(&sess, path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %d, lacks %s", path, rec.Code, want)
		}
	}
}

// The Environments and Images pages and the new environment form and
// preview fit a phone without scrolling sideways.
func TestBrowserCatalogPages(t *testing.T) {
	h := browserHarness(t)
	addImages(h)
	addEnv(h, true)
	h.api.Add(proxmox.VM{VMID: 150, Name: "teak-build", Node: "n1", Pool: h.cfg.Images.Pool, Status: "stopped"},
		map[string]string{"ostype": "win11", "agent": "1", "description": "Login: admin"})
	h.api.Add(proxmox.VM{VMID: 151, Name: "oak-build", Node: "n1", Pool: h.cfg.Images.Pool, Status: "running"}, map[string]string{"ostype": "l26"})
	lead := h.login(asLead)
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			for _, page := range []struct{ path, name string }{
				{"/environments", "environments"},
				{"/images", "images"},
				{"/environments/new", "env-new"},
				{"/images/register", "image-register"},
				{"/environments/add?domain=kilo.alpha", "env-add"},
				{"/environments/dns?domain=kilo.alpha", "env-dns"},
				{"/environments/add", "env-pick"},
			} {
				b.open(page.path)
				b.noSideways(page.name)
				b.shot(page.name + "-" + size.name)
			}
			b.open("/environments/new")
			b.run("filling the form", chromedp.SendKeys("#domain", "kilo.bravo"), chromedp.SendKeys("#router", "gw=gw"),
				chromedp.SendKeys("#env-hosts", "teak=debian-12:10\noak=debian-12"))
			b.click(`form[action="/environments/new/preview"] button[type=submit]`)
			b.waitFor("the preview", `location.pathname === "/environments/new/preview" && document.readyState === "complete"`)
			b.noSideways("env preview")
			b.shot("env-preview-" + size.name)
			b.clean()
		})
	}
}

// xssNotes is notes a Proxmox user could write to break out of the page.
const xssNotes = "Login: root / pw\n<script>alert(1)</script><img src=x onerror=alert(2)>\"'></textarea></details>{{.CSRF}}"

// Notes show on the images page, the environment forms and the environments
// page, always as text: anyone with VM.Config.Options can write them.
func TestNotesShowAsText(t *testing.T) {
	h := newHarness(t)
	h.api.Add(proxmox.VM{VMID: 8000, Name: "gw-v1", Node: "n1", Template: true, Tags: "base;router"},
		map[string]string{"description": xssNotes, "net0": "virtio=BC:24:11:00:80:00,bridge=vmbr0", "net1": "virtio=BC:24:11:00:80:01,bridge=vmbr0"})
	h.api.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"description": xssNotes + "\nbattleship: base=gw-v1 dns=router"})
	h.api.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v1", Node: "n1", Template: true, Tags: "base;linux;deprecated"},
		map[string]string{"description": xssNotes, "agent": "1", "net0": "virtio=BC:24:11:00:80:02,bridge=vmbr0"})
	h.api.Add(proxmox.VM{VMID: 8002, Name: "debian-12-v2", Node: "n1", Template: true, Tags: "base;linux"},
		map[string]string{"description": xssNotes, "agent": "1", "net0": "virtio=BC:24:11:00:80:03,bridge=vmbr0"})
	sess := h.login(asLead)
	pages := map[string]string{}
	for _, path := range []string{"/images", "/environments", "/environments/new", "/environments/add?domain=kilo.alpha"} {
		pages[path] = h.get(&sess, path).Body.String()
		pages["panel "+path] = h.panelGet(&sess, path, nil).Body.String()
	}
	// A refused preview shows the form again.
	pages["refused preview"] = h.post(&sess, "/environments/new/preview", url.Values{
		"domain": {"-bad-"}, "router": {"gw=gw"}, "env_hosts": {"teak=debian-12"}}).Body.String()
	for path, body := range pages {
		if !strings.Contains(body, "Login: root / pw") {
			t.Errorf("%s doesn't show the notes", path)
		}
		for _, bad := range []string{"<script>alert", "<img src=x", "</textarea></details>{{"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s has unescaped notes %q", path, bad)
			}
		}
		if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("%s doesn't show the notes' markup as text", path)
		}
		if strings.Contains(body, "battleship: base=gw-v1") {
			t.Errorf("%s shows battleship's own notes line", path)
		}
	}
}

// An image registered from a VM offers its next version from the same VM in
// one click, and the register form lists each family once.
func TestImagesPageOffersNewVersionFromSource(t *testing.T) {
	h := newHarness(t)
	h.api.Add(proxmox.VM{VMID: 8000, Name: "gw-v1", Node: "n1", Template: true, Tags: "base;router;deprecated"},
		map[string]string{"description": "battleship: source=150", "net0": "virtio=BC:24:11:00:80:00,bridge=vmbr0"})
	h.api.Add(proxmox.VM{VMID: 8001, Name: "gw-v2", Node: "n1", Template: true, Tags: "base;router"},
		map[string]string{"description": "Login: admin\nbattleship: source=150", "net0": "virtio=BC:24:11:00:80:01,bridge=vmbr0"})
	sess := h.login(asLead)
	body := h.get(&sess, "/images").Body.String()
	if strings.Count(body, `href="/images/register?family=gw&amp;vmid=150"`) != 1 {
		t.Errorf("images page lacks one New version link to VM 150:\n%s", body)
	}
	if strings.Contains(body, "source=150") {
		t.Errorf("images page shows battleship's notes line")
	}
	form := h.get(&sess, "/images/register?family=gw&vmid=150").Body.String()
	if !strings.Contains(form, `value="150"`) || !strings.Contains(form, `value="gw" required`) {
		t.Errorf("register form isn't prefilled:\n%s", form)
	}
	if n := strings.Count(form, `<option value="gw">`); n != 1 {
		t.Errorf("family gw listed %d times, want once", n)
	}
}

// Deprecated versions are folded away under the current ones, offering only
// Delete.
func TestImagesPageFoldsDeprecated(t *testing.T) {
	h := newHarness(t)
	h.api.Add(proxmox.VM{VMID: 8000, Name: "gw-v1", Node: "n1", Template: true, Tags: "base;router;deprecated"},
		map[string]string{"description": "old login", "net0": "virtio=BC:24:11:00:80:00,bridge=vmbr0"})
	h.api.Add(proxmox.VM{VMID: 8001, Name: "gw-v2", Node: "n1", Template: true, Tags: "base;router"},
		map[string]string{"description": "new login", "net0": "virtio=BC:24:11:00:80:01,bridge=vmbr0"})
	sess := h.login(asLead)
	body := h.get(&sess, "/images").Body.String()
	table, folded, ok := strings.Cut(body, `<details class="olds">`)
	if !ok {
		t.Fatalf("no deprecated section:\n%s", body)
	}
	if strings.Contains(table, "gw-v1") || !strings.Contains(table, "gw-v2") {
		t.Errorf("the table should list gw-v2 only")
	}
	if !strings.Contains(folded, "Deprecated versions (1)") || !strings.Contains(folded, `/images/delete?image=gw-v1`) ||
		strings.Contains(folded, "old login") || strings.Contains(folded, "/images/deprecate") {
		t.Errorf("deprecated section = %s", folded)
	}
}

// A stopped VM in the images pool is offered for registering, its form
// filled in from its name and OS type; a running one shows why it can't be,
// and templates aren't listed.
func TestImagesPageOffersReadyVMs(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	pool := h.cfg.Images.Pool
	h.api.Add(proxmox.VM{VMID: 150, Name: "teak-build", Node: "n1", Pool: pool, Status: "stopped"},
		map[string]string{"ostype": "win11", "agent": "1", "description": "Login: admin"})
	h.api.Add(proxmox.VM{VMID: 151, Name: "oak-build", Node: "n1", Pool: pool, Status: "running"}, map[string]string{"ostype": "l26"})
	sess := h.login(asLead)
	body := h.get(&sess, "/images").Body.String()
	link := `href="/images/register?vmid=150&amp;family=teak&amp;image_kind=windows"`
	if !strings.Contains(body, "VMs in the images pool") || strings.Count(body, link) != 1 || !strings.Contains(body, "Login: admin") {
		t.Errorf("images page lacks teak-build ready to register:\n%s", body)
	}
	if !strings.Contains(body, "oak-build is running; shut it down first") {
		t.Errorf("images page doesn't say why oak-build can't be registered:\n%s", body)
	}
	for _, not := range []string{"vmid=151", "vmid=8000", "vmid=8001"} {
		if strings.Contains(body, not) {
			t.Errorf("images page offers %s", not)
		}
	}
	form := h.get(&sess, "/images/register?vmid=150&family=teak&image_kind=windows").Body.String()
	if !strings.Contains(form, `value="150"`) || !strings.Contains(form, `value="teak" required`) ||
		!strings.Contains(form, `value="windows" checked`) {
		t.Errorf("register form isn't filled in:\n%s", form)
	}
}

// addEnv puts kilo.alpha in dev-01: a router (if router) and a Linux host.
func addEnv(h *harness, router bool) {
	if router {
		h.api.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha;router", Status: "running"},
			map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	}
	h.api.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha;linux", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=teak"})
}

// Opened from an environment, Add hosts names it rather than asking, and
// asks for a router only when it has none.
func TestAddHostsFormKnowsItsEnvironment(t *testing.T) {
	for _, router := range []bool{true, false} {
		h := newHarness(t)
		addImages(h)
		addEnv(h, router)
		sess := h.login(asLead)
		body := h.get(&sess, "/environments/add?domain=kilo.alpha").Body.String()
		if !strings.Contains(body, `<input type="hidden" name="domain" value="kilo.alpha">`) || strings.Contains(body, `id="domain"`) {
			t.Errorf("router %v: the domain isn't fixed:\n%s", router, body)
		}
		if hasInput := strings.Contains(body, `id="router"`); hasInput == router {
			t.Errorf("router %v: router input shown = %v", router, hasInput)
		}
		if router && !strings.Contains(body, `<dt>Router</dt><dd><span class="name">gw.kilo.alpha</span></dd>`) {
			t.Errorf("the environment's router isn't shown:\n%s", body)
		}
		// A router given (to resume one a failed create left) is kept.
		body = h.get(&sess, "/environments/add?domain=kilo.alpha&router=gw%3Dgw").Body.String()
		if !strings.Contains(body, `id="router" name="router" value="gw=gw"`) {
			t.Errorf("router %v: a given router is dropped:\n%s", router, body)
		}
		// A refused preview keeps the environment fixed.
		body = h.post(&sess, "/environments/add/preview", url.Values{"domain": {"kilo.alpha"}, "env_hosts": {""}}).Body.String()
		if !strings.Contains(body, `<input type="hidden" name="domain" value="kilo.alpha">`) || !strings.Contains(body, `role="alert"`) {
			t.Errorf("router %v: a refused preview loses the environment:\n%s", router, body)
		}
	}
}

// Without an environment, Add hosts and Set DNS ask which one, from a list.
func TestEnvironmentFormsPickAnEnvironment(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	for _, path := range []string{"/environments/add", "/environments/dns", "/environments/add?domain=kilo.bravo"} {
		body := h.get(&sess, path).Body.String()
		if !strings.Contains(body, `method="get"`) || !strings.Contains(body, `<option value="kilo.alpha">kilo.alpha</option>`) ||
			strings.Contains(body, `name="env_hosts"`) {
			t.Errorf("%s doesn't ask for the environment:\n%s", path, body)
		}
		if bad := strings.Contains(path, "bravo"); bad != strings.Contains(body, "There&#39;s no environment kilo.bravo") {
			t.Errorf("%s: says the environment is missing = %v", path, !bad)
		}
	}
}

// Set DNS offers the router and the hosts that can serve DNS, with the
// current choice selected.
func TestSetDNSFormListsChoices(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	body := h.get(&sess, "/environments/dns?domain=kilo.alpha").Body.String()
	if !strings.Contains(body, `<option value="router">router</option><option value="teak" selected>teak</option></select>`) {
		t.Errorf("DNS choices wrong:\n%s", body)
	}
}

// A refused preview for an environment that's gone asks which environment,
// keeping what was typed for the form it leads to.
func TestEnvironmentPickerKeepsWhatWasTyped(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	body := h.post(&sess, "/environments/add/preview", url.Values{
		"domain": {"kilo.bravo"}, "router": {"gw=gw"}, "env_hosts": {"oak=debian-12\nelm=debian-12"}}).Body.String()
	for _, want := range []string{`method="get"`, `<input type="hidden" name="router" value="gw=gw">`,
		`<input type="hidden" name="env_hosts" value="oak=debian-12` + "\n" + `elm=debian-12">`, `<option value="kilo.alpha">`} {
		if !strings.Contains(body, want) {
			t.Errorf("picker lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `name="domain" value="kilo.bravo"`) {
		t.Errorf("picker carries the missing domain")
	}
	// Picking carries them into the form.
	form := h.get(&sess, "/environments/add?domain=kilo.alpha&router=gw%3Dgw&env_hosts=oak%3Ddebian-12%0Aelm%3Ddebian-12").Body.String()
	if !strings.Contains(form, `id="router" name="router" value="gw=gw"`) || !strings.Contains(form, "oak=debian-12\nelm=debian-12") {
		t.Errorf("the form loses what was carried:\n%s", form)
	}
}

// The Create environment form never shows an existing environment as its
// own, even when the typed domain is one.
func TestCreateFormDoesNotFixAnExistingDomain(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	sess := h.login(asLead)
	body := h.post(&sess, "/environments/new/preview", url.Values{"domain": {"kilo.alpha"}, "router": {"gw=gw"}}).Body.String()
	if !strings.Contains(body, `role="alert"`) || strings.Contains(body, `type="hidden" name="domain"`) || strings.Contains(body, "<dt>Environment</dt>") {
		t.Errorf("create form shows kilo.alpha as fixed:\n%s", body)
	}
}

// Two slots holding one domain list it once.
func TestEnvironmentPickerListsADomainOnce(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true)
	h.api.Add(proxmox.VM{VMID: 2201, Name: "oak.kilo.alpha", Node: "n1", Pool: "dev-02", Tags: "dev;kilo.alpha;linux", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.11 os=linux base=debian-12-v1"})
	sess := h.login(asLead)
	if n := strings.Count(h.get(&sess, "/environments/dns").Body.String(), `<option value="kilo.alpha">`); n != 1 {
		t.Errorf("kilo.alpha listed %d times", n)
	}
}

// A DNS choice whose host is gone is said, not silently replaced.
func TestSetDNSFormSaysAGoneChoice(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	h.api.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha;router", Status: "running"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	h.api.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha;linux", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=dc"})
	sess := h.login(asLead)
	if body := h.get(&sess, "/environments/dns?domain=kilo.alpha").Body.String(); !strings.Contains(body, "it was dc, which is no longer one") {
		t.Errorf("the gone choice isn't said:\n%s", body)
	}
}
