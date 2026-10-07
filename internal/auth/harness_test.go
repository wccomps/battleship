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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

var ctx = context.Background()

// Users at the fake identity provider.
var (
	alice = idpUser{Sub: "sub-alice", Name: "Alice Operator", Email: "alice@example.org", Groups: []string{"staff", "battleship-operators"}}
	lena  = idpUser{Sub: "sub-lena", Name: "Lena Lead", Email: "lena@example.org", Groups: []string{"battleship-leads", "battleship-operators"}}
	nobby = idpUser{Sub: "sub-nobby", Name: "Nobby Nobody", Email: "nobby@example.org", Groups: []string{"staff"}}
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

// harness is the app on httptest with the fake identity provider, the test
// database and a shared fake clock.
type harness struct {
	t     *testing.T
	clock *clock
	idp   *fakeIDP
	pve   *pvetest.Server // Proxmox: its OpenID login, tickets and their renewal
	st    *store.Store
	cfg   config.Config
	svc   *auth.Service
	app   *httptest.Server
	logs  *logBuffer
	mux   atomic.Pointer[http.ServeMux] // what app serves
}

func webConfig(issuer, clientID, secret, baseURL string) config.Config {
	cfg := config.Default()
	cfg.Web.BaseURL = baseURL
	cfg.Web.SessionIdle = 30 * time.Minute // the session tests count from 30m
	cfg.OIDC.Issuer = issuer
	cfg.OIDC.ClientID = clientID
	cfg.OIDC.ClientSecret = secret
	return cfg
}

func newHarness(t *testing.T, mut ...func(*config.Config)) *harness {
	t.Helper()
	h := &harness{t: t, clock: newClock(), logs: &logBuffer{}}
	h.idp = newFakeIDP(t, h.clock)
	h.pve = pvetest.New(t)
	h.pve.SetClock(h.clock.Now)
	for _, u := range []idpUser{alice, lena, nobby} {
		h.pve.AddUser(pveUser(u), nil)
	}
	h.st = storetest.New(t)

	h.app = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.mux.Load().ServeHTTP(w, r) }))
	t.Cleanup(h.app.Close)

	h.cfg = webConfig(h.idp.issuer(), h.idp.clientID, h.idp.secret, h.app.URL)
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
	svc, err := auth.NewService(ctx, cfg, h.st, auth.Options{Now: h.clock.Now, Logf: h.logs.Logf, Proxmox: h.pve.Client()})
	if err != nil {
		h.t.Fatalf("NewService: %v", err)
	}
	svc.SetRender(renderPage)
	h.svc = svc
	h.mux.Store(h.routes())
}

// renderPage draws auth's pages for the tests: status, title, message and
// link, escaped.
func renderPage(w http.ResponseWriter, _ *http.Request, p auth.Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(p.Status)
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
		sess, err := h.st.Session(r.Context(), auth.SessionID(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "%s ok: user=%s groups=%s pve=%s", r.Method, u.Name, strings.Join(sess.Groups, ","), cred.User)
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

// get fetches target, following redirects.
func (b *browser) get(target string) response {
	b.h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, b.abs(target), nil)
	if err != nil {
		b.h.t.Fatal(err)
	}
	return b.send(b.follow, req)
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
func (b *browser) login(u idpUser) response {
	b.h.t.Helper()
	b.h.idp.setUser(u)
	b.h.pve.SignIn(pveUser(u))
	resp := b.get("/auth/login?next=/private")
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/private" {
		b.h.t.Fatalf("login as %s ended at %s with %d: %s", u.Sub, resp.Request.URL, resp.StatusCode, resp.body)
	}
	return resp
}

// startLogin goes through /auth/login and the provider's authorize step,
// and returns the callback URL the provider sent the browser back to.
func (b *browser) startLogin(u idpUser, next string) *url.URL {
	b.h.t.Helper()
	b.h.idp.setUser(u)
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
	n, err := h.st.DeleteExpiredSessions(ctx, h.clock.Now().Add(100*365*24*time.Hour))
	if err != nil {
		h.t.Fatal(err)
	}
	return n
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
func pveUser(u idpUser) string { return strings.TrimPrefix(u.Sub, "sub-") + "@" + pvetest.Realm }
