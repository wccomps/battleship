package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

func TestGridPage(t *testing.T) {
	h := newHarness(t)
	h.api.setStatus(10102, "stopped") // team01-web
	h.api.add(proxmox.VM{VMID: 10303, Name: "team03-ftp", Node: "n2", Status: "running", Pool: "wrong-pool"}, cleanConfig("03"))
	h.poll()
	lead := h.login(asLead)

	rec := h.get(&lead, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	contains(t, "grid page", body,
		"<title>Grid · battleship</title>",
		// Headers select their column or row.
		`<a class="hb" href="/?select=host:dc" data-select="host:dc" role="button" aria-pressed="false"><span class="sr">Select every </span>dc</a>`,
		`data-select="host:ftp"`, `data-select="host:web"`,
		`<a class="hb" href="/?select=team:01" data-select="team:01" role="button" aria-pressed="false"><span class="sr">Select team </span>01</a>`,
		`data-select="team:02"`, `data-select="team:03"`,
		// Every state is a shape, with its word in the tooltip and for
		// screen readers.
		`<div class="gc" role="cell" id="cell-01-dc"><label class="cell is-running" title="team01-dc · running"><input type="checkbox" name="vms" value="team01-dc" data-team="01" data-host="dc">`,
		`<span class="sr">team01-dc, running</span>`,
		`<div class="gc" role="cell" id="cell-01-web"><label class="cell is-stopped" title="team01-web · stopped">`,
		`<div class="gc" role="cell" id="cell-01-ftp"><span class="cell is-missing" title="team01-ftp · missing">`,
		`<div class="gc" role="cell" id="cell-03-ftp"><label class="cell is-drifted" title="team03-ftp · drifted — in pool &#34;wrong-pool&#34;, expected &#34;pool-03&#34;">`,
		// The legend counts.
		`<li class="is-running"><i class="g" aria-hidden="true"></i><b>5</b>running</li>`,
		"<b>1</b>stopped", "<b>2</b>missing", "<b>1</b>drifted</li></ul>",
		// The live dot, and the live-update hook.
		`<div class="app" id="app" data-live="live">`,
		`<div class="pop" id="grid-status" role="status" data-live="live">`, "<b>Live</b>",
		`data-events="/events/grid"`,
		`<script src="/static/app.js?v=`,
	)
	if n := strings.Count(body, `<div class="gc" role="cell" id="cell-`); n != 9 {
		t.Errorf("grid has %d cells, want 3 teams × 3 hosts", n)
	}
	lacks(t, "a fresh grid", body, "No answer from Proxmox")
}

func TestGridLayoutShowsUserAndNav(t *testing.T) {
	h := newHarness(t)
	h.poll()

	lead := h.login(asLead)
	body := h.get(&lead, "/").Body.String()
	contains(t, "lead's page", body,
		`<a href="/" aria-current="page">Grid</a>`,
		`<a href="/logs">Logs</a>`,
		`<nav class="lead-ops" aria-label="Team operations">`, `<a class="btn" href="/deploy" data-panel-link>`, "Deploy</a>",
		`<summary aria-label="Account: Test lead, test-lead@auth.example.org"><span class="who" aria-hidden="true">T</span></summary>`,
		"Test lead", `<span class="role">test-lead@auth.example.org · `,
		`<form method="post" action="/auth/logout">`,
		`<input type="hidden" name="csrf" value="`+lead.CSRF+`">`,
		`<button type="submit">Log out</button>`,
	)

	op := h.login(asOperator)
	body = h.get(&op, "/").Body.String()
	contains(t, "operator's page", body, `<a href="/logs">Logs</a>`, `<span class="role">test-operator@auth.example.org · `)
	lacks(t, "operator's page", body, `href="/deploy"`, lead.CSRF)
}

func TestGridBannerWhileWaitingForFirstPoll(t *testing.T) {
	h := newHarness(t)
	op := h.login(asOperator)
	body := h.get(&op, "/").Body.String()
	contains(t, "grid before the first poll", body,
		`<div class="app" id="app" data-live="stale">`,
		"<b>Waiting for live data</b>",
	)
	// Not read yet isn't empty: no empty state, and no rows, until a read
	// finds team VMs or finds none.
	lacks(t, "grid before the first poll", body, "<b>Live</b>", `id="grid-empty"`, `data-select="team:`)
}

func TestGridStaleBanner(t *testing.T) {
	h := newHarness(t)
	h.poll()
	h.clock.Advance(3 * time.Minute)
	h.api.setListErr(&proxmox.APIError{Status: 503, Message: "proxy timeout"})
	if err := h.poller.Poll(t.Context()); err == nil {
		t.Fatal("Poll with Proxmox down = nil, want an error")
	}
	op := h.login(asOperator)
	body := h.get(&op, "/").Body.String()
	contains(t, "stale grid", body,
		`<div class="app" id="app" data-live="stale">`,
		`<div class="pop" id="grid-status" role="status" data-live="stale" data-since="`,
		"<b>No answer from Proxmox</b>",
		"proxy timeout",
		"last update 09:00:00 UTC",
		// The cells stay; the page fades them.
		`id="cell-01-dc"><label class="cell is-running"`,
	)
	lacks(t, "stale grid", body, "<b>Live</b>")
}

func TestGridScanNote(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	contains(t, "grid before a scan", h.get(&op, "/").Body.String(), "config check: not run yet")

	h.api.setReadErr(10201, errors.New("connection reset"))
	h.clock.Advance(30 * time.Second)
	if err := h.poller.Scan(t.Context()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	contains(t, "grid after a scan", h.get(&op, "/").Body.String(),
		"config check 09:00:30 UTC · every",
		"1 of 6 VMs could not be read",
	)
}

func TestGridShowsOnlyWhatProxmoxShows(t *testing.T) {
	h := newHarness(t)
	h.poll()

	rec := h.get(nil, "/")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/auth/login?next=%2F" {
		t.Errorf("anonymous GET / = %d to %q, want 303 to the login", rec.Code, rec.Header().Get("Location"))
	}
	rec = h.get(nil, "/events/grid")
	if rec.Code != http.StatusSeeOther {
		t.Errorf("anonymous GET /events/grid = %d, want 303", rec.Code)
	}

	// Whoever Authentik lets in gets the grid, with the VMs Proxmox lets
	// them see: none, for a student or anyone without VM.Audit.
	student := h.loginStudent()
	rec = h.get(&student, "/")
	if rec.Code != http.StatusOK {
		t.Errorf("student GET / = %d, want 200", rec.Code)
	}
	lacks(t, "student's grid", rec.Body.String(), `id="cell-01-dc"`, "team01-dc")
	none := h.login(asNobody)
	lacks(t, "nobody's grid", h.get(&none, "/").Body.String(), `id="cell-01-dc"`, "team01-dc")
}

func TestGridFragmentSize(t *testing.T) {
	// At 32 teams × 12 hosts the full grid stays small and a one-VM change
	// is a patch of a few hundred bytes.
	h := newHarness(t)
	for team := 1; team <= 32; team++ {
		for host := range 10 {
			tm := fmt.Sprintf("%02d", team)
			vm := teamVM(tm, fmt.Sprintf("host%d", host), 10000+team*100+host+10)
			if host == 3 {
				vm.Pool = "elsewhere"
			}
			h.api.add(vm, cleanConfig(tm))
		}
	}
	h.hist.set("team07-host5", store.ItemResult{JobID: 12, JobKind: "deploy", Status: store.ItemFailed, Step: "network", Error: "net1: bridge int07 does not exist"})
	h.poll()
	g := h.poller.Grid()
	if len(g.Hosts) != 12 || len(g.Rows) != 32 {
		t.Fatalf("grid is %d×%d, want 32×12 (host0-host9, dc and web)", len(g.Rows), len(g.Hosts))
	}
	v := h.srv.newGridView(g, h.clock.Now(), nil)
	frag, err := gridFragment(v)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(frag, `<div class="gc" role="cell" id="cell-`); n != 32*12 {
		t.Errorf("fragment has %d cells, want %d", n, 32*12)
	}
	if len(frag) > 128<<10 {
		t.Errorf("grid fragment is %d bytes, want at most 128 KiB", len(frag))
	}

	before, err := renderParts(v)
	if err != nil {
		t.Fatal(err)
	}
	h.api.setStatus(10110, "stopped") // team01-host0
	h.poll()
	after, err := renderParts(h.srv.newGridView(h.poller.Grid(), h.clock.Now(), nil))
	if err != nil {
		t.Fatal(err)
	}
	patch := after.changedSince(before.pieceSet)
	if n := strings.Count(patch, `<div class="gc" role="cell" id="cell-`); n != 1 || !strings.Contains(patch, `id="cell-01-host0"><label class="cell is-stopped"`) {
		t.Errorf("patch after one VM stopped:\n%s", patch)
	}
	if len(patch) > 2<<10 {
		t.Errorf("patch is %d bytes, want at most 2 KiB", len(patch))
	}
	t.Logf("32×12 grid: whole fragment %d bytes, one-VM patch %d bytes", len(frag), len(patch))
}

func TestGridCellDriftFromJob(t *testing.T) {
	h := newHarness(t)
	h.hist.set("team02-dc", store.ItemResult{JobID: 41, JobKind: "reset", Status: store.ItemFailed, Step: "rollback", Error: "snapshot missing"})
	h.poll()
	op := h.login(asOperator)
	contains(t, "grid", h.get(&op, "/").Body.String(),
		`id="cell-02-dc"><label class="cell is-drifted"`,
		"failed in reset job 41",
	)
	if c, _ := h.poller.Grid().Cell("02", "dc"); c.State != status.StateDrifted {
		t.Fatalf("cell = %+v", c)
	}
}

// A drifted cell's reasons are in its name as text, for keyboards and
// screen readers, not only in the hover title.
func TestGridDriftReasonsWithoutHover(t *testing.T) {
	h := newHarness(t)
	h.hist.set("team02-dc", store.ItemResult{JobID: 41, JobKind: "reset", Status: store.ItemFailed, Step: "rollback", Error: "snapshot missing"})
	h.poll()
	op := h.login(asOperator)
	contains(t, "grid", h.get(&op, "/").Body.String(),
		`<span class="sr">team02-dc, drifted, failed in reset job 41: snapshot missing</span>`)
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "app.css", string(css), ".sr{position:absolute;width:1px;height:1px;overflow:hidden;clip-path:inset(50%)")
}

