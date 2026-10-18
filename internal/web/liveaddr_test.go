package web

import (
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
)

// addLiveEnv puts kilo.alpha in dev-01 with guest agents: its router,
// WAN 10.50.12.133; teak where it was put; yew moved from .5 to .7; elm
// stopped; oak without an agent; ash whose agent hangs if hang.
func addLiveEnv(h *harness, hang bool) {
	mac := func(vmid int) string {
		return "BC:24:11:00:" + strconv.Itoa(vmid/100) + ":" + strconv.Itoa(vmid%100+10)
	}
	for _, vm := range []struct {
		vmid             int
		name, tags, desc string
		status           string
		agent            string
		ips              []netip.Prefix
	}{
		{2101, "teak.kilo.alpha", "dev;kilo.alpha;linux", "battleship: ip=192.168.100.10 os=linux base=debian-12-v2", "running", "1", podstest.IPs("192.168.100.10/24", "fe80::10/64")},
		{2102, "yew.kilo.alpha", "dev;kilo.alpha;linux", "battleship: ip=192.168.100.5 os=linux base=debian-12-v2", "running", "enabled=1", podstest.IPs("192.168.100.7/24")},
		{2103, "elm.kilo.alpha", "dev;kilo.alpha;windows", "battleship: ip=192.168.100.20 os=windows base=win10-v1", "stopped", "1", podstest.IPs("192.168.100.20/24")},
		{2104, "oak.kilo.alpha", "dev;kilo.alpha", "battleship: ip=192.168.100.30 base=plain-v1", "running", "0", podstest.IPs("192.168.100.30/24")},
		{2105, "ash.kilo.alpha", "dev;kilo.alpha;linux", "battleship: ip=192.168.100.40 os=linux base=debian-12-v2", "running", "1", podstest.IPs("192.168.100.40/24")},
	} {
		h.api.Add(proxmox.VM{VMID: vm.vmid, Name: vm.name, Node: "n1", Pool: "dev-01", Tags: vm.tags, Status: vm.status},
			map[string]string{"description": vm.desc, "agent": vm.agent, "net0": "virtio=" + mac(vm.vmid) + ",bridge=devint01"})
		h.api.SetGuest(vm.vmid, podstest.Guest{Up: true, NICs: []proxmox.GuestNIC{
			{Name: "lo", MAC: "00:00:00:00:00:00", IPs: podstest.IPs("127.0.0.1/8", "::1/128")},
			{Name: "eth0", MAC: strings.ToLower(mac(vm.vmid)), IPs: vm.ips},
		}})
	}
	h.api.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha;router", Status: "running"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router", "agent": "1",
			"net0": "virtio=BC:24:11:00:21:00,bridge=devext01", "net1": "virtio=BC:24:11:00:21:01,bridge=devint01"})
	// The NICs' names say nothing: a FreeBSD router's are vtnet0 and vtnet1, in either order.
	h.api.SetGuest(2100, podstest.Guest{Up: true, NICs: []proxmox.GuestNIC{
		{Name: "lan", MAC: "bc:24:11:00:21:00", IPs: podstest.IPs("fe80::1/64", "10.50.12.133/24")},
		{Name: "wan", MAC: "bc:24:11:00:21:01", IPs: podstest.IPs("192.168.100.2/24")},
	}})
	if hang {
		h.api.Mu.Lock()
		h.api.agentHangs = map[int]bool{2105: true}
		h.api.Mu.Unlock()
	}
}

