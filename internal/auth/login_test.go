package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func TestLoginRoundTrip(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	resp := b.login(alice)
	if want := "GET ok: user=Alice Operator groups=staff,battleship-operators pve=alice@auth.example.org"; resp.body != want {
		t.Errorf("body = %q, want %q", resp.body, want)
	}
	token := b.sessionToken()
	if len(token) != 43 {
		t.Errorf("session token %q has %d characters, want 43 (32 bytes base64url)", token, len(token))
	}
	sess := b.session()
	if sess.Subject != alice.Sub || sess.Name != alice.Name || sess.Email != alice.Email ||
		!slices.Equal(sess.Groups, alice.Groups) {
		t.Errorf("session = %+v, want alice's identity and groups", sess)
	}
	if rt := b.refreshToken(); !slices.Contains(h.idp.refreshTokens(), rt) {
		t.Errorf("stored refresh token %q is not one the provider issued", rt)
	}
	now := h.clock.Now()
	if !sess.CreatedAt.Equal(now) || !sess.LastSeen.Equal(now) || !sess.RefreshedAt.Equal(now) ||
		!sess.ExpiresAt.Equal(now.Add(30*time.Minute)) {
		t.Errorf("session times = %v %v %v %v, want now and now+idle", sess.CreatedAt, sess.LastSeen, sess.RefreshedAt, sess.ExpiresAt)
	}
	if !strings.Contains(h.logs.String(), `login: subject="sub-alice"`) {
		t.Errorf("logs = %q, want a login line", h.logs.String())
	}
	var exchange url.Values
	h.idp.set(func(f *fakeIDP) { exchange = f.tokenRequests[0] })
	if exchange.Get("grant_type") != "authorization_code" || len(exchange.Get("code_verifier")) != 43 ||
		exchange.Get("redirect_uri") != h.app.URL+"/auth/callback" {
		t.Errorf("code exchange = %v, want the PKCE verifier and redirect URI", exchange)
	}
	if exchange.Has("client_secret") {
		t.Error("client secret sent in the form; discovery offers client_secret_basic")
	}
}

func TestDatabaseStoresOnlyTheTokenHash(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	token := b.sessionToken()
	if _, err := h.st.Session(ctx, token); err == nil {
		t.Fatal("the raw cookie token finds a session; the database must be keyed by its hash")
	}
	sess := b.session()
	if sess.ID != auth.HashToken(token) || sess.ID == token {
		t.Errorf("session ID = %q, want the SHA-256 hex of the token", sess.ID)
	}
	for _, field := range append([]string{sess.ID, sess.Subject, sess.Name, sess.Email, sess.RefreshToken, sess.CSRF}, sess.Groups...) {
		if strings.Contains(field, token) {
			t.Errorf("session field %q contains the raw token", field)
		}
	}
}

