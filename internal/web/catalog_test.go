package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
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

func TestNewEnvironmentPreviewShowsSlotAndAddresses(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	sess := h.login(asLead)
	body := h.post(&sess, "/environments/new/preview", url.Values{
		"domain": {"kilo.alpha"}, "router": {"gw=gw"}, "env_hosts": {"teak=debian-12:10"}}).Body.String()
	for _, want := range []string{"gw.kilo.alpha", "teak.kilo.alpha", "into <span class=\"name\">dev-01</span>", "<code>.10</code>", "Create 2 VMs</button>"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	if strings.Contains(body, "you don't have") {
		t.Errorf("a lead is blocked:\n%s", body)
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
		"/environments/kilo.alpha/dns":         `action="/environments/kilo.alpha/dns/preview"`,
		"/images/register":                     `name="vmid"`,
	} {
		rec := h.get(&sess, path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %d, lacks %s", path, rec.Code, want)
		}
	}
}

// The Environments and Images pages, a family's page and sheet, the
// register review and the environment forms fit a phone without scrolling
// sideways; leaving an environment's form lands on its block.
func TestBrowserCatalogPages(t *testing.T) {
	h := browserHarness(t)
	addCatalog(h)
	addEnvs(h)
	// A slot of two domains, and one used from outside.
	h.api.Add(proxmox.VM{VMID: 2200, Name: "a.kilo.bravo", Node: "n1", Pool: "dev-02", Status: "running"},
		map[string]string{"description": "a long login line that wraps: Administrator / Changeme1! then more words to fill it\nbattleship: ip=192.168.100.20"})
	h.api.Add(proxmox.VM{VMID: 2201, Name: "b.other.zone", Node: "n1", Pool: "dev-02", Status: "stopped"}, nil)
	h.api.Add(proxmox.VM{VMID: 160, Name: "stray-vm", Node: "n1", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:16:00,bridge=devint03"})
	// Enough below oscar.lima that its block can scroll to the top.
	for i, d := range []string{"lima.papa", "mike.november"} {
		for j, host := range []string{"dc", "web", "app", "db", "wks1", "wks2"} {
			h.api.Add(proxmox.VM{VMID: 2600 + 100*i + j, Name: host + "." + d, Node: "n1", Pool: "dev-0" + strconv.Itoa(6+i), Status: "running"},
				map[string]string{"description": "user / Changeme1!\nbattleship: ip=192.168.100." + strconv.Itoa(10+j)})
		}
	}
	lead := h.login(asLead)
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			for _, page := range []struct{ path, name string }{
				{"/environments", "environments"},
				{"/images", "images"},
				{"/images/family/debian-12", "family"},
				{"/images/family/gw", "family-source"},
				{"/environments/new", "env-new"},
				{"/images/register", "image-register"},
				{"/environments/kilo.alpha/add", "env-add"},
				{"/environments/kilo.alpha/dns", "env-dns"},
			} {
				b.open(page.path)
				b.noSideways(page.name)
				b.shot(page.name + "-" + size.name)
			}
			b.open("/environments")
			b.fullShot("environments-full-" + size.name)

			// Cancel on Add hosts lands on its environment's block.
			b.open("/environments/oscar.lima/add")
			b.click(`.pf a[href="/environments#oscar.lima"]`)
			b.waitFor("the Environments page", `location.pathname === "/environments" && location.hash === "#oscar.lima" && document.readyState === "complete"`)
			// Its top shows just below the sticky header.
			var at []float64
			b.eval(`[document.getElementById("oscar.lima").getBoundingClientRect().top, document.querySelector(".top").getBoundingClientRect().bottom]`, &at)
			if len(at) != 2 || at[0] < at[1]-1 || at[0] > at[1]+24 {
				t.Errorf("after Cancel, oscar.lima's block is at %vpx, the header ends at %vpx", at[0], at[1])
			}
			b.shot("environments-anchor-" + size.name)

			// A kind to pick dims Register until one is picked.
			b.open("/images")
			b.run("scrolling to the catalogue", chromedp.ScrollIntoView("#cat", chromedp.ByQuery))
			b.shot("images-catalogue-" + size.name)
			elm := `form.rrow:has(input[name=vmid][value="152"])`
			if !b.is(`getComputedStyle(document.querySelector('` + elm + ` button')).opacity < 0.5`) {
				t.Errorf("elm-build's Register isn't dimmed before a kind is picked")
			}
			b.click(elm + ` input[value=router]`)
			if !b.is(`getComputedStyle(document.querySelector('` + elm + ` button')).opacity == 1`) {
				t.Errorf("elm-build's Register stays dimmed after picking a kind")
			}
			// A family opens in the panel over Images.
			b.click(`a.fam[href="/images/family/debian-12"]`)
			b.waitFor("the family sheet", `document.querySelector("#panel[open] .sheet.family") !== null`)
			b.noSideways("family sheet")
			b.shot("family-panel-" + size.name)
			b.click(`#panel [data-close]`)
			b.waitFor("the panel to close", `!document.querySelector("#panel[open]")`)
			// A row's Register opens its review in the panel.
			b.click(elm + ` button`)
			b.waitFor("the register review", `(document.querySelector("#panel[open] #pv-title")||{}).textContent === "Review · Register elm-build"`)
			b.noSideways("register review")
			b.shot("register-review-" + size.name)
			// Without the panel, it is a page.
			b.run("posting the review", chromedp.Evaluate(`document.querySelector('#panel').close();
				document.querySelector('`+elm+`').removeAttribute("data-panel");
				document.querySelector('.rlist').removeAttribute("data-panel");
				HTMLFormElement.prototype.submit.call(document.querySelector('`+elm+`'))`, nil))
			b.waitFor("the review page", `location.pathname === "/images/register/preview" && document.readyState === "complete"`)
			b.noSideways("register review page")
			b.shot("register-page-" + size.name)

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
	for _, path := range []string{"/images", "/images/family/debian-12", "/environments", "/environments/new", "/environments/kilo.alpha/add"} {
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
		// The Images page shows only each family's first line, the login.
		if !strings.HasSuffix(path, "/images") && !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("%s doesn't show the notes' markup as text", path)
		}
		if strings.Contains(body, "battleship: base=gw-v1") {
			t.Errorf("%s shows battleship's own notes line", path)
		}
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

// Two registers previewed while the first still waits in the queue get
// different VMIDs: the first's isn't made yet, but it's taken.
func TestQueuedRegisterReservesItsVMID(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	pool := h.cfg.Images.Pool
	for _, vm := range []proxmox.VM{
		{VMID: 150, Name: "teak-build", Node: "n1", Pool: pool, Status: "stopped"},
		{VMID: 151, Name: "oak-build", Node: "n1", Pool: pool, Status: "stopped"},
	} {
		h.api.Add(vm, map[string]string{"agent": "1", "ostype": "l26"})
	}
	sess := h.login(asLead)
	var vmids []int
	for _, vm := range []string{"150", "151"} {
		family := map[string]string{"150": "teak", "151": "oak"}[vm]
		action, fields, _ := h.preview(&sess, "/images/register", url.Values{"vmid": {vm}, "family": {family}, "image_kind": {"linux"}})
		if rec := h.post(&sess, action, fields); rec.Code != http.StatusSeeOther {
			t.Fatalf("confirm %s = %d\n%s", vm, rec.Code, rec.Body)
		}
		js := h.jobsInStore()
		items, err := h.st.Items(context.Background(), js[0].ID)
		if err != nil || len(items) != 1 {
			t.Fatalf("items = %v, %v", items, err)
		}
		vmids = append(vmids, items[0].VMID)
	}
	if vmids[0] == vmids[1] {
		t.Errorf("both registers planned VMID %d", vmids[0])
	}
}

// An image or environment operation's form, review and job page mark its
// section current in the nav; a team operation's form marks its Lead
// button, and its job page Logs.
func TestNavMarksTheSection(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	addEnvs(h)
	h.poll()
	sess := h.login(asLead)
	current := regexp.MustCompile(`<a (?:class="btn[^"]*" )?href="([^"]*)" aria-current="page">`)
	marked := func(body string) []string {
		var out []string
		for _, m := range current.FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		return out
	}
	review := func(path string, form url.Values) string {
		_, _, body := h.preview(&sess, path, form)
		return body
	}
	envJob := h.submitJob(jobs.Inputs{Kind: pods.KindEnvDNS, Domain: "kilo.alpha", DNS: "router"}, byLead)
	imageJob := h.submitJob(jobs.Inputs{Kind: pods.KindImageEdit, Family: "gw"}, byLead)
	teamJob := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byLead)
	for _, c := range []struct {
		what, body string
		want       []string
	}{
		{"Create", h.get(&sess, "/environments/new").Body.String(), []string{envsPath}},
		{"Add hosts", h.get(&sess, "/environments/kilo.alpha/add").Body.String(), []string{envsPath}},
		{"Set DNS", h.get(&sess, "/environments/kilo.alpha/dns").Body.String(), []string{envsPath}},
		{"Set DNS's review", review("/environments/kilo.alpha/dns", url.Values{"dns": {"router"}}), []string{envsPath}},
		{"Tear down's review", h.get(&sess, "/environments/kilo.alpha/teardown").Body.String(), []string{envsPath}},
		{"an environment job", h.get(&sess, "/logs/"+itoa(envJob)).Body.String(), []string{envsPath}},
		{"Register", h.get(&sess, "/images/register?vmid=152&family=elm").Body.String(), []string{"/images"}},
		{"Edit's review", review("/images/edit", url.Values{"family": {"gw"}}), []string{"/images"}},
		{"an image job", h.get(&sess, "/logs/"+itoa(imageJob)).Body.String(), []string{"/images"}},
		{"Deploy", h.get(&sess, "/deploy").Body.String(), []string{"/deploy"}},
		{"Power", h.get(&sess, "/power").Body.String(), nil},
		{"a team job", h.get(&sess, "/logs/"+itoa(teamJob)).Body.String(), []string{logsPath}},
	} {
		if got := marked(c.body); !slices.Equal(got, c.want) {
			t.Errorf("%s marks %q current, want %q", c.what, got, c.want)
		}
	}
}
