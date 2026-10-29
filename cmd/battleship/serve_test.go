package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// serveAPI is the CLI fake plus a listing that can fail, holdable task waits
// and Proxmox's sign-in. Its fields are guarded by the fake's Mu.
type serveAPI struct {
	*fakeAPI
	powered chan int // gets the VMID of every power call

	down    error         // non-nil: listing the cluster fails with it
	gate    chan struct{} // non-nil: WaitTask blocks until it closes (or ctx ends)
	hang    bool          // WaitTask ignores ctx too, like a Proxmox call that hangs
	waiting chan struct{} // gets a value each time WaitTask starts waiting at the gate

	// login is the Proxmox access layer (pvetest) the fake signs users in
	// and renews tickets through, as the real client does.
	login auth.ProxmoxLogin

	binds []bound
}

type bound struct {
	user    string
	refused bool
}

func (a *serveAPI) OpenIDAuthURL(ctx context.Context, realm, redirectURL string) (string, string, error) {
	return a.login.OpenIDAuthURL(ctx, realm, redirectURL)
}

func (a *serveAPI) OpenIDLogin(ctx context.Context, endpoint, code, state, redirectURL string, now time.Time) (proxmox.Credential, error) {
	return a.login.OpenIDLogin(ctx, endpoint, code, state, redirectURL, now)
}

func (a *serveAPI) RenewTicket(ctx context.Context, cred proxmox.Credential, now time.Time) (proxmox.Credential, error) {
	return a.login.RenewTicket(ctx, cred, now)
}

// Bind records each binding and returns the fake, which checks no credential.
func (a *serveAPI) Bind(cred func() proxmox.Credential, refused func()) pods.API {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.binds = append(a.binds, bound{user: cred().User, refused: refused != nil})
	return a
}

func newServeAPI(vms ...proxmox.VM) *serveAPI {
	a := &serveAPI{
		fakeAPI: newFakeAPI(vms...),
		powered: make(chan int, 100),
		waiting: make(chan struct{}, 100),
	}
	cli := a.Gate
	a.Gate = func(ctx context.Context, key string) (func(), error) {
		a.Mu.Lock()
		down := a.down
		a.Mu.Unlock()
		if key == "cluster" && down != nil {
			return nil, down
		}
		return cli(ctx, key)
	}
	a.PowerHook = func(vmid int, _ string) { a.powered <- vmid }
	a.WaitGate = a.waitGate
	return a
}

