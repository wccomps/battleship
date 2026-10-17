package web

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"pgregory.net/rapid"

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

var (
	choiceRE  = regexp.MustCompile(`<label class="choice"><input type="radio" name="dns" value="([^"]*)" required( checked)?><span class="nm">(.*?)</span>(<span class="chip">current</span>)?<span class="meta">(.*?)</span></label>`)
	dnsHintRE = regexp.MustCompile(`<p class="hint">(.*?)</p>`)
)

// dnsCards reads Set DNS's cards as "value name · meta", with " (current)"
// and " checked" when so.
func dnsCards(body string) []string {
	var out []string
	for _, m := range choiceRE.FindAllStringSubmatch(body, -1) {
		c := m[1] + " " + m[3] + " · " + m[5]
		if m[4] != "" {
			c += " (current)"
		}
		if m[2] != "" {
			c += " checked"
		}
		out = append(out, c)
	}
	return out
}

// Set DNS offers a card per choice the environment allows (the router,
// then each Windows or Linux host with an address), each with its address
// and image. The current choice has a chip and is chosen unless the form
// came back with another; a choice whose host is gone is said. One line
// names whose DNS changes.
func TestSetDNSForm(t *testing.T) {
	h := newHarness(t)
	addDNSEnv(h, "teak")
	sess := h.login(asLead)
	body := h.get(&sess, "/environments/kilo.alpha/dns").Body.String()
	contains(t, "the form", body, `Set DNS for <span class="vm">kilo.alpha</span></h1>`, "<legend>DNS server for every host</legend>",
		`<button type="submit" class="btn pri">Review</button>`)
	want := []string{
		"router Router · gw · 192.168.100.2 · gw-v1",
		"teak teak · 192.168.100.10 · debian-12-v1 (current) checked",
		"yew yew · 192.168.100.5 · debian-12-v1",
	}
	if got := dnsCards(body); !slices.Equal(got, want) {
		t.Errorf("cards = %q, want %q", got, want)
	}
	if m := dnsHintRE.FindStringSubmatch(body); m == nil || m[1] != "Changes teak and yew through the guest agent. The router and elm keep their own settings." {
		t.Errorf("hint = %q", m)
	}
	lacks(t, "the form", body, "which is no longer one", "<select")

	// Back from the review: the choice made stays chosen.
	body = h.get(&sess, "/environments/kilo.alpha/dns?dns=yew").Body.String()
	want[1], want[2] = strings.TrimSuffix(want[1], " checked"), want[2]+" checked"
	if got := dnsCards(body); !slices.Equal(got, want) {
		t.Errorf("back from the review, cards = %q, want %q", got, want)
	}
	// Something that isn't a choice chooses nothing, and isn't taken for
	// the environment's choice.
	body = h.get(&sess, "/environments/kilo.alpha/dns?dns=elm").Body.String()
	if got := dnsCards(body); slices.ContainsFunc(got, func(c string) bool { return strings.HasSuffix(c, " checked") }) || len(got) != 3 {
		t.Errorf("dns=elm: cards = %q, want none checked", got)
	}
	lacks(t, "dns=elm", body, "which is no longer one")
}

// A current choice whose host is gone has no card: it is said, nothing is
// chosen for the user, and no card is marked current.
func TestSetDNSFormSaysAGoneChoice(t *testing.T) {
	h := newHarness(t)
	addDNSEnv(h, "dc")
	sess := h.login(asLead)
	body := h.get(&sess, "/environments/kilo.alpha/dns").Body.String()
	contains(t, "the form", body, `<span class="hint">It was dc, which is no longer one: choose another.</span>`)
	want := []string{"router Router · gw · 192.168.100.2 · gw-v1", "teak teak · 192.168.100.10 · debian-12-v1", "yew yew · 192.168.100.5 · debian-12-v1"}
	if got := dnsCards(body); !slices.Equal(got, want) {
		t.Errorf("cards = %q, want %q", got, want)
	}
}

