package auth_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/store"
)

// hostileBody is an error page as a proxy (or a hostile provider) might
// send: it tries to forge a log line and is far longer than a log needs.
var hostileBody = "<html>blocked\nauth: login: subject=\"forged\"\n" + strings.Repeat("x", 2000) + "BODYTAIL</html>"

// checkQuotedBody checks the logs show an identity-provider body only
// quoted (no raw newline from it) and truncated (not its tail).
func checkQuotedBody(t *testing.T, logs string) {
	t.Helper()
	if strings.Contains(logs, "\nauth: login: subject=\"forged\"") {
		t.Errorf("a body's newline reached the logs unquoted:\n%s", logs)
	}
	if strings.Contains(logs, "BODYTAIL") {
		t.Errorf("a body reached the logs untruncated:\n%s", logs)
	}
	if !strings.Contains(logs, `blocked\nauth: login: subject=\"forged\"`) {
		t.Errorf("logs don't show the start of the body, quoted:\n%s", logs)
	}
}

func TestProviderBodiesAreQuotedAndTruncated(t *testing.T) {
	t.Run("userinfo at login", func(t *testing.T) {
		h := newHarness(t)
		h.idp.set(func(f *fakeIDP) {
			f.groupsInIDToken = false
			f.userinfoStatus = http.StatusForbidden
			f.failBody = hostileBody
		})
		b := h.browser()
		h.idp.setUser(alice)
		if resp := b.get("/auth/login?next=/private"); resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("login = %d, want 500", resp.StatusCode)
		}
		checkQuotedBody(t, h.logs.String())
	})
	t.Run("userinfo at refresh", func(t *testing.T) {
		h := newHarness(t)
		b := h.browser()
		b.login(alice)
		h.idp.set(func(f *fakeIDP) {
			f.groupsInIDToken = false
			f.userinfoStatus = http.StatusServiceUnavailable
			f.failBody = hostileBody
		})
		h.clock.Advance(5 * time.Minute)
		if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("/private during a userinfo outage = %d, want 200", resp.StatusCode)
		}
		checkQuotedBody(t, h.logs.String())
	})
	t.Run("JWKS at refresh", func(t *testing.T) {
		h := newHarness(t)
		b := h.browser()
		b.login(alice)
		h.idp.set(func(f *fakeIDP) {
			f.kid = "k2" // forces a key fetch
			f.jwksStatus = http.StatusBadGateway
			f.failBody = hostileBody
		})
		h.clock.Advance(5 * time.Minute)
		if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("/private during a JWKS outage = %d, want 200", resp.StatusCode)
		}
		checkQuotedBody(t, h.logs.String())
	})
}

// A token answer that isn't an OAuth error and can't be parsed (here a 200
// HTML page) is logged quoted, like the provider's other answers.
func TestUnparsableTokenAnswerIsQuoted(t *testing.T) {
	h := newHarness(t)
	h.idp.set(func(f *fakeIDP) {
		f.tokenStatus = http.StatusOK
		f.failBody = hostileBody
		f.failContentType = "application/json"
	})
	b := h.browser()
	h.idp.setUser(alice)
	if resp := b.get("/auth/login?next=/private"); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("login = %d, want 500", resp.StatusCode)
	}
	want := `exchanging the code: "oauth2: cannot parse json: invalid character '<' looking for beginning of value"`
	if !strings.Contains(h.logs.String(), want) {
		t.Errorf("logs don't show the parse error quoted (%s):\n%s", want, h.logs)
	}
}

// Refresh tokens are sealed at rest: the database holds a versioned
// AES-GCM box, not the token, at login and after a rotation.
func TestRefreshTokenIsSealedAtRest(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	check := func(when string) {
		t.Helper()
		stored := b.session().RefreshToken
		if !strings.HasPrefix(stored, "v1:") {
			t.Errorf("%s: stored refresh token %q has no v1: prefix", when, stored)
		}
		for _, rt := range h.idp.refreshTokens() {
			if strings.Contains(stored, rt) || strings.Contains(stored, strings.TrimPrefix(rt, "rt-")) {
				t.Errorf("%s: the stored value contains the provider's refresh token", when)
			}
		}
		if rt := b.refreshToken(); !slices.Contains(h.idp.refreshTokens(), rt) {
			t.Errorf("%s: the stored value opens to %q, not a live refresh token", when, rt)
		}
	}
	check("after login")
	h.clock.Advance(5 * time.Minute)
	if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: %d", resp.StatusCode)
	}
	if h.idp.refreshCount() != 1 {
		t.Fatalf("refreshed %d times, want 1", h.idp.refreshCount())
	}
	check("after a rotation")
}

