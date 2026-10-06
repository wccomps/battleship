package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth"
)

func TestAnonymousRequests(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	wantLoginRedirect(t, b.do(http.MethodGet, "/private?team=3&x=a%20b", nil, nil), "/private?team=3&x=a%20b")
	wantLoginRedirect(t, b.do(http.MethodHead, "/private", nil, nil), "/private")
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if resp := b.do(m, "/private", nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d, want 401", m, resp.StatusCode)
		}
	}
}

func TestUnknownOrMalformedCookie(t *testing.T) {
	h := newHarness(t)
	for name, value := range map[string]string{
		"unknown token": strings.Repeat("A", 43),
		"too short":     "abc",
		"not base64url": strings.Repeat("*", 43),
	} {
		t.Run(name, func(t *testing.T) {
			b := h.browser()
			resp := b.do(http.MethodGet, "/private", nil, http.Header{"Cookie": {auth.SessionCookie + "=" + value}})
			wantLoginRedirect(t, resp, "/private")
			if c := resp.cookie(auth.SessionCookie); c == nil || c.MaxAge >= 0 {
				t.Errorf("bad session cookie = %+v, want it cleared", c)
			}
		})
	}
}

func TestIdleExpiry(t *testing.T) {
	h := newHarness(t) // session_idle 30m, session_refresh 5m
	b := h.browser()
	b.login(alice)
	token := b.sessionToken()
	// Activity every 29 minutes keeps the session alive well past 30m.
	for i := 0; i < 3; i++ {
		h.clock.Advance(29 * time.Minute)
		if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("after %d idle periods: %d %s", i+1, resp.StatusCode, resp.body)
		}
	}
	h.clock.Advance(30 * time.Minute)
	wantLoginRedirect(t, b.do(http.MethodGet, "/private", nil, nil), "/private")
	h.sessionGone(token)
	if !strings.Contains(h.logs.String(), `session expired: subject="sub-alice"`) {
		t.Errorf("logs = %q, want the expiry", h.logs.String())
	}
}

func TestAbsoluteExpiry(t *testing.T) {
	h := newHarness(t) // session_max 12h
	b := h.browser()
	b.login(alice)
	token := b.sessionToken()
	start := h.clock.Now()
	for h.clock.Now().Sub(start) < 12*time.Hour-20*time.Minute {
		h.clock.Advance(20 * time.Minute)
		if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("at %v: %d %s", h.clock.Now().Sub(start), resp.StatusCode, resp.body)
		}
	}
	// The expiry never slides past created_at + session_max.
	if exp := b.session().ExpiresAt; !exp.Equal(start.Add(12 * time.Hour)) {
		t.Errorf("ExpiresAt = %v, want capped at %v", exp, start.Add(12*time.Hour))
	}
	h.clock.Advance(20 * time.Minute) // 12h exactly, still active
	wantLoginRedirect(t, b.do(http.MethodGet, "/private", nil, nil), "/private")
	h.sessionGone(token)
}

