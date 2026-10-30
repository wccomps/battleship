package auth_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

var ctx = context.Background()

// Users at the fake identity provider.
var (
	alice = authtest.User{Subject: "sub-alice", Name: "Alice Operator", Email: "alice@example.org"}
	lena  = authtest.User{Subject: "sub-lena", Name: "Lena Lead", Email: "lena@example.org"}
	nobby = authtest.User{Subject: "sub-nobby", Name: "Nobby Nobody", Email: "nobby@example.org"}
)

// logBuffer collects the service's log lines.
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

// clock is the fake time shared by the app, the identity provider and
// Proxmox.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness is the app on httptest with the fake identity provider, the test
// database and a shared fake clock.
type harness struct {
	t     *testing.T
	clock *clock
	idp   *authtest.Provider
	pve   *pvetest.Server // Proxmox: its OpenID login, tickets and their renewal
	st    *store.Store
	cfg   config.Config
	svc   *auth.Service
	app   *httptest.Server
	logs  *logBuffer
	mux   atomic.Pointer[http.ServeMux] // what app serves
}

// webConfig is the tests' config, with [oidc] pointing at a new identity
// provider on c's time.
func webConfig(t *testing.T, c *clock, baseURL string) (config.Config, *authtest.Provider) {
	cfg := config.Default()
	cfg.Web.BaseURL = baseURL
	cfg.Web.SessionIdle = 30 * time.Minute // the session tests count from 30m
	idp := authtest.NewProvider(t, &cfg)
	idp.Now = c.Now
	return cfg, idp
}

func newHarness(t *testing.T, mut ...func(*config.Config)) *harness {
	t.Helper()
	h := &harness{t: t, clock: newClock(), logs: &logBuffer{}}
	h.pve = pvetest.New(t)
	h.pve.SetClock(h.clock.Now)
	for _, u := range []authtest.User{alice, lena, nobby} {
		h.pve.AddUser(pveUser(u), nil)
	}
	h.st = storetest.New(t)

	h.app = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.mux.Load().ServeHTTP(w, r) }))
	t.Cleanup(h.app.Close)

	h.cfg, h.idp = webConfig(t, h.clock, h.app.URL)
	h.cfg.Proxmox.URL = h.pve.URL()
	for _, m := range mut {
		m(&h.cfg)
	}
	h.restart(h.cfg)
	return h
}

// restart builds a new service from cfg, as a redeploy would, and serves it.
// Sessions in the database carry over.
func (h *harness) restart(cfg config.Config) {
	h.t.Helper()
	svc, err := auth.NewService(ctx, cfg, h.st, auth.Options{Now: h.clock.Now, Logf: h.logs.Logf, Proxmox: h.pve.Client(), RevokeRetry: 20 * time.Millisecond})
	if err != nil {
		h.t.Fatalf("NewService: %v", err)
	}
	svc.SetRender(renderPage)
	h.svc = svc
	h.mux.Store(h.routes())
}

// renderPage draws auth's pages for the tests: refresh, status, title,
// message and link, escaped.
func renderPage(w http.ResponseWriter, _ *http.Request, p auth.Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(p.Status)
	if p.Refresh != "" {
		fmt.Fprintf(w, "<meta http-equiv=\"refresh\" content=\"0;url=%s\">\n", html.EscapeString(p.Refresh))
	}
	fmt.Fprintf(w, "<h1>%s</h1>\n<p>%s</p>\n", html.EscapeString(p.Title), html.EscapeString(p.Message))
	if p.Link != "" {
		fmt.Fprintf(w, "<p><a href=\"%s\">%s</a></p>\n", html.EscapeString(p.Link), html.EscapeString(p.LinkText))
	}
}