// Markup in what Proxmox or a job reports is shown as text everywhere:
// a VM name, a config value and a drift reason.
func TestMarkupIsEscaped(t *testing.T) {
	h := newHarness(t)
	evil := `<img src=x onerror=alert(1)>`
	cfg := cleanConfig("01")
	cfg["net1"] = "virtio=BC:24:11:00:00:02,bridge=int02,firewall=<script>alert(2)</script>"
	h.api.add(teamVM("01", evil, 10150), cfg, "initial")
	h.hist.set("team02-dc", store.ItemResult{JobID: 41, JobKind: "deploy", Status: store.ItemFailed, Step: "network", Error: `<b onclick="x()">boom</b>`})
	h.poll()
	if err := h.poller.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	op := h.login(asOperator)
	raw := []string{"<img", "<script>", "<b onclick"}

	grid := h.get(&op, "/").Body.String()
	contains(t, "grid", grid, "&lt;img src=x onerror=alert(1)&gt;", "&lt;script&gt;alert(2)&lt;/script&gt;", "&lt;b onclick=")
	lacks(t, "grid", grid, raw...)
	ev := h.openSSE(&op, "/events/grid").next()
	lacks(t, "grid event", ev.Data, raw...)
	contains(t, "grid event", ev.Data, "&lt;img src=x onerror=alert(1)&gt;")

	cell := h.get(&op, "/vm/01/"+url.PathEscape(evil))
	if cell.Code != http.StatusOK {
		t.Fatalf("cell page = %d", cell.Code)
	}
	body := cell.Body.String()
	contains(t, "cell page", body, "team01-&lt;img src=x onerror=alert(1)&gt;", "firewall=&lt;script&gt;alert(2)&lt;/script&gt;")
	lacks(t, "cell page", body, raw...)
	job := h.get(&op, "/vm/02/dc").Body.String()
	contains(t, "cell page 02/dc", job, "&lt;b onclick=&#34;x()&#34;&gt;boom&lt;/b&gt;")
	lacks(t, "cell page 02/dc", job, raw...)
}