func TestShorterSessionMaxAppliesToExistingSessions(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	// A redeploy with session_max 1h ends a 2h-old session even though its
	// stored expiry is later.
	h.clock.Advance(2 * time.Hour)
	if err := h.st.TouchSession(ctx, auth.HashToken(b.sessionToken()), h.clock.Now(), h.clock.Now().Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cfg := h.cfg
	cfg.Web.SessionMax = time.Hour
	h.restart(cfg)
	wantLoginRedirect(t, b.do(http.MethodGet, "/private", nil, nil), "/private")
}

func TestActivityExtendsCookie(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	h.clock.Advance(3 * time.Minute)
	resp := b.do(http.MethodGet, "/private", nil, nil)
	c := resp.cookie(auth.SessionCookie)
	if c == nil || c.MaxAge != 1800 || c.Value != b.sessionToken() {
		t.Errorf("cookie after activity = %+v, want the same token with Max-Age 1800", c)
	}
	sess := b.session()
	if !sess.LastSeen.Equal(h.clock.Now()) || !sess.ExpiresAt.Equal(h.clock.Now().Add(30*time.Minute)) {
		t.Errorf("LastSeen %v ExpiresAt %v, want now and now+30m", sess.LastSeen, sess.ExpiresAt)
	}
	// Touches are throttled: another request a second later writes nothing.
	h.clock.Advance(time.Second)
	resp = b.do(http.MethodGet, "/private", nil, nil)
	if c := resp.cookie(auth.SessionCookie); c != nil {
		t.Errorf("cookie re-set a second after a touch: %+v", c)
	}
	if got := b.session().LastSeen; got.Equal(h.clock.Now()) {
		t.Error("LastSeen written a second after the last touch")
	}
}

func TestCookieMaxAgeNeverPassesAbsoluteExpiry(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	for i := 0; i < 35; i++ { // 11h40m of activity
		h.clock.Advance(20 * time.Minute)
		b.do(http.MethodGet, "/private", nil, nil)
	}
	h.clock.Advance(10 * time.Minute) // 11h50m: 10 minutes left
	resp := b.do(http.MethodGet, "/private", nil, nil)
	if c := resp.cookie(auth.SessionCookie); c == nil || c.MaxAge != 600 {
		t.Errorf("cookie = %+v, want Max-Age 600 (what's left of session_max)", c)
	}
}

func TestRefreshRereadsGroups(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(lena)
	firstRT := b.refreshToken()

	// An admin removes Lena from battleship-leads. Until the next refresh
	// her session keeps the groups it had.
	h.idp.setGroups(lena.Sub, "battleship-operators")
	h.clock.Advance(5*time.Minute - time.Second)
	if resp := b.do(http.MethodGet, "/private", nil, nil); !strings.Contains(resp.body, "groups=battleship-leads,battleship-operators") {
		t.Fatalf("before the refresh: /private = %q", resp.body)
	}
	if n := h.idp.refreshCount(); n != 0 {
		t.Fatalf("refreshed %d times before session_refresh passed", n)
	}

	h.clock.Advance(time.Second)
	if resp := b.do(http.MethodGet, "/private", nil, nil); !strings.Contains(resp.body, "groups=battleship-operators pve") {
		t.Errorf("/private = %q, want the new groups", resp.body)
	}
	sess := b.session()
	if rt := b.refreshToken(); rt == firstRT || !slices.Contains(h.idp.refreshTokens(), rt) {
		t.Errorf("refresh token not rotated: %q", rt)
	}
	if !sess.RefreshedAt.Equal(h.clock.Now()) {
		t.Errorf("RefreshedAt = %v, want now", sess.RefreshedAt)
	}
	if n := h.idp.refreshCount(); n != 1 {
		t.Errorf("refreshed %d times, want 1", n)
	}
}

func TestRefreshWithoutIDTokenUsesUserinfo(t *testing.T) {
	h := newHarness(t)
	h.idp.set(func(f *fakeIDP) { f.noIDTokenRefresh = true })
	b := h.browser()
	b.login(lena)
	h.idp.setGroups(lena.Sub, "battleship-operators")
	h.clock.Advance(5 * time.Minute)
	if resp := b.do(http.MethodGet, "/private", nil, nil); !strings.Contains(resp.body, "groups=battleship-operators pve") {
		t.Errorf("/private = %d %q, want the groups from userinfo", resp.StatusCode, resp.body)
	}
}

func TestRefreshRejectedEndsSession(t *testing.T) {
	// The provider (or its token) says no: the session ends at once, with
	// no grace.
	cases := map[string]func(h *harness){
		"invalid_grant": func(h *harness) { h.idp.revokeAll() },
		"client rejected": func(h *harness) {
			h.idp.set(func(f *fakeIDP) { f.secret = "rotated" }) // 401 invalid_client
		},
		"ID token for another user": func(h *harness) {
			h.idp.set(func(f *fakeIDP) { f.mutateIDToken = func(c map[string]any) { c["sub"] = "sub-mallory" } })
		},
		"ID token for another app": func(h *harness) {
			h.idp.set(func(f *fakeIDP) { f.mutateIDToken = func(c map[string]any) { c["aud"] = "other" } })
		},
		"ID token signed with an unknown key": func(h *harness) { h.idp.set(func(f *fakeIDP) { f.signWithOther = true }) },
		"userinfo 401": func(h *harness) {
			h.idp.set(func(f *fakeIDP) { f.groupsInIDToken = false; f.userinfoStatus = http.StatusUnauthorized })
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser()
			b.login(alice)
			token := b.sessionToken()
			refreshToken := b.refreshToken()
			breakIt(h)
			h.clock.Advance(5 * time.Minute)
			wantLoginRedirect(t, b.do(http.MethodGet, "/private", nil, nil), "/private")
			h.sessionGone(token)
			logs := h.logs.String()
			if !strings.Contains(logs, `refresh rejected: subject="sub-alice"`) {
				t.Errorf("logs = %q, want the rejection", logs)
			}
			if strings.Contains(logs, refreshToken) {
				t.Errorf("logs contain the refresh token: %q", logs)
			}
		})
	}
}