// hold makes task waits block until the returned func is called.
func (a *serveAPI) hold(hang bool) (release func()) {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.gate, a.hang = make(chan struct{}), hang
	gate := a.gate
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

func (a *serveAPI) waitGate(ctx context.Context, _ string) error {
	a.Mu.Lock()
	gate, hang := a.gate, a.hang
	a.Mu.Unlock()
	if gate == nil {
		return nil
	}
	a.waiting <- struct{}{}
	if hang {
		<-gate
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// serveEnv is battleship serve running in this process on a random local port,
// against the test database, a fake Proxmox and a test identity provider.
type serveEnv struct {
	t    *testing.T
	cfg  config.Config
	st   *store.Store
	api  *serveAPI
	url  string
	logs *syncBuffer
	http *http.Client
	idp  *authtest.Provider // withLogin only
	pve  *pvetest.Server    // Proxmox's sign-in and tickets

	stop func() // the stop signal
	done chan error
}

// serveOption changes how a serveEnv is set up before battleship serve starts.
type serveOption func(*serveSetup)

type serveSetup struct {
	st     *store.Store
	cfg    *config.Config
	deps   *serveDeps
	login  bool            // a login-capable provider instead of the stub
	parent context.Context // also stops serve when done; nil means none
}

// withStopReserve replaces how long before web.shutdown_timeout runs out
// serve stops running jobs.
func withStopReserve(d time.Duration) serveOption {
	return func(s *serveSetup) { s.deps.stopReserve = d }
}

// underContext also stops battleship serve when ctx is done.
func underContext(ctx context.Context) serveOption {
	return func(s *serveSetup) { s.parent = ctx }
}

// withConfig changes the config battleship serve gets.
func withConfig(mut func(*config.Config)) serveOption {
	return func(s *serveSetup) { mut(s.cfg) }
}

// onStore runs battleship serve on st instead of a new test database.
func onStore(st *store.Store) serveOption {
	return func(s *serveSetup) { s.st = st }
}

// withLogin gives battleship serve an identity provider people can log in
// through (env.idp), instead of the stub that only answers discovery.
func withLogin() serveOption {
	return func(s *serveSetup) { s.login = true }
}

// withDrain replaces the wait between failing readiness and stopping HTTP.
func withDrain(drain func()) serveOption {
	return func(s *serveSetup) { s.deps.drain = drain }
}

// serveConfig is a config battleship serve accepts, apart from [oidc] and the
// base URL, which startServe fills in.
func serveConfig() config.Config {
	cfg := config.Default()
	cfg.Proxmox.URL = "https://pve.example:8006"
	cfg.Proxmox.InsecureSkipVerify = true // the fake Proxmox has no certificate to check
	cfg.Database.URL = "postgres://battleship:database-password@db.example/battleship"
	cfg.Database.SealKey = testSealKey
	cfg.Retry.Attempts, cfg.Retry.Rounds = 1, 0
	cfg.Jobs.Poll = 50 * time.Millisecond
	cfg.Web.StatusPoll = time.Second
	cfg.Web.ShutdownTimeout = 30 * time.Second
	return cfg
}

func teamPodVMs() []proxmox.VM {
	return []proxmox.VM{
		{VMID: 10101, Name: "team01-dc", Node: "n1", Status: "stopped", Pool: "pool-01"},
		{VMID: 10102, Name: "team01-web", Node: "n1", Status: "stopped", Pool: "pool-01"},
		{VMID: 10201, Name: "team02-dc", Node: "n1", Status: "stopped", Pool: "pool-02"},
	}
}

// startServe starts battleship serve and stops it when the test ends.
func startServe(t *testing.T, opts ...serveOption) *serveEnv {
	t.Helper()
	cfg := serveConfig()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &serveEnv{t: t, api: newServeAPI(teamPodVMs()...), url: "http://" + ln.Addr().String(), logs: &syncBuffer{}, done: make(chan error, 1)}
	cfg.Web.BaseURL = e.url
	cfg.Web.Listen = ln.Addr().String()
	d := serveDeps{
		newCluster: func(config.Proxmox, func(string, ...any)) cluster { return e.api },
		openStore: func(context.Context, config.Database) (*store.Store, func(), error) {
			return e.st, func() {}, nil
		},
		listen: func(network, addr string) (net.Listener, error) {
			if network != "tcp" || addr != cfg.Web.Listen {
				return nil, errors.New("listening on an unexpected address " + addr)
			}
			return ln, nil
		},
		log:   slog.New(slog.NewJSONHandler(e.logs, nil)),
		drain: func() {},
	}
	setup := &serveSetup{cfg: &cfg, deps: &d}
	for _, o := range opts {
		o(setup)
	}
	if setup.st == nil {
		setup.st = storetest.New(t)
	}
	e.st = setup.st
	e.pve = pvetest.New(t)
	e.api.login = e.pve.Client()
	if setup.login {
		e.idp = authtest.NewProvider(t, &cfg)
	} else {
		authtest.StubProvider(t, &cfg)
	}
	e.cfg = cfg

	if setup.parent == nil {
		setup.parent = context.Background()
	}
	ctx, cancel := context.WithCancel(setup.parent)
	e.stop = cancel
	go func() { e.done <- serve(ctx, cfg, d) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(time.Minute):
			t.Error("battleship serve didn't stop")
		}
	})
	e.http = &http.Client{Timeout: 10 * time.Second}
	waitFor(t, "battleship serve to answer /healthz", func() bool {
		code, _ := e.get("/healthz")
		return code == http.StatusOK
	})
	return e
}

// get fetches path without following redirects.
func (e *serveEnv) get(path string) (int, string) {
	req, err := http.NewRequest(http.MethodGet, e.url+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	c := *e.http
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// shutdown sends the stop signal and waits for serve to return, failing the
// test if that takes longer than within.
func (e *serveEnv) shutdown(within time.Duration) (error, time.Duration) {
	e.t.Helper()
	start := time.Now()
	e.stop()
	select {
	case err := <-e.done:
		e.done <- err // for the cleanup
		return err, time.Since(start)
	case <-time.After(within):
		e.t.Fatalf("battleship serve didn't return within %s of the stop signal", within)
		return nil, 0
	}
}

// ready waits for /readyz to pass: the database answers.
func (e *serveEnv) ready() {
	e.t.Helper()
	waitFor(e.t, "/readyz to pass", func() bool {
		code, body := e.get("/readyz")
		return code == http.StatusOK && body == "ready\n"
	})
}

// submit previews in against the fake cluster and queues it, as the web
// app's confirm does.
func (e *serveEnv) submit(in jobs.Inputs) int64 {
	e.t.Helper()
	plan, err := jobs.BuildPlan(context.Background(), pods.NewPlanner(e.api, e.cfg), in)
	if err != nil {
		e.t.Fatal(err)
	}
	creds, err := jobs.NewCredentials(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	now := time.Now()
	ticket := proxmox.TicketCredential("alice@auth.example.org", "PVE:alice", "csrf", now)
	ticket.LoginAt = now
	id, err := jobs.Submit(context.Background(), e.st, in, plan, jobs.Submitter{User: "alice@example.org", Credential: ticket, Seal: creds})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *serveEnv) job(id int64) store.Job {
	e.t.Helper()
	j, err := e.st.Job(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// records parses the JSON log lines that have msg.
func (e *serveEnv) records(msg string) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(e.logs.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			e.t.Fatalf("log line isn't JSON: %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func TestServeHealthAndReadiness(t *testing.T) {
	e := startServe(t)
	if code, body := e.get("/healthz"); code != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Errorf("/healthz = %d %q", code, body)
	}
	e.ready()
	if code, body := e.get("/readyz"); code != http.StatusOK || body != "ready\n" {
		t.Errorf("/readyz = %d %q", code, body)
	}
	serving := e.records("serving")
	if len(serving) != 1 || serving[0]["listen"] != e.cfg.Web.Listen || serving[0]["workers"] != float64(2) {
		t.Errorf("serving records = %v, want one naming the address and 2 workers", serving)
	}
	if code, _ := e.get("/"); code != http.StatusSeeOther {
		t.Errorf("anonymous grid = %d, want a redirect to log in", code)
	}
}

// Readiness never calls Proxmox (serve holds no credential), so a Proxmox
// outage can't take every replica out of the load balancer.
func TestServeStaysReadyWithoutProxmox(t *testing.T) {
	e := startServe(t)
	e.api.Mu.Lock()
	e.api.down = &proxmox.APIError{Status: 595, Message: "no route to host"}
	e.api.Mu.Unlock()
	e.ready()
	if code, body := e.get("/readyz"); code != http.StatusOK || body != "ready\n" {
		t.Errorf("/readyz with Proxmox down = %d %q, want 200 ready", code, body)
	}
}

func TestServeRunsQueuedJobs(t *testing.T) {
	e := startServe(t)
	id := e.submit(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "start"})
	waitFor(t, "the job to finish", func() bool { return !e.job(id).Active() })
	j := e.job(id)
	if j.Status != store.StatusSucceeded || !isWorkerID(j.ClaimedBy, "serve") {
		t.Fatalf("job = %s (%s), claimed by %q; want succeeded by a serve worker", j.Status, j.Error, j.ClaimedBy)
	}
	if got := signalled(t, e.api.powered, "a power call"); got != 10101 {
		t.Errorf("powered VM %d, want 10101", got)
	}
	e.api.Mu.Lock()
	binds := slices.Clone(e.api.binds)
	e.api.Mu.Unlock()
	if !slices.Contains(binds, bound{user: "alice@auth.example.org", refused: true}) {
		t.Errorf("binds = %v, want the job bound to its submitter's ticket, reporting refusal", binds)
	}
	workers := map[any]bool{}
	for _, r := range e.records("worker started") {
		workers[r["worker"]] = true
	}
	if len(workers) != 2 || !workers[j.ClaimedBy] {
		t.Errorf("worker started records name %v, want 2 workers including %s", workers, j.ClaimedBy)
	}
	waitFor(t, "the finish to be logged", func() bool { return len(e.records("job finished")) == 1 })
	fin := e.records("job finished")[0]
	if fin["job"] != float64(id) || fin["status"] != store.StatusSucceeded || fin["worker"] != j.ClaimedBy {
		t.Errorf("job finished record = %v", fin)
	}
}

func TestServeShutdownInterruptsRunningJob(t *testing.T) {
	e := startServe(t)
	release := e.api.hold(false)
	defer release()
	id := e.submit(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "start"})
	signalled(t, e.api.waiting, "the job to wait for its Proxmox task")

	err, took := e.shutdown(20 * time.Second)
	if err != nil {
		t.Fatalf("serve = %v, want a clean stop", err)
	}
	if took >= e.cfg.Web.ShutdownTimeout {
		t.Errorf("shutdown took %s, want less than web.shutdown_timeout (%s)", took, e.cfg.Web.ShutdownTimeout)
	}
	j := e.job(id)
	if j.Status != store.StatusInterrupted || !strings.Contains(j.Error, "shut down") {
		t.Errorf("job = %s (%s), want interrupted by the shutdown", j.Status, j.Error)
	}
	items, err := e.st.Items(context.Background(), id)
	if err != nil || len(items) != 1 || items[0].Status != store.ItemInterrupted {
		t.Errorf("items = %+v, %v; want team01-dc interrupted", items, err)
	}
	if recs := e.records("workers stopped"); len(recs) != 1 {
		t.Errorf("workers stopped records = %v, want 1", recs)
	}
}

func TestServeShutdownGivesUpAfterTimeout(t *testing.T) {
	e := startServe(t, withConfig(func(c *config.Config) { c.Web.ShutdownTimeout = time.Second }))
	release := e.api.hold(true) // the task wait ignores the shutdown
	id := e.submit(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "start"})
	signalled(t, e.api.waiting, "the job to wait for its Proxmox task")

	err, took := e.shutdown(20 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "web.shutdown_timeout (1s)") {
		t.Errorf("serve = %v, want it to give up at web.shutdown_timeout", err)
	}
	if took < time.Second || took > 10*time.Second {
		t.Errorf("shutdown took %s, want about web.shutdown_timeout (1s)", took)
	}
	// Let the abandoned job end, so it doesn't outlive the test's database.
	release()
	waitFor(t, "the abandoned job to end", func() bool { return !e.job(id).Active() })
}

// stopped waits for serve to return after e.stop, failing the test if that
// takes longer than within.
func (e *serveEnv) stopped(within time.Duration) error {
	e.t.Helper()
	select {
	case err := <-e.done:
		e.done <- err // for the cleanup
		return err
	case <-time.After(within):
		e.t.Fatalf("battleship serve didn't return within %s", within)
		return nil
	}
}

// logged waits for a log line with msg.
func (e *serveEnv) logged(msg string) {
	e.t.Helper()
	waitFor(e.t, "a "+msg+" log line", func() bool { return len(e.records(msg)) > 0 })
}

// heartbeatAt reads job id's heartbeat stamp straight from the database.
func heartbeatAt(t *testing.T, dbURL string, id int64) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var at time.Time
	if err := conn.QueryRow(ctx, `SELECT heartbeat_at FROM jobs WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// A shutdown far from its deadline lets a running job finish as it would
// have, and logs that it finished during the shutdown.
func TestServeShutdownLetsRunningJobFinish(t *testing.T) {
	e := startServe(t, withStopReserve(time.Second))
	release := e.api.hold(false)
	defer release()
	id := e.submit(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "start"})
	signalled(t, e.api.waiting, "the job to wait for its Proxmox task")

	e.stop()
	e.logged("shutting down")
	if j := e.job(id); j.Status != store.StatusRunning {
		t.Fatalf("job = %s (%s) once shutting down, want still running", j.Status, j.Error)
	}
	release()
	if err := e.stopped(20 * time.Second); err != nil {
		t.Fatalf("serve = %v, want a clean stop", err)
	}
	if j := e.job(id); j.Status != store.StatusSucceeded {
		t.Errorf("job = %s (%s), want succeeded: the shutdown let it finish", j.Status, j.Error)
	}
	fin := e.records("job finished during shutdown")
	if len(fin) != 1 || fin[0]["job"] != float64(id) || fin[0]["status"] != store.StatusSucceeded {
		t.Errorf("job finished during shutdown records = %v, want job %d succeeded", fin, id)
	}
	if recs := e.records("shutdown deadline near: stopping running jobs so they clean up and end interrupted before it"); len(recs) != 0 {
		t.Errorf("deadline records = %v, want none: the job finished first", recs)
	}
}

// Once shutting down, serve's workers claim nothing new, even while one of
// them still runs a job; another replica takes the queue.
func TestServeShutdownClaimsNoNewJobs(t *testing.T) {
	e := startServe(t, withStopReserve(time.Second))
	release := e.api.hold(false)
	defer release()
	running := e.submit(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "start"})
	signalled(t, e.api.waiting, "the job to wait for its Proxmox task")

	e.stop()
	e.logged("shutting down")
	queued := e.submit(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Hosts: []string{"dc"}, Action: "start"})
	// The idle worker would claim it within a poll (50ms) or the hub's wake.
	time.Sleep(time.Second)
	if j := e.job(queued); j.Status != store.StatusPending || j.ClaimedBy != "" {
		t.Fatalf("job queued during shutdown = %s, claimed by %q; want still pending for another replica", j.Status, j.ClaimedBy)
	}
	release()
	if err := e.stopped(20 * time.Second); err != nil {
		t.Fatalf("serve = %v, want a clean stop", err)
	}
	if j := e.job(running); j.Status != store.StatusSucceeded {
		t.Errorf("running job = %s (%s), want succeeded", j.Status, j.Error)
	}
	if j := e.job(queued); j.Status != store.StatusPending || j.ClaimedBy != "" {
		t.Errorf("job queued during shutdown = %s, claimed by %q once serve stopped; want still pending", j.Status, j.ClaimedBy)
	}
}

// A job still running when the deadline nears is stopped then, so it
// cleans up and ends interrupted before serve gives up.
func TestServeShutdownStopsJobsNearDeadline(t *testing.T) {
	e := startServe(t, withConfig(func(c *config.Config) { c.Web.ShutdownTimeout = 4 * time.Second }), withStopReserve(3*time.Second))
	release := e.api.hold(false)
	defer release()
	id := e.submit(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "start"})
	signalled(t, e.api.waiting, "the job to wait for its Proxmox task")

	err, took := e.shutdown(20 * time.Second)
	if err != nil {
		t.Fatalf("serve = %v, want a clean stop before web.shutdown_timeout", err)
	}
	if took < time.Second || took >= 4*time.Second {
		t.Errorf("shutdown took %s, want from 1s (the deadline less the reserve) to under 4s (the deadline)", took)
	}
	j := e.job(id)
	if j.Status != store.StatusInterrupted || !strings.Contains(j.Error, "shut down") {
		t.Errorf("job = %s (%s), want interrupted by the shutdown", j.Status, j.Error)
	}
	if recs := e.records("shutdown deadline near: stopping running jobs so they clean up and end interrupted before it"); len(recs) != 1 {
		t.Errorf("deadline records = %v, want 1", recs)
	}
	if fin := e.records("job finished during shutdown"); len(fin) != 1 || fin[0]["status"] != store.StatusInterrupted {
		t.Errorf("job finished during shutdown records = %v, want it interrupted", fin)
	}
}

// While a stopping replica finishes its job, it keeps heartbeating, so the
// other replica's reaper leaves the job be; that replica runs new jobs.
func TestServeShutdownKeepsJobClaimedWhileFinishing(t *testing.T) {
	st, dbURL := storetest.NewWithURL(t)
	fast := withConfig(func(c *config.Config) { c.Jobs.Heartbeat, c.Jobs.StaleAfter = 200*time.Millisecond, 2*time.Second })
	a := startServe(t, onStore(st), fast, withStopReserve(time.Second))
	release := a.api.hold(false)
	defer release()
	id := a.submit(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "start"})
	signalled(t, a.api.waiting, "the job to wait for its Proxmox task")
	b := startServe(t, onStore(st), fast)

	a.stop()
	a.logged("shutting down")
	claimer := a.job(id).ClaimedBy
	// Three stale_afters: the other replica reaps a silent job within one.
	last := heartbeatAt(t, dbURL, id)
	beats := 0
	for end := time.Now().Add(6 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		j := a.job(id)
		if j.Status != store.StatusRunning || j.ClaimedBy != claimer {
			t.Fatalf("job = %s (%s), claimed by %q; want still running on %s", j.Status, j.Error, j.ClaimedBy, claimer)
		}
		if at := heartbeatAt(t, dbURL, id); at.After(last) {
			last, beats = at, beats+1
		}
	}
	if beats < 10 {
		t.Errorf("heartbeat stamp advanced %d times in 6s of shutdown, want about every 200ms", beats)
	}

	other := b.submit(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Hosts: []string{"dc"}, Action: "start"})
	waitFor(t, "the other replica to run the new job", func() bool { return !b.job(other).Active() })
	if j := b.job(other); j.Status != store.StatusSucceeded || j.ClaimedBy == claimer || !isWorkerID(j.ClaimedBy, "serve") {
		t.Errorf("new job = %s (%s), claimed by %q; want succeeded by the other replica", j.Status, j.Error, j.ClaimedBy)
	}

	release()
	if err := a.stopped(20 * time.Second); err != nil {
		t.Fatalf("serve = %v, want a clean stop", err)
	}
	if j := a.job(id); j.Status != store.StatusSucceeded {
		t.Errorf("job = %s (%s), want succeeded", j.Status, j.Error)
	}
}

// signalChildEnv makes TestServeSecondSignalExitsAtOnce's process the
// child: serve under real signals, with a job running through the shutdown.
const signalChildEnv = "BATTLESHIP_TEST_SIGNAL_CHILD_DB"

// A second signal kills serve at once, even while it waits for a job.
func TestServeSecondSignalExitsAtOnce(t *testing.T) {
	if dbURL := os.Getenv(signalChildEnv); dbURL != "" {
		signalChild(t, dbURL)
		return
	}
	_, dbURL := storetest.NewWithURL(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeSecondSignalExitsAtOnce$")
	cmd.Env = append(os.Environ(), signalChildEnv+"="+dbURL)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	lines := make(chan string, 100)
	go func() {
		s := bufio.NewScanner(out)
		for s.Scan() {
			lines <- s.Text()
		}
		close(lines)
	}()
	expect := func(want string) {
		t.Helper()
		for {
			line, ok := <-lines
			if !ok {
				t.Fatalf("child ended before printing %q", want)
			}
			if line == want {
				return
			}
		}
	}
	expect("job running")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	expect("first signal handled")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("serve didn't exit within 10s of a second signal")
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("child ended %s, want killed by the second SIGTERM", cmd.ProcessState)
	}
}

// signalChild runs serve under stopOnSignal with a job held, printing
// progress for the parent, which kills it.
func signalChild(t *testing.T, dbURL string) {
	st, err := store.Open(context.Background(), dbURL, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := stopOnSignal(func() { fmt.Println("first signal handled") })
	defer stop()
	e := startServe(t, onStore(st), underContext(ctx), withStopReserve(time.Second))
	e.api.hold(false)
	e.submit(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Hosts: []string{"dc"}, Action: "start"})
	signalled(t, e.api.waiting, "the job to wait for its Proxmox task")
	fmt.Println("job running")
	time.Sleep(time.Minute)
	t.Error("still running a minute after the job started; the parent should have killed it")
}

func TestServeReadinessFailsFirstOnShutdown(t *testing.T) {
	draining := make(chan struct{})
	release := make(chan struct{})
	e := startServe(t, withDrain(func() { close(draining); <-release }))
	e.ready()
	e.stop()
	<-draining
	if code, body := e.get("/readyz"); code != http.StatusServiceUnavailable || strings.TrimSpace(body) != "shutting down" {
		t.Errorf("/readyz while draining = %d %q, want 503 shutting down", code, body)
	}
	if code, _ := e.get("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz while draining = %d, want 200", code)
	}
	close(release)
	select {
	case err := <-e.done:
		e.done <- err
		if err != nil {
			t.Errorf("serve = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve didn't stop after draining")
	}
	if code, _ := e.get("/healthz"); code != 0 {
		t.Errorf("/healthz after shutdown = %d, want the connection refused", code)
	}
}

func TestServeShutdownEndsEventStreams(t *testing.T) {
	e := startServe(t)
	e.ready()
	sess := authtest.Login(t, e.st, e.cfg, authtest.User{})
	var streams []*bufio.Reader
	for _, path := range []string{"/events/grid", "/events/jobs"} {
		req, err := http.NewRequest(http.MethodGet, e.url+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		sess.Apply(req)
		resp, err := (&http.Client{}).Do(req) // no timeout: it streams
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("%s = %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		r := bufio.NewReader(resp.Body)
		if _, err := r.ReadString('\n'); err != nil { // the stream is open
			t.Fatal(err)
		}
		streams = append(streams, r)
	}

	if err, _ := e.shutdown(20 * time.Second); err != nil {
		t.Fatalf("serve = %v", err)
	}
	for i, r := range streams {
		if _, err := io.Copy(io.Discard, r); err != nil {
			t.Errorf("stream %d ended with %v, want a clean end", i, err)
		}
	}
}

func TestServeStartupFailsClearly(t *testing.T) {
	for _, c := range []struct {
		name     string
		teams    string // web.teams, which was removed, if set
		database bool   // database.url is set
		secret   string // BATTLESHIP_OIDC_CLIENT_SECRET
		want     string
	}{
		{"no OIDC secret", "", true, "", "oidc.client_secret (or BATTLESHIP_OIDC_CLIENT_SECRET) is required"},
		{"a leftover web.teams", "1-32", true, "oidc-client-secret", "these settings were removed: web.teams. Battleship shows the pods that have VMs; delete the line"},
		{"no database", "", false, "oidc-client-secret", "database.url (or BATTLESHIP_DATABASE_URL) is required"},
		{"a leftover service token", "", true, "oidc-client-secret", "BATTLESHIP_PROXMOX_TOKEN_SECRET is set, but battleship serve holds no Proxmox token"},
		{"Proxmox's certificate unchecked", "", true, "oidc-client-secret", "proxmox.ca_file is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			toml := "[proxmox]\nurl = \"https://pve.example:8006\"\n" +
				"[web]\nbase_url = \"https://battleship.example.org\"\n"
			if c.teams != "" {
				toml += "teams = \"" + c.teams + "\"\n"
			}
			toml += "[oidc]\nclient_id = \"battleship\"\n"
			if c.database {
				toml += "[database]\nurl = \"postgres://battleship:database-password@db.example/battleship\"\n"
			}
			path := filepath.Join(t.TempDir(), "battleship.toml")
			if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BATTLESHIP_DATABASE_URL", "")
			t.Setenv("BATTLESHIP_SEAL_KEY", testSealKey)
			t.Setenv("BATTLESHIP_OIDC_CLIENT_SECRET", c.secret)
			if c.name == "a leftover service token" {
				t.Setenv("BATTLESHIP_PROXMOX_TOKEN_SECRET", "proxmox-token-secret")
			}
			stderr := &syncBuffer{}
			d := deps{
				newCluster: func(config.Proxmox, func(string, ...any)) cluster {
					t.Error("serve made a Proxmox client before its checks")
					return newServeAPI()
				},
				openStore: func(context.Context, config.Database) (*store.Store, func(), error) {
					t.Error("serve opened the database before its checks")
					return nil, nil, errors.New("not in this test")
				},
				stderr: stderr,
			}
			if code := run(context.Background(), []string{"serve", "-config", path}, d); code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			out := stderr.String()
			if !strings.Contains(out, c.want) || !strings.Contains(out, "battleship serve can't start") {
				t.Errorf("stderr doesn't say %q:\n%s", c.want, out)
			}
			for _, secret := range []string{"proxmox-token-secret", "database-password", "oidc-client-secret"} {
				if strings.Contains(out, secret) {
					t.Errorf("stderr shows a secret (%s):\n%s", secret, out)
				}
			}
		})
	}
}

func TestServeFlags(t *testing.T) {
	var stderr strings.Builder
	d := deps{stderr: &stderr}
	if code := run(context.Background(), []string{"-h"}, d); code != 0 {
		t.Errorf("-h: exit %d, want 0", code)
	}
	for _, prefix := range [][]string{nil, {"serve"}} {
		stderr.Reset()
		if code := run(context.Background(), append(prefix, "-log-format", "xml"), d); code != 2 ||
			!strings.Contains(stderr.String(), `-log-format must be text or json, not "xml"`) {
			t.Errorf("%v bad -log-format: exit %d, stderr %q", prefix, code, stderr.String())
		}
		stderr.Reset()
		if code := run(context.Background(), append(prefix, "extra"), d); code != 2 {
			t.Errorf("%v stray argument: exit %d, want 2", prefix, code)
		}
	}
}

func TestServeFailsFastWithoutIdentityProvider(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	issuer := "http://" + ln.Addr().String() + "/"
	ln.Close() // nothing listens here

	cfg := serveConfig()
	cfg.Web.BaseURL = "http://battleship.test"
	cfg.OIDC.Issuer, cfg.OIDC.ClientID, cfg.OIDC.ClientSecret = issuer, "battleship", "oidc-client-secret"
	st := storetest.New(t)
	logs := &syncBuffer{}
	d := serveDeps{
		newCluster: func(config.Proxmox, func(string, ...any)) cluster { return newServeAPI() },
		openStore:  func(context.Context, config.Database) (*store.Store, func(), error) { return st, func() {}, nil },
		listen: func(string, string) (net.Listener, error) {
			t.Error("serve listened without an identity provider")
			return nil, errors.New("no")
		},
		log:       slog.New(slog.NewTextHandler(logs, nil)),
		oidcWait:  500 * time.Millisecond,
		oidcRetry: 100 * time.Millisecond,
	}
	start := time.Now()
	err = serve(context.Background(), cfg, d)
	if err == nil || !strings.Contains(err.Error(), "oidc.issuer "+issuer+": discovery failed") ||
		!strings.Contains(err.Error(), "gave up after 500ms") {
		t.Errorf("serve = %v, want the issuer's discovery failure and the time it tried", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("gave up after %s, want about 500ms", took)
	}
	if strings.Contains(err.Error()+logs.String(), "oidc-client-secret") {
		t.Errorf("the error or logs show the client secret: %v\n%s", err, logs)
	}
	if !strings.Contains(logs.String(), "identity provider not reachable yet") {
		t.Errorf("logs = %s, want the retries", logs)
	}
}

// A stop signal while startup waits to retry the identity provider says
// serve was stopped, not that it gave up on the provider.
func TestServeStoppedWhileWaitingForIdentityProvider(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	issuer := "http://" + ln.Addr().String() + "/"
	ln.Close() // nothing listens here

	cfg := serveConfig()
	cfg.Web.BaseURL = "http://battleship.test"
	cfg.OIDC.Issuer, cfg.OIDC.ClientID, cfg.OIDC.ClientSecret = issuer, "battleship", "oidc-client-secret"
	st := storetest.New(t)
	logs := &syncBuffer{}
	d := serveDeps{
		newCluster: func(config.Proxmox, func(string, ...any)) cluster { return newServeAPI() },
		openStore:  func(context.Context, config.Database) (*store.Store, func(), error) { return st, func() {}, nil },
		listen: func(string, string) (net.Listener, error) {
			t.Error("serve listened without an identity provider")
			return nil, errors.New("no")
		},
		log:       slog.New(slog.NewTextHandler(logs, nil)),
		oidcWait:  time.Minute,
		oidcRetry: time.Minute, // the signal comes during this pause
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, d) }()
	waitFor(t, "a failed discovery attempt", func() bool { return strings.Contains(logs.String(), "identity provider not reachable yet") })
	cancel()
	err = signalled(t, done, "serve to stop")
	if err == nil || !strings.Contains(err.Error(), "stopped while reaching the identity provider") || strings.Contains(err.Error(), "gave up") {
		t.Errorf("serve = %v, want it stopped while reaching the identity provider", err)
	}
}

// fakeCleaner gives cleanUp a session sweep that can hang.
type fakeCleaner struct {
	hangSessions bool
	previewsCtx  error // ctx.Err() when DeleteExpiredPreviews was called
}

func (c *fakeCleaner) EndExpiredSessions(ctx context.Context, _ time.Time) (int64, error) {
	if c.hangSessions {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return 1, nil
}

func (c *fakeCleaner) DeleteExpiredPreviews(ctx context.Context, _ time.Time) (int64, error) {
	c.previewsCtx = ctx.Err()
	return 2, ctx.Err()
}

// A session delete that uses up its timeout doesn't leave the preview
// delete without time: each has its own.
func TestCleanUpTimesEachQuery(t *testing.T) {
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	c := &fakeCleaner{hangSessions: true}
	cleanUp(context.Background(), c.EndExpiredSessions, c.DeleteExpiredPreviews, time.Now(), 50*time.Millisecond, log)
	if c.previewsCtx != nil {
		t.Errorf("the preview delete ran with its context already %v", c.previewsCtx)
	}
	if !strings.Contains(logs.String(), `"msg":"cleanup failed"`) || !strings.Contains(logs.String(), `"previews":2`) {
		t.Errorf("logs = %s, want the session delete's failure and the previews cleaned up", logs)
	}
}

func TestServeWaitsForIdentityProvider(t *testing.T) {
	// The provider refuses discovery until serve's first attempt failed, as
	// when a pod starts before the provider (or its network) is ready.
	var up atomic.Bool
	var issuer string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "authorize", "token_endpoint": issuer + "token",
			"jwks_uri": issuer + "jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	t.Cleanup(idp.Close)
	issuer = idp.URL + "/"

	cfg := serveConfig()
	cfg.OIDC.Issuer, cfg.OIDC.ClientID, cfg.OIDC.ClientSecret = issuer, "battleship", "oidc-client-secret"
	st := storetest.New(t)
	logs := &syncBuffer{}
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Web.BaseURL = "http://" + app.Addr().String()
	d := serveDeps{
		newCluster: func(config.Proxmox, func(string, ...any)) cluster { return newServeAPI(teamPodVMs()...) },
		openStore:  func(context.Context, config.Database) (*store.Store, func(), error) { return st, func() {}, nil },
		listen:     func(string, string) (net.Listener, error) { return app, nil },
		log:        slog.New(slog.NewTextHandler(logs, nil)),
		drain:      func() {},
		oidcWait:   20 * time.Second,
		oidcRetry:  50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, d) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve = %v", err)
		}
	}()

	waitFor(t, "a failed discovery attempt", func() bool { return strings.Contains(logs.String(), "identity provider not reachable yet") })
	up.Store(true)
	waitFor(t, "battleship serve to start once the provider is up", func() bool { return strings.Contains(logs.String(), "msg=serving") })
}

func TestServeCleansUpExpiredSessions(t *testing.T) {
	st := storetest.New(t)
	old := time.Now().Add(-2 * time.Hour)
	for _, id := range []string{"expired", "live"} {
		exp := old.Add(time.Hour)
		if id == "live" {
			exp = time.Now().Add(time.Hour)
		}
		err := st.CreateSession(context.Background(), store.Session{
			ID: id, Subject: "sub-" + id, RefreshToken: "rt", CSRF: "c",
			CreatedAt: old, LastSeen: old, RefreshedAt: old, ExpiresAt: exp,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	err := st.CreatePreview(context.Background(), store.Preview{
		ID: "stale-preview", SessionID: "live", Kind: "reset", Inputs: json.RawMessage(`{"kind":"reset","teams":"1"}`),
		Fingerprint: "fp", CreatedAt: old, ExpiresAt: old.Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	e := startServe(t, onStore(st))
	waitFor(t, "the startup cleanup", func() bool { return len(e.records("cleaned up")) == 1 })
	if _, err := st.Session(context.Background(), "expired"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("expired session: %v, want deleted", err)
	}
	if _, err := st.Session(context.Background(), "live"); err != nil {
		t.Errorf("live session: %v, want kept", err)
	}
	if _, err := st.Preview(context.Background(), "live", "stale-preview"); !errors.Is(err, store.ErrPreviewNotFound) {
		t.Errorf("expired preview: %v, want deleted", err)
	}
	rec := e.records("cleaned up")[0]
	if rec["sessions"] != float64(1) || rec["previews"] != float64(1) {
		t.Errorf("cleaned up record = %v, want 1 session and 1 preview", rec)
	}
}

// signalled receives from ch, failing the test if nothing comes within 20s.
func signalled[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func TestServeCommand(t *testing.T) {
	// battleship serve as the binary runs it: flags, the config file, JSON logs.
	st := storetest.New(t)
	var oidc config.Config
	authtest.StubProvider(t, &oidc)
	toml := "[proxmox]\nurl = \"https://pve.example:8006\"\ninsecure_skip_verify = true\n" +
		"[database]\nurl = \"postgres://battleship@db.example/battleship\"\n" +
		"[web]\nlisten = \"127.0.0.1:0\"\nbase_url = \"http://battleship.test\"\n" +
		"[oidc]\nissuer = \"" + oidc.OIDC.Issuer + "\"\nclient_id = \"" + oidc.OIDC.ClientID + "\"\n"
	path := filepath.Join(t.TempDir(), "battleship.toml")
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BATTLESHIP_DATABASE_URL", "")
	t.Setenv("BATTLESHIP_SEAL_KEY", testSealKey)
	t.Setenv("BATTLESHIP_OIDC_CLIENT_SECRET", oidc.OIDC.ClientSecret)
	stderr := &syncBuffer{}
	d := deps{
		newCluster: func(config.Proxmox, func(string, ...any)) cluster { return newServeAPI(teamPodVMs()...) },
		openStore:  func(context.Context, config.Database) (*store.Store, func(), error) { return st, func() {}, nil },
		stderr:     stderr,
		drain:      func() {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan int, 1)
	go func() { exit <- run(ctx, []string{"serve", "-config", path, "-log-format", "json"}, d) }()
	waitFor(t, "battleship serve to start", func() bool { return strings.Contains(stderr.String(), `"msg":"serving"`) })
	cancel()
	if code := signalled(t, exit, "battleship serve to exit"); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	var msgs []string
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line isn't JSON: %q", line)
		}
		msgs = append(msgs, rec["msg"].(string))
		if rec["msg"] == "serving" && !strings.HasPrefix(rec["listen"].(string), "127.0.0.1:") {
			t.Errorf("serving record = %v, want the address it listens on", rec)
		}
	}
	for _, want := range []string{"starting", "serving", "shutting down", "stopped"} {
		if !slices.Contains(msgs, want) {
			t.Errorf("log messages %q lack %q", msgs, want)
		}
	}
	if strings.Contains(stderr.String(), "context canceled") {
		t.Errorf("stopping logged errors:\n%s", stderr)
	}
	if strings.Contains(stderr.String(), oidc.OIDC.ClientSecret) || strings.Contains(stderr.String(), "proxmox-token-secret") {
		t.Errorf("the log shows a secret:\n%s", stderr)
	}
}
