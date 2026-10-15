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
	sess := h.login(asLead)
	for path, want := range map[string]string{
		"/images/deprecate?image=debian-12-v1": `value="debian-12-v1"`,
		"/images/edit?family=debian-12":        `value="debian-12"`,
		"/environments/dns?domain=kilo.alpha":  `value="kilo.alpha"`,
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
	h.api.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha;router", Status: "running"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	h.api.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "n2", Pool: "dev-01", Tags: "dev;kilo.alpha;linux", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=router"})
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