// outages are ways the identity provider can be unreachable during a
// refresh. Each returns a function that ends the outage.
var outages = map[string]func(h *harness) (restore func()){
	"token endpoint 500": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) { f.tokenStatus = http.StatusInternalServerError })
		return func() { h.idp.set(func(f *fakeIDP) { f.tokenStatus = 0 }) }
	},
	"token endpoint 503": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) { f.tokenStatus = http.StatusServiceUnavailable })
		return func() { h.idp.set(func(f *fakeIDP) { f.tokenStatus = 0 }) }
	},
	"JWKS down during a key rotation": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) { f.kid = "k2"; f.jwksStatus = http.StatusInternalServerError })
		return func() { h.idp.set(func(f *fakeIDP) { f.jwksStatus = 0 }) }
	},
	"userinfo 503": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) { f.groupsInIDToken = false; f.userinfoStatus = http.StatusServiceUnavailable })
		return func() { h.idp.set(func(f *fakeIDP) { f.userinfoStatus = 0 }) }
	},
	// Only a 400 or 401 carrying an OAuth error code is a rejection; other
	// answers come from a struggling provider or a proxy in front of it.
	"token endpoint 429": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) { f.tokenStatus = http.StatusTooManyRequests })
		return func() { h.idp.set(func(f *fakeIDP) { f.tokenStatus = 0 }) }
	},
	"proxy 403 page": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) {
			f.tokenStatus = http.StatusForbidden
			f.failBody = "<html><h1>403 Forbidden</h1>WAF</html>"
		})
		return func() { h.idp.set(func(f *fakeIDP) { f.tokenStatus = 0; f.failBody = "" }) }
	},
	"proxy 404 page": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) { f.tokenStatus = http.StatusNotFound; f.failBody = "<html>Not Found</html>" })
		return func() { h.idp.set(func(f *fakeIDP) { f.tokenStatus = 0; f.failBody = "" }) }
	},
	"proxy 400 page without an OAuth error": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) { f.tokenStatus = http.StatusBadRequest; f.failBody = "<html>400 Bad Request</html>" })
		return func() { h.idp.set(func(f *fakeIDP) { f.tokenStatus = 0; f.failBody = "" }) }
	},
	"token endpoint 401 plain text": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) {
			f.tokenStatus = http.StatusUnauthorized
			f.failBody = "Unauthorized"
			f.failContentType = "text/plain"
		})
		return func() { h.idp.set(func(f *fakeIDP) { f.tokenStatus = 0; f.failBody = ""; f.failContentType = "" }) }
	},
	"garbled 200": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) { f.tokenStatus = http.StatusOK; f.failBody = "<html>maintenance</html>" })
		return func() { h.idp.set(func(f *fakeIDP) { f.tokenStatus = 0; f.failBody = "" }) }
	},
	"connection dropped": func(h *harness) func() {
		h.idp.set(func(f *fakeIDP) { f.dropConnections = true })
		return func() { h.idp.set(func(f *fakeIDP) { f.dropConnections = false }) }
	},
}

