package web

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

// The browser tests drive the real pages, script and stylesheet in
// headless Chrome. They need a browser's DevTools endpoint, e.g.
//
//	docker run -d --rm --network host chromedp/headless-shell
//	BATTLESHIP_BROWSER=http://127.0.0.1:9222 go test ./internal/web -run Browser
//
// and are skipped without it. BATTLESHIP_SHOTS, if set, is a directory for
// screenshots of each state.

// browser is a tab on the app under test.
type browser struct {
	t    *testing.T
	h    *harness
	srv  *httptest.Server
	ctx  context.Context
	mu   sync.Mutex
	errs []string // script errors and CSP refusals
	// allow403: the page under test is a 403 page, which Chrome logs.
	allow403 bool
	// phone: click elements directly, since the sticky team column may
	// cover a cell scrolled under it.
	phone bool
}

// needBrowser skips the test without a browser to drive.
func needBrowser(t *testing.T) {
	t.Helper()
	if os.Getenv("BATTLESHIP_BROWSER") == "" {
		t.Skip("set BATTLESHIP_BROWSER to a Chrome DevTools endpoint to run the browser tests")
	}
}

// browserServers are the browser harnesses' HTTP servers.
var browserServers sync.Map // *harness → *httptest.Server

// browserHarness is newHarness served over HTTP at its web.base_url, which
// the browser's posts must come from (auth checks their Origin).
func browserHarness(t *testing.T, mut ...func(*config.Config)) *harness {
	t.Helper()
	needBrowser(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	h := newHarness(t, append(mut, func(c *config.Config) { c.Web.BaseURL = base; longSessions(c) })...)
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h.h}}
	srv.Start()
	t.Cleanup(func() {
		srv.CloseClientConnections() // the event streams
		srv.Close()
	})
	browserServers.Store(h, srv)
	// Run the fake clock, which paces the event streams, at twice real time.
	stop := make(chan struct{})
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				h.clock.Advance(100 * time.Millisecond)
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-ticked })
	return h
}

// newBrowser opens a tab on h's app, logged in as sess (nil: nobody), at
// width × height, dark or not.
func newBrowser(t *testing.T, h *harness, sess *authtest.Session, width, height int64, dark bool) *browser {
	t.Helper()
	endpoint := os.Getenv("BATTLESHIP_BROWSER")
	v, ok := browserServers.Load(h)
	if !ok {
		t.Fatal("newBrowser needs a browserHarness")
	}
	srv := v.(*httptest.Server)
	alloc, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), endpoint)
	t.Cleanup(cancelAlloc)
	// Each browser gets its own browser context: cookies are per host, not
	// per port, so tabs sharing one would trade sessions between harnesses
	// (other tests, or another test run using the same Chrome).
	conn, cancelConn := chromedp.NewContext(alloc)
	t.Cleanup(cancelConn)
	if err := chromedp.Run(conn); err != nil {
		t.Fatalf("connecting to the browser: %v", err)
	}
	ctx, cancel := chromedp.NewContext(conn, chromedp.WithNewBrowserContext())
	t.Cleanup(cancel)
	ctx, cancelTimeout := context.WithTimeout(ctx, 90*time.Second)
	t.Cleanup(cancelTimeout)
	b := &browser{t: t, h: h, srv: srv, ctx: ctx, phone: width <= 640}
	chromedp.ListenTarget(ctx, func(ev any) {
		switch ev := ev.(type) {
		case *runtime.EventExceptionThrown:
			b.fail("script error: " + ev.ExceptionDetails.Error())
		case *log.EventEntryAdded:
			b.mu.Lock()
			allow := b.allow403
			b.mu.Unlock()
			if ev.Entry.Level == log.LevelError && !strings.Contains(ev.Entry.Text, "favicon") && !(allow && strings.Contains(ev.Entry.Text, "status of 403")) {
				b.fail("console: " + ev.Entry.Text)
			}
		case *runtime.EventConsoleAPICalled:
			if ev.Type == runtime.APITypeError {
				b.fail("console.error")
			}
		}
	})
	scheme := "light"
	if dark {
		scheme = "dark"
	}
	tasks := chromedp.Tasks{
		log.Enable(),
		runtime.Enable(),
		chromedp.EmulateViewport(width, height),
		emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}}),
	}
	if sess != nil {
		tasks = append(tasks, network.SetCookie(sess.Cookie.Name, sess.Cookie.Value).WithURL(srv.URL).WithHTTPOnly(true))
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		t.Fatalf("starting the browser: %v", err)
	}
	return b
}

func (b *browser) fail(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.errs = append(b.errs, msg)
}

// expect drops the errors logged so far that contain s, such as a status
// a step answers with on purpose.
func (b *browser) expect(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.errs = slices.DeleteFunc(b.errs, func(e string) bool { return strings.Contains(e, s) })
}

// clean fails the test if the page logged script errors or CSP refusals.
func (b *browser) clean() {
	b.t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.errs {
		b.t.Errorf("browser: %s", e)
	}
	b.errs = nil
}

func (b *browser) run(what string, actions ...chromedp.Action) {
	b.t.Helper()
	if err := chromedp.Run(b.ctx, actions...); err != nil {
		b.t.Fatalf("%s: %v", what, err)
	}
}

// open loads path and waits for the page.
func (b *browser) open(path string) {
	b.t.Helper()
	b.run("opening "+path, chromedp.Navigate(b.srv.URL+path), chromedp.WaitReady("body"))
	b.settle()
}