func (h *harness) routes() *http.ServeMux {
	mux := http.NewServeMux()
	h.svc.Routes(mux)
	mux.Handle("/private", h.svc.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFrom(r.Context())
		if !ok {
			http.Error(w, "no user in context", http.StatusInternalServerError)
			return
		}
		cred, _ := auth.ProxmoxCredential(r.Context())
		fmt.Fprintf(w, "%s ok: user=%s pve=%s", r.Method, u.Name, cred.User)
	})))
	mux.Handle("GET /csrf", h.svc.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, auth.CSRFToken(r.Context()))
	})))
	mux.Handle("GET /session-id", h.svc.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, auth.SessionID(r.Context()))
	})))
	mux.Handle("GET /refreshed-at", h.svc.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, auth.SessionRefreshedAt(r.Context()).UTC().Format(time.RFC3339Nano))
	})))
	return mux
}

// response is a finished HTTP exchange with its body read.
type response struct {
	*http.Response
	body string
}

func (r response) cookie(name string) *http.Cookie {
	for _, c := range r.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// browser is a cookie-keeping client. get follows redirects across the app
// and the identity provider; do doesn't follow them.
type browser struct {
	h      *harness
	jar    *cookiejar.Jar
	follow *http.Client
	raw    *http.Client
	// loginCookie is the battleship_oidc cookie the last startLogin was given,
	// with its attributes.
	loginCookie *http.Cookie
}

func (h *harness) browser() *browser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	// The fake Proxmox's identity provider uses a self-signed certificate.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test servers
	return &browser{
		h: h, jar: jar,
		follow: &http.Client{Jar: jar, Transport: tr},
		raw: &http.Client{Jar: jar, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

func (b *browser) send(c *http.Client, req *http.Request) response {
	b.h.t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		b.h.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.h.t.Fatal(err)
	}
	return response{resp, string(body)}
}

func (b *browser) abs(target string) string {
	if strings.HasPrefix(target, "http") {
		return target
	}
	return b.h.app.URL + target
}

// get fetches target, following redirects and, like a browser, pages'
// refreshes.
func (b *browser) get(target string) response {
	b.h.t.Helper()
	for range 10 {
		req, err := http.NewRequest(http.MethodGet, b.abs(target), nil)
		if err != nil {
			b.h.t.Fatal(err)
		}
		resp := b.send(b.follow, req)
		to := refreshOf(resp.body)
		if to == "" {
			return resp
		}
		target = to
	}
	b.h.t.Fatalf("%s: too many refreshes", target)
	return response{}
}

// refreshTag is the refresh renderPage writes.
var refreshTag = regexp.MustCompile(`<meta http-equiv="refresh" content="0;url=([^"]*)">`)

// refreshOf is the URL body's page refreshes to, or "".
func refreshOf(body string) string {
	m := refreshTag.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// do sends one request without following redirects.
func (b *browser) do(method, target string, body io.Reader, header http.Header) response {
	b.h.t.Helper()
	req, err := http.NewRequest(method, b.abs(target), body)
	if err != nil {
		b.h.t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	return b.send(b.raw, req)
}

// postForm posts form values without following redirects.
func (b *browser) postForm(target string, form url.Values, header http.Header) response {
	b.h.t.Helper()
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(http.MethodPost, target, strings.NewReader(form.Encode()), header)
}

// login logs u in through the full redirect chain and checks it lands on
// /private.
func (b *browser) login(u authtest.User) response {
	b.h.t.Helper()
	b.h.idp.SignIn(u)
	b.h.pve.SignIn(pveUser(u))
	resp := b.get("/auth/login?next=/private")
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/private" {
		b.h.t.Fatalf("login as %s ended at %s with %d: %s", u.Subject, resp.Request.URL, resp.StatusCode, resp.body)
	}
	return resp
}

// startLogin goes through /auth/login and the provider's authorize step,
// and returns the callback URL the provider sent the browser back to.
func (b *browser) startLogin(u authtest.User, next string) *url.URL {
	b.h.t.Helper()
	b.h.idp.SignIn(u)
	resp := b.do(http.MethodGet, "/auth/login?next="+url.QueryEscape(next), nil, nil)
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		b.h.t.Fatalf("/auth/login = %d, want a redirect", resp.StatusCode)
	}
	b.loginCookie = resp.cookie("battleship_oidc")
	if b.loginCookie == nil {
		b.loginCookie = resp.cookie("__Host-battleship_oidc") // https base URL
	}
	resp = b.do(http.MethodGet, resp.Header.Get("Location"), nil, nil)
	if resp.StatusCode != http.StatusFound {
		b.h.t.Fatalf("authorize = %d: %s", resp.StatusCode, resp.body)
	}
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		b.h.t.Fatal(err)
	}
	return cb
}

// dropTicket leaves the session without a Proxmox ticket: none stored and no
// ticket cookie.
func (b *browser) dropTicket() {
	b.h.t.Helper()
	if err := b.h.st.ClearSessionTicket(ctx, b.session().ID, time.Time{}); err != nil {
		b.h.t.Fatal(err)
	}
	u, _ := url.Parse(b.h.app.URL)
	b.jar.SetCookies(u, []*http.Cookie{{Name: auth.TicketCookie, Value: "", Path: "/", MaxAge: -1}})
}

// sessionToken is the browser's battleship_session cookie value, or "".
func (b *browser) sessionToken() string {
	u, _ := url.Parse(b.h.app.URL)
	for _, c := range b.jar.Cookies(u) {
		if c.Name == auth.SessionCookie {
			return c.Value
		}
	}
	return ""
}

// csrf reads the session's CSRF token through a handler that renders it.
func (b *browser) csrf() string {
	b.h.t.Helper()
	resp := b.do(http.MethodGet, "/csrf", nil, nil)
	if resp.StatusCode != http.StatusOK || resp.body == "" {
		b.h.t.Fatalf("/csrf = %d %q", resp.StatusCode, resp.body)
	}
	return resp.body
}

// session reads the browser's session row, failing if there is none.
func (b *browser) session() store.Session {
	b.h.t.Helper()
	sess, err := b.h.st.Session(ctx, auth.HashToken(b.sessionToken()))
	if err != nil {
		b.h.t.Fatalf("session: %v", err)
	}
	return sess
}

// sessionGone checks the database no longer has the session for token.
func (h *harness) sessionGone(token string) {
	h.t.Helper()
	if _, err := h.st.Session(ctx, auth.HashToken(token)); err != store.ErrSessionNotFound {
		h.t.Errorf("session still stored (err = %v), want it deleted", err)
	}
}

// countSessions removes every session and reports how many there were.
func (h *harness) countSessions() int64 {
	h.t.Helper()
	n, err := h.st.EndExpiredSessions(ctx, h.clock.Now().Add(100*365*24*time.Hour), 1000, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return int64(n)
}

// wantLoginRedirect checks resp redirects to the login page with next.
func wantLoginRedirect(t *testing.T, resp response, next string) {
	t.Helper()
	want := "/auth/login?next=" + url.QueryEscape(next)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != want {
		t.Errorf("got %d to %q, want 303 to %q", resp.StatusCode, resp.Header.Get("Location"), want)
	}
}

// refreshToken is the browser's session's refresh token, opened from its
// sealed stored form.
func (b *browser) refreshToken() string {
	b.h.t.Helper()
	sess := b.session()
	rt, err := auth.OpenRefreshToken(b.h.svc, sess.ID, sess.RefreshToken)
	if err != nil {
		b.h.t.Fatalf("opening the stored refresh token: %v", err)
	}
	return rt
}

// pveUser is u's Proxmox user in the OpenID realm.
func pveUser(u authtest.User) string {
	return strings.TrimPrefix(u.Subject, "sub-") + "@" + pvetest.Realm
}

// revoked is the refresh tokens the provider revoked, once the queued
// revocations that are due have been sent.
func (h *harness) revoked() []string {
	if err := h.svc.DrainRevocations(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	return h.idp.Revoked()
}

// liveTokens is the refresh tokens the provider still honours, once the
// queued revocations that are due have been sent.
func (h *harness) liveTokens() []string {
	if err := h.svc.DrainRevocations(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	return h.idp.RefreshTokens()
}
