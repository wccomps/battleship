package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// reviewRow is the markup of the review's row for VM name.
func reviewRowOf(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`<tr[^>]*><td><span class="vm">` + regexp.QuoteMeta(name) + `</span>.*?</tr>`).FindString(body)
	if m == "" {
		t.Fatalf("no review row for %s in:\n%s", name, body)
	}
	return m
}

// Tear down opens on its review: one sentence, a row per VM with its
// image version, address and VMID · node, the domain to type, and a red
// button naming it. Close and Back go to its block on Environments.
func TestEnvTeardownReview(t *testing.T) {
	h := newHarness(t)
	addEnvs(h)
	sess := h.login(asLead)
	rec := h.get(&sess, "/environments/kilo.alpha/teardown")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	contains(t, "the review", body,
		`<span class="eyebrow">Review ·</span> Tear down <span class="vm">kilo.alpha</span></h1>`,
		`<p class="say bad"><svg class="i" viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16`,
		`<span>Deletes 4 VMs in <span class="name">dev-01</span>. The slot is free again afterwards.</span>`,
		`<label for="typed">Type <code>kilo.alpha</code> to confirm</label>`,
		`pattern="\s*kilo\.alpha\s*"`, `data-typed="kilo.alpha"`,
		`<button type="submit" class="btn pri bad">`, `Tear down kilo.alpha</button>`,
		`<a class="btn ghost sq" href="/environments#kilo.alpha" data-close aria-label="Close">`,
		`<a class="btn first" href="/environments#kilo.alpha" data-close>Back</a>`)
	lacks(t, "the review", body, "Not touched", `class="blk"`)
	contains(t, "gw's row", reviewRowOf(t, body, "gw.kilo.alpha"), `<span class="chip">router</span>`, "<code>gw-v1</code>", "<code>.2</code>", "2100 · n1")
	contains(t, "teak's row", reviewRowOf(t, body, "teak.kilo.alpha"), "<code>debian-12-v2</code>", "<code>.10</code>", "2101 · n1")
	contains(t, "elm's row", reviewRowOf(t, body, "elm.kilo.alpha"), "<code>win10-v1</code>", "2102 · n1")
	lacks(t, "the review", body, "ad.oscar.lima")
	action, _ := confirmOf(t, body)
	if action != "/environments/kilo.alpha/teardown/confirm" {
		t.Errorf("confirm posts to %s", action)
	}
}