func TestRefreshUnreachableKeepsSession(t *testing.T) {
	for name, outage := range outages {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser()
			b.login(lena)
			refreshed := b.session().RefreshedAt
			h.idp.setGroups(lena.Sub, "staff") // unseen until a refresh succeeds
			outage(h)
			h.clock.Advance(5 * time.Minute)
			resp := b.do(http.MethodGet, "/private", nil, nil)
			if resp.StatusCode != http.StatusOK || !strings.Contains(resp.body, "groups=battleship-leads,battleship-operators") {
				t.Fatalf("during the outage: %d %q, want 200 with the existing groups", resp.StatusCode, resp.body)
			}
			sess := b.session()
			now := h.clock.Now()
			if !sess.RefreshedAt.Equal(refreshed) || !sess.RefreshFailedAt.Equal(now) || !sess.RefreshRetryAt.Equal(now.Add(time.Minute)) {
				t.Errorf("refreshed %v failed %v retry %v; want unchanged, now, now+1m", sess.RefreshedAt, sess.RefreshFailedAt, sess.RefreshRetryAt)
			}
			logs := h.logs.String()
			if !strings.Contains(logs, `refresh failed: subject="sub-lena"`) || !strings.Contains(logs, "keeping the session") ||
				!strings.Contains(logs, "until 2026-10-03T09:15:00Z") {
				t.Errorf("logs = %q, want the failure and the end of the grace", logs)
			}
			if strings.Contains(logs, "<nil>") {
				t.Errorf("logs = %q, want the reason for the failure", logs)
			}
		})
	}
}

func TestRefreshRetriesAfterBackoff(t *testing.T) {
	for name, outage := range outages {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser()
			b.login(lena)
			restore := outage(h)
			h.clock.Advance(5 * time.Minute)
			b.do(http.MethodGet, "/private", nil, nil) // fails, backs off
			restore()
			h.idp.setGroups(lena.Sub, "battleship-operators")
			tried := h.idp.tokenRequestCount()

			h.clock.Advance(time.Minute - time.Second)
			if resp := b.do(http.MethodGet, "/private", nil, nil); !strings.Contains(resp.body, "groups=battleship-leads,battleship-operators") {
				t.Errorf("during the backoff: %d %q, want the existing groups", resp.StatusCode, resp.body)
			}
			if n := h.idp.tokenRequestCount(); n != tried {
				t.Errorf("%d token requests during the backoff, want none", n-tried)
			}

			h.clock.Advance(time.Second)
			if resp := b.do(http.MethodGet, "/private", nil, nil); !strings.Contains(resp.body, "groups=battleship-operators pve") {
				t.Errorf("after the backoff: %d %q, want the refreshed groups", resp.StatusCode, resp.body)
			}
			sess := b.session()
			if !sess.RefreshedAt.Equal(h.clock.Now()) || !sess.RefreshFailedAt.IsZero() || !sess.RefreshRetryAt.IsZero() {
				t.Errorf("refreshed %v failed %v retry %v; want now and cleared", sess.RefreshedAt, sess.RefreshFailedAt, sess.RefreshRetryAt)
			}
		})
	}
}

func TestOutageLongerThanGraceEndsSession(t *testing.T) {
	// session_refresh is 5m, so the grace ends 15m after the last success.
	cases := map[string]struct {
		served []time.Duration // requests served within the grace, as clock steps
		last   time.Duration   // the step to the request that ends the session
	}{
		// Retries fail every 1m30s, up to 14m; the retry at 15m30s is past
		// the grace.
		"at a failed retry": {
			served: []time.Duration{5 * time.Minute, 90 * time.Second, 90 * time.Second, 90 * time.Second,
				90 * time.Second, 90 * time.Second, 90 * time.Second},
			last: 90 * time.Second,
		},
		// The retry at 14m30s fails; at 15m the grace has run out but the
		// session is backing off, so it is kept until its next retry fails
		// at 15m30s.
		"after the backoff": {
			served: []time.Duration{5 * time.Minute, 9*time.Minute + 30*time.Second, 30 * time.Second},
			last:   30 * time.Second,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser()
			b.login(alice)
			token := b.sessionToken()
			start := h.clock.Now()
			h.idp.set(func(f *fakeIDP) { f.tokenStatus = http.StatusInternalServerError })
			for _, d := range tc.served {
				h.clock.Advance(d)
				if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusOK {
					t.Fatalf("at %v: %d, want 200 until a retry fails past the grace", h.clock.Now().Sub(start), resp.StatusCode)
				}
			}
			h.clock.Advance(tc.last)
			if h.clock.Now().Sub(start) < 15*time.Minute {
				t.Fatalf("test steps end at %v, before the grace", h.clock.Now().Sub(start))
			}
			wantLoginRedirect(t, b.do(http.MethodGet, "/private", nil, nil), "/private")
			h.sessionGone(token)
			if logs := h.logs.String(); !strings.Contains(logs, "no successful refresh since") {
				t.Errorf("logs = %q, want the grace to have run out", logs)
			}
		})
	}
}

