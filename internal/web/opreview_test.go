package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/wccomps/battleship/internal/proxmox"
)

// addDNSEnv puts kilo.alpha in dev-01: the router gw, Linux hosts teak
// (.10, running) and yew (.5, stopped), and elm, an other-kind host with
// no address. Each host's notes record dns.
func addDNSEnv(h *harness, dns string) {
	for _, vm := range []struct {
		vmid          int
		name, tags    string
		status, notes string
	}{
		{2100, "gw.kilo.alpha", "dev;kilo.alpha;router", "running", "base=gw-v1 dns=router"},
		{2101, "teak.kilo.alpha", "dev;kilo.alpha;linux", "running", "ip=192.168.100.10 os=linux base=debian-12-v1 dns=" + dns},
		{2102, "elm.kilo.alpha", "dev;kilo.alpha", "running", "base=appliance-v2 dns=" + dns},
		{2103, "yew.kilo.alpha", "dev;kilo.alpha;linux", "stopped", "ip=192.168.100.5 os=linux base=debian-12-v1 dns=" + dns},
	} {
		h.api.add(proxmox.VM{VMID: vm.vmid, Name: vm.name, Node: "n1", Pool: "dev-01", Tags: vm.tags, Status: vm.status},
			map[string]string{"description": "battleship: " + vm.notes, "net0": "virtio=BC:24:11:00:21:00,bridge=devint01"})
	}
}

// The Create review says what will happen in one sentence and lists every
// VM: its full name, exact image version, address (picked ones marked),
// and VMID and node. No step column, no counts; the button carries the
// count, Back goes to the form filled in and close to Environments.
func TestCreateReview(t *testing.T) {
	h := newHarness(t)
	addFormImages(h)
	sess := h.login(asLead)
	form := url.Values{"domain": {"kilo.bravo"}, "router": {"gw=gw"}, "env_hosts": {"dc=win2022:10\nweb=debian-12\napp=appliance"}}
	_, _, body := h.preview(&sess, "/environments/new", form)
	contains(t, "the review", body,
		`<h1 id="pv-title"><span class="eyebrow">Review ·</span> Create <span class="vm">kilo.bravo</span></h1>`,
		`<span>Clones 4 VMs into <span class="name">dev-01</span> and starts them. Then dc and web get their name, address, gateway .2 and DNS (the router) through the guest agent.</span>`,
		`<thead><tr><th scope="col">VM</th><th scope="col">Image</th><th scope="col">Address</th><th scope="col" class="r">VMID · node</th></tr></thead>`,
		`<tr><td><span class="vm">gw.kilo.bravo</span> <span class="chip">router</span></td><td class="wide"><code>gw-v1</code></td><td class="c2"><code>.2</code></td><td class="r wide meta">2100 · n1</td></tr>`,
		`<tr><td><span class="vm">dc.kilo.bravo</span></td><td class="wide"><code>win2022-v3</code></td><td class="c2"><code>.10</code></td><td class="r wide meta">2101 · n1</td></tr>`,
		`<tr><td><span class="vm">app.kilo.bravo</span></td><td class="wide"><code>appliance-v2</code></td><td class="c2">—</td><td class="r wide meta">2103 · n1</td></tr>`,
		`<a class="btn ghost sq" href="/environments" data-close aria-label="Close">`,
		`<a class="btn first" href="/environments/new?domain=kilo.bravo&amp;env_hosts=dc%3Dwin2022%3A10%0Aweb%3Ddebian-12%0Aapp%3Dappliance&amp;router=gw%3Dgw" data-panel-link>Back</a>`,
		"Create 4 VMs</button>")
	if !regexp.MustCompile(`<tr><td><span class="vm">web.kilo.bravo</span></td><td class="wide"><code>debian-12-v1</code></td><td class="c2"><code>\.\d+</code> <span class="meta">picked</span></td><td class="r wide meta">2102 · n1</td></tr>`).MatchString(body) {
		t.Errorf("web's row doesn't show its picked address:\n%s", body)
	}
	if n := strings.Count(body, "picked</span>"); n != 1 {
		t.Errorf("%d addresses marked picked, want 1", n)
	}
	lacks(t, "the review", body, "will run", ">Steps<", "configure-guest", "<dt>", `class="blk"`)
}

