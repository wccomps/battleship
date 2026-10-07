package web

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// fakeAPI is the fake cluster on n1 and n2, plus settable failures and a
// gate on config reads. Fields are guarded by the fake's Mu.
type fakeAPI struct {
	*podstest.Fake
	listErr error
	readErr map[int]error
	// gate, if set, holds each VMConfig call until it receives; entered,
	// if set, gets the VMID of each VMConfig call as it starts.
	gate        chan struct{}
	entered     chan int
	inFlight    int // VMConfig calls under way
	maxInFlight int
	configReads int
}

func (f *fakeAPI) readCounts() (reads, inFlight, maxInFlight int) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.configReads, f.inFlight, f.maxInFlight
}

func newFakeAPI() *fakeAPI {
	f := &fakeAPI{Fake: podstest.New("n1", "n2"), readErr: map[int]error{}}
	f.Gate = f.hold
	return f
}

func (f *fakeAPI) add(vm proxmox.VM, cfg map[string]string, snaps ...string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Add(vm, cfg, snaps...)
}

// remove deletes the VMs with these names, as a teardown would.
func (f *fakeAPI) remove(names ...string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	maps.DeleteFunc(f.VMs, func(_ int, vm *podstest.VM) bool { return slices.Contains(names, vm.Name) })
}

// noTeamVMs removes the harness's starting team VMs (teams 01-03).
func (h *harness) noTeamVMs() {
	var names []string
	for _, team := range []string{"01", "02", "03"} {
		for _, host := range []string{"dc", "web"} {
			names = append(names, "team"+team+"-"+host)
		}
	}
	h.api.remove(names...)
}

func (f *fakeAPI) setStatus(vmid int, s string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if vm, ok := f.VMs[vmid]; ok {
		vm.Status = s
	}
}

func (f *fakeAPI) setListErr(err error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.listErr = err
}

func (f *fakeAPI) setReadErr(vmid int, err error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.readErr[vmid] = err
}

// setSnapshots replaces a VM's snapshots.
func (f *fakeAPI) setSnapshots(vmid int, snaps ...string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.VMs[vmid].Snapshots = snaps
}

// hold is the fake's Gate: listings fail with listErr; config reads are
// counted, reported on entered, held by gate, then fail with readErr.
func (f *fakeAPI) hold(ctx context.Context, key string) (func(), error) {
	if key == "cluster" {
		f.Mu.Lock()
		defer f.Mu.Unlock()
		return nil, f.listErr
	}
	id, ok := strings.CutPrefix(key, "config:")
	if !ok {
		return nil, nil
	}
	vmid, _ := strconv.Atoi(id)
	f.Mu.Lock()
	f.configReads++
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	gate, entered := f.gate, f.entered
	f.Mu.Unlock()
	done := func() {
		f.Mu.Lock()
		f.inFlight--
		f.Mu.Unlock()
	}
	if entered != nil {
		entered <- vmid
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return done, ctx.Err()
		}
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return done, f.readErr[vmid]
}

// fakeHistory answers LastItemResults from a map, with no clock skew.
type fakeHistory struct {
	mu      sync.Mutex
	results map[string]store.ItemResult
	clock   *fakeClock
}

func (h *fakeHistory) Now(context.Context) (time.Time, error) { return h.clock.Now(), nil }

func (h *fakeHistory) set(name string, r store.ItemResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.results == nil {
		h.results = map[string]store.ItemResult{}
	}
	h.results[name] = r
}

func (h *fakeHistory) LastItemResults(_ context.Context, names, _ []string) (map[string]store.ItemResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]store.ItemResult{}
	for _, n := range names {
		if r, ok := h.results[n]; ok {
			out[n] = r
		}
	}
	return out, nil
}

// fakeClock only moves when Advance is called. BlockUntil waits, without
// sleeping, until goroutines are waiting on it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []clockWaiter
	changed chan struct{} // closed and replaced whenever a waiter is added
}

type clockWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), changed: make(chan struct{})}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, clockWaiter{at: c.now.Add(d), ch: ch})
	close(c.changed)
	c.changed = make(chan struct{})
	return ch
}