func TestRefreshSurvivesClientDisconnect(t *testing.T) {
	// The provider rotates the refresh token as it answers. If the refresh
	// were tied to the browser's request, a user closing the tab mid-refresh
	// would leave the session holding a spent token, ending it at the next
	// refresh.
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	h.clock.Advance(5 * time.Minute)

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	h.idp.set(func(f *fakeIDP) {
		f.onRefresh = func(r *http.Request) bool {
			cancel() // the browser goes away
			// If the app's call to the provider was cut off with the
			// browser, this request's context ends quickly; otherwise
			// answer after a short grace period.
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
			return false
		}
	})
	req := httptest.NewRequestWithContext(reqCtx, http.MethodGet, "/private", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: b.sessionToken()})
	h.mux.Load().ServeHTTP(httptest.NewRecorder(), req)
	h.idp.set(func(f *fakeIDP) { f.onRefresh = nil })

	if rt := b.refreshToken(); !slices.Contains(h.idp.refreshTokens(), rt) {
		t.Fatalf("stored refresh token %q is no longer valid at the provider", rt)
	}
	h.clock.Advance(5 * time.Minute)
	if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("next refresh: %d, want 200", resp.StatusCode)
	}
}

func TestRefreshFailureOnPostIs401(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	csrf := b.csrf()
	h.idp.revokeAll()
	h.clock.Advance(5 * time.Minute)
	resp := b.postForm("/private", url.Values{"csrf": {csrf}}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST after a failed refresh = %d, want 401", resp.StatusCode)
	}
}

func TestConcurrentRequestsRefreshOnce(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	h.clock.Advance(5 * time.Minute)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := b.raw.Get(h.app.URL + "/private")
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d = %d, want 200", i, c)
		}
	}
	// The provider rotates refresh tokens, so spending one twice would have
	// failed a request and ended the session.
	if n := h.idp.refreshCount(); n != 1 {
		t.Errorf("refreshed %d times, want exactly 1", n)
	}
	if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("after the refresh: %d", resp.StatusCode)
	}
}

func TestCSRFMatrix(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	good := b.csrf()
	other := h.browser()
	other.login(lena)
	othersToken := other.csrf()
	origin := h.app.URL

	type req struct {
		form   url.Values
		query  string
		header http.Header
	}
	cases := []struct {
		name string
		req  req
		ok   bool
	}{
		{"no token", req{}, false},
		{"empty token", req{form: url.Values{"csrf": {""}}}, false},
		{"wrong token", req{form: url.Values{"csrf": {flipFirst(good)}}}, false},
		{"truncated token", req{form: url.Values{"csrf": {good[:20]}}}, false},
		{"another session's token", req{form: url.Values{"csrf": {othersToken}}}, false},
		{"token in the query string only", req{query: "?csrf=" + url.QueryEscape(good)}, false},
		{"token in form", req{form: url.Values{"csrf": {good}}}, true},
		{"token in a header only", req{header: http.Header{"X-Csrf-Token": {good}}}, false},
		{"a header doesn't spoil a good form", req{form: url.Values{"csrf": {good}}, header: http.Header{"X-Csrf-Token": {"nope"}}}, true},
		{"good token, matching Origin", req{form: url.Values{"csrf": {good}}, header: http.Header{"Origin": {origin}}}, true},
		{"good token, foreign Origin", req{form: url.Values{"csrf": {good}}, header: http.Header{"Origin": {"https://evil.example"}}}, false},
		{"good token, null Origin", req{form: url.Values{"csrf": {good}}, header: http.Header{"Origin": {"null"}}}, false},
		{"good token, Origin with a path trick", req{form: url.Values{"csrf": {good}}, header: http.Header{"Origin": {origin + ".evil.example"}}}, false},
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, tc := range cases {
			t.Run(m+"/"+tc.name, func(t *testing.T) {
				header := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
				for k, v := range tc.req.header {
					header[k] = v
				}
				resp := b.do(m, "/private"+tc.req.query, strings.NewReader(tc.req.form.Encode()), header)
				// Go parses form bodies only for POST, PUT and PATCH, so a
				// DELETE never carries the token.
				ok := tc.ok && m != http.MethodDelete
				switch {
				case ok && (resp.StatusCode != http.StatusOK || resp.body[:len(m)] != m):
					t.Errorf("%d %q, want 200", resp.StatusCode, resp.body)
				case !ok && resp.StatusCode != http.StatusForbidden:
					t.Errorf("%d %q, want 403", resp.StatusCode, resp.body)
				}
			})
		}
	}
	// Safe methods need no token, and a foreign Origin doesn't matter.
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		resp := b.do(m, "/private", nil, http.Header{"Origin": {"https://evil.example"}})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s without a token = %d, want 200", m, resp.StatusCode)
		}
	}
	logs := h.logs.String()
	if !strings.Contains(logs, `csrf rejected: subject="sub-alice" POST "/private"`) {
		t.Errorf("logs = %q, want CSRF rejections", logs)
	}
	if strings.Contains(logs, good) || strings.Contains(logs, othersToken) {
		t.Error("logs contain a CSRF token")
	}
}