// settle waits for fonts and a frame.
func (b *browser) settle() {
	b.t.Helper()
	var ok bool
	b.run("settling", chromedp.Evaluate(`document.fonts.ready.then(() => new Promise(r => requestAnimationFrame(() => r(true))))`, &ok,
		func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
}

// eval runs js and stores its result in out.
func (b *browser) eval(js string, out any) {
	b.t.Helper()
	b.run("evaluating "+js, chromedp.Evaluate(js, out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
}

// is evaluates a boolean expression.
func (b *browser) is(js string) bool {
	b.t.Helper()
	var v bool
	b.eval(js, &v)
	return v
}

// text is an element's text, trimmed.
func (b *browser) text(sel string) string {
	b.t.Helper()
	var s string
	b.eval(fmt.Sprintf(`(document.querySelector(%q)||{}).textContent||""`, sel), &s)
	return strings.Join(strings.Fields(s), " ")
}

// click clicks the first element matching sel.
func (b *browser) click(sel string) {
	b.t.Helper()
	if b.phone {
		b.run("waiting for "+sel, chromedp.WaitVisible(sel, chromedp.ByQuery))
		b.eval(fmt.Sprintf(`document.querySelector(%q).click()`, sel), nil)
		return
	}
	b.run("clicking "+sel, chromedp.Click(sel, chromedp.ByQuery, chromedp.NodeVisible))
}

// where says where the tab is and what it shows, for failures.
func (b *browser) where() string {
	var s string
	b.eval(`location.pathname + " " + document.title + " live=" + document.getElementById("app").dataset.live + " stop-disabled=" + (document.querySelector('#actionbar button[value=stop]')||{}).disabled + " panel=" + (document.getElementById("panel")||{}).outerHTML`, &s)
	b.mu.Lock()
	defer b.mu.Unlock()
	return s + " errors: " + strings.Join(b.errs, "; ")
}

// waitFor waits until js is true. A check a navigation cuts off (such as a
// form submit's just clicked) counts as false.
func (b *browser) waitFor(what, js string) {
	b.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var ok bool
		err := chromedp.Run(b.ctx, chromedp.Evaluate(js, &ok, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
		switch {
		case err == nil && ok:
			b.settle()
			return
		case err != nil && !navigatedAway(err):
			b.t.Fatalf("evaluating %s: %v", js, err)
		case time.Now().After(deadline):
			b.t.Fatalf("waiting for %s (%s); the tab shows %s", what, js, b.where())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// navigatedAway is whether err is an evaluation the page left before it
// answered.
func navigatedAway(err error) bool {
	return strings.Contains(err.Error(), "Inspected target navigated or closed") || strings.Contains(err.Error(), "Execution context was destroyed")
}

// noSideways fails if the page scrolls sideways.
func (b *browser) noSideways(what string) {
	b.t.Helper()
	var w []int
	b.eval(`[document.documentElement.scrollWidth, window.innerWidth]`, &w)
	if len(w) == 2 && w[0] > w[1] {
		b.t.Errorf("%s: the page is %dpx wide in a %dpx window: it scrolls sideways", what, w[0], w[1])
	}
}

// shot saves a screenshot named name, if BATTLESHIP_SHOTS is set.
func (b *browser) shot(name string) {
	b.t.Helper()
	dir := os.Getenv("BATTLESHIP_SHOTS")
	if dir == "" {
		return
	}
	b.settle()
	var png []byte
	b.run("screenshot", chromedp.CaptureScreenshot(&png))
	if err := os.WriteFile(filepath.Join(dir, name+".png"), png, 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// fullShot saves a screenshot of the whole page named name, if
// BATTLESHIP_SHOTS is set.
func (b *browser) fullShot(name string) {
	b.t.Helper()
	dir := os.Getenv("BATTLESHIP_SHOTS")
	if dir == "" {
		return
	}
	b.settle()
	var png []byte
	b.run("screenshot", chromedp.FullScreenshot(&png, 100))
	if err := os.WriteFile(filepath.Join(dir, name+".png"), png, 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// sizes are the laptop and phone sizes of the designs, light and dark.
var sizes = []struct {
	name          string
	width, height int64
	dark          bool
}{
	{"laptop", 1366, 768, false},
	{"laptop-dark", 1366, 768, true},
	{"phone", 390, 844, false},
	{"phone-dark", 390, 844, true},
}

// competitionHosts are a typical pod's hosts.
var competitionHosts = []string{"dc", "web", "ftp", "mail", "dns", "db", "fs", "wks1", "wks2", "wks3", "siem", "fw"}

// bigHarness is a polled and scanned competition of 32 teams × 12 hosts,
// with VMs in every state.
func bigHarness(t *testing.T, mut ...func(*config.Config)) (*harness, int64) {
	t.Helper()
	h := browserHarness(t, mut...)
	h.api.Mu.Lock()
	clear(h.api.VMs)
	h.api.Mu.Unlock()
	missing := map[string]bool{"14-dns": true, "30-siem": true, "31-siem": true, "32-siem": true}
	stopped := map[string]bool{"02-wks3": true, "05-wks1": true, "05-wks2": true, "17-fs": true, "23-mail": true}
	for i := 1; i <= 32; i++ {
		team := fmt.Sprintf("%02d", i)
		for j, host := range competitionHosts {
			if missing[team+"-"+host] {
				continue
			}
			vm := teamVM(team, host, 10000+i*100+j+1)
			vm.Node = []string{"n1", "n2"}[j%2]
			if stopped[team+"-"+host] || team == "12" {
				vm.Status = "stopped"
			}
			if team+"-"+host == "09-web" {
				vm.Pool = "wrong-pool"
			}
			h.api.add(vm, cleanConfig(team), "initial", "before-scoring")
		}
	}
	h.hist.set("team27-ftp", store.ItemResult{JobID: 1, JobKind: "deploy", Status: store.ItemFailed, Step: "start", Error: "bridge vmbr27 not found",
		FinishedAt: h.clock.Now()})
	busy := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "19", Hosts: []string{"web", "db"}, Action: "reboot"}, byOperator)
	h.poll()
	if err := h.poller.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h, busy
}

func TestBrowserGridSelection(t *testing.T) {
	h, busy := bigHarness(t)
	lead := h.login(asLead)
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/")
			if got := b.text("#grid-counts"); got != "359running17stopped4missing2drifted2busy" {
				t.Errorf("legend = %q", got)
			}
			if !b.is(`document.getElementById("app").dataset.live === "live"`) {
				t.Error("the live dot isn't live")
			}
			if b.is(`getComputedStyle(document.getElementById("actionbar")).display !== "none"`) {
				t.Error("the action bar shows with nothing selected")
			}
			if size.dark != b.is(`getComputedStyle(document.body).backgroundColor === "rgb(13, 15, 18)"`) {
				t.Errorf("dark = %v, but the page background doesn't match", size.dark)
			}
			b.noSideways("grid")
			b.shot("grid-" + size.name)

			// A busy cell is a link to its job, which opens in the panel.
			if !b.is(`!!document.querySelector('#cell-19-web a.cell.is-busy[href="/logs/` + itoa(busy) + `"]')`) {
				t.Error("team19-web isn't a busy link to its job")
			}
			// A column header selects the column; again unselects it.
			b.click(`#grid-table a.hb[data-select="host:dc"]`)
			if n := b.text("#sel-count"); n != "32" {
				t.Errorf("after selecting the dc column, count = %q", n)
			}
			if !b.is(`document.querySelector('a.hb[data-select="host:dc"]').getAttribute("aria-pressed") === "true"`) {
				t.Error("the dc header isn't pressed")
			}
			b.click(`#grid-table a.hb[data-select="host:dc"]`)
			if n := b.text("#sel-count"); n != "0" {
				t.Errorf("after unselecting the dc column, count = %q", n)
			}
			// Shift-click 07 db→dns (db, dc, dns), then 08's three: 6.
			b.click(`#cell-07-db label`)
			b.eval(`(function(){var e=new MouseEvent("click",{bubbles:true,shiftKey:true});document.querySelector('#cell-07-dns input').dispatchEvent(e);})()`, nil)
			b.click(`#cell-08-db label`)
			b.click(`#cell-08-dc label`)
			b.click(`#cell-08-dns label`)
			if n := b.text("#sel-count"); n != "6" {
				var on []string
				b.eval(`[...document.querySelectorAll("input[name=vms]:checked")].map(b => b.value)`, &on)
				t.Errorf("after selecting 07-08 dc-ftp, count = %q: %v", n, on)
			}
			if !b.is(`getComputedStyle(document.getElementById("actionbar")).display !== "none"`) {
				t.Error("the action bar is hidden with VMs selected")
			}
			if !b.is(`!document.getElementById("ab-details").hidden && document.getElementById("ab-details").getAttribute("href") === "/vm/08/dns" && document.getElementById("ab-details").title === "team08-dns"`) {
				t.Error("Details isn't linked to the last ticked VM on multi-select")
			}
			b.noSideways("grid with the action bar")
			b.shot("grid-selection-" + size.name)

			// Select all, and none.
			b.click(`.all input`)
			if n := b.text("#sel-count"); n != "378" {
				t.Errorf("select all: count %q, want every VM that isn't missing or busy", n)
			}
			b.click(`.all input`)
			if n := b.text("#sel-count"); n != "0" {
				t.Errorf("after unselecting all, count = %q", n)
			}

			// One VM: Details opens its panel.
			b.click(`#cell-09-web label`)
			b.click(`#ab-details`)
			b.waitFor("the VM panel", `!!document.querySelector("#panel[open] #vm-title")`)
			if got := b.text("#panel .reasons"); !strings.Contains(got, "wrong-pool") {
				t.Errorf("drift reasons = %q", got)
			}
			b.noSideways("VM panel")
			b.shot("cell-detail-" + size.name)
			b.click(`#panel [data-close]`)
			b.waitFor("the panel to close", `!document.querySelector("#panel[open]")`)

			// Escape clears the selection.
			b.run("escape", chromedp.KeyEvent("\x1b"))
			if n := b.text("#sel-count"); n != "0" {
				t.Errorf("after Escape, count = %q", n)
			}

			// Arrow keys move across the grid, Enter jumps to the action bar,
			// and double-clicking a cell opens its VM panel directly.
			b.eval(`(function(){var el=document.querySelector('#cell-01-dc input');el.focus();el.dispatchEvent(new KeyboardEvent("keydown",{key:"ArrowDown",bubbles:true}));})()`, nil)
			if !b.is(`document.activeElement === document.querySelector('#cell-02-dc input')`) {
				t.Error("ArrowDown from 01-dc didn't focus 02-dc")
			}
			b.click(`#cell-02-dc label`)
			b.eval(`(function(){var el=document.querySelector('#cell-02-dc input');el.focus();el.dispatchEvent(new KeyboardEvent("keydown",{key:"Enter",bubbles:true}));})()`, nil)
			if !b.is(`document.activeElement === document.querySelector('#actionbar .ab-ops button:not(:disabled)')`) {
				t.Error("Enter on a checked cell didn't focus the action bar")
			}
			b.run("escape", chromedp.KeyEvent("\x1b"))
			b.eval(`document.querySelector('#cell-09-web label').dispatchEvent(new MouseEvent("dblclick",{bubbles:true}))`, nil)
			b.waitFor("the VM panel from dblclick", `!!document.querySelector("#panel[open] #vm-title")`)
			b.click(`#panel [data-close]`)
			b.waitFor("the panel to close", `!document.querySelector("#panel[open]")`)
			b.clean()
		})
	}
}

func TestBrowserPreviewAndJob(t *testing.T) {
	h, _ := bigHarness(t)
	lead := h.login(asLead)
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/")
			b.click(`#cell-07-web label`)
			b.click(`#cell-07-ftp label`)
			b.click(`#cell-05-wks1 label`)
			b.click(`#actionbar button[value="stop"]`)
			b.waitFor("the force stop preview", `!!document.querySelector("#panel[open] #pv-title")`)
			if got := b.text("#pv-title"); got != "Force stop" {
				t.Errorf("preview title = %q", got)
			}
			if got := b.text("#panel .chips"); got != "1 already stopped" {
				t.Errorf("chips = %q", got)
			}
			if got := b.text("#panel .say.bad"); got != "3 VMs lose power at once. Unsaved work is lost." {
				t.Errorf("consequence = %q", got)
			}
			if got := b.text(`#panel button.pri.bad`); got != "Force stop 3 VMs" {
				t.Errorf("confirm button = %q", got)
			}
			if b.is(`getComputedStyle(document.getElementById("actionbar")).display !== "none"`) {
				t.Error("the action bar shows over the open panel")
			}
			b.noSideways("preview")
			b.shot("preview-power-" + size.name)

			// Confirm: the job opens in the panel and follows the job.
			b.click(`#panel button.pri.bad`)
			b.waitFor("the job panel", `!!document.querySelector("#panel[open] #job-title")`)
			id := strings.TrimPrefix(b.text("#panel .ph .vm"), "#")
			if b.text("#job-head .st") != "queued" {
				t.Errorf("new job status = %q", b.text("#job-head .st"))
			}
			n, _ := parseID(id)
			h.start(n)
			h.event(n, "team07-web", string(pods.StepPower), string(apply.EventDone), "")
			publishJob(h, n)
			b.waitFor("the job to run", `document.querySelector("#job-head .st").textContent.trim().startsWith("running · ")`)
			b.waitFor("the log line", `document.querySelectorAll("#job-log li").length > 0`)
			b.shot("job-running-" + size.name)
			h.finish(n, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
				"team07-web":  {Status: store.ItemDone},
				"team07-ftp":  {Status: store.ItemFailed, Error: "stop: VM is locked (backup)"},
				"team05-wks1": {Status: store.ItemDone},
			}})
			publishJob(h, n)
			b.waitFor("the job to finish", `document.querySelector("#job-head .st").textContent.trim().startsWith("completed with failures · ")`)
			if got := b.text("#job-actions"); got != "Retry 1 VM" {
				t.Errorf("job actions = %q", got)
			}
			if b.is(`!!document.querySelector("#job-items .tally")`) {
				t.Errorf("a 3-VM job has a tally")
			}
			b.noSideways("job panel")
			b.shot("job-failures-" + size.name)
			if got := b.text("#panel .pf a.first"); got != "Log" {
				t.Errorf("the panel's link to the job's page = %q, want Log", got)
			}
			b.click(`#panel [data-close]`)
			b.waitFor("the panel to close", `!document.querySelector("#panel[open]")`)

			// The job page and the job list.
			b.open("/logs/" + id)
			b.noSideways("job page")
			b.shot("job-page-" + size.name)
			b.open("/logs")
			if !b.is(`document.querySelector(".job .j-st").textContent.includes("completed with failures")`) {
				t.Errorf("jobs list first row = %q", b.text(".job"))
			}
			if got := b.text(".nav a[aria-current=page]") + " / " + b.text("h1.ptitle"); got != "Logs / Logs" {
				t.Errorf("nav / heading = %q", got)
			}
			b.noSideways("jobs list")
			b.shot("jobs-list-" + size.name)
			b.clean()
		})
	}
}

// The action bar offers an action only if a ticked VM allows it, and says
// when one isn't.
func TestBrowserActionsFollowPrivileges(t *testing.T) {
	h, _ := bigHarness(t)
	op := h.login(asOperator)
	b := newBrowser(t, h, &op, 1366, 768, false)
	b.open("/")
	b.click(`#grid-table a.hb[data-select="host:dc"]`)
	if got := b.text("#ab-why"); got != "" {
		t.Errorf("operator's all-teams selection: why = %q", got)
	}
	if b.is(`[...document.querySelectorAll('#actionbar .ab-ops button')].some(b => b.disabled)`) {
		t.Error("an operator's all-teams selection has a locked button")
	}
	if b.is(`!!document.querySelector(".lead-ops")`) {
		t.Error("an operator has the Deploy and Teardown buttons")
	}
	b.clean()

	cred := h.ticketWith("test-powerer", []string{"VM.Audit"})
	for _, vmid := range []int{10101, 10102} {
		h.pve.Grant(cred.User, "/vms/"+itoa(int64(vmid)), "VM.Audit", "VM.PowerMgmt")
	}
	pw := authtest.Login(t, h.st, h.cfg, authtest.User{Subject: "test-powerer", At: h.clock.Now(), Proxmox: cred})
	h.openView(cred)
	b = newBrowser(t, h, &pw, 1366, 768, false)
	b.open("/")
	b.click(`#cell-01-dc label`)
	if !b.is(`[...document.querySelectorAll('#actionbar button[name=action]')].every(b => !b.disabled)`) {
		t.Error("power is locked on a VM the user may power")
	}
	if !b.is(`document.querySelector('#actionbar button[formaction="/snapshot"]').disabled && document.querySelector('#actionbar button[formaction="/reset"]').disabled`) {
		t.Error("snapshot or reset is offered on a VM the user may only power")
	}
	if got := b.text("#ab-why"); got != "Some actions aren't permitted on these VMs" {
		t.Errorf("why = %q", got)
	}
	b.shot("grid-actions-by-privilege")
	b.clean()
}

// Snapshot from the grid: the panel form checks the name as Proxmox does,
// the preview blocks a VM that already has it, and confirming opens the job.
func TestBrowserSnapshotFlow(t *testing.T) {
	h, _ := bigHarness(t)
	op := h.login(asOperator)
	for i, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			team := []string{"07", "08", "10", "11"}[i]
			h.api.setSnapshots(10000+atoi(team)*100+3, "initial", "before-scoring", "round2") // its ftp has the name
			b := newBrowser(t, h, &op, size.width, size.height, size.dark)
			b.open("/")
			b.click(`#cell-` + team + `-web label`)
			b.click(`#cell-` + team + `-ftp label`)
			b.click(`#actionbar button[formaction="/snapshot"]`)
			b.waitFor("the snapshot form", `!!document.querySelector("#panel[open] #form-title")`)
			if got := b.text("#form-title"); got != "Take snapshot" {
				t.Errorf("form title = %q", got)
			}
			if got := b.text(`#panel ul.hosts`); got != "team"+team+"-ftp team"+team+"-web" && got != "team"+team+"-ftpteam"+team+"-web" {
				t.Errorf("VMs = %q", got)
			}
			if b.is(`document.querySelector('#panel input[name=vmstate]').checked`) {
				t.Error("Include RAM is on by default")
			}
			// A name Proxmox would refuse doesn't leave the form.
			b.run("typing a bad name", chromedp.SendKeys(`#snap-name`, "1bad", chromedp.ByQuery))
			b.click(`#panel button.pri`)
			if !b.is(`document.getElementById("snap-name").validity.patternMismatch`) || b.is(`!!document.querySelector("#panel #pv-title")`) {
				t.Error("a name starting with a digit went to the preview")
			}
			b.eval(`document.getElementById("snap-name").value = ""`, nil)
			b.run("typing the name", chromedp.SendKeys(`#snap-name`, "round2", chromedp.ByQuery))
			b.run("typing the description", chromedp.SendKeys(`#snap-desc`, "after lunch", chromedp.ByQuery))
			b.noSideways("snapshot form")
			b.shot("snapshot-form-" + size.name)

			b.click(`#panel button.pri`)
			b.waitFor("the snapshot preview", `!!document.querySelector("#panel[open] #pv-title")`)
			if got := b.text("#pv-title"); got != "Take snapshot round2" {
				t.Errorf("preview title = %q", got)
			}
			if got := b.text("#panel .chips"); got != "1 blocked" {
				t.Errorf("chips = %q", got)
			}
			if got := b.text("#panel tr.blk"); !strings.Contains(got, "team"+team+"-ftp") || !strings.Contains(got, `already has a snapshot named "round2"`) {
				t.Errorf("blocked row = %q", got)
			}
			if got := b.text(`#panel button.pri`); got != "Snapshot 1 VM" || b.is(`document.querySelector("#panel button.pri").classList.contains("bad")`) {
				t.Errorf("confirm button = %q, or marked dangerous", got)
			}
			b.noSideways("snapshot preview")
			b.shot("preview-snapshot-" + size.name)

			b.click(`#panel button.pri`)
			b.waitFor("the job panel", `!!document.querySelector("#panel[open] #job-title")`)
			if got := b.text("#job-title"); !strings.Contains(got, "Take snapshot") || !strings.Contains(got, "round2") {
				t.Errorf("job title = %q", got)
			}
			b.shot("job-snapshot-" + size.name)
			b.clean()
		})
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func TestBrowserStaleAndDisconnected(t *testing.T) {
	h, _ := bigHarness(t)
	lead := h.login(asLead)
	b := newBrowser(t, h, &lead, 1366, 768, false)
	b.open("/")
	b.click(`#cell-07-dc label`)
	h.clock.Advance(41 * time.Second)
	h.api.setListErr(&proxmox.APIError{Status: 503, Message: "proxy timeout"})
	_ = h.poller.Poll(context.Background())
	b.waitFor("the stale grid", `document.getElementById("app").dataset.live === "stale"`)
	if !b.is(`document.querySelector("details.live > summary").title.includes("ago")`) {
		t.Error("stale dot title doesn't include the staleness age")
	}
	if got := b.text("#ab-why"); got != "Waiting for live data" {
		t.Errorf("lock reason = %q", got)
	}
	if !b.is(`[...document.querySelectorAll('#actionbar .ab-ops button')].every(b => b.disabled)`) {
		t.Error("actions aren't locked on a stale grid")
	}
	b.waitFor("the stale grid to fade", `getComputedStyle(document.querySelector(".gscroll")).opacity === "0.5"`)
	b.click(`details.live > summary`)
	if got := b.text("#grid-status .on b"); got != "No answer from Proxmox" {
		t.Errorf("pop-over = %q", got)
	}
	b.shot("grid-stale")
	for _, sz := range []struct {
		name string
		w, h int64
	}{{"grid-stale-phone", 390, 844}} {
		p := newBrowser(t, h, &lead, sz.w, sz.h, false)
		p.open("/")
		p.noSideways("stale grid")
		p.shot(sz.name)
		p.clean()
	}

	// The server goes away: the dot turns hollow.
	h.srv.CloseStreams()
	b.waitFor("disconnected", `document.getElementById("app").dataset.live === "off"`)
	if !b.is(`getComputedStyle(document.querySelector("#grid-status .off")).display !== "none"`) {
		t.Error("the pop-over doesn't say disconnected")
	}
	b.shot("grid-disconnected")
	b.mu.Lock()
	b.errs = nil // the refused stream logs errors, as it should
	b.mu.Unlock()
}

func TestBrowserEmptyGridAndDeploy(t *testing.T) {
	h := browserHarness(t, func(c *config.Config) {
		c.Web.Templates = "*.kilo.alpha"
	})
	h.api.Mu.Lock()
	clear(h.api.VMs)
	h.api.Mu.Unlock()
	for i, host := range competitionHosts {
		h.api.add(proxmox.VM{VMID: 5001 + i, Name: host + ".kilo.alpha", Node: []string{"n1", "n2"}[i%2], Status: "running", Tags: "dev"},
			map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0"})
	}
	for i, host := range []string{"dc", "web", "wks1", "wks2", "siem", "vault"} {
		status := "running"
		if host == "vault" {
			status = "stopped"
		}
		h.api.add(proxmox.VM{VMID: 5101 + i, Name: host + ".kilo.bravo", Node: "n1", Status: status, Tags: "dev"},
			map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0"})
	}
	h.poll()
	lead := h.login(asLead)
	op := h.login(asOperator)
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			o := newBrowser(t, h, &op, size.width, size.height, size.dark)
			o.open("/")
			if o.is(`!!document.querySelector("#grid-start .set-deploy")`) {
				t.Error("an operator's template set cards have Deploy buttons")
			}
			if o.is(`!!document.querySelector("#grid-live #grid-status")`) {
				t.Error("grid-status leaked into #grid-live")
			}
			o.noSideways("operator's empty grid")
			o.shot("empty-operator-" + size.name)
			o.clean()

			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/")
			if b.is(`document.getElementById("grid-start").hidden`) {
				t.Fatal("the template sets are hidden while nothing is deployed")
			}
			if got := b.text("#grid-start .card .masters"); got != "12/12masters" {
				t.Errorf("masters = %q", got)
			}
			if b.is(`!!document.querySelector("#grid-live #grid-status")`) {
				t.Error("grid-status leaked into #grid-live")
			}
			b.noSideways("lead's empty grid")
			b.shot("empty-lead-" + size.name)
			b.click(`#grid-start .set-deploy`)
			b.waitFor("the deploy page", `location.pathname === "/deploy" && document.readyState === "complete"`)
			if !b.is(`document.querySelector('main input[name=pattern][value="*.kilo.alpha"]').checked`) {
				t.Error("the card's set isn't chosen")
			}
			if got := b.is(`[...document.querySelectorAll('main input[name=hosts]')].every(b => b.checked) && document.querySelectorAll('main input[name=hosts]').length === 12`); !got {
				t.Error("the set's 12 hosts aren't all ticked")
			}
			if got := b.text(`main [data-hosts-toggle]`); got != "Deselect all" {
				t.Errorf("hosts toggle = %q, want Deselect all", got)
			}
			b.click(`main [data-hosts-toggle]`)
			if !b.is(`[...document.querySelectorAll('main input[name=hosts]')].every(b => !b.checked) && document.querySelector('main input[name=hosts]').required && document.querySelector('main input[name=hosts]').validationMessage === "Tick at least one host."`) || b.text(`main [data-hosts-toggle]`) != "Select all" {
				t.Error("Deselect all didn't untick all hosts, set validation message, and switch to Select all")
			}
			b.click(`main [data-hosts-toggle]`)
			if !b.is(`[...document.querySelectorAll('main input[name=hosts]')].every(b => b.checked) && !document.querySelector('main input[name=hosts]').required && document.querySelector('main input[name=hosts]').validationMessage === ""`) || b.text(`main [data-hosts-toggle]`) != "Deselect all" {
				t.Error("Select all didn't tick all hosts, clear validation message, and switch to Deselect all")
			}
			b.shot("deploy-form-" + size.name)
			// Another set shows its own hosts.
			b.click(`main label.opt input[value="*.kilo.bravo"] + span`)
			if !b.is(`document.querySelectorAll('main input[name=hosts]').length === 6 && [...document.querySelectorAll('main input[name=hosts]')].every(b => b.checked)`) {
				t.Error("choosing kilo.bravo doesn't show its 6 hosts ticked")
			}
			b.click(`main label.opt input[value="*.kilo.alpha"] + span`)
			// No team has VMs yet: the teams are typed.
			if !b.is(`(function(){var t=document.querySelector('main #teams');return t.value===""&&t.required&&!t.checkValidity()})()`) {
				t.Error("the teams field isn't empty and required")
			}
			b.run("typing the teams", chromedp.SendKeys(`main #teams`, "1-32", chromedp.ByQuery))
			b.click(`main button.pri`)
			b.waitFor("the deploy preview page", `location.pathname === "/deploy/preview" && document.readyState === "complete"`)
			if got := b.text("#pv-title"); got != "Deploy kilo.alpha" {
				t.Errorf("preview title = %q", got)
			}
			if got := b.text("main .kv"); got != "Pods01-32" {
				t.Errorf("recap = %q (want all hosts: none sent)", got)
			}
			// Only a teardown is typed (pods.Kind.TypedConfirm).
			if b.is(`!!document.querySelector('#typed')`) || b.is(`document.querySelector('main button.pri').disabled`) {
				t.Error("the deploy asks for the range typed, or its button is off")
			}
			if got := b.text(`main button.pri`); got != "Deploy 384 VMs" {
				t.Errorf("deploy button = %q", got)
			}
			if !b.is(`document.querySelectorAll("main details.logbox").length === 2 && [...document.querySelectorAll("main details.logbox")].every(d => !d.open) && !document.querySelector("main .say.bad")`) {
				t.Error("healthy deploy preview didn't fold its Templates and VMs tables or showed a danger callout")
			}
			b.noSideways("deploy preview")
			b.shot("preview-deploy-" + size.name)
			b.clean()
		})
	}
}