// Waits returns how far in the future each pending waiter fires, soonest
// first.
func (c *fakeClock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.waiters))
	for i, w := range c.waiters {
		out[i] = w.at.Sub(c.now)
	}
	slices.Sort(out)
	return out
}

// Advance moves the clock and fires the waiters that are due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

// BlockUntil waits until n waiters are pending (10s hang guard).
func (c *fakeClock) BlockUntil(t testing.TB, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		have, changed := len(c.waiters), c.changed
		c.mu.Unlock()
		if have >= n {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("clock: %d waiters after 10s, want %d", have, n)
		}
	}
}

// logBuffer collects log lines.
type logBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuffer) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// cleanConfig is a converged two-NIC team VM config.
func cleanConfig(team string) map[string]string {
	return map[string]string{
		"name":  "team" + team + "-x",
		"net0":  "virtio=BC:24:11:00:00:01,bridge=ext" + team,
		"net1":  "virtio=BC:24:11:00:00:02,bridge=int" + team,
		"scsi0": "competitions:base-9005-disk-0/vm-1-disk-0,mbps_rd=300,mbps_wr=300,size=32G",
		"ide2":  "none,media=cdrom",
	}
}

// teamVM is a running team VM in its team's pool.
func teamVM(team, host string, vmid int) proxmox.VM {
	return proxmox.VM{VMID: vmid, Name: "team" + team + "-" + host, Node: "n1", Status: "running", Pool: "pool-" + team}
}

// harness is the web app over the test database, a fake cluster read by a
// real status poller on a fake clock, and a publish-only hub.
type harness struct {
	t     *testing.T
	cfg   config.Config
	st    *store.Store
	api   *fakeAPI
	hist  *fakeHistory
	clock *fakeClock
	hub   *status.Hub
	views *status.Views
	// poller polls, scans and reads every viewer's grid (see allViews).
	poller allViews
	srv    *Server
	h      http.Handler
	logs   *logBuffer
	stop   context.CancelFunc // stops the hub
	// dbURL is for tests that break the database.
	dbURL string
	// pve is Proxmox's access layer: users, their tickets and privileges.
	// The cluster itself is api.
	pve *pvetest.Server
	// creds seals the credentials of confirmed jobs.
	creds jobs.Credentials

	credMu sync.Mutex
	used   []proxmox.Credential // the credential of every API the app asked for
	// polled and scanned let views opened later catch up (catchUp).
	polled, scanned bool
}

// openView opens cred's view, as a first page would, and catches it up.
func (h *harness) openView(cred proxmox.Credential) {
	v, release := h.views.Open(cred.User, cred)
	h.t.Cleanup(release)
	h.credMu.Lock()
	polled, scanned := h.polled, h.scanned
	h.credMu.Unlock()
	if polled && v.Grid().PolledAt.IsZero() {
		_ = v.Poll(context.Background())
		if scanned {
			_ = v.Scan(context.Background())
		}
	}
}

// as is Deps.As: the fake cluster, seen as cred. It records cred.
func (h *harness) as(cred proxmox.Credential) pods.API {
	h.credMu.Lock()
	h.used = append(h.used, cred)
	h.credMu.Unlock()
	return h.bind(func() proxmox.Credential { return cred })
}

// bind is the fake cluster seen through the credential cred returns: its
// privileges are the pvetest user's.
func (h *harness) bind(cred func() proxmox.Credential) pods.API {
	return permAPI{fakeAPI: h.api, cred: cred, pve: h.pve}
}

// permAPI is the fake cluster that also answers the caller's privileges,
// from pvetest.
type permAPI struct {
	*fakeAPI
	cred func() proxmox.Credential
	pve  *pvetest.Server
}

// ClusterVMs lists only the VMs the caller has VM.Audit on, as Proxmox
// does.
func (p permAPI) ClusterVMs(ctx context.Context) ([]proxmox.VM, error) {
	vms, err := p.fakeAPI.ClusterVMs(ctx)
	if err != nil {
		return nil, err
	}
	user := p.cred().User
	return slices.DeleteFunc(vms, func(vm proxmox.VM) bool {
		return !p.pve.Allowed(user, "/vms/"+strconv.Itoa(vm.VMID), "VM.Audit")
	}), nil
}

func (p permAPI) Permissions(ctx context.Context) (proxmox.Permissions, error) {
	return p.pve.Client().As(p.cred()).Permissions(ctx)
}

