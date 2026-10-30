package web

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// A body over maxBody is refused with 413 before any handler, or auth's
// CSRF check, parses it: signed in or not, with or without a length. One at
// the cap works.
func TestBodyCap(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	// send posts a power preview form padded to n bytes.
	send := func(signedIn, chunked bool, target string, n int) *httptest.ResponseRecorder {
		form := "teams=1&action=stop&" + url.Values{auth.CSRFField: {op.CSRF}}.Encode() + "&pad="
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form+strings.Repeat("x", n-len(form))))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if signedIn {
			for _, c := range op.Cookies() {
				req.AddCookie(c)
			}
		}
		if chunked {
			req.ContentLength = -1
		}
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		return rec
	}
	for _, signedIn := range []bool{false, true} {
		for _, chunked := range []bool{false, true} {
			for _, target := range []string{"/power/preview", "/auth/logout"} {
				rec := send(signedIn, chunked, target, maxBody+1)
				if rec.Code != http.StatusRequestEntityTooLarge {
					t.Errorf("signed in %v, chunked %v: POST %s over the cap = %d, want 413", signedIn, chunked, target, rec.Code)
				}
				contains(t, "over-cap page", rec.Body.String(), "Request too large", "more than 1 MiB")
			}
			for _, n := range []int{60_000, maxBody - 1, maxBody} {
				want := http.StatusUnauthorized
				if signedIn {
					want = http.StatusOK
				}
				if rec := send(signedIn, chunked, "/power/preview", n); rec.Code != want {
					t.Errorf("signed in %v, chunked %v: preview of %d bytes = %d, want %d\n%s", signedIn, chunked, n, rec.Code, want, rec.Body)
				}
			}
		}
	}
	if rec := send(true, false, "/auth/logout", maxBody); rec.Code != http.StatusSeeOther {
		t.Errorf("logout at the cap = %d, want 303", rec.Code)
	}
	h.noJobs("after capped posts")
}

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
		{"login redirect", false, "/"},
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
	for _, path := range []string{"/", "/vm/01/dc", "/nope",
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

// forwardedModel is the client a request names, per forwarded's contract:
// an untrusted peer speaks for itself; a trusted one's X-Forwarded-For is
// read right to left as far as it parses, and the client is the first hop
// not trusted, else the leftmost hop read, else the peer. The scheme is the
// last X-Forwarded-Proto value a trusted peer sends.
func forwardedModel(trusted []netip.Prefix, remote string, xff, proto []string, tls bool) (string, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	isTrusted := func(a netip.Addr) bool {
		return slices.ContainsFunc(trusted, func(p netip.Prefix) bool { return p.Contains(a.Unmap()) })
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !isTrusted(peer) {
		return host, tls
	}
	var hops []string
	for _, v := range xff {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	var read []netip.Addr // right to left
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			break
		}
		read = append(read, a.Unmap())
	}
	ip := peer.Unmap().String()
	if i := slices.IndexFunc(read, func(a netip.Addr) bool { return !isTrusted(a) }); i >= 0 {
		ip = read[i].String()
	} else if len(read) > 0 {
		ip = read[len(read)-1].String()
	}
	https := tls
	if len(proto) > 0 {
		last := proto[len(proto)-1]
		https = strings.EqualFold(strings.TrimSpace(last[strings.LastIndex(last, ",")+1:]), "https")
	}
	return ip, https
}

// forwardedHops are X-Forwarded-For entries: trusted and other addresses,
// mapped and zoned ones, and junk.
var forwardedHops = []string{"10.0.0.5", "10.9.9.9", "fd00::5", "::ffff:10.0.0.7", "192.0.2.1", "198.51.100.1", "::ffff:192.0.2.1",
	"2001:db8::1", "", " ", "unknown", "192.0.2.1:80", "[2001:db8::1]", "1.2.3", "fe80::1%eth0", "fd00::1%eth0", "0300.0.0.1", "10.0.0.5 "}

// The client address and scheme follow the trusted-proxy model, and no
// handler sees a forwarding header.
func TestPropForwarded(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8"), netip.MustParsePrefix("127.0.0.1/32")}
	var got peer
	var leaked []string
	mw := forwarded(trusted, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.ip, got.https = clientIP(r), isHTTPS(r)
		leaked = nil
		for _, k := range forwardedHeaders {
			if r.Header.Values(k) != nil {
				leaked = append(leaked, k)
			}
		}
	}))
	check := func(t interface{ Fatalf(string, ...any) }, remote string, xff, proto []string, secure bool) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		if secure {
			req.TLS = &tls.ConnectionState{}
		}
		for _, v := range xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		for _, v := range proto {
			req.Header.Add("X-Forwarded-Proto", v)
		}
		req.Header.Set("Forwarded", "for=192.0.2.9")
		req.Header.Set("X-Real-Ip", "192.0.2.9")
		mw.ServeHTTP(httptest.NewRecorder(), req)
		ip, https := forwardedModel(trusted, remote, xff, proto, secure)
		if got.ip != ip || got.https != https || len(leaked) > 0 {
			t.Fatalf("peer %s, XFF %q, proto %q, TLS %v: client %s https %v, leaked %v; want %s https %v",
				remote, xff, proto, secure, got.ip, got.https, leaked, ip, https)
		}
	}
	remotes := []string{"10.1.2.3:4444", "[::ffff:10.1.2.3]:4444", "[fd00::1]:80", "127.0.0.1:1", "203.0.113.9:5555", "[2001:db8::9]:1",
		"[fe80::1%eth0]:1", "garbage", "10.1.2.3"}
	for _, remote := range remotes {
		for _, hop := range forwardedHops {
			check(t, remote, []string{hop + ", 10.9.9.9"}, []string{"http", "HTTPS "}, false)
			check(t, remote, []string{"198.51.100.1", hop}, []string{"https, http"}, true)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		hop := rapid.OneOf(rapid.SampledFrom(forwardedHops[:8]), rapid.SampledFrom(forwardedHops))
		header := rapid.Custom(func(t *rapid.T) string {
			return strings.Join(rapid.SliceOfN(hop, 0, 4).Draw(t, "hops"), rapid.SampledFrom([]string{",", ", ", " ,"}).Draw(t, "sep"))
		})
		proto := rapid.SampledFrom([]string{"https", "http", "HTTPS", " https ", "https, http", "http,https", "", "wss"})
		check(t, rapid.SampledFrom(remotes).Draw(t, "remote"), rapid.SliceOfN(header, 0, 2).Draw(t, "xff"),
			rapid.SliceOfN(proto, 0, 2).Draw(t, "proto"), rapid.Bool().Draw(t, "tls"))
	})
}
