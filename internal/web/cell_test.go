package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

func TestCellPage(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	rec := h.get(&op, "/vm/01/dc")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /vm/01/dc = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	contains(t, "cell page", body,
		"<title>team01-dc · battleship</title>",
		`<h1 id="vm-title"><span class="vm">team01-dc</span></h1>`,
		`<span class="chip is-running"><i class="g" aria-hidden="true"></i>running</span>`,
		`<dt>VMID</dt><dd><span class="vm">10101</span></dd>`,
		`<dt>Node</dt><dd><span class="vm">n1</span></dd>`,
		`<dt>Pool</dt><dd><span class="vm">pool-01</span></dd>`,
		`<dt>Power</dt><dd><span class="gl is-running"><i class="g" aria-hidden="true"></i>running</span></dd>`,
		// Snapshots, baseline marked.
		`<tr><td><span class="vm">initial</span></td><td class="muted">baseline</td></tr>`,
		`<tr><td><span class="vm">before-scoring</span></td><td class="muted"></td></tr>`,
		// Network as Proxmox has it.
		"<dt>net0</dt><dd><code>virtio=BC:24:11:00:00:01,bridge=ext01</code></dd>",
		// Reset goes to the snapshot picker for this VM.
		`<form method="post" action="/reset">`,
		`<input type="hidden" name="csrf" value="`+op.CSRF+`">`,
		`<input type="hidden" name="from" value="grid">`,
		`<input type="hidden" name="vms" value="team01-dc">`,
		"Reset to snapshot…</button>",
		// Power: what makes sense for a running VM.
		`<form method="post" action="/power/preview">`,
		`<input type="hidden" name="teams" value="01">`,
		`<input type="hidden" name="hosts" value="dc">`,
		`<button type="submit" class="btn" name="action" value="shutdown">`,
		`<button type="submit" class="btn bad" name="action" value="stop">`,
		`<button type="submit" class="btn" name="action" value="reboot">`,
		`<a class="btn ghost sq" href="/" data-close aria-label="Close">`,
	)
	lacks(t, "cell page", body, `value="start"`, `class="reasons`, "Last job")
}

func TestCellPageShowsDrift(t *testing.T) {
	h := newHarness(t)
	h.hist.set("team02-web", store.ItemResult{JobID: 41, JobKind: "deploy", Status: store.ItemFailed, Step: "network", Error: "bridge int02 missing"})
	h.api.mu.Lock()
	delete(h.api.configs[10202], "net1")
	h.api.snaps[10202] = []string{"other"}
	h.api.mu.Unlock()
	h.poll()
	op := h.login(asOperator)

	body := h.get(&op, "/vm/02/web").Body.String()
	contains(t, "drifted cell page", body,
		`<span class="chip is-drifted">`,
		`<ul class="reasons is-drifted" aria-label="Drift">`,
		"failed in deploy job 41: bridge int02 missing",
		`<a href="/logs/41" data-panel-link>#41</a>`,
		"no &#34;initial&#34; or fresh_clone_* baseline snapshot",
		// No baseline: the table shows it missing.
		`<span class="gl is-missing"><i class="g" aria-hidden="true"></i><span class="vm">initial</span></span></td><td class="muted">missing</td>`,
		`<tr><td><span class="vm">other</span></td>`,
	)
	// The live read is now in the viewer's grid too.
	if c, _ := h.gridOf(asOperator).Cell("02", "web"); c.State != status.StateDrifted || len(c.Drift) < 2 {
		t.Errorf("grid cell after the page = %+v", c)
	}
}