func (p permAPI) PermissionsAt(ctx context.Context, path string) (map[string]bool, error) {
	return p.pve.Client().As(p.cred()).PermissionsAt(ctx, path)
}

// allViews is every view the test's sessions opened. Later views catch up,
// so a test can poll before anyone logs in.
type allViews struct{ h *harness }

// Poll polls every view, returning the first error.
func (a allViews) Poll(ctx context.Context) error {
	a.h.credMu.Lock()
	a.h.polled = true
	a.h.credMu.Unlock()
	var first error
	a.h.views.Each(func(v *status.View) {
		if err := v.Poll(ctx); err != nil && first == nil {
			first = err
		}
	})
	return first
}

// Scan scans every view, returning the first error.
func (a allViews) Scan(ctx context.Context) error {
	a.h.credMu.Lock()
	a.h.scanned = true
	a.h.credMu.Unlock()
	var first error
	a.h.views.Each(func(v *status.View) {
		if err := v.Scan(ctx); err != nil && first == nil {
			first = err
		}
	})
	return first
}

// Grid is the first view's grid by user name (the lead's, if signed in).
func (a allViews) Grid() status.Grid {
	var first *status.View
	a.h.views.Each(func(v *status.View) {
		if first == nil || v.User < first.User {
			first = v
		}
	})
	if first == nil {
		return status.Grid{}
	}
	return first.Grid()
}

// usedCreds lists the credentials the app read the cluster with.
func (h *harness) usedCreds() []proxmox.Credential {
	h.credMu.Lock()
	defer h.credMu.Unlock()
	return slices.Clone(h.used)
}

// testSealKey seals the harness's job credentials.
const testSealKey = "a seal key for the web tests, long enough"

// testConfig is the default config at http://battleship.test.
func testConfig() config.Config {
	cfg := config.Default()
	cfg.Web.BaseURL = "http://battleship.test"
	return cfg
}

// newHarness builds the app over a cluster where teams 01-03 each have a
// clean, running dc and web, not yet polled.
func newHarness(t *testing.T, mut ...func(*config.Config)) *harness {
	t.Helper()
	return buildHarness(t, false, mut...)
}

// newListeningHarness is newHarness with a hub listening on the test
// database, as serve's does. It returns once the hub listens.
func newListeningHarness(t *testing.T, mut ...func(*config.Config)) *harness {
	t.Helper()
	h := buildHarness(t, true, mut...)
	// The first connection's resync takes a sequence number.
	for deadline := time.Now().Add(10 * time.Second); h.hub.Seq() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hub didn't listen within 10s")
		}
	}
	return h
}