// Template sets come and go live: replaced by the grid when a team VM
// appears, back when the last goes, without a reload.
func TestBrowserTemplateSetsFollowTheGrid(t *testing.T) {
	h := browserHarness(t)
	h.noTeamVMs()
	h.api.add(proxmox.VM{VMID: 5001, Name: "dc.kilo.alpha", Node: "n1", Status: "stopped", Tags: "dev"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0"})
	h.poll()
	lead := h.login(asLead)
	b := newBrowser(t, h, &lead, 1366, 768, false)
	b.open("/")
	b.run("marking the page", chromedp.Evaluate(`window.notReloaded = true`, nil))
	if got := b.text("#grid-start .card .masters"); got != "0/1masters" {
		t.Errorf("masters = %q", got)
	}
	h.api.setStatus(5001, "running")
	h.poll()
	b.waitFor("the master to show running", `(document.querySelector("#grid-start .card .masters b")||{}).textContent === "1/1"`)

	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	h.poll()
	b.waitFor("the grid to replace the sets", `!!document.getElementById("grid-table") && !document.getElementById("grid-start")`)
	h.api.remove("team01-dc")
	h.poll()
	b.waitFor("the sets to come back", `!!document.querySelector("#grid-start .set-deploy") && !document.getElementById("grid-table")`)
	if !b.is(`window.notReloaded === true`) {
		t.Error("the page reloaded")
	}
	b.clean()
}

// setsHarness is bigHarness with sets: every pod's wks hosts are
// kilo.alpha, pods 01-16's other hosts tango.delta, and pods 17-32's other
// hosts deployed before sets were tagged.
func setsHarness(t *testing.T) *harness {
	t.Helper()
	h, _ := bigHarness(t)
	h.api.Mu.Lock()
	for _, vm := range h.api.VMs {
		team, host, ok := h.srv.naming.ParseVMName(vm.Name)
		switch {
		case !ok:
		case strings.HasPrefix(host, "wks"):
			vm.Config["tags"] = "set-kilo.alpha"
		case team <= "16":
			vm.Config["tags"] = "set-tango.delta"
		}
	}
	h.api.Mu.Unlock()
	h.poll()
	return h
}

func TestBrowserTeardownForm(t *testing.T) {
	h := setsHarness(t)
	lead := h.login(asLead)
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/")
			b.click(`.lead-ops a[href="/teardown"]`)
			b.waitFor("the teardown page", `location.pathname === "/teardown" && document.readyState === "complete"`)
			if got := b.text("#host-field legend"); got != "Hosts" {
				t.Errorf("hosts legend = %q", got)
			}
			if got := b.text(`label[for="teams"]`); got != "Pods" {
				t.Errorf("pods label = %q", got)
			}
			if b.is(`!!document.getElementById("teardown-pick")`) {
				t.Error("step 2 shows before the pods are chosen")
			}
			b.noSideways("teardown step 1")
			b.shot("teardown-step1-" + size.name)

			b.run("typing the pods", chromedp.SendKeys(`#teams`, "1-32", chromedp.ByQuery))
			b.click(`#teardown-which button[type=submit]`)
			b.waitFor("step 2", `location.search === "?teams=1-32" && !!document.getElementById("teardown-pick")`)
			if got := b.text(`#held h2`); got != "In pods 01-32" {
				t.Errorf("step 2 heading = %q", got)
			}
			deployed := b.text(`#held dl`)
			for _, want := range []string{"kilo.alpha3 hosts in each of pods 01-32", "tango.delta", "unknown set"} {
				if !strings.Contains(deployed, want) {
					t.Errorf("deployed = %q, want %q", deployed, want)
				}
			}
			if !b.is(`document.querySelectorAll('#teardown-pick input[name=set]').length === 3 && !document.querySelector('#teardown-pick input[name=set]:checked')`) {
				t.Error("want three chips (two sets, everything), none chosen")
			}
			if got := b.text(`#teardown-pick .hint`); !strings.Contains(got, "can't tell the set of") {
				t.Errorf("hint = %q", got)
			}
			b.noSideways("teardown step 2")
			b.shot("teardown-step2-" + size.name)

			b.click(`#teardown-pick input[value="kilo.alpha"] + span`)
			b.click(`button[form="teardown-pick"]`)
			b.waitFor("the review", `location.pathname === "/teardown/preview" && document.readyState === "complete"`)
			if got := b.text("#pv-title"); got != "Review · Tear down kilo.alpha" {
				t.Errorf("review title = %q", got)
			}
			if got := b.text("main .say.bad"); !strings.HasPrefix(got, "Tears down kilo.alpha in pods 01-32: 96 VMs.") {
				t.Errorf("review says %q", got)
			}
			if !b.is(`document.querySelector('main button.pri').disabled`) {
				t.Error("the confirm is on before the range is typed")
			}
			b.run("typing the range", chromedp.SendKeys(`#typed`, "1-32", chromedp.ByQuery))
			if b.is(`document.querySelector('main button.pri').disabled`) {
				t.Error("the confirm is off once the range is typed")
			}
			if got := b.text(`main button.pri`); got != "Delete 96 VMs" {
				t.Errorf("confirm = %q", got)
			}
			b.noSideways("teardown review")
			if size.width > 640 {
				b.fullShot("teardown-review-" + size.name)
			} else {
				b.shot("teardown-review-" + size.name) // a whole page of 96 rows is too tall to read
			}
			b.clean()
		})
	}
}