func TestHashToken(t *testing.T) {
	// SHA-256 of "abc".
	if got, want := auth.HashToken("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Errorf("HashToken = %s, want %s", got, want)
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secure bool
		// The cookie names and the login cookie's path: an https base URL
		// gets the __Host- prefix, which needs Secure, Path=/ and no Domain.
		sessName, loginName, loginPath string
	}{
		{"http base URL", false, "battleship_session", "battleship_oidc", "/auth/"},
		{"https base URL", true, "__Host-battleship_session", "__Host-battleship_oidc", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var muts []func(*config.Config)
			if tc.secure {
				// The cookie flags follow base_url, not the request, so an
				// https base URL gives Secure cookies even on this http server.
				muts = append(muts, func(c *config.Config) { c.Web.BaseURL = "https://battleship.example.org" })
			}
			h := newHarness(t, muts...)
			b := h.browser()
			cb := b.startLogin(alice, "/private")
			login := b.loginCookie
			if login == nil || login.Name != tc.loginName || !login.HttpOnly || login.Secure != tc.secure || login.SameSite != http.SameSiteLaxMode ||
				login.Path != tc.loginPath || login.Domain != "" || login.MaxAge != 600 {
				t.Errorf("login cookie = %+v, want %s, HttpOnly, Secure=%v, Lax, Path %s, Max-Age 600", login, tc.loginName, tc.secure, tc.loginPath)
			}
			// Deliver the callback straight to the app's handler, with the
			// login cookie, so the https case needs no TLS.
			resp := b.callbackDirect(cb, login)
			if resp.Code != http.StatusSeeOther || resp.Header().Get("Location") != "/auth/proxmox?next=%2Fprivate" {
				t.Fatalf("callback = %d to %q: %s", resp.Code, resp.Header().Get("Location"), resp.Body)
			}
			var sess, cleared *http.Cookie
			for _, c := range resp.Result().Cookies() {
				switch c.Name {
				case tc.sessName:
					sess = c
				case tc.loginName:
					cleared = c
				}
			}
			if sess == nil || !sess.HttpOnly || sess.Secure != tc.secure || sess.SameSite != http.SameSiteLaxMode ||
				sess.Path != "/" || sess.Domain != "" || sess.MaxAge != 1800 {
				t.Errorf("session cookie %s = %+v, want HttpOnly, Secure=%v, Lax, Path /, Max-Age 1800 (cookies: %v)",
					tc.sessName, sess, tc.secure, resp.Result().Cookies())
			}
			if cleared == nil || cleared.MaxAge >= 0 || cleared.Path != tc.loginPath || cleared.Secure != tc.secure {
				t.Errorf("%s after callback = %+v, want it cleared", tc.loginName, cleared)
			}
			if got := auth.SessionCookieName(h.cfg.Web.BaseURL); got != tc.sessName {
				t.Errorf("SessionCookieName(%q) = %q, want %q", h.cfg.Web.BaseURL, got, tc.sessName)
			}
			if cc := resp.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
		})
	}
}

// callbackDirect sends the callback to the app handler in-process.
func (b *browser) callbackDirect(cb *url.URL, login *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?"+cb.RawQuery, nil)
	if login != nil {
		req.AddCookie(&http.Cookie{Name: login.Name, Value: login.Value})
	}
	rec := httptest.NewRecorder()
	b.h.routes().ServeHTTP(rec, req)
	return rec
}

