package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	wantCSP := "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self' " + originOf(t, h.cfg.OIDC.Issuer)

	for _, tc := range []struct {
		name string
		sess bool
		path string
	}{
		{"grid", true, "/"},
		{"help", true, "/help"},
		{"login redirect", false, "/"},
		{"access page", false, "/access"},
		{"not found", false, "/nope"},
		{"script", false, "/static/app.js"},
		{"health", false, "/healthz"},
		{"readiness", false, "/readyz"},
		{"grid events", true, "/events/grid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hdr http.Header
			if tc.path == "/events/grid" {
				c := h.openSSE(&op, tc.path)
				hdr = c.resp.Header
			} else {
				var rec *httptest.ResponseRecorder
				if tc.sess {
					rec = h.get(&op, tc.path)
				} else {
					rec = h.get(nil, tc.path)
				}
				hdr = rec.Header()
			}
			for k, want := range map[string]string{
				"Content-Security-Policy": wantCSP,
				"X-Frame-Options":         "DENY",
				"Referrer-Policy":         "same-origin",
				"X-Content-Type-Options":  "nosniff",
			} {
				if got := hdr.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if got := hdr.Get("Strict-Transport-Security"); got != "" {
				t.Errorf("Strict-Transport-Security over plain HTTP = %q, want none", got)
			}
		})
	}

	// Pages are per user and never cached.
	if got := h.get(&op, "/").Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("grid Cache-Control = %q, want no-store", got)
	}
}

func TestPagesHaveNoInlineScript(t *testing.T) {
	h := newHarness(t)
	h.poll()
	lead := h.login(asLead)
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byLead)
	for _, path := range []string{"/", "/vm/01/dc", "/help", "/access", "/nope",
		"/power", "/reset?teams=1", "/deploy", "/teardown", "/logs", "/logs/" + itoa(id)} {
		body := h.get(&lead, path).Body.String()
		lacks(t, path, body, "<script>", "onclick=", "onload=", "style=", "javascript:")
	}
	body := h.post(&lead, "/teardown/preview", url.Values{"teams": {"1"}}).Body.String()
	lacks(t, "preview", body, "<script>", "onclick=", "onload=", "style=", "javascript:")
}

func originOf(t *testing.T, raw string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, raw, nil)
	return req.URL.Scheme + "://" + req.URL.Host
}

func TestTrustedProxies(t *testing.T) {
	var got struct {
		ip     string
		secure bool
		fwd    string
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.ip = clientIP(r)
		got.secure = isHTTPS(r)
		got.fwd = r.Header.Get("X-Forwarded-For") + "|" + r.Header.Get("X-Forwarded-Proto") + "|" + r.Header.Get("X-Forwarded-Host")
	})
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	mw := forwarded(trusted, echo)

	for _, tc := range []struct {
		name       string
		remote     string
		xff, proto string
		wantIP     string
		wantSecure bool
	}{
		{"direct, no headers", "203.0.113.9:5555", "", "", "203.0.113.9", false},
		{"untrusted peer's headers ignored", "203.0.113.9:5555", "198.51.100.1", "https", "203.0.113.9", false},
		{"trusted proxy", "10.1.2.3:4444", "198.51.100.1", "https", "198.51.100.1", true},
		{"trusted chain: rightmost untrusted wins", "10.1.2.3:4444", "6.6.6.6, 198.51.100.1, 10.9.9.9", "https", "198.51.100.1", true},
		{"spoofed left entry can't hide the client", "10.1.2.3:4444", "10.0.0.1, 198.51.100.1", "http", "198.51.100.1", false},
		{"all trusted: leftmost", "10.1.2.3:4444", "10.5.5.5, 10.9.9.9", "", "10.5.5.5", false},
		{"garbage entry stops the walk", "10.1.2.3:4444", "198.51.100.1, nonsense", "https", "10.1.2.3", true},
		{"trusted, no XFF", "10.1.2.3:4444", "", "https", "10.1.2.3", true},
		{"IPv6 trusted proxy", "[fd00::1]:80", "2001:db8::7", "https", "2001:db8::7", true},
		{"multiple proto values: last one", "10.1.2.3:4444", "198.51.100.1", "https, http", "198.51.100.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remote
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			req.Header.Set("X-Forwarded-Host", "evil.example")
			mw.ServeHTTP(httptest.NewRecorder(), req)
			if got.ip != tc.wantIP || got.secure != tc.wantSecure {
				t.Errorf("client = %s secure %v, want %s secure %v", got.ip, got.secure, tc.wantIP, tc.wantSecure)
			}
			if got.fwd != "||" {
				t.Errorf("handlers still see X-Forwarded-* headers %q; they must use clientIP and isHTTPS", got.fwd)
			}
		})
	}
}

func TestHSTSOnlyOverHTTPS(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Web.TrustedProxies = []string{"192.0.2.0/24"} })
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "192.0.2.10:1234" // httptest's default peer
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("HSTS via a trusted TLS proxy = %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec = httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS from an untrusted peer's header = %q, want none", got)
	}
	_, _ = io.Copy(io.Discard, rec.Body)
}

func TestRequestLog(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Web.TrustedProxies = []string{"192.0.2.0/24"} })
	h.poll()
	op := h.login(asOperator)

	// Reading pages isn't logged.
	h.get(&op, "/")
	h.get(nil, "/healthz")
	if strings.Contains(h.logs.String(), "web: GET") {
		t.Errorf("logs = %q, want no GET lines", h.logs)
	}

	// Anything that could change something is, with the forwarded client.
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", "198.51.100.23")
	req.AddCookie(op.Cookie)
	h.h.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(h.logs.String(), `web: POST "/auth/logout" 403 from 198.51.100.23 in `) {
		t.Errorf("logs = %q, want the refused POST with the client", h.logs)
	}
}