func TestCellPageMissingVM(t *testing.T) {
	h := newHarness(t)
	h.api.add(teamVM("01", "ftp", 10103), cleanConfig("01"))
	h.poll()

	lead := h.login(asLead)
	body := h.get(&lead, "/vm/02/ftp").Body.String()
	contains(t, "missing cell page", body,
		`<span class="vm">team02-ftp</span></h1>`,
		`<span class="chip is-missing">`,
		`<a class="btn" href="/deploy?baseline=yes&amp;hosts=ftp&amp;pattern=&amp;teams=02" data-panel-link>`,
	)
	lacks(t, "missing cell page", body, `action="/reset"`, `action="/power/preview"`, "<dt>VMID</dt>")

	op := h.login(asOperator)
	body = h.get(&op, "/vm/02/ftp").Body.String()
	contains(t, "operator's missing cell page", body, `<span class="chip is-missing">`)
	lacks(t, "operator's missing cell page", body, `href="/deploy`)
}

func TestCellPageReadError(t *testing.T) {
	h := newHarness(t)
	h.poll()
	h.api.setReadErr(10101, errors.New("connection refused"))
	op := h.login(asOperator)

	rec := h.get(&op, "/vm/01/dc")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	contains(t, "cell page with a failed read", rec.Body.String(),
		`<p class="warn stale">`,
		"Couldn't read it from Proxmox just now: reading", "connection refused",
		`<dt>VMID</dt><dd><span class="vm">10101</span></dd>`,
		`<h2 class="sub">Snapshots</h2>
<p class="muted">unknown</p>`,
		`action="/power/preview"`,
	)
	lacks(t, "cell page with a failed read", rec.Body.String(), `class="reasons`, `action="/reset"`)
}

func TestCellPageUnknown(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	for _, path := range []string{"/vm/09/dc", "/vm/01/nope", "/vm/1/dc"} {
		rec := h.get(&op, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		contains(t, path, rec.Body.String(), "No such VM", `<a class="btn" href="/">Back to the grid</a>`)
	}
}

func TestCellPageNeedsASessionAndVMAudit(t *testing.T) {
	h := newHarness(t)
	h.poll()
	if rec := h.get(nil, "/vm/01/dc"); rec.Code != http.StatusSeeOther {
		t.Errorf("anonymous = %d, want 303", rec.Code)
	}
	// A student's grid has no VMs: Proxmox shows them none.
	student := h.loginStudent()
	if rec := h.get(&student, "/vm/01/dc"); rec.Code != http.StatusNotFound {
		t.Errorf("student = %d, want 404", rec.Code)
	}
}

func TestCellPageOffersOnlyWhatYouMayDo(t *testing.T) {
	h := newHarness(t)
	h.poll()
	// Someone who may power team 01's dc, but not roll it back or snapshot
	// it (the ACL on the VM replaces the one on /).
	cred := h.ticketWith("test-powerer", []string{"VM.Audit"})
	h.pve.Grant(cred.User, "/vms/10101", "VM.Audit", "VM.PowerMgmt")
	sess := authtest.Login(t, h.st, h.cfg, authtest.User{Subject: "test-powerer", At: h.clock.Now(), Proxmox: cred})
	h.openView(cred)
	body := h.get(&sess, "/vm/01/dc").Body.String()
	contains(t, "powerer's page", body, `action="/power/preview"`)
	lacks(t, "powerer's page", body, `action="/reset"`, `action="/snapshot"`)

	op := h.login(asOperator)
	contains(t, "operator's page", h.get(&op, "/vm/01/dc").Body.String(), `action="/reset"`, `action="/power/preview"`)
}

// getAsync serves a GET in the background and returns its recorder once
// the request is done.
func (h *harness) getAsync(sess *authtest.Session, target string) <-chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		sess.Apply(req)
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		out <- rec
	}()
	return out
}

// awaitRead waits for the fake cluster to start a VMConfig read. The 10s
// limit only guards against a hang.
func awaitRead(t *testing.T, entered <-chan int) int {
	t.Helper()
	select {
	case id := <-entered:
		return id
	case <-time.After(10 * time.Second):
		t.Fatal("no VM read started within 10s")
	}
	return 0
}