func buildHarness(t *testing.T, listen bool, mut ...func(*config.Config)) *harness {
	t.Helper()
	cfg := testConfig()
	for _, m := range mut {
		m(&cfg)
	}
	authtest.StubProvider(t, &cfg)
	h := &harness{t: t, cfg: cfg, api: newFakeAPI(), clock: newFakeClock(), logs: &logBuffer{}}
	h.hist = &fakeHistory{clock: h.clock}
	for i, team := range []string{"01", "02", "03"} {
		for j, host := range []string{"dc", "web"} {
			h.api.add(teamVM(team, host, 10000+(i+1)*100+j+1), cleanConfig(team), "initial", "before-scoring")
		}
	}
	h.st, h.dbURL = storetest.NewWithURL(t)
	h.pve = pvetest.New(t)
	h.pve.SetClock(h.clock.Now)
	cfg.Proxmox.URL = h.pve.URL()
	cfg.Database.SealKey = testSealKey
	h.cfg = cfg
	creds, err := jobs.NewCredentials(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.creds = creds
	svc, err := auth.NewService(context.Background(), cfg, h.st, auth.Options{Logf: h.logs.Logf, Now: h.clock.Now, Proxmox: h.pve.Client()})
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	var lf status.ListenFunc
	if listen {
		lf = h.st.Notifications
	}
	h.hub = status.NewHub(lf, status.HubOptions{Logf: h.logs.Logf})
	ctx, stop := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() { h.hub.Run(ctx); close(hubDone) }()
	h.stop = stop
	t.Cleanup(func() { stop(); <-hubDone })
	// The test drives each viewer's grid: h.poll and h.poller poll them.
	h.views, err = status.NewViews(h.bind, h.hist, apply.NewLimits(cfg.Concurrency), cfg,
		status.Options{Clock: h.clock, Hub: h.hub, Logf: h.logs.Logf, Manual: true}, time.Minute)
	if err != nil {
		t.Fatalf("status.NewViews: %v", err)
	}
	t.Cleanup(h.views.Close)
	h.poller = allViews{h}
	// The lead's and operator's grids exist up front so a test can poll
	// before signing anyone in.
	for _, p := range []persona{asLead, asOperator} {
		h.openView(h.ticketWith("test-"+string(p), personaPrivileges[p]))
	}
	h.srv, err = New(Deps{
		Config: cfg, Store: h.st, As: h.as, Credentials: h.creds, Auth: svc, Views: h.views, Hub: h.hub,
		Clock: h.clock, Logf: h.logs.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.srv.jitter = func() float64 { return 0 } // streams end at session_refresh exactly
	h.srv.firstPoll = 0                        // views are polled by the test
	h.h = h.srv.Handler()
	return h
}

// longSessions lets sessions survive an hour of fake-clock moves: the stub
// provider rejects session refreshes, which would end the session.
func longSessions(c *config.Config) {
	c.Web.SessionIdle = 2 * time.Hour
	c.Web.SessionRefresh = time.Hour
}

// poll reads the fake cluster once, failing the test on a poll error.
func (h *harness) poll() {
	h.t.Helper()
	if err := h.poller.Poll(context.Background()); err != nil {
		h.t.Fatalf("Poll: %v", err)
	}
}

// login stores a session for a user with role.
func (h *harness) login(p persona) authtest.Session {
	h.t.Helper()
	subject := "test-" + string(p)
	cred := h.ticketWith(subject, personaPrivileges[p])
	sess := authtest.Login(h.t, h.st, h.cfg, authtest.User{Subject: subject, Name: "Test " + string(p), At: h.clock.Now(), Proxmox: cred})
	h.openView(cred) // their grid, which h.poll polls
	return sess
}

// persona is a kind of user, defined by their Proxmox privileges on /.
type persona string

const (
	asLead     persona = "lead"     // everything
	asOperator persona = "operator" // see, power, snapshot and roll back
	asNobody   persona = "nobody"   // nothing, not even see a VM
)

var personaPrivileges = map[persona][]string{
	asLead:     allPrivileges,
	asOperator: {"VM.Audit", "VM.PowerMgmt", "VM.Snapshot", "VM.Snapshot.Rollback", "Pool.Audit", "Sys.Audit", "Datastore.Audit"},
}

// ticket is a fresh ticket for <subject>@auth.example.org, created on first
// use with every privilege on /.
func (h *harness) ticket(subject string) proxmox.Credential {
	h.t.Helper()
	return h.ticketWith(subject, allPrivileges)
}

// ticketWith is ticket for a user created with privs on /.
func (h *harness) ticketWith(subject string, privs []string) proxmox.Credential {
	h.t.Helper()
	user := subject + "@" + pvetest.Realm
	if !h.pve.Known(user) {
		h.pve.AddUser(user, nil)
		if len(privs) > 0 {
			h.pve.Grant(user, "/", privs...)
		}
	}
	return h.pve.Ticket(user)
}

// allPrivileges are the privileges an administrator holds.
var allPrivileges = []string{"VM.Audit", "VM.PowerMgmt", "VM.Snapshot", "VM.Snapshot.Rollback", "VM.Clone", "VM.Allocate",
	"VM.Config.Network", "VM.Config.Disk", "VM.Config.Cloudinit", "VM.Config.CDROM", "VM.Config.Options", "Datastore.AllocateSpace",
	"Datastore.Allocate", "Datastore.Audit", "Pool.Audit", "Pool.Allocate", "Sys.Audit", "Sys.Modify", "SDN.Use", "Mapping.Use"}

// loginStudent stores a session for a team 01 member (no role).
func (h *harness) loginStudent() authtest.Session {
	h.t.Helper()
	cred := h.ticketWith("test-student", nil)
	sess := authtest.Login(h.t, h.st, h.cfg, authtest.User{Subject: "test-student", Groups: []string{"pool-01"}, At: h.clock.Now(), Proxmox: cred})
	h.openView(cred)
	return sess
}

// do serves one request as sess (nil: no cookie or CSRF token).
func (h *harness) do(sess *authtest.Session, method, target string, body io.Reader) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if sess != nil {
		sess.Apply(req)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

// get serves a GET as sess (nil: anonymous).
func (h *harness) get(sess *authtest.Session, target string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(sess, http.MethodGet, target, nil)
}

// sseEvent is one server-sent event, or one comment.
type sseEvent struct {
	Event, ID, Data string
	Comment         string
}

// sseClient reads a server-sent event stream.
type sseClient struct {
	t    testing.TB
	resp *http.Response
	rd   *bufio.Reader
	evs  chan sseEvent
	done chan struct{} // closed at the end of the stream
}

// openSSE streams from a real HTTP server, so events arrive as flushed.
func (h *harness) openSSE(sess *authtest.Session, path string) *sseClient {
	h.t.Helper()
	return h.openSSEWith(sess, path, nil)
}

// openSSEWith is openSSE with extra request headers, e.g. Last-Event-ID.
func (h *harness) openSSEWith(sess *authtest.Session, path string, header http.Header) *sseClient {
	h.t.Helper()
	srv := httptest.NewServer(h.h)
	h.t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if sess != nil {
		sess.Apply(req)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	c := &sseClient{t: h.t, resp: resp, rd: bufio.NewReader(resp.Body), evs: make(chan sseEvent, 100), done: make(chan struct{})}
	go c.read()
	return c
}

func (c *sseClient) read() {
	defer close(c.done)
	var ev sseEvent
	var data []string
	for {
		line, err := c.rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if ev.Event != "" || len(data) > 0 {
				ev.Data = strings.Join(data, "\n")
				c.evs <- ev
			}
			ev, data = sseEvent{}, nil
		case strings.HasPrefix(line, ":"):
			c.evs <- sseEvent{Comment: strings.TrimSpace(strings.TrimPrefix(line, ":"))}
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				ev.Event = value
			case "id":
				ev.ID = value
			case "data":
				data = append(data, value)
			}
		}
	}
}

// next returns the next event or comment (10s hang guard).
func (c *sseClient) next() sseEvent {
	c.t.Helper()
	select {
	case ev := <-c.evs:
		return ev
	case <-c.done:
		select {
		case ev := <-c.evs:
			return ev
		default:
		}
		c.t.Fatal("the stream ended, want an event")
	case <-time.After(10 * time.Second):
		c.t.Fatal("no event after 10s")
	}
	return sseEvent{}
}

// ended waits for the stream to end, failing on any event (10s hang guard).
func (c *sseClient) ended() {
	c.t.Helper()
	select {
	case ev := <-c.evs:
		c.t.Fatalf("got %+v, want the stream to end", ev)
	case <-c.done:
	case <-time.After(10 * time.Second):
		c.t.Fatal("the stream is still open after 10s")
	}
}

// contains fails the test unless body has every one of wants.
func contains(t testing.TB, what, body string, wants ...string) {
	t.Helper()
	missing := false
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s does not contain %q", what, w)
			missing = true
		}
	}
	if missing {
		t.Logf("%s:\n%s", what, body)
	}
}

// lacks fails the test if body has any of unwanted.
func lacks(t testing.TB, what, body string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(body, u) {
			t.Errorf("%s contains %q", what, u)
		}
	}
}

// ctxAs is a signed-in p user's request context, for calling handler
// helpers directly.
func (h *harness) ctxAs(p persona) context.Context {
	ctx := auth.WithProxmoxCredential(context.Background(), h.ticketWith("test-"+string(p), personaPrivileges[p]))
	rv := &requestView{} // as require gives a request
	h.t.Cleanup(rv.release)
	return context.WithValue(ctx, requestViewKey{}, rv)
}

// gridOf is the grid a p user sees.
func (h *harness) gridOf(p persona) status.Grid {
	cred := h.ticketWith("test-"+string(p), personaPrivileges[p])
	v, release := h.views.Open(cred.User, cred)
	defer release()
	return v.Grid()
}