// waitFor outlasts a check the page navigates away from: here the first
// check itself leaves for /teardown.
func TestBrowserWaitForOutlivesANavigation(t *testing.T) {
	h, _ := bigHarness(t)
	lead := h.login(asLead)
	b := newBrowser(t, h, &lead, 1280, 800, false)
	b.open("/")
	b.waitFor("the teardown page", `location.pathname === "/teardown" || new Promise(() => { location.href = "/teardown" })`)
	b.clean()
}

func TestBrowserGridSizes(t *testing.T) {
	for _, c := range []struct {
		name   string
		teams  int
		hosts  int
		height int64
	}{{"grid-20x8", 8, 20, 768}, {"grid-12x50", 50, 12, 768}} {
		t.Run(c.name, func(t *testing.T) {
			h := browserHarness(t)
			h.api.Mu.Lock()
			clear(h.api.VMs)
			h.api.Mu.Unlock()
			for i := 1; i <= c.teams; i++ {
				team := fmt.Sprintf("%02d", i)
				for j := 0; j < c.hosts; j++ {
					h.api.add(teamVM(team, fmt.Sprintf("h%02d", j+1), 10000+i*100+j+1), cleanConfig(team), "initial")
				}
			}
			h.poll()
			lead := h.login(asLead)
			b := newBrowser(t, h, &lead, 1366, c.height, false)
			b.open("/")
			b.noSideways(c.name)
			if c.teams > 36 && !b.is(`document.querySelector(".gscroll").classList.contains("tall")`) {
				t.Error("a tall grid doesn't scroll by itself")
			}
			b.shot(c.name)
			b.clean()
		})
	}
}