// With no team VMs the page shows template sets and no grid. The empty
// state is viewer-independent so the live fragment can be cached; Deploy
// buttons are rendered per request, for leads only.
func TestGridEmptyState(t *testing.T) {
	h := newHarness(t)
	h.noTeamVMs()
	h.api.add(proxmox.VM{VMID: 501, Name: "bugs.looney.tunes", Node: "n1", Status: "running", Tags: "dev"}, nil)
	h.poll()
	const empty = `<section class="start" id="grid-start" aria-labelledby="sets-h">`

	lead := h.login(asLead)
	body := h.get(&lead, "/").Body.String()
	contains(t, "lead's empty grid", body, empty, "<h3>looney.tunes</h3>", `<a class="btn set-deploy" href="/deploy?`)
	lacks(t, "lead's empty grid", body, `id="grid-table"`, `data-select="host:bugs"`)

	op := h.login(asOperator)
	body = h.get(&op, "/").Body.String()
	contains(t, "operator's empty grid", body, empty, "<h3>looney.tunes</h3>")
	lacks(t, "operator's empty grid", body, `id="grid-table"`, "set-deploy", `href="/deploy`)

	// The event stream's fragment has the empty state, as the lead's page.
	ev := h.openSSE(&lead, "/events/grid").next()
	contains(t, "empty grid event", ev.Data, empty, `<a class="btn set-deploy" href="/deploy?`)
	lacks(t, "empty grid event", ev.Data, `id="grid-table"`, `data-select="host:bugs"`)
}