func TestLoginRedirectsToProvider(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	resp := b.do(http.MethodGet, "/auth/login?next=/jobs", nil, nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != h.idp.srv.URL+"/authorize" {
		t.Errorf("redirect to %s, want the provider's authorize endpoint", got)
	}
	checks := map[string]string{
		"response_type":         "code",
		"client_id":             "battleship",
		"redirect_uri":          h.app.URL + "/auth/callback",
		"scope":                 "openid profile email groups offline_access",
		"code_challenge_method": "S256",
	}
	for k, want := range checks {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if len(q.Get(k)) < 43 {
			t.Errorf("%s = %q, want at least 32 random bytes", k, q.Get(k))
		}
	}
	login := resp.cookie("battleship_oidc")
	if login == nil {
		t.Fatal("no battleship_oidc cookie")
	}
	// The login cookie is sealed: its value reveals neither state nor nonce.
	for _, k := range []string{"state", "nonce"} {
		if strings.Contains(login.Value, q.Get(k)) {
			t.Errorf("battleship_oidc cookie shows the %s", k)
		}
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	// Two logins get different state and nonce.
	again := b.do(http.MethodGet, "/auth/login", nil, nil)
	loc2, _ := url.Parse(again.Header.Get("Location"))
	if loc2.Query().Get("state") == q.Get("state") || loc2.Query().Get("nonce") == q.Get("nonce") {
		t.Error("state or nonce repeated across logins")
	}
}

func TestLoginRedirectsToNext(t *testing.T) {
	cases := []struct{ next, want string }{
		{"/jobs/5?page=2", "/jobs/5?page=2"},
		{"/", "/"},
		{"", "/"},
		{"//evil.example/x", "/"},
		{"/\\evil.example", "/"},
		{"https://evil.example/", "/"},
		{"javascript:alert(1)", "/"},
		{"/auth/login", "/"},
		{"/auth/callback?code=x", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.next, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser()
			cb := b.startLogin(alice, tc.next)
			resp := b.do(http.MethodGet, cb.String(), nil, nil)
			// On to the Proxmox sign-in, which then goes to next.
			want := "/auth/proxmox?next=" + url.QueryEscape(tc.want)
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != want {
				t.Errorf("callback = %d to %q, want 303 to %q", resp.StatusCode, resp.Header.Get("Location"), want)
			}
		})
	}
}

// rejectCase is a login that must fail at the callback without a session.
type rejectCase struct {
	name   string
	setup  func(h *harness)                                   // before the login starts
	tamper func(h *harness, b *browser, cb *url.URL) *url.URL // between authorize and callback
	status int
	want   string // in the page body
}

func TestCallbackRejects(t *testing.T) {
	cases := []rejectCase{
		{name: "state mismatch", status: http.StatusBadRequest, want: "try again",
			tamper: func(h *harness, b *browser, cb *url.URL) *url.URL {
				q := cb.Query()
				q.Set("state", "not-the-state")
				cb.RawQuery = q.Encode()
				return cb
			}},
		{name: "missing state", status: http.StatusBadRequest, want: "try again",
			tamper: func(h *harness, b *browser, cb *url.URL) *url.URL {
				q := cb.Query()
				q.Del("state")
				cb.RawQuery = q.Encode()
				return cb
			}},
		{name: "missing code", status: http.StatusBadRequest, want: "try again",
			tamper: func(h *harness, b *browser, cb *url.URL) *url.URL {
				q := cb.Query()
				q.Del("code")
				cb.RawQuery = q.Encode()
				return cb
			}},
		{name: "no login cookie", status: http.StatusBadRequest, want: "try again",
			tamper: func(h *harness, b *browser, cb *url.URL) *url.URL {
				b.clearLoginCookie()
				return cb
			}},
		{name: "tampered login cookie", status: http.StatusBadRequest, want: "try again",
			tamper: func(h *harness, b *browser, cb *url.URL) *url.URL {
				b.tamperLoginCookie()
				return cb
			}},
		{name: "login took over 10 minutes", status: http.StatusBadRequest, want: "try again",
			tamper: func(h *harness, b *browser, cb *url.URL) *url.URL {
				h.clock.Advance(10*time.Minute + time.Second)
				return cb
			}},
		{name: "code already used", status: http.StatusInternalServerError, want: "Authentik",
			tamper: func(h *harness, b *browser, cb *url.URL) *url.URL {
				h.idp.set(func(f *fakeIDP) { clear(f.codes) })
				return cb
			}},
		{name: "provider refused", status: http.StatusForbidden, want: "access_denied",
			setup: func(h *harness) { h.idp.set(func(f *fakeIDP) { f.authorizeError = "access_denied" }) }},
		{name: "bad nonce", status: http.StatusForbidden, want: "be verified",
			setup: mutateIDToken(func(c map[string]any) { c["nonce"] = "some-other-nonce" })},
		{name: "no nonce", status: http.StatusForbidden, want: "be verified",
			setup: mutateIDToken(func(c map[string]any) { delete(c, "nonce") })},
		{name: "wrong audience", status: http.StatusForbidden, want: "be verified",
			setup: mutateIDToken(func(c map[string]any) { c["aud"] = "some-other-app" })},
		{name: "wrong issuer", status: http.StatusForbidden, want: "be verified",
			setup: mutateIDToken(func(c map[string]any) { c["iss"] = "https://evil.example/" })},
		{name: "expired ID token", status: http.StatusForbidden, want: "be verified",
			setup: mutateIDToken(func(c map[string]any) { c["exp"] = c["iat"].(int64) - 60 })},
		{name: "access token hash mismatch", status: http.StatusForbidden, want: "be verified",
			setup: mutateIDToken(func(c map[string]any) { c["at_hash"] = "AAAAAAAAAAAAAAAAAAAAAA" })},
		{name: "signed with an unknown key", status: http.StatusForbidden, want: "be verified",
			setup: func(h *harness) { h.idp.set(func(f *fakeIDP) { f.signWithOther = true }) }},
		{name: "groups claim of the wrong type", status: http.StatusInternalServerError, want: "groups",
			setup: mutateIDToken(func(c map[string]any) { c["groups"] = 42 })},
		{name: "userinfo for another subject", status: http.StatusForbidden, want: "be verified",
			setup: func(h *harness) {
				h.idp.set(func(f *fakeIDP) { f.groupsInIDToken = false; f.userinfoSub = "sub-mallory" })
			}},
		{name: "JWKS unavailable", status: http.StatusInternalServerError, want: "Authentik",
			setup: func(h *harness) { h.idp.set(func(f *fakeIDP) { f.jwksStatus = http.StatusServiceUnavailable }) }},
		{name: "token endpoint down", status: http.StatusInternalServerError, want: "Authentik",
			setup: func(h *harness) { h.idp.set(func(f *fakeIDP) { f.tokenStatus = http.StatusInternalServerError }) }},
		{name: "no refresh token", status: http.StatusInternalServerError, want: "offline_access",
			setup: func(h *harness) { h.idp.set(func(f *fakeIDP) { f.noRefreshToken = true }) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			b := h.browser()
			cb := b.startLogin(alice, "/private")
			if tc.tamper != nil {
				cb = tc.tamper(h, b, cb)
			}
			resp := b.do(http.MethodGet, cb.String(), nil, nil)
			if resp.StatusCode != tc.status || !strings.Contains(resp.body, tc.want) {
				t.Errorf("callback = %d %q, want %d mentioning %q", resp.StatusCode, resp.body, tc.status, tc.want)
			}
			if c := resp.cookie(auth.SessionCookie); c != nil && c.MaxAge >= 0 {
				t.Errorf("a failed login set a session cookie: %+v", c)
			}
			if c := resp.cookie("battleship_oidc"); c == nil || c.MaxAge >= 0 {
				t.Errorf("battleship_oidc after a failed callback = %+v, want it cleared", c)
			}
			if n := h.countSessions(); n != 0 {
				t.Errorf("%d sessions stored, want 0", n)
			}
			if !strings.Contains(h.logs.String(), "login failed") {
				t.Errorf("logs = %q, want a login failure", h.logs.String())
			}
		})
	}
}

func mutateIDToken(fn func(map[string]any)) func(h *harness) {
	return func(h *harness) { h.idp.set(func(f *fakeIDP) { f.mutateIDToken = fn }) }
}

func (b *browser) loginCookieFromJar() *http.Cookie {
	u, _ := url.Parse(b.h.app.URL + "/auth/")
	for _, c := range b.jar.Cookies(u) {
		if c.Name == "battleship_oidc" {
			return c
		}
	}
	return nil
}

func (b *browser) clearLoginCookie() {
	u, _ := url.Parse(b.h.app.URL + "/auth/")
	b.jar.SetCookies(u, []*http.Cookie{{Name: "battleship_oidc", Path: "/auth/", MaxAge: -1}})
}

func (b *browser) tamperLoginCookie() {
	c := b.loginCookieFromJar()
	if c == nil {
		b.h.t.Fatal("no login cookie")
	}
	v := []byte(c.Value)
	i := len(v) / 2
	if v[i] == 'A' {
		v[i] = 'B'
	} else {
		v[i] = 'A'
	}
	u, _ := url.Parse(b.h.app.URL + "/auth/")
	b.jar.SetCookies(u, []*http.Cookie{{Name: "battleship_oidc", Value: string(v), Path: "/auth/"}})
}

func TestLoginCookieFromAnotherLoginIsRejected(t *testing.T) {
	// Two tabs: the second login replaces the first's login cookie, so the
	// first tab's callback fails cleanly instead of mixing the two.
	h := newHarness(t)
	b := h.browser()
	first := b.startLogin(alice, "/private")
	b.startLogin(alice, "/private")
	resp := b.do(http.MethodGet, first.String(), nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("first tab's callback = %d, want 400", resp.StatusCode)
	}
}

// Anyone Authentik signs in gets a session, whatever their groups; Proxmox
// decides what they may do.
func TestAnyGroupSignsIn(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	stu := idpUser{Sub: "sub-stu", Name: "Stu Dent", Email: "stu@example.org", Groups: []string{"pool-07"}}
	h.pve.AddUser(pveUser(stu), nil)
	resp := b.login(stu)
	if !strings.Contains(resp.body, "groups=pool-07 pve=stu@auth.example.org") {
		t.Fatalf("body = %q", resp.body)
	}
}

func TestGroupsFromUserinfo(t *testing.T) {
	h := newHarness(t)
	h.idp.set(func(f *fakeIDP) { f.groupsInIDToken = false })
	b := h.browser()
	resp := b.login(lena)
	if !strings.Contains(resp.body, "groups=battleship-leads,battleship-operators") {
		t.Errorf("body = %q, want the groups read from userinfo", resp.body)
	}
}

func TestCustomGroupsClaim(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.OIDC.GroupsClaim = "roles" })
	h.idp.set(func(f *fakeIDP) {
		f.mutateIDToken = func(c map[string]any) { c["roles"] = c["groups"]; delete(c, "groups") }
	})
	b := h.browser()
	if resp := b.login(lena); !strings.Contains(resp.body, "groups=battleship-leads,battleship-operators") {
		t.Errorf("body = %q, want the groups from the roles claim", resp.body)
	}
}