// publishJob notifies the streams that job id changed, as the store does.
func publishJob(h *harness, id int64) {
	h.hub.Publish(status.Msg{Topic: status.TopicJobs})
	h.hub.Publish(status.Msg{Topic: status.JobTopic(id)})
}

// parseID reads a job number.
func parseID(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscan(s, &n)
	return n, err
}

// A partial team 00 alongside teams 1-32 behaves like any other row and
// is dropped live once torn down.
func TestBrowserTeam00Row(t *testing.T) {
	h, _ := bigHarness(t)
	for j, host := range competitionHosts[:9] {
		vm := teamVM("00", host, 10001+j)
		vm.Node = []string{"n1", "n2"}[j%2]
		h.api.add(vm, cleanConfig("00"), "initial", "before-scoring")
	}
	h.poll()
	op := h.login(asOperator)
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &op, size.width, size.height, size.dark)
			b.open("/")
			if !b.is(`(function(){var a=document.querySelector('#grid-table a.hb[data-select="team:00"]'),c=document.querySelector('#grid-table a.hb[data-select="team:01"]');return !!a&&!!c&&a.className===c.className&&!a.title&&getComputedStyle(a).color===getComputedStyle(c).color})()`) {
				t.Error("team 00's row header doesn't look like team 01's")
			}
			if got := b.text("#grid-counts"); got != "368running17stopped7missing2drifted2busy" {
				t.Errorf("legend = %q", got)
			}
			b.noSideways("grid with team 00")
			b.shot("grid-team00-" + size.name)

			b.click(`#grid-table a.hb[data-select="team:00"]`)
			if n := b.text("#sel-count"); n != "9" {
				t.Errorf("after selecting team 00, count = %q", n)
			}
			if b.text("#ab-why") != "" || b.is(`document.querySelector('#actionbar button[value=start]').disabled`) {
				t.Errorf("team 00 is locked for an operator (why %q)", b.text("#ab-why"))
			}
			b.shot("grid-team00-selected-" + size.name)
			b.click(`#grid-table a.hb[data-select="team:00"]`)

			// Every team's dc: the operator may power them all.
			b.click(`#grid-table a.hb[data-select="host:dc"]`)
			if n, why := b.text("#sel-count"), b.text("#ab-why"); n != "33" || why != "" {
				t.Errorf("dc column: count %q, why %q", n, why)
			}
			b.run("escape", chromedp.KeyEvent("\x1b"))
			b.clean()
		})
	}

	b := newBrowser(t, h, &op, 1366, 768, false)
	b.open("/")
	h.api.Mu.Lock()
	maps.DeleteFunc(h.api.VMs, func(_ int, vm *podstest.VM) bool { return strings.HasPrefix(vm.Name, "team00-") })
	h.api.Mu.Unlock()
	h.poll()
	b.waitFor("team 00's row to go", `!document.querySelector('[data-select="team:00"]') && !document.getElementById("cell-00-dc")`)
	b.clean()
}