// The Add hosts review stripes a blocked row with its own reason, and the
// button counts only the hosts that will be added.
func TestAddHostsReview(t *testing.T) {
	h := newHarness(t)
	addImages(h)
	addEnv(h, true) // teak at .10; its DNS is teak
	sess := h.login(asLead)
	_, _, body := h.preview(&sess, "/environments/kilo.alpha/add", url.Values{"env_hosts": {"oak=debian-12:11\nelm=debian-12:10"}})
	contains(t, "the review", body,
		`<span class="eyebrow">Review ·</span> Add hosts to <span class="vm">kilo.alpha</span></h1>`,
		`<span>Clones oak into <span class="name">dev-01</span> and starts it. Then oak gets its name, address, gateway .2 and DNS (.10) through the guest agent.</span>`,
		`<tr><td><span class="vm">oak.kilo.alpha</span></td><td class="wide"><code>debian-12-v1</code></td><td class="c2"><code>.11</code></td><td class="r wide meta">2102 · n1</td></tr>`,
		`<tr class="blk"><td><span class="vm">elm.kilo.alpha</span></td><td class="wide"><code>debian-12-v1</code></td><td class="c2"><code>.10</code></td><td class="r wide"><span class="st s-cancel">`,
		"<span>address .10 is already taken in <span class=\"name\">kilo.alpha</span></span></span></td></tr>",
		`<a class="btn ghost sq" href="/environments#kilo.alpha" data-close aria-label="Close">`,
		`<a class="btn first" href="/environments/kilo.alpha/add?env_hosts=oak%3Ddebian-12%3A11%0Aelm%3Ddebian-12%3A10" data-panel-link>Back</a>`,
		"Add 1 host</button>")
	lacks(t, "the review", body, "picked</span>", "will run", "Once a domain controller")

	// Every host blocked: the reasons, and nothing to confirm.
	rec := h.post(&sess, "/environments/kilo.alpha/add/preview", url.Values{"env_hosts": {"elm=debian-12:10"}})
	contains(t, "every host blocked", rec.Body.String(), `<p class="say bad">`, "Every VM is blocked: nothing can run.", `<tr class="blk">`)
	lacks(t, "every host blocked", rec.Body.String(), "/confirm", "</button>\n</div>\n</form>")

	// A Windows host added where DNS is the router: the hint to set DNS
	// once a domain controller is up is Create's alone.
	h = newHarness(t)
	addFormImages(h)
	addDNSEnv(h, "router")
	sess = h.login(asLead)
	_, _, body = h.preview(&sess, "/environments/kilo.alpha/add", url.Values{"env_hosts": {"wks2=win2022"}})
	contains(t, "a Windows host added", body, "DNS (the router) through the guest agent.", "Add 1 host</button>")
	lacks(t, "a Windows host added", body, "Once a domain controller")
}

// Opening a base image for editing, deprecating it and deleting it each
// review the one VM it acts on, with a button naming the result. Back goes
// to the family's page, where their buttons are; close to Images.
func TestImageReviews(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	sess := h.login(asLead)
	for _, c := range []struct {
		path string
		form url.Values
		want []string
	}{
		{"/images/deprecate", url.Values{"image": {"debian-12-v2"}}, []string{
			`<span class="eyebrow">Review ·</span> Deprecate <span class="vm">debian-12-v2</span></h1>`,
			`<span><span class="name">debian-12-v2</span> will no longer be offered for new hosts. Hosts made from it keep working.</span>`,
			`<thead><tr><th scope="col">VM</th><th scope="col" class="r">VMID · node</th></tr></thead>`,
			`<tr><td><span class="vm">debian-12-v2</span></td><td class="r wide meta">8002 · n1</td></tr>`,
			`<a class="btn first" href="/images/family/debian-12" data-panel-link>Back</a>`,
			`<button type="submit" class="btn pri">`, "Deprecate debian-12-v2</button>"}},
		{"/images/delete", url.Values{"image": {"debian-12-v1"}}, []string{
			`<span class="eyebrow">Review ·</span> Delete <span class="vm">debian-12-v1</span></h1>`,
			"will be deleted. Hosts made from it keep working: they are full copies. This can&#39;t be undone.",
			`<tr><td><span class="vm">debian-12-v1</span></td><td class="r wide meta">8001 · n1</td></tr>`,
			`<a class="btn first" href="/images/family/debian-12" data-panel-link>Back</a>`,
			`<button type="submit" class="btn pri bad">`, "Delete debian-12-v1</button>"}},
		{"/images/edit", url.Values{"family": {"gw"}}, []string{
			`<span class="eyebrow">Review ·</span> Edit <span class="vm">gw</span></h1>`,
			`<span>Copies <span class="name">gw-v1</span> as <span class="name">gw-edit</span> on the edit network, for you to patch and register as the next version.</span>`,
			`<thead><tr><th scope="col">VM</th><th scope="col">Image</th><th scope="col" class="r">VMID · node</th></tr></thead>`,
			`<tr><td><span class="vm">gw-edit</span></td><td class="wide"><code>gw-v1</code></td><td class="r wide meta">`,
			`<a class="btn first" href="/images/family/gw" data-panel-link>Back</a>`,
			"Copy as gw-edit</button>"}},
	} {
		_, _, body := h.preview(&sess, c.path, c.form)
		contains(t, c.path, body, append(c.want, `<a class="btn ghost sq" href="/images" data-close aria-label="Close">`)...)
		lacks(t, c.path, body, "will run", ">Steps<", "<dt>", `class="blk"`)
	}

	// Blocked: the row striped with its reason, and nothing to confirm.
	for path, form := range map[string]url.Values{
		"/images/deprecate": {"image": {"debian-12-v1"}},
		"/images/edit":      {"family": {"debian-12"}},
	} {
		rec := h.post(&sess, path+"/preview", form)
		body := rec.Body.String()
		contains(t, path+" blocked", body, `<p class="say bad">`, "Every VM is blocked: nothing can run.", `<tr class="blk">`)
		lacks(t, path+" blocked", body, "/confirm", `class="btn pri`)
	}
}