func TestLoginReplacesOldSession(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	old := b.sessionToken()
	b.login(lena)
	if b.sessionToken() == old {
		t.Fatal("second login kept the old token")
	}
	h.sessionGone(old)
}

func TestLogout(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	token := b.sessionToken()
	resp := b.postForm("/auth/logout", url.Values{"csrf": {b.csrf()}}, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != h.idp.srv.URL+"/end-session" {
		t.Errorf("logout = %d to %q, want 303 to the provider's end_session_endpoint", resp.StatusCode, resp.Header.Get("Location"))
	}
	if c := resp.cookie(auth.SessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("session cookie after logout = %+v, want it cleared", c)
	}
	h.sessionGone(token)
	wantLoginRedirect(t, b.do(http.MethodGet, "/private", nil, nil), "/private")
	if !strings.Contains(h.logs.String(), `logout: subject="sub-alice"`) {
		t.Errorf("logs = %q, want a logout line", h.logs.String())
	}
}

func TestLogoutWithoutEndSessionGoesHome(t *testing.T) {
	h := newHarness(t)
	h.idp.set(func(f *fakeIDP) { f.noEndSession = true })
	h.restart(h.cfg) // discovery happens in NewService
	b := h.browser()
	b.login(alice)
	resp := b.postForm("/auth/logout", url.Values{"csrf": {b.csrf()}}, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("logout = %d to %q, want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLogoutNeedsCSRF(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	token := b.sessionToken()
	for name, form := range map[string]url.Values{"missing": {}, "wrong": {"csrf": {"nope"}}} {
		resp := b.postForm("/auth/logout", form, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s token: logout = %d, want 403", name, resp.StatusCode)
		}
	}
	if _, err := h.st.Session(ctx, auth.HashToken(token)); err != nil {
		t.Errorf("session after refused logouts: %v, want it kept", err)
	}
	// Logout is POST only.
	if resp := b.do(http.MethodGet, "/auth/logout", nil, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /auth/logout = %d, want 405", resp.StatusCode)
	}
}

func TestLogoutWithoutSessionGoesHome(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	resp := b.postForm("/auth/logout", url.Values{}, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("logout = %d to %q, want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestNewServiceChecksConfig(t *testing.T) {
	c := newClock()
	idp := newFakeIDP(t, c)
	st := storetest.New(t)
	good := webConfig(idp.issuer(), idp.clientID, idp.secret, "https://battleship.example.org")
	pve := pvetest.New(t).Client()
	cases := map[string]struct {
		mut  func(*config.Config)
		want string
	}{
		"no client secret":   {func(c *config.Config) { c.OIDC.ClientSecret = "" }, "oidc.client_secret"},
		"no offline_access":  {func(c *config.Config) { c.OIDC.Scopes = []string{"openid", "groups"} }, "offline_access"},
		"no base URL":        {func(c *config.Config) { c.Web.BaseURL = "" }, "web.base_url"},
		"issuer unreachable": {func(c *config.Config) { c.OIDC.Issuer = "http://127.0.0.1:1/" }, "oidc.issuer"},
		"issuer mismatch":    {func(c *config.Config) { c.OIDC.Issuer = idp.srv.URL + "/other/" }, "oidc.issuer"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := good
			cfg.OIDC.Scopes = slices.Clone(good.OIDC.Scopes)
			tc.mut(&cfg)
			_, err := auth.NewService(ctx, cfg, st, auth.Options{Now: c.Now, Proxmox: pve})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewService = %v, want an error mentioning %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), idp.secret) {
				t.Errorf("error shows the client secret: %v", err)
			}
		})
	}
	if _, err := auth.NewService(ctx, good, st, auth.Options{Proxmox: pve}); err != nil {
		t.Errorf("NewService(good) = %v", err)
	}
	if _, err := auth.NewService(ctx, good, st, auth.Options{}); err == nil || !strings.Contains(err.Error(), "Proxmox") {
		t.Errorf("NewService without a Proxmox client = %v", err)
	}
	if _, err := auth.NewService(ctx, good, nil, auth.Options{}); !errors.Is(err, auth.ErrNoStore) {
		t.Errorf("NewService(nil store) = %v, want ErrNoStore", err)
	}
}

// With an https base URL, the whole flow uses only the __Host- names.
func TestHostPrefixedCookiesWorkThroughTheFlow(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Web.BaseURL = "https://battleship.example.org" })
	b := h.browser()
	cb := b.startLogin(alice, "/private")
	if b.loginCookie == nil || b.loginCookie.Name != "__Host-battleship_oidc" {
		t.Fatalf("login cookie = %+v, want __Host-battleship_oidc", b.loginCookie)
	}
	// A login cookie under the plain name doesn't count.
	if resp := b.callbackDirect(cb, &http.Cookie{Name: "battleship_oidc", Value: b.loginCookie.Value}); resp.Code != http.StatusBadRequest {
		t.Errorf("callback with the plain login cookie name = %d, want 400", resp.Code)
	}
	cb = b.startLogin(alice, "/private")
	resp := b.callbackDirect(cb, b.loginCookie)
	var token string
	for _, c := range resp.Result().Cookies() {
		if c.Name == "__Host-battleship_session" {
			token = c.Value
		}
	}
	if resp.Code != http.StatusSeeOther || token == "" {
		t.Fatalf("callback = %d, cookies %v", resp.Code, resp.Result().Cookies())
	}
	serve := func(method, target, name string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: name, Value: token})
		rec := httptest.NewRecorder()
		h.mux.Load().ServeHTTP(rec, req)
		return rec
	}
	// The session works: it goes on to the Proxmox sign-in, not to log in.
	if rec := serve(http.MethodGet, "/private", "__Host-battleship_session", nil); rec.Code != http.StatusSeeOther ||
		rec.Header().Get("Location") != "/auth/proxmox?next=%2Fprivate" {
		t.Fatalf("/private with __Host-battleship_session = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	// As if that sign-in had finished.
	id := auth.HashToken(token)
	if err := h.st.SetSessionTicket(ctx, id, time.Time{}, auth.SealSessionTicket(h.cfg, id, h.pve.Ticket(pveUser(alice)))); err != nil {
		t.Fatal(err)
	}
	if rec := serve(http.MethodGet, "/private", "__Host-battleship_session", nil); rec.Code != http.StatusOK {
		t.Fatalf("/private with __Host-battleship_session = %d %s", rec.Code, rec.Body)
	}
	if rec := serve(http.MethodGet, "/private", "battleship_session", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("/private with the plain name = %d, want a login redirect", rec.Code)
	}
	csrf := serve(http.MethodGet, "/csrf", "__Host-battleship_session", nil).Body.String()
	rec := serve(http.MethodPost, "/auth/logout", "__Host-battleship_session", url.Values{"csrf": {csrf}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body)
	}
	var cleared *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "__Host-battleship_session" {
			cleared = c
		}
	}
	if cleared == nil || cleared.MaxAge >= 0 || !cleared.Secure || cleared.Path != "/" {
		t.Errorf("logout cookies = %v, want __Host-battleship_session cleared", rec.Result().Cookies())
	}
	h.sessionGone(token)
}