// addrCell is the address cell of VM vmid in body.
func addrCell(t *testing.T, body string, vmid int) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<td class="ad" id="addr-` + strconv.Itoa(vmid) + `">.*?</td>`).FindString(body)
	if m == "" {
		t.Fatalf("no address cell of %d in:\n%s", vmid, body)
	}
	return m
}

// checkLiveCells checks kilo.alpha's address cells with live addresses.
func checkLiveCells(t *testing.T, body string) {
	t.Helper()
	contains(t, "the router", addrCell(t, body, 2100), `<code>192.168.100.2</code>`, `<span class="meta">WAN</span> <code>10.50.12.133</code>`)
	lacks(t, "the router", addrCell(t, body, 2100), "differs", "no guest agent", "external")
	contains(t, "teak", addrCell(t, body, 2101), `<code>192.168.100.10</code>`, `<span class="meta">external</span> <code>10.50.12.10</code>`)
	lacks(t, "teak", addrCell(t, body, 2101), "differs", "no guest agent", "fe80", "WAN")
	contains(t, "yew", addrCell(t, body, 2102), `<code>192.168.100.7</code> <span class="chip differs">differs</span>`,
		`<span class="al meta">assigned 192.168.100.5</span>`, `<code>10.50.12.7</code>`)
	contains(t, "elm (stopped)", addrCell(t, body, 2103), `<span class="al"><code>192.168.100.20</code></span>`, `<code>10.50.12.20</code>`)
	lacks(t, "elm (stopped)", addrCell(t, body, 2103), "no guest agent", "differs")
	for vmid, assigned := range map[int]string{2104: "30", 2105: "40"} {
		contains(t, strconv.Itoa(vmid), addrCell(t, body, vmid), `<code>192.168.100.`+assigned+`</code> <span class="meta">· no guest agent</span>`,
			`<code>10.50.12.`+assigned+`</code>`)
	}
}

// The page comes with assigned addresses and reads no agent; the script
// fills in the live ones from pieces, and without it a link shows the page
// with them. An agent that doesn't answer holds up the pieces for the read
// timeout, not the minute all the reads may take.
func TestEnvironmentsLiveAddresses(t *testing.T) {
	h := newHarness(t)
	addLiveEnv(h, true)
	h.srv.addrTimeout, h.srv.addrWithin = 500*time.Millisecond, time.Minute
	sess := h.login(asLead)

	body := h.get(&sess, "/environments").Body.String()
	contains(t, "the page", body, `<p class="meta liveaddr" id="env-live" data-fill="/environments/addresses">`,
		`<a href="/environments?live=1">Show live addresses</a>`,
		`<td class="ad" id="addr-2102"><span class="al"><code>192.168.100.5</code></span></td>`,
		`<td class="ad" id="addr-2100"><span class="al"><span class="meta">no address</span></span></td>`)
	lacks(t, "the page", body, "no guest agent", "<code>192.168.100.2</code>", "10.50.12", "differs")
	if n := h.api.agentReadCount(); n != 0 {
		t.Errorf("the page read %d agents", n)
	}

	start := time.Now()
	rec := h.get(&sess, "/environments/addresses")
	took := time.Since(start)
	pieces := rec.Body.String()
	if rec.Code != http.StatusOK || rec.Header().Get(piecesHeader) != "1" {
		t.Fatalf("addresses = %d %v:\n%s", rec.Code, rec.Header(), pieces)
	}
	if took < h.srv.addrTimeout || took > h.srv.addrTimeout+1500*time.Millisecond {
		t.Errorf("the addresses took %v with ash's agent hung, read timeout %v", took, h.srv.addrTimeout)
	}
	checkLiveCells(t, pieces)
	contains(t, "the pieces", pieces, `<template><p class="meta liveaddr" id="env-live"><span>Addresses are live from the guest agents, where they answered.</span>`)
	if n := strings.Count(pieces, "<template>"); n != 7 {
		t.Errorf("%d pieces, want the 6 cells and the line", n)
	}
	for _, vmid := range []int{2103, 2104} {
		if n := h.api.Called("agent-nics:" + strconv.Itoa(vmid)); n != 0 {
			t.Errorf("read %d's agent %d times; it can't answer", vmid, n)
		}
	}

	body = h.get(&sess, "/environments?live=1").Body.String()
	checkLiveCells(t, body)
	contains(t, "the live page", body, `<p class="meta liveaddr" id="env-live"><span>Addresses are live`, `<a href="/environments?live=1">Read again</a>`)
	lacks(t, "the live page", body, "data-fill")
	if got := strings.Join(hostNames(envSection(t, body, "kilo.alpha")), " "); got != "gw yew teak elm oak ash" {
		t.Errorf("hosts in order %q, by assigned address", got)
	}
}

// All of the reads are bounded together, whatever each one's timeout.
func TestEnvironmentsLiveAddressesBounded(t *testing.T) {
	h := newHarness(t)
	addLiveEnv(h, true)
	h.srv.addrTimeout, h.srv.addrWithin = time.Minute, 300*time.Millisecond
	sess := h.login(asLead)
	start := time.Now()
	body := h.get(&sess, "/environments?live=1").Body.String()
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the live page took %v with ash's agent hung", took)
	}
	checkLiveCells(t, body)
}

// Without VM.GuestAgent.Audit anywhere, the page says so, offers no live
// addresses and reads no agent; with it on only some VMs, those are live.
func TestEnvironmentsLiveAddressesPrivileges(t *testing.T) {
	h := newHarness(t)
	addLiveEnv(h, false)
	op := h.login(asOperator)
	body := h.get(&op, "/environments").Body.String()
	contains(t, "as an operator", body, "Showing live addresses needs VM.GuestAgent.Audit, which you don&#39;t have.",
		`<code>192.168.100.5</code>`)
	lacks(t, "as an operator", body, "data-fill", "live=1", `id="env-live"`)
	lacks(t, "the operator's live page", h.get(&op, "/environments?live=1").Body.String(), "10.50.12", "no guest agent")
	// Asked for the pieces anyway, the server says what the page says.
	rec := h.get(&op, "/environments/addresses")
	if rec.Code != http.StatusForbidden || rec.Header().Get(piecesHeader) != "" ||
		strings.TrimSpace(rec.Body.String()) != "Showing live addresses needs VM.GuestAgent.Audit, which you don't have." {
		t.Errorf("the operator's pieces: %d %v\n%s", rec.Code, rec.Header(), rec.Body)
	}
	if n := h.api.agentReadCount(); n != 0 {
		t.Errorf("read %d agents for a user without VM.GuestAgent.Audit", n)
	}

	cred := h.ticketWith("test-partial", personaPrivileges[asOperator])
	for _, vmid := range []string{"2100", "2102"} {
		h.pve.Grant("test-partial@"+pvetest.Realm, "/vms/"+vmid, "VM.Audit", "VM.GuestAgent.Audit")
	}
	partial := authtest.Login(t, h.st, h.cfg, authtest.User{Subject: "test-partial", Name: "Partial", At: h.clock.Now(), Proxmox: cred})
	h.openView(cred)
	pieces := h.get(&partial, "/environments/addresses").Body.String()
	contains(t, "the router", addrCell(t, pieces, 2100), `<code>10.50.12.133</code>`)
	contains(t, "yew", addrCell(t, pieces, 2102), `<span class="chip differs">differs</span>`)
	contains(t, "teak", addrCell(t, pieces, 2101), `<code>192.168.100.10</code> <span class="meta">· no guest agent</span>`, `<code>10.50.12.10</code>`)
}

// A stopped router shows no address and leaves its hosts without external
// addresses; a running router without an agent says so, and one configured
// at the wrong LAN address is flagged against the gateway.
func TestEnvironmentsLiveAddressesWithoutRouter(t *testing.T) {
	h := newHarness(t)
	addLiveEnv(h, false)
	h.api.setStatus(2100, "stopped")
	h.api.SetGuest(2101, podstest.Guest{Up: true, NICs: []proxmox.GuestNIC{{MAC: "bc:24:11:00:21:11", IPs: podstest.IPs("fe80::11/64", "169.254.1.1/16", "127.0.0.1/8")}}})
	h.api.FailOn("agent-nics:2102", &proxmox.APIError{Status: 500, Message: "QEMU guest agent is not running"})
	sess := h.login(asLead)
	pieces := h.get(&sess, "/environments/addresses").Body.String()
	lacks(t, "the pieces", pieces, "10.50.12", "fe80", "169.254", "external")
	contains(t, "the stopped router", addrCell(t, pieces, 2100), `<span class="al"><span class="meta">no address</span></span>`)
	lacks(t, "the stopped router", addrCell(t, pieces, 2100), "192.168.100.2", "no guest agent")
	contains(t, "teak", addrCell(t, pieces, 2101), `<span class="meta">no live address</span>`, `assigned 192.168.100.10`)
	contains(t, "yew", addrCell(t, pieces, 2102), `<code>192.168.100.5</code> <span class="meta">· no guest agent</span>`)

	// A running router whose guest agent isn't running says no guest agent,
	// not 192.168.100.2.
	h.api.setStatus(2100, "running")
	h.api.FailOn("agent-nics:2100", &proxmox.APIError{Status: 500, Message: "QEMU guest agent is not running"})
	cell := addrCell(t, h.get(&sess, "/environments/addresses").Body.String(), 2100)
	contains(t, "router without agent", cell, `<span class="al"><span class="meta">no guest agent</span></span>`)
	lacks(t, "router without agent", cell, "192.168.100.2", "no address")

	// A running router set up at the wrong LAN address shows its live
	// address and flags it against the expected gateway.
	h.api.FailOn("agent-nics:2100", nil)
	h.api.SetGuest(2100, podstest.Guest{Up: true, NICs: []proxmox.GuestNIC{
		{Name: "wan", MAC: "bc:24:11:00:21:00", IPs: podstest.IPs("10.50.12.133/24")},
		{Name: "lan", MAC: "bc:24:11:00:21:01", IPs: podstest.IPs("192.168.100.1/24")},
	}})
	cell = addrCell(t, h.get(&sess, "/environments/addresses").Body.String(), 2100)
	contains(t, "router at .1", cell, `<code>192.168.100.1</code> <span class="chip differs">differs</span>`,
		`<span class="al meta">expected 192.168.100.2</span>`, `<span class="meta">WAN</span> <code>10.50.12.133</code>`)
}

// A VM battleship didn't make has no assigned address: its live one shows
// plainly, neither flagged as differing nor beside an assigned one.
func TestEnvironmentsLiveAddressWithoutAssigned(t *testing.T) {
	h := newHarness(t)
	addLiveEnv(h, false)
	h.api.Add(proxmox.VM{VMID: 2106, Name: "fir.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:21:16,bridge=devint01"})
	h.api.SetGuest(2106, podstest.Guest{Up: true, NICs: []proxmox.GuestNIC{{MAC: "bc:24:11:00:21:16", IPs: podstest.IPs("192.168.100.50/24")}}})
	sess := h.login(asLead)
	cell := addrCell(t, h.get(&sess, "/environments/addresses").Body.String(), 2106)
	contains(t, "fir", cell, `<span class="al"><code>192.168.100.50</code></span>`, `<code>10.50.12.50</code>`)
	lacks(t, "fir", cell, "differs", "assigned", "none")
}

// Live address reads also check whether SSH and the ops CA key are ready on
// each answering host, showing a warning chip in the address cell when not.
func TestEnvironmentsLiveSSHWarnings(t *testing.T) {
	h := newHarness(t)
	addLiveEnv(h, false)
	h.api.SetGuest(2101, podstest.Guest{
		Up:     true,
		NICs:   []proxmox.GuestNIC{{MAC: "bc:24:11:00:21:11", IPs: podstest.IPs("192.168.100.10/24")}},
		SSHOut: "sshd=running\n===SSHD===\npermitrootlogin yes\n===CA===\n===PRINCIPALS===\n===AUTHKEYS===\n",
	})
	h.api.SetGuest(2102, podstest.Guest{
		Up:     true,
		NICs:   []proxmox.GuestNIC{{MAC: "bc:24:11:00:21:12", IPs: podstest.IPs("192.168.100.5/24")}},
		SSHOut: "sshd=stopped\n===SSHD===\n",
	})
	sess := h.login(asLead)
	for _, path := range []string{"/environments/addresses", "/environments?live=1"} {
		h.api.FailOn("agent-ssh:2105", &proxmox.APIError{Status: 500, Message: "Command guest-exec has been disabled"})
		body := h.get(&sess, path).Body.String()
		lacks(t, "the router at "+path, addrCell(t, body, 2100), "is-drifted")
		contains(t, "teak at "+path, addrCell(t, body, 2101), `<span class="al"><span class="chip is-drifted">missing ops CA key</span></span>`)
		contains(t, "yew at "+path, addrCell(t, body, 2102), `<span class="al"><span class="chip is-drifted">SSH not running</span></span>`)
		contains(t, "ash at "+path, addrCell(t, body, 2105), `<span class="al"><span class="chip is-drifted">guest-exec disabled</span></span>`)
	}
}

// In the browser, the page fills in the live addresses after it loads, as
// the server renders them, without scrolling sideways on a phone.
func TestBrowserEnvironmentsLiveAddresses(t *testing.T) {
	h := browserHarness(t)
	addLiveEnv(h, true)
	h.srv.addrTimeout = 300 * time.Millisecond
	h.api.Add(proxmox.VM{VMID: 2400, Name: "ad.oscar.lima", Node: "n1", Pool: "dev-04", Tags: "dev;oscar.lima;windows", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.10 os=windows base=win10-v1", "agent": "1", "net0": "virtio=BC:24:11:00:24:00,bridge=devint04"})
	h.api.SetGuest(2400, podstest.Guest{Up: true, NICs: []proxmox.GuestNIC{{MAC: "bc:24:11:00:24:00", IPs: podstest.IPs("fe80::5/64", "2001:db8:100::10/64")}}})
	lead := h.login(asLead)
	want := map[string]string{}
	live := h.get(&lead, "/environments?live=1").Body.String()
	for _, vmid := range []int{2100, 2101, 2102, 2103, 2104, 2105, 2400} {
		want[strconv.Itoa(vmid)] = addrCell(t, live, vmid)
	}
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			b.open("/environments")
			b.waitFor("the live addresses", `!document.querySelector("[data-fill]") && document.getElementById("addr-2102").textContent.includes("differs")`)
			for vmid, cell := range want {
				var got string
				b.eval(`document.getElementById("addr-`+vmid+`").outerHTML`, &got)
				if oneLine(got) != oneLine(cell) {
					t.Errorf("%s's cell after the fill:\n%s\nwant\n%s", vmid, got, cell)
				}
			}
			b.noSideways("live addresses")
			b.shot("environments-live-" + size.name)
			b.fullShot("environments-live-full-" + size.name)
			b.clean()
		})
	}
	// Without the script, the page keeps its assigned addresses, and its
	// link reads the live ones.
	for _, size := range sizes {
		t.Run("noscript-"+size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &lead, size.width, size.height, size.dark)
			// The page runs no script, so nothing settles it (browser.settle).
			b.run("opening without scripts", emulation.SetScriptExecutionDisabled(true),
				chromedp.Navigate(b.srv.URL+"/environments"), chromedp.WaitReady("body"))
			noScriptShot(b, "environments-noscript-"+size.name)
			if got := b.text("#env-live"); got != "Addresses as assigned. Show live addresses" {
				t.Errorf("the line says %q", got)
			}
			b.run("following the link", chromedp.Click(`#env-live a`, chromedp.ByQuery), chromedp.WaitVisible(`#env-live:not([data-fill]) a`, chromedp.ByQuery))
			var search string
			b.eval(`location.search`, &search)
			if got := b.text("#addr-2102"); search != "?live=1" || !strings.Contains(got, "differs") {
				t.Errorf("at %q, yew's cell says %q", search, got)
			}
			b.noSideways("the live page")
			noScriptShot(b, "environments-noscript-live-"+size.name)
		})
	}
}