// The image and environment reviews fit a phone and a laptop, light and
// dark. A blocked row is striped.
func TestBrowserReviews(t *testing.T) {
	h := browserHarness(t)
	addFormImages(h)
	addDNSEnv(h, "router")
	// A VM outside the slot already has elm's name: its row is blocked.
	h.api.add(proxmox.VM{VMID: 170, Name: "elm2.kilo.alpha", Node: "n1", Status: "stopped"}, nil)
	lead := h.login(asLead)
	fits := `[...document.querySelectorAll(".tscroll")].every(e => e.scrollWidth <= e.clientWidth)`
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/environments/kilo.alpha/add")
			b.click(`[data-add-row]`)
			b.waitFor("a second row", `document.querySelectorAll("[data-row]").length === 2`)
			b.run("filling the rows", chromedp.SendKeys(`input[aria-label="Host 1 name"]`, "wks2", chromedp.ByQuery),
				chromedp.SetValue(`select[aria-label="Host 1 image"]`, "win2022", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 2 name"]`, "elm2", chromedp.ByQuery),
				chromedp.SetValue(`select[aria-label="Host 2 image"]`, "debian-12", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 2 last octet"]`, "13", chromedp.ByQuery))
			b.click(`.pf button[type=submit]`)
			b.waitFor("the add review", `location.pathname === "/environments/kilo.alpha/add/preview" && document.querySelector("tr.blk") !== null`)
			b.noSideways("add review")
			b.shot("add-review-" + size.name)
			if !b.is(fits) {
				t.Errorf("the add review's table scrolls inside its box")
			}
			if cards := b.is(`getComputedStyle(document.querySelector(".rvw tr")).display === "grid"`); cards != b.phone {
				t.Errorf("phone %v: review rows are cards: %v", b.phone, cards)
			}
			if !b.is(`getComputedStyle(document.querySelector("tr.blk")).backgroundImage.includes("gradient") || getComputedStyle(document.querySelector("tr.blk td")).backgroundImage.includes("gradient")`) {
				t.Errorf("the blocked row isn't striped")
			}

			b.open("/environments/new")
			b.click(`[data-add-row]`)
			b.waitFor("a second row", `document.querySelectorAll("[data-row]").length === 2`)
			b.click(`[data-add-row]`)
			b.waitFor("a third row", `document.querySelectorAll("[data-row]").length === 3`)
			b.run("filling the create form", chromedp.SendKeys("#domain", "kilo.bravo"),
				chromedp.SendKeys(`input[aria-label="Host 1 name"]`, "dc", chromedp.ByQuery),
				chromedp.SetValue(`select[aria-label="Host 1 image"]`, "win2022", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 1 last octet"]`, "10", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 2 name"]`, "web", chromedp.ByQuery),
				chromedp.SetValue(`select[aria-label="Host 2 image"]`, "debian-12", chromedp.ByQuery),
				chromedp.SendKeys(`input[aria-label="Host 3 name"]`, "app", chromedp.ByQuery),
				chromedp.SetValue(`select[aria-label="Host 3 image"]`, "appliance", chromedp.ByQuery))
			b.click(`.pf button[type=submit]`)
			b.waitFor("the create review", `location.pathname === "/environments/new/preview" && document.querySelector("table.rvw") !== null`)
			b.noSideways("create review")
			b.shot("create-review-" + size.name)

			b.open("/images/family/debian-12")
			b.click(`form[action="/images/deprecate/preview"] button`)
			b.waitFor("the deprecate review", `location.pathname === "/images/deprecate/preview"`)
			b.noSideways("deprecate review")
			b.shot("deprecate-review-" + size.name)
			b.clean()
		})
	}
}