// The template sets show only on the empty grid.
func TestGridDeployLinkGoneWithColumns(t *testing.T) {
	h := newHarness(t)
	h.poll()
	lead := h.login(asLead)
	body := h.get(&lead, "/").Body.String()
	contains(t, "lead's grid", body, `id="grid-table"`)
	lacks(t, "lead's grid", body, `id="grid-start"`, "set-deploy")
}

// The grid's cells size by its counts: narrower for many hosts, taller
// for few teams; a little gap every 8 teams; a box of its own past 36.
func TestGridSizing(t *testing.T) {
	for _, c := range []struct {
		hosts, teams int
		want         string
	}{{12, 32, ""}, {13, 32, "cols-m"}, {16, 16, "cols-m rows-s"}, {20, 8, "cols-l rows-s"}, {24, 17, "cols-l"}, {25, 50, "cols-xl"}} {
		if got := gridClass(c.hosts, c.teams); got != c.want {
			t.Errorf("gridClass(%d hosts, %d teams) = %q, want %q", c.hosts, c.teams, got, c.want)
		}
	}
	h := newHarness(t)
	for team := 4; team <= 40; team++ {
		tm := fmt.Sprintf("%02d", team)
		h.api.add(teamVM(tm, "dc", 10000+team*100+1), cleanConfig(tm))
	}
	h.poll()
	v := h.srv.newGridView(h.poller.Grid(), h.clock.Now(), nil)
	frag, err := gridFragment(v)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(frag, `<div class="gr grp" role="row">`); n != 4 {
		t.Errorf("40 teams have %d group gaps, want 4 (before 09, 17, 25 and 33)", n)
	}
	contains(t, "tall grid", frag, `<div class="gscroll tall" role="region"`, `<div class="grid" id="grid-table"`)
}
