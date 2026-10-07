package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// serveAPI is the fake Proxmox for battleship serve: the CLI fake plus a
// listing that can fail, task waits that tests can hold, and Proxmox's
// sign-in. Its fields are guarded by the fake's Mu.
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
	st    *store.Store
	cfg   *config.Config
	deps  *serveDeps
	login bool // a login-capable provider instead of the stub
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
		newAPI: func(config.Proxmox) pods.API { return e.api },
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

	ctx, cancel := context.WithCancel(context.Background())
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

// Readiness never calls Proxmox (serve holds no credential to call it
// with), so a Proxmox outage, shared by every replica, can't take them all
// out of the load balancer.
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
		{"a leftover web.teams", "1-32", true, "oidc-client-secret", "these settings were removed: web.teams. Battleship shows the teams that have VMs; delete the line"},
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
				newAPI: func(config.Proxmox) pods.API {
					t.Error("serve made a Proxmox client before its checks")
					return newServeAPI()
				},
				openStore: func(context.Context, config.Database) (*store.Store, func(), error) {
					t.Error("serve opened the database before its checks")
					return nil, nil, errors.New("not in this test")
				},
				stdout: io.Discard,
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
	d := deps{stdout: io.Discard, stderr: &stderr}
	if code := run(context.Background(), []string{"serve", "-log-format", "xml"}, d); code != 2 ||
		!strings.Contains(stderr.String(), `-log-format must be text or json, not "xml"`) {
		t.Errorf("bad -log-format: exit %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"serve", "extra"}, d); code != 2 {
		t.Errorf("stray argument: exit %d, want 2", code)
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
		newAPI:    func(config.Proxmox) pods.API { return newServeAPI() },
		openStore: func(context.Context, config.Database) (*store.Store, func(), error) { return st, func() {}, nil },
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
		newAPI:    func(config.Proxmox) pods.API { return newServeAPI() },
		openStore: func(context.Context, config.Database) (*store.Store, func(), error) { return st, func() {}, nil },
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

// fakeCleaner is a store for cleanUp whose session delete can hang.
type fakeCleaner struct {
	hangSessions bool
	previewsCtx  error // ctx.Err() when DeleteExpiredPreviews was called
}

func (c *fakeCleaner) DeleteExpiredSessions(ctx context.Context, _ time.Time) (int64, error) {
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
	cleanUp(context.Background(), c, time.Now(), 50*time.Millisecond, log)
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
		newAPI:    func(config.Proxmox) pods.API { return newServeAPI(teamPodVMs()...) },
		openStore: func(context.Context, config.Database) (*store.Store, func(), error) { return st, func() {}, nil },
		listen:    func(string, string) (net.Listener, error) { return app, nil },
		log:       slog.New(slog.NewTextHandler(logs, nil)),
		drain:     func() {},
		oidcWait:  20 * time.Second,
		oidcRetry: 50 * time.Millisecond,
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
			ID: id, Subject: "sub-" + id, RefreshToken: "rt", CSRF: "c", Groups: []string{"battleship-operators"},
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
	stderr, stdout := &syncBuffer{}, &syncBuffer{}
	d := deps{
		newAPI:    func(config.Proxmox) pods.API { return newServeAPI(teamPodVMs()...) },
		openStore: func(context.Context, config.Database) (*store.Store, func(), error) { return st, func() {}, nil },
		stdout:    stdout,
		stderr:    stderr,
		drain:     func() {},
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
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want everything in the log on stderr", stdout)
	}
	if strings.Contains(stderr.String(), "context canceled") {
		t.Errorf("stopping logged errors:\n%s", stderr)
	}
	if strings.Contains(stderr.String(), oidc.OIDC.ClientSecret) || strings.Contains(stderr.String(), "proxmox-token-secret") {
		t.Errorf("the log shows a secret:\n%s", stderr)
	}
}