// noScriptShot is browser.shot for a page that runs no script: it waits
// for the fonts by polling rather than with a promise.
func noScriptShot(b *browser, name string) {
	b.t.Helper()
	dir := os.Getenv("BATTLESHIP_SHOTS")
	if dir == "" {
		return
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var status string
		b.run("reading the fonts' status", chromedp.Evaluate(`document.fonts.status`, &status))
		if status == "loaded" {
			break
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("fonts still %s after 10s", status)
		}
	}
	var png []byte
	b.run("screenshot", chromedp.CaptureScreenshot(&png))
	if err := os.WriteFile(filepath.Join(dir, name+".png"), png, 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// oneLine is markup without its newlines.
func oneLine(s string) string { return strings.ReplaceAll(s, "\n", "") }

// The page names the paths the server serves the live addresses at.
func TestTemplateKnowsLiveAddrPaths(t *testing.T) {
	html, err := templateFS.ReadFile("templates/catalog.html")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "catalog.html", string(html), `data-fill="`+envAddrsPath+`"`,
		`<a href="`+envsPath+`?live=1">Read again</a>`, `<a href="`+envsPath+`?live=1">Show live addresses</a>`)
}

// A keyboard user on the line's link when the live addresses land stays on
// it: on its Read again link.
func TestBrowserLiveAddressesKeepFocus(t *testing.T) {
	h := browserHarness(t)
	addLiveEnv(h, true)
	h.srv.addrTimeout = 2 * time.Second // ash's hung agent holds the pieces up this long
	lead := h.login(asLead)
	b := newBrowser(t, h, &lead, 1280, 800, false)
	b.open("/environments")
	b.run("focusing the link", chromedp.Focus(`#env-live a`, chromedp.ByQuery))
	if !b.is(`document.querySelector("[data-fill]") !== null && document.activeElement === document.querySelector("#env-live a")`) {
		t.Fatal("the addresses landed before the link had focus")
	}
	b.waitFor("the live addresses", `!document.querySelector("[data-fill]")`)
	var focused string
	b.eval(`document.activeElement.closest("#env-live") ? document.activeElement.textContent : document.activeElement.tagName`, &focused)
	if focused != "Read again" {
		t.Errorf("focus is on %q after the fill", focused)
	}
	b.clean()
}

// The script reads the pieces only with the header the server marks them
// with.
func TestScriptKnowsPiecesHeader(t *testing.T) {
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "app.js", string(js), `r.headers.get("`+piecesHeader+`") !== "1"`)
}