// Without a router VM or a host to change, the router is still a choice,
// and the hint says only the choice is recorded.
func TestSetDNSFormWithoutHostsToChange(t *testing.T) {
	h := newHarness(t)
	h.api.add(proxmox.VM{VMID: 2102, Name: "elm.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"description": "battleship: base=appliance-v2 dns=router"})
	sess := h.login(asLead)
	body := h.get(&sess, "/environments/kilo.alpha/dns").Body.String()
	if got := dnsCards(body); !slices.Equal(got, []string{"router Router · 192.168.100.2 (current) checked"}) {
		t.Errorf("cards = %q", got)
	}
	if m := dnsHintRE.FindStringSubmatch(body); m == nil || m[1] != "No host here is a Windows or Linux host with an address: the choice is only recorded." {
		t.Errorf("hint = %q", m)
	}
}

// The Set DNS review is a before-and-after table of the hosts whose DNS
// changes: a stopped one is a striped row with its reason, the others
// (the router, a host without an address) only record the choice, and the
// button counts the hosts that change.
func TestSetDNSReview(t *testing.T) {
	h := newHarness(t)
	addDNSEnv(h, "router")
	sess := h.login(asLead)
	action, fields, body := h.preview(&sess, "/environments/kilo.alpha/dns", url.Values{"dns": {"teak"}})
	contains(t, "the review", body,
		`<h1 id="pv-title"><span class="eyebrow">Review ·</span> Set DNS for <span class="vm">kilo.alpha</span></h1>`,
		"<span>Points teak at teak (.10) through the guest agent. The router keeps its own settings.</span></p>",
		`<table class="tbl dnsr">`, `<thead><tr><th scope="col">Host</th><th scope="col">DNS now</th><th scope="col">After</th></tr></thead>`,
		`<tr><td><span class="vm">teak</span></td><td class="meta"><code>.2</code></td><td><code>.10</code> <span class="meta">itself</span></td></tr>`,
		`<tr class="blk"><td><span class="vm">yew</span></td><td class="meta"><code>.2</code></td><td><span class="st s-cancel">`, `<span><span class="name">yew.kilo.alpha</span> is stopped`,
		"is stopped; start it first so its DNS can be set</span></span></td></tr>",
		`<a class="btn ghost sq" href="/environments#kilo.alpha" data-close aria-label="Close">`,
		`<a class="btn first" href="/environments/kilo.alpha/dns?dns=teak" data-panel-link>Back</a>`,
		"Set DNS on 1 host</button>")
	lacks(t, "the review", body, "will run", `<span class="vm">gw</span>`, `<span class="vm">elm</span>`, "<dt>")
	if action != "/environments/kilo.alpha/dns/confirm" || fields.Get("dns") != "teak" {
		t.Errorf("confirm = %s %v", action, fields)
	}

	// Back to the router from teak: teak's DNS now is itself.
	h = newHarness(t)
	addDNSEnv(h, "teak")
	sess = h.login(asLead)
	_, _, body = h.preview(&sess, "/environments/kilo.alpha/dns", url.Values{"dns": {"router"}})
	contains(t, "back to the router", body, "Points teak at the router (.2) through the guest agent.",
		`<tr><td><span class="vm">teak</span></td><td class="meta"><code>.10</code></td><td><code>.2</code></td></tr>`)
}

// A host whose DNS named a host that is gone shows that name as its DNS
// now. The review reads the cluster no more than the form does: what each
// host's DNS was comes with the plan.
func TestSetDNSReviewFromAGoneChoice(t *testing.T) {
	h := newHarness(t)
	addDNSEnv(h, "dc")
	sess := h.login(asLead)
	before, _, _ := h.api.readCounts()
	h.get(&sess, "/environments/kilo.alpha/dns")
	form, _, _ := h.api.readCounts()
	_, _, body := h.preview(&sess, "/environments/kilo.alpha/dns", url.Values{"dns": {"router"}})
	review, _, _ := h.api.readCounts()
	contains(t, "the review", body, `<tr><td><span class="vm">teak</span></td><td class="meta"><code>dc</code></td><td><code>.2</code></td></tr>`)
	if form-before == 0 || review-form != form-before {
		t.Errorf("the form read %d configs, the review %d; want the same", form-before, review-form)
	}
}

// When no host's DNS can change, the review says the choice is only
// recorded, and the button says so.
func TestSetDNSReviewWithEveryHostBlocked(t *testing.T) {
	h := newHarness(t)
	addDNSEnv(h, "router")
	h.api.setStatus(2101, "stopped")
	sess := h.login(asLead)
	_, _, body := h.preview(&sess, "/environments/kilo.alpha/dns", url.Values{"dns": {"router"}})
	contains(t, "the review", body, "No host&#39;s DNS changes now: each one is blocked. Confirming records the router (.2) as the choice.",
		"Record the choice</button>")
}

// Whatever DNS choice is posted, a review to confirm comes back just when
// it is one the environment offers (or blank, the router), with the choice
// trimmed in its fields; anything else is the form again, saying why.
// The form, given it, shows just that card chosen, or none.
func TestPropDNSChoice(t *testing.T) {
	h := newHarness(t)
	addDNSEnv(h, "teak")
	sess := h.login(asLead)
	choices := []string{"router", "teak", "yew"}
	rapid.Check(t, func(t *rapid.T) {
		dns := rapid.OneOf(
			rapid.SampledFrom(append(choices, "elm", "gw", "dc", "Teak", "teak.kilo.alpha", "192.168.100.10", "")),
			rapid.Map(rapid.SampledFrom(choices), func(c string) string { return " " + c + "\t" }),
			rapid.SampledFrom(hostileStrings), rapid.String(),
		).Draw(t, "dns")
		trimmed := strings.TrimSpace(dns)
		valid := trimmed == "" || slices.Contains(choices, trimmed)
		rec := h.post(&sess, "/environments/kilo.alpha/dns/preview", url.Values{"dns": {dns}})
		body := rec.Body.String()
		confirms := strings.Contains(body, `action="/environments/kilo.alpha/dns/confirm"`)
		if valid != confirms {
			t.Fatalf("dns %q: confirm offered = %v (status %d)", dns, confirms, rec.Code)
		}
		if valid {
			if _, fields := confirmOf(h.t, body); fields.Get("dns") != trimmed {
				t.Fatalf("dns %q: confirms %q", dns, fields.Get("dns"))
			}
		} else if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, `role="alert"`) {
			t.Fatalf("dns %q: %d without a reason", dns, rec.Code)
		}
		if _, err := checkMarkup(body); err != nil {
			t.Fatalf("dns %q: %v", dns, err)
		}

		form := h.get(&sess, "/environments/kilo.alpha/dns?"+url.Values{"dns": {dns}}.Encode()).Body.String()
		var checked []string
		for _, c := range dnsCards(form) {
			if v, ok := strings.CutSuffix(c, " checked"); ok {
				checked = append(checked, strings.Fields(v)[0])
			}
		}
		want := []string{trimmed}
		switch {
		case trimmed == "":
			want = []string{"teak"} // the current choice
		case !valid:
			want = nil
		}
		if !slices.Equal(checked, want) {
			t.Fatalf("dns %q: the form checks %q, want %q", dns, checked, want)
		}
		if _, err := checkMarkup(form); err != nil {
			t.Fatalf("dns %q form: %v", dns, err)
		}
	})
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
		`<p class="meta">Once a domain controller is up, use Set DNS on <span class="name">kilo.bravo</span> to point the hosts at it.</p>`,
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

// Set DNS's cards and the image and environment reviews fit a phone and a
// laptop, light and dark. The chosen card is outlined, without a script:
// choosing another moves the outline. A blocked row is striped.
func TestBrowserReviews(t *testing.T) {
	h := browserHarness(t)
	addFormImages(h)
	addDNSEnv(h, "router")
	// A VM outside the slot already has elm's name: its row is blocked.
	h.api.add(proxmox.VM{VMID: 170, Name: "elm2.kilo.alpha", Node: "n1", Status: "stopped"}, nil)
	lead := h.login(asLead)
	fits := `[...document.querySelectorAll(".tscroll")].every(e => e.scrollWidth <= e.clientWidth)`
	outlined := `[...document.querySelectorAll(".choice")].filter(c => getComputedStyle(c).boxShadow !== "none").map(c => c.querySelector("input").value).join()`
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/environments/kilo.alpha/dns")
			b.noSideways("set DNS")
			b.shot("dns-" + size.name)
			if !b.is(outlined + ` === "router"`) {
				t.Errorf("outlined cards: %s", b.where())
			}
			b.click(`.choice input[value=teak]`)
			if !b.is(outlined + ` === "teak"`) {
				t.Errorf("after choosing teak, outlined cards aren't just teak")
			}
			b.click(`.pf button[type=submit]`)
			b.waitFor("the DNS review", `location.pathname === "/environments/kilo.alpha/dns/preview" && document.querySelector("tr.blk") !== null`)
			b.noSideways("DNS review")
			b.shot("dns-review-" + size.name)
			if !b.is(fits) {
				t.Errorf("the DNS review's table scrolls inside its box")
			}

			b.open("/environments/kilo.alpha/add")
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