// A sealed token copied into another session's row doesn't open; that
// session is rejected at its next refresh without spending the token.
func TestSealedRefreshTokenIsBoundToItsSession(t *testing.T) {
	h := newHarness(t)
	ba, bl := h.browser(), h.browser()
	ba.login(alice)
	bl.login(lena)
	aliceSealed, lenaSess := ba.session().RefreshToken, bl.session()
	if _, err := auth.OpenRefreshToken(h.svc, lenaSess.ID, aliceSealed); err == nil {
		t.Fatal("alice's sealed token opened as lena's")
	}
	err := h.st.UpdateSessionGroups(ctx, lenaSess.ID, store.SessionUpdate{
		Name: lenaSess.Name, Email: lenaSess.Email, Groups: lenaSess.Groups,
		RefreshToken: aliceSealed, RefreshedAt: lenaSess.RefreshedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := bl.sessionToken()
	h.clock.Advance(5 * time.Minute)
	wantLoginRedirect(t, bl.do(http.MethodGet, "/private", nil, nil), "/private")
	h.sessionGone(token)
	if n := h.idp.tokenRequestCount(); n != 2 { // the two logins
		t.Errorf("%d token requests, want only the 2 logins", n)
	}
	if logs := h.logs.String(); !strings.Contains(logs, `refresh rejected: subject="sub-lena"`) {
		t.Errorf("logs = %q, want lena's refresh rejected", logs)
	}
}

// Rotating the client secret invalidates stored refresh tokens: sessions
// are rejected at their next refresh and must log in again.
func TestClientSecretRotationEndsSessionsAtRefresh(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	token := b.sessionToken()
	h.idp.set(func(f *fakeIDP) { f.secret = "rotated-secret" })
	cfg := h.cfg
	cfg.OIDC.ClientSecret = "rotated-secret"
	h.restart(cfg)

	h.clock.Advance(5*time.Minute - time.Second)
	// The Proxmox ticket is sealed with a key from the secret too, so it no
	// longer opens: the session signs in to Proxmox again, silently.
	if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusSeeOther ||
		resp.Header.Get("Location") != "/auth/proxmox?next=%2Fprivate" {
		t.Fatalf("before the refresh: %d to %q, want the Proxmox sign-in", resp.StatusCode, resp.Header.Get("Location"))
	}
	h.pve.SignIn(pveUser(alice))
	if resp := b.get("/private"); resp.StatusCode != http.StatusOK {
		t.Fatalf("before the refresh: %d, want 200", resp.StatusCode)
	}
	h.clock.Advance(time.Second)
	wantLoginRedirect(t, b.do(http.MethodGet, "/private", nil, nil), "/private")
	h.sessionGone(token)
	if n := h.idp.tokenRequestCount(); n != 1 {
		t.Errorf("%d token requests, want only the login's", n)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, `refresh rejected: subject="sub-alice"`) || !strings.Contains(logs, "client_secret") {
		t.Errorf("logs = %q, want the rejection naming the client secret", logs)
	}
	// Logging in again works under the new secret.
	b.login(alice)
}

// unsafeAlgs are ID token signatures battleship must never accept, even when the
// provider's discovery document advertises them.
var unsafeAlgs = []string{"none", "HS256-secret", "HS256-jwk"}

func advertiseUnsafe(f *fakeIDP) { f.advertiseAlgs = []string{"none", "HS256"} }

func TestUnsafeIDTokenAlgorithmsAreRejectedAtLogin(t *testing.T) {
	for _, alg := range unsafeAlgs {
		t.Run(alg, func(t *testing.T) {
			h := newHarness(t)
			h.idp.set(advertiseUnsafe)
			h.restart(h.cfg) // rediscover with the advertised algorithms
			h.idp.set(func(f *fakeIDP) { f.signAlg = alg })
			b := h.browser()
			cb := b.startLogin(alice, "/private")
			resp := b.do(http.MethodGet, cb.String(), nil, nil)
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(resp.body, "be verified") {
				t.Errorf("callback = %d %q, want 403 unverified", resp.StatusCode, resp.body)
			}
			// Refused for its algorithm, before any key is tried.
			if logs := h.logs.String(); !strings.Contains(logs, "unexpected signature algorithm") {
				t.Errorf("logs = %q, want the algorithm refused", logs)
			}
			if n := h.countSessions(); n != 0 {
				t.Errorf("%d sessions stored, want 0", n)
			}
		})
	}
}

func TestUnsafeIDTokenAlgorithmsAreRejectedAtRefresh(t *testing.T) {
	for _, alg := range unsafeAlgs {
		t.Run(alg, func(t *testing.T) {
			h := newHarness(t)
			h.idp.set(advertiseUnsafe)
			h.restart(h.cfg)
			b := h.browser()
			b.login(alice)
			token := b.sessionToken()
			h.idp.set(func(f *fakeIDP) { f.signAlg = alg })
			h.clock.Advance(5 * time.Minute)
			wantLoginRedirect(t, b.do(http.MethodGet, "/private", nil, nil), "/private")
			h.sessionGone(token)
			if logs := h.logs.String(); !strings.Contains(logs, `refresh rejected: subject="sub-alice"`) ||
				!strings.Contains(logs, "unexpected signature algorithm") {
				t.Errorf("logs = %q, want the rejection for the algorithm", logs)
			}
		})
	}
}

// checkNoLeak checks the logs hold none of the given secrets, nor any of
// an echoed token endpoint body.
func checkNoLeak(t *testing.T, logs string, secrets map[string]string) {
	t.Helper()
	if strings.Contains(logs, echoMarker) {
		t.Errorf("logs contain the token endpoint's body:\n%s", logs)
	}
	for name, v := range secrets {
		if v != "" && strings.Contains(logs, v) {
			t.Errorf("logs contain the %s:\n%s", name, logs)
		}
	}
}

// A token endpoint that echoes the request in its error body doesn't get
// any of it logged.
func TestFailureLogsOmitCodeBodyAndSecret(t *testing.T) {
	for _, jsonEcho := range []bool{false, true} {
		name := "html page"
		if jsonEcho {
			name = "oauth error"
		}
		t.Run("login/"+name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser()
			cb := b.startLogin(alice, "/private")
			h.idp.set(func(f *fakeIDP) { f.tokenEcho = http.StatusBadRequest; f.tokenEchoJSON = jsonEcho })
			resp := b.do(http.MethodGet, cb.String(), nil, nil)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("callback = %d, want 500", resp.StatusCode)
			}
			logs := h.logs.String()
			if !strings.Contains(logs, "login failed") {
				t.Errorf("logs = %q, want the login failure", logs)
			}
			var verifier string
			h.idp.set(func(f *fakeIDP) { verifier = f.tokenRequests[len(f.tokenRequests)-1].Get("code_verifier") })
			checkNoLeak(t, logs, map[string]string{
				"authorization code": cb.Query().Get("code"), "PKCE verifier": verifier, "client secret": h.idp.secret,
			})
		})
		t.Run("refresh/"+name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser()
			b.login(alice)
			rt := b.refreshToken()
			h.idp.set(func(f *fakeIDP) { f.tokenEcho = http.StatusBadRequest; f.tokenEchoJSON = jsonEcho })
			h.clock.Advance(5 * time.Minute)
			b.do(http.MethodGet, "/private", nil, nil)
			logs := h.logs.String()
			if !strings.Contains(logs, "refresh rejected") && !strings.Contains(logs, "refresh failed") {
				t.Errorf("logs = %q, want the refresh failure", logs)
			}
			checkNoLeak(t, logs, map[string]string{"refresh token": rt, "client secret": h.idp.secret})
		})
	}
}