// awaitJoin waits for a request to join a cell read. The 10s limit only
// guards against a hang.
func awaitJoin(t *testing.T, joined <-chan struct{}) {
	t.Helper()
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("no request joined a cell read within 10s")
	}
}

// Cell pages read live, but at most two at a time, so a crowd of them
// can't take the config-call slots the job executors need.
func TestCellPageReadsAreCapped(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	h.api.gate, h.api.entered = make(chan struct{}), make(chan int, 10)
	cells := []string{"/vm/01/dc", "/vm/01/web", "/vm/02/dc", "/vm/02/web", "/vm/03/dc"}
	var done []<-chan *httptest.ResponseRecorder
	for _, c := range cells {
		done = append(done, h.getAsync(&op, c))
	}
	awaitRead(t, h.api.entered)
	awaitRead(t, h.api.entered)
	// A third read would start at once if nothing held it back.
	select {
	case id := <-h.api.entered:
		t.Fatalf("a third cell read (VMID %d) started while two were under way", id)
	case <-time.After(100 * time.Millisecond):
	}
	for range cells {
		h.api.gate <- struct{}{}
	}
	for i, ch := range done {
		if rec := <-ch; rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", cells[i], rec.Code)
		}
	}
	if _, _, most := h.api.readCounts(); most > 2 {
		t.Errorf("%d cell reads ran at once, want at most 2", most)
	}
}