func TestCSRFRejectedBeforeRefresh(t *testing.T) {
	// A forged request must not make the session spend its refresh token.
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	h.clock.Advance(5 * time.Minute)
	resp := b.postForm("/private", url.Values{}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged POST = %d, want 403", resp.StatusCode)
	}
	if n := h.idp.refreshCount(); n != 0 {
		t.Errorf("forged POST refreshed the session %d times", n)
	}
}

func TestCSRFTokenIsPerSession(t *testing.T) {
	h := newHarness(t)
	a, l := h.browser(), h.browser()
	a.login(alice)
	l.login(lena)
	ta, tl := a.csrf(), l.csrf()
	if ta == tl || len(ta) != 43 {
		t.Errorf("CSRF tokens %q and %q, want distinct 32-byte tokens", ta, tl)
	}
	if ta != a.session().CSRF {
		t.Error("CSRFToken(ctx) doesn't match the stored token")
	}
	// It stays the same across requests and refreshes.
	h.clock.Advance(5 * time.Minute)
	if got := a.csrf(); got != ta {
		t.Errorf("token changed after a refresh: %q -> %q", ta, got)
	}
}

// SessionID names the session in the store, for records that belong to
// it (the web app's previews): the token's hash, never the token.
func TestSessionIDIsTheStoredKey(t *testing.T) {
	h := newHarness(t)
	a, l := h.browser(), h.browser()
	a.login(alice)
	l.login(lena)
	ida := a.do(http.MethodGet, "/session-id", nil, nil).body
	idl := l.do(http.MethodGet, "/session-id", nil, nil).body
	if ida != auth.HashToken(a.sessionToken()) || ida != a.session().ID {
		t.Errorf("SessionID = %q, want the stored ID %q", ida, a.session().ID)
	}
	if ida == idl {
		t.Error("two sessions have the same SessionID")
	}
	if got := auth.SessionID(context.Background()); got != "" {
		t.Errorf("SessionID outside RequireUser = %q, want empty", got)
	}
}

// SessionRefreshedAt is the session's last group check: its login, then
// each refresh, including one the request itself just made.
func TestSessionRefreshedAt(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	at := func() string { return b.do(http.MethodGet, "/refreshed-at", nil, nil).body }
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	login := h.clock.Now()
	if got := at(); got != stamp(login) {
		t.Errorf("after login = %s, want %s", got, stamp(login))
	}
	h.clock.Advance(time.Minute)
	if got := at(); got != stamp(login) {
		t.Errorf("a minute later = %s, want the login's %s", got, stamp(login))
	}
	h.clock.Advance(5 * time.Minute) // refresh due: this request makes it
	if got, want := at(), stamp(h.clock.Now()); got != want {
		t.Errorf("on the refreshing request = %s, want %s", got, want)
	}
	if got := auth.SessionRefreshedAt(context.Background()); !got.IsZero() {
		t.Errorf("outside RequireUser = %v, want zero", got)
	}
}