// VMs outside the pool on its networks are listed as not touched, and the
// sentence says the slot stays in use for them.
func TestEnvTeardownReviewListsVMsItLeaves(t *testing.T) {
	h := newHarness(t)
	addEnvs(h)
	h.api.add(proxmox.VM{VMID: 151, Name: "stray-two", Node: "n1", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:22:00,bridge=devint01"})
	sess := h.login(asLead)
	body := h.get(&sess, "/environments/kilo.alpha/teardown").Body.String()
	contains(t, "the review", body,
		`<span>Deletes 4 VMs in <span class="name">dev-01</span>. VMs outside <span class="name">dev-01</span> stay on its networks, so the slot isn&#39;t free afterwards.</span>`,
		`<h2 class="sub">Not touched</h2>`, `<span><span class="name">stray-two</span> is outside <span class="name">dev-01</span> but has a NIC on its networks.</span>`)
	lacks(t, "the review", body, `<span class="vm">stray-two</span></td>`, `<div class="say caution">`)
}

// VMs the user lacks a privilege on are blocked rows saying which; the
// rest can still be torn down.
func TestEnvTeardownReviewBlocksWhatTheUserMayNotDelete(t *testing.T) {
	h := newHarness(t)
	addEnvs(h)
	sess := h.loginWith("test-partial", []int{2100, 2101, 2103}, "VM.Allocate", "VM.PowerMgmt")
	body := h.get(&sess, "/environments/kilo.alpha/teardown").Body.String()
	contains(t, "elm's row", reviewRowOf(t, body, "elm.kilo.alpha"), `class="blk"`,
		`you don&#39;t have <span class="name">VM.PowerMgmt</span> on /vms/2102; you don&#39;t have <span class="name">VM.Allocate</span> on /vms/2102`)
	contains(t, "the review", body, `<span>Deletes 3 VMs in <span class="name">dev-01</span>. The blocked VM stays, so the slot isn&#39;t free afterwards.</span>`,
		"Tear down kilo.alpha</button>")
	lacks(t, "teak's row", reviewRowOf(t, body, "teak.kilo.alpha"), `class="blk"`)
}

// A slot of mixed domains, a domain no slot holds, and a path that names
// no domain can't be torn down; nothing is offered to confirm.
func TestEnvTeardownRefusals(t *testing.T) {
	h := newHarness(t)
	addEnvs(h)
	h.api.add(proxmox.VM{VMID: 2200, Name: "a.kilo.bravo", Node: "n1", Pool: "dev-02"}, nil)
	h.api.add(proxmox.VM{VMID: 2201, Name: "b.other.zone", Node: "n1", Pool: "dev-02"}, nil)
	sess := h.login(asLead)
	for path, want := range map[string]string{
		"/environments/kilo.bravo/teardown":   "Dev slot 02 holds VMs of several domains",
		"/environments/nowhere.zone/teardown": `No dev slot holds <span class="name">nowhere.zone</span>`,
	} {
		rec := h.get(&sess, path)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("GET %s = %d, want 422", path, rec.Code)
		}
		contains(t, path, rec.Body.String(), want, `href="/environments#`)
		lacks(t, path, rec.Body.String(), "/confirm")
	}
	if rec := h.get(&sess, "/environments/Not%20A%20Domain/teardown"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/environments" ||
		flashOf(rec) != "Add hosts and Tear down start from an environment: pick one below." {
		t.Errorf("a path naming no domain = %d %s %q", rec.Code, rec.Header().Get("Location"), flashOf(rec))
	}
	// Add hosts once named the domain in a field; a teardown
	// never did, so no such address leads to one.
	if rec := h.get(&sess, "/environments/teardown?domain=kilo.alpha"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /environments/teardown?domain= = %d, want 404", rec.Code)
	}
	h.noJobs("refusals")
}

// The domain must be typed to confirm: anything else shows the review
// again, unsubmitted; the domain, with stray spaces, submits the job.
func TestEnvTeardownTypedConfirm(t *testing.T) {
	h := newHarness(t)
	addEnvs(h)
	sess := h.login(asLead)
	action, form := confirmOf(t, h.get(&sess, "/environments/kilo.alpha/teardown").Body.String())
	for _, wrong := range []string{"", "kilo", "KILO.ALPHA", "kiloxalpha", "oscar.lima", "kilo.alpha.", "1-2"} {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		if wrong != "" {
			f.Set(fieldTyped, wrong)
		}
		rec := h.post(&sess, action, f)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("typed %q: confirm = %d, want 422", wrong, rec.Code)
		}
		contains(t, "typed "+wrong, rec.Body.String(), "Type the domain to confirm", `exactly as shown: <span class="name">kilo.alpha</span>. Nothing was submitted.`)
		if _, again := confirmOf(t, rec.Body.String()); again.Get(fieldNonce) != form.Get(fieldNonce) {
			t.Errorf("typed %q: the page has another preview", wrong)
		}
	}
	h.noJobs("wrongly typed")
	form.Set(fieldTyped, " kilo.alpha ")
	rec := h.post(&sess, action, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("typed right: confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 || js[0].Kind != string(pods.KindEnvTeardown) {
		t.Fatalf("jobs = %+v", js)
	}
	var in jobs.Inputs
	if err := json.Unmarshal(js[0].Inputs, &in); err != nil || in.Domain != "kilo.alpha" {
		t.Errorf("inputs = %+v, %v", in, err)
	}
}

// Once the job has run, the environment is gone from Environments and its
// slot is free; the job page goes back to Environments.
func TestEnvTeardownFreesTheSlot(t *testing.T) {
	h := newHarness(t)
	addEnvs(h)
	sess := h.login(asLead)
	action, form := confirmOf(t, h.get(&sess, "/environments/kilo.alpha/teardown").Body.String())
	form.Set(fieldTyped, "kilo.alpha")
	if rec := h.post(&sess, action, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	j := h.jobsInStore()[0]
	var plan pods.Plan
	if err := json.Unmarshal(j.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	h.start(j.ID)
	exec := &apply.Executor{API: h.api, Cfg: h.cfg, Sleep: func(context.Context, time.Duration) error { return nil }}
	if res := exec.Run(context.Background(), &plan); len(res.Succeeded) != 4 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	h.finish(j.ID, store.Outcome{Status: store.StatusSucceeded})
	body := h.get(&sess, "/environments").Body.String()
	lacks(t, "Environments", body, `id="kilo.alpha"`, "dev-01</span><span>router")
	contains(t, "Environments", body, `<span class="vm">dev-01 – dev-03, dev-05 – dev-10</span><span class="meta">· Create uses dev-01</span></p>`)
	contains(t, "the job page", h.get(&sess, "/logs/"+itoa(j.ID)).Body.String(), `<a class="btn ghost back" href="/environments#kilo.alpha">`, `</svg>Environments</a>`,
		`<path d="M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3"/></svg>Tear down <span class="vm">kilo.alpha</span>`)
}

// A teardown that can't be planned has no form to show again: its page
// says why, and leads back to Environments.
func TestEnvTeardownRefused(t *testing.T) {
	h := newHarness(t)
	addEnvs(h)
	sess := h.login(asLead)
	rec := h.get(&sess, "/environments/kilo.bravo/teardown")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("teardown of an environment that isn't there = %d\n%s", rec.Code, rec.Body)
	}
	contains(t, "the page", rec.Body.String(), "Can&#39;t tear down kilo.bravo", `href="/environments#kilo.bravo"`, "Back to Environments")
}

// A teardown that failed partway is retried in its slot: the review plans
// what is left there, the disks a delete left included, and says it
// retries the job.
func TestEnvTeardownRetry(t *testing.T) {
	h := newHarness(t)
	addEnvs(h)
	sess := h.login(asLead)
	id := h.submitJob(jobs.Inputs{Kind: pods.KindEnvTeardown, Domain: "kilo.alpha"}, byLead)
	h.start(id)
	h.api.remove("gw.kilo.alpha", "yew.kilo.alpha")
	h.api.Mu.Lock()
	h.api.Vols["competitions:vm-2100-disk-0"] = 2100 // gw's delete left a disk
	h.api.Mu.Unlock()
	h.finish(id, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"gw.kilo.alpha": {Status: store.ItemDone}, "yew.kilo.alpha": {Status: store.ItemDone},
		"teak.kilo.alpha": {Status: store.ItemFailed, Error: "delete: refused"},
	}})
	// Its page offers the teardown again, not its failed VMs: a retry
	// plans what is left whole.
	page := h.get(&sess, "/logs/"+itoa(id)).Body.String()
	contains(t, "the job page", page, `title="Battleship already tried its failed VMs again by itself (1 automatic round; none for a missing privilege), so fix the cause first. Plans the teardown again, of what is left, after a preview.">`,
		"Retry the teardown</button>")
	lacks(t, "the job page", page, "Retry 0", "for 0 VMs")
	rec := h.post(&sess, "/logs/"+itoa(id)+"/retry", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	contains(t, "the retry", body, `<span>Deletes 2 VMs in <span class="name">dev-01</span>. Frees the disks deleted VMs left at VMID 2100. The slot is free again afterwards.</span>`,
		`<span class="vm">disks-2100</span>`,
		`<p class="meta">Retry of #`+itoa(id)+": the teardown of what is left.</p>", `<input type="hidden" name="slot" value="01">`)
	lacks(t, "the retry", body, "didn&#39;t finish")
	action, _ := confirmOf(t, body)
	if action != "/environments/kilo.alpha/teardown/confirm" {
		t.Errorf("retry confirms at %s", action)
	}
	for _, gone := range []string{"gw.kilo.alpha", "yew.kilo.alpha"} {
		if strings.Contains(body, `<span class="vm">`+gone+`</span>`) {
			t.Errorf("the retry lists %s, which is gone", gone)
		}
	}
}

// Tear down is on the block, red; its review fits a phone and a laptop,
// light and dark, with its blocked row striped and the VMs it leaves
// listed. The button wakes once the domain is typed, and confirming
// opens the job.
func TestBrowserEnvTeardown(t *testing.T) {
	h := browserHarness(t)
	addEnvs(h)
	h.api.add(proxmox.VM{VMID: 151, Name: "stray-two", Node: "n1", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:22:00,bridge=devint01"})
	h.api.add(proxmox.VM{VMID: 2150, Name: "base.kilo.alpha", Node: "n1", Pool: "dev-01", Template: true}, nil)
	h.api.add(proxmox.VM{VMID: 2500, Name: "gw.workstations.blue-team.lab", Node: "n1", Pool: "dev-05", Tags: "dev;workstations.blue-team.lab;router"}, nil)
	lead := h.login(asLead)
	fits := `[...document.querySelectorAll(".tscroll")].every(e => e.scrollWidth <= e.clientWidth)`
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/environments")
			b.noSideways("environments")
			b.fullShot("envs-" + size.name)
			if !b.is(`[...document.querySelectorAll(".envh .acts .btn")].every(e => e.scrollWidth <= e.clientWidth && e.getBoundingClientRect().right <= e.closest(".env").getBoundingClientRect().right)`) {
				t.Errorf("a block's actions are cut off")
			}
			if !b.is(`getComputedStyle(document.querySelector('#kilo\\.alpha .acts a[href$="/teardown"]')).color !== getComputedStyle(document.querySelector('#kilo\\.alpha .acts a[href$="/add"]')).color`) {
				t.Errorf("Tear down isn't in the danger colour")
			}
			b.click(`#kilo\.alpha .acts a[href$="/teardown"]`)
			b.waitFor("the teardown review", `location.pathname === "/environments/kilo.alpha/teardown" && document.querySelector("table.rvw") !== null`)
			b.noSideways("teardown review")
			b.shot("teardown-review-" + size.name)
			b.fullShot("teardown-review-full-" + size.name)
			if !b.is(fits) {
				t.Errorf("the teardown review's table scrolls inside its box")
			}
			if !b.is(`getComputedStyle(document.querySelector("tr.blk")).backgroundImage.includes("gradient") || getComputedStyle(document.querySelector("tr.blk td")).backgroundImage.includes("gradient")`) {
				t.Errorf("the blocked row isn't striped")
			}
			if !b.is(`document.querySelector(".pf button[type=submit]").disabled`) {
				t.Errorf("the button is awake before the domain is typed")
			}
			b.run("typing the domain", chromedp.SendKeys("#typed", "kilo.alpha", chromedp.ByID))
			if b.is(`document.querySelector(".pf button[type=submit]").disabled`) {
				t.Errorf("the button sleeps with the domain typed")
			}
			b.click(`.pf button[type=submit]`)
			b.waitFor("the job", `location.pathname.startsWith("/logs/")`)
			b.noSideways("job page")
			b.clean()
			for _, j := range h.jobsInStore() {
				if err := h.st.RequestCancel(context.Background(), j.ID, "test"); err != nil && !errors.Is(err, store.ErrNotActive) {
					t.Fatal(err)
				}
			}
		})
	}
}