// Requests for the same cell while its read is under way share that read.
func TestCellPageReadsAreShared(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	h.api.gate, h.api.entered = make(chan struct{}), make(chan int, 10)
	joined := make(chan struct{}, 10)
	h.srv.afterCellJoin = func() { joined <- struct{}{} }
	var done []<-chan *httptest.ResponseRecorder
	for range 3 {
		done = append(done, h.getAsync(&op, "/vm/01/dc"))
	}
	awaitRead(t, h.api.entered)
	for range 3 {
		awaitJoin(t, joined)
	}
	close(h.api.gate)
	for _, ch := range done {
		rec := <-ch
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<h1 id="vm-title"><span class="vm">team01-dc</span></h1>`) {
			t.Errorf("GET /vm/01/dc = %d", rec.Code)
		}
	}
	if reads, _, _ := h.api.readCounts(); reads != 1 {
		t.Errorf("3 requests for one cell made %d config reads, want 1 shared", reads)
	}
}

// A request that gives up doesn't cancel the read others share.
func TestCellPageSharedReadOutlivesItsFirstRequest(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	h.api.gate, h.api.entered = make(chan struct{}), make(chan int, 10)
	joined := make(chan struct{}, 10)
	h.srv.afterCellJoin = func() { joined <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/vm/01/dc", nil)
		op.Apply(req)
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		first <- rec
	}()
	awaitRead(t, h.api.entered)
	awaitJoin(t, joined)
	second := h.getAsync(&op, "/vm/01/dc")
	awaitJoin(t, joined)
	cancel()
	<-first
	close(h.api.gate)
	if rec := <-second; rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Couldn't read this VM") {
		t.Errorf("second request = %d, want the shared read's page:\n%s", rec.Code, rec.Body)
	}
}

// A shared read that times out waiting for a read slot falls back to the
// grid's copy of the cell, like a failed read, rather than a blank page.
func TestCellPageReadTimeoutShowsGridCell(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	h.srv.cellTimeout = 50 * time.Millisecond
	for range cellReadSlots {
		h.srv.cellSlots <- struct{}{} // every read slot busy
	}
	rec := h.get(&op, "/vm/01/dc")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	contains(t, "cell page after a timed-out read", rec.Body.String(),
		`<h1 id="vm-title"><span class="vm">team01-dc</span></h1>`,
		`<p class="warn stale">`,
		"Couldn't read it from Proxmox just now",
		`<dt>VMID</dt><dd><span class="vm">10101</span></dd>`,
	)
}

// Shutting the server down cancels the shared cell reads, which run on its
// lifetime rather than a request's; the page waiting on one falls back to
// the grid's copy of the cell.
func TestCellPageReadStopsAtShutdown(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	h.api.gate, h.api.entered = make(chan struct{}), make(chan int, 10)
	defer close(h.api.gate)
	done := h.getAsync(&op, "/vm/01/dc")
	awaitRead(t, h.api.entered)
	h.srv.CloseStreams()
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d", rec.Code)
		}
		contains(t, "cell page cut off by shutdown", rec.Body.String(),
			`<h1 id="vm-title"><span class="vm">team01-dc</span></h1>`,
			"Couldn't read it from Proxmox just now",
			`<dt>VMID</dt><dd><span class="vm">10101</span></dd>`,
		)
	case <-time.After(10 * time.Second):
		t.Fatal("the cell read carried on after the server shut down")
	}
}

// A VM's page names the newest job that has it, linked, with how it went
// for this VM.
func TestCellPageShowsLastJob(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	lacks(t, "cell page before any job", h.get(&op, "/vm/01/web").Body.String(), "Last job")

	id := h.submitJob(jobs.Inputs{Kind: pods.KindReset, Teams: "1", Snapshot: "initial"}, byOperator)
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"team01-dc": {Status: store.ItemDone}, "team01-web": {Status: store.ItemFailed, Error: "locked"}}})
	contains(t, "failed VM's page", h.get(&op, "/vm/01/web").Body.String(),
		`<h2 class="sub">Last job</h2>`, `<a class="lastjob" href="/logs/`+itoa(id)+`" data-panel-link><span class="st s-fail">`,
		"Reset to snapshot<span class=\"sr\"> failed</span>", "#"+itoa(id)+" · ")
	contains(t, "done VM's page", h.get(&op, "/vm/01/dc").Body.String(), `<span class="st s-ok">`, "Reset to snapshot<span class=\"sr\"> succeeded</span>")

	// A job under way shows as such.
	power := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "reboot"}, byOperator)
	contains(t, "VM with a pending job", h.get(&op, "/vm/01/dc").Body.String(), `href="/logs/`+itoa(power)+`"`, `<span class="st s-wait">`, "Reboot<span class=\"sr\"> pending</span>")
}

// The reset form's snapshot picker narrows the first VM's snapshots to
// those every VM has without touching the cell read it shares with others
// (run with -race, it also catches the two reaching the one list at once).
func TestSnapshotPickerLeavesSharedReadAlone(t *testing.T) {
	h := newHarness(t)
	h.api.setSnapshots(10101, "initial", "extra", "before-scoring")
	h.poll()
	h.api.gate, h.api.entered = make(chan struct{}), make(chan int, 10)
	joined := make(chan struct{}, 10)
	h.srv.afterCellJoin = func() { joined <- struct{}{} }
	ctx := h.ctxAs(asLead)

	picked := make(chan struct{})
	go func() {
		var p snapshotPicker
		h.srv.commonSnapshots(ctx, &p, []string{"team01-dc", "team01-web"})
		close(picked)
	}()
	awaitRead(t, h.api.entered)
	awaitJoin(t, joined)
	seen := make(chan []string, 1)
	go func() {
		d, err := h.srv.detail(ctx, "01", "dc")
		if err != nil {
			t.Error(err)
		}
		snaps := append([]string(nil), d.Snapshots...) // reads them as the picker narrows its copy
		<-picked
		seen <- append(snaps, d.Snapshots...)
	}()
	awaitJoin(t, joined)
	close(h.api.gate)
	got := <-seen
	want := []string{"initial", "extra", "before-scoring"}
	if strings.Join(got, ",") != strings.Join(append(want, want...), ",") {
		t.Errorf("the cell read shared with the picker has snapshots %q, then %q; want %q", got[:3], got[3:], want)
	}
}