func TestLogsHaveNoSecrets(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(lena)
	token, csrf, rt1, sealed1 := b.sessionToken(), b.csrf(), b.refreshToken(), b.session().RefreshToken
	h.clock.Advance(5 * time.Minute)
	b.do(http.MethodGet, "/private", nil, nil)
	rt2, sealed2 := b.refreshToken(), b.session().RefreshToken
	b.postForm("/private", url.Values{}, nil) // CSRF rejection
	b.postForm("/auth/logout", url.Values{"csrf": {csrf}}, nil)
	logs := h.logs.String()
	for name, secret := range map[string]string{
		"session token": token, "token hash": auth.HashToken(token), "csrf": csrf,
		"refresh token": rt1, "rotated refresh token": rt2, "client secret": h.idp.secret,
		"sealed refresh token": sealed1, "sealed rotated refresh token": sealed2,
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain the %s", name)
		}
	}
	for _, want := range []string{"login:", "csrf rejected:", "logout:"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q: %s", want, logs)
		}
	}
}

// flipFirst changes a token's first character, so the result always differs.
func flipFirst(tok string) string {
	if tok[0] == 'A' {
		return "B" + tok[1:]
	}
	return "A" + tok[1:]
}

func TestTrackSubject(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	token, csrf := b.sessionToken(), b.csrf()

	// serve runs one request through the app the way a request log does:
	// from outside, with only TrackSubject to see who it was.
	serve := func(method, target string, cookie bool, csrf string) (int, string) {
		req := httptest.NewRequest(method, target, strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie {
			req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
		}
		ctx, subject := auth.TrackSubject(req.Context())
		rec := httptest.NewRecorder()
		h.mux.Load().ServeHTTP(rec, req.WithContext(ctx))
		return rec.Code, subject()
	}
	for _, c := range []struct {
		name           string
		method, target string
		cookie         bool
		csrf           string
		status         int
		subject        string
	}{
		{"signed in", http.MethodGet, "/private", true, "", http.StatusOK, alice.Sub},
		{"signed-in POST", http.MethodPost, "/private", true, csrf, http.StatusOK, alice.Sub},
		{"refused for CSRF, still attributed", http.MethodPost, "/private", true, "wrong", http.StatusForbidden, alice.Sub},
		{"anonymous", http.MethodGet, "/private", false, "", http.StatusSeeOther, ""},
		{"route without a session check", http.MethodGet, "/auth/login", true, "", http.StatusFound, ""},
		{"logout refused for CSRF", http.MethodPost, "/auth/logout", true, "wrong", http.StatusForbidden, alice.Sub},
		{"anonymous logout", http.MethodPost, "/auth/logout", false, "", http.StatusSeeOther, ""},
	} {
		status, subject := serve(c.method, c.target, c.cookie, c.csrf)
		if status != c.status || subject != c.subject {
			t.Errorf("%s: %s %s = %d, subject %q; want %d, %q", c.name, c.method, c.target, status, subject, c.status, c.subject)
		}
	}

	h.clock.Advance(31 * time.Minute) // past session_idle
	if status, subject := serve(http.MethodGet, "/private", true, ""); status != http.StatusSeeOther || subject != "" {
		t.Errorf("expired session = %d, subject %q; want 303 and no subject", status, subject)
	}
}

// An event stream can't follow a redirect to the login page, so a request
// that asks for one gets 401 instead, anonymous or with an ended session;
// the page's script then reloads to log in.
func TestEventStreamGets401(t *testing.T) {
	h := newHarness(t)
	sse := http.Header{"Accept": {"text/event-stream"}}
	b := h.browser()
	for _, accept := range []string{"text/event-stream", "text/html, text/event-stream;q=0.9"} {
		resp := b.do(http.MethodGet, "/private", nil, http.Header{"Accept": {accept}})
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Location") != "" {
			t.Errorf("anonymous stream (Accept %q) = %d to %q, want 401", accept, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	b.login(alice)
	if resp := b.do(http.MethodGet, "/private", nil, sse); resp.StatusCode != http.StatusOK {
		t.Fatalf("stream with a live session = %d, want 200", resp.StatusCode)
	}
	h.clock.Advance(31 * time.Minute) // past session_idle
	if resp := b.do(http.MethodGet, "/private", nil, sse); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("stream with an expired session = %d, want 401", resp.StatusCode)
	}
	// A page still redirects to the login.
	wantLoginRedirect(t, b.do(http.MethodGet, "/private", nil, http.Header{"Accept": {"text/html"}}), "/private")
}
