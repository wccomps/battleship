package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                              "/",
		"/":                             "/",
		"/jobs":                         "/jobs",
		"/jobs/5?page=2#items":          "/jobs/5?page=2#items",
		"/vm/01/dc":                     "/vm/01/dc",
		"//evil.example":                "/",
		"//evil.example/path":           "/",
		"///evil.example":               "/",
		"/\\evil.example":               "/",
		"/x\\y":                         "/",
		"\\\\evil.example":              "/",
		"https://evil.example/":         "/",
		"http:/evil.example":            "/",
		"javascript:alert(1)":           "/",
		"evil.example":                  "/",
		"jobs":                          "/",
		"/jobs\r\nSet-Cookie:x":         "/",
		"/jobs\x00":                     "/",
		"/jobs\t":                       "/",
		"/auth/login":                   "/",
		"/auth/callback?code=x":         "/",
		"/auth":                         "/",
		"/./auth/login":                 "/",
		"/%2e%2e/auth/login":            "/",
		"/jobs/../auth/login":           "/",
		"/auth//..":                     "/",
		"/auth//../x":                   "/",
		"/x/..//auth/login":             "/",
		"/jobs/../help":                 "/jobs/../help",
		"/%2F%2Fevil.example":           "/%2F%2Fevil.example",
		"/authors":                      "/authors",
		"/" + strings.Repeat("a", 2048): "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSafeNextStaysOnSite proves that whatever next a login is given, the
// browser is sent somewhere on this site, outside /auth/.
func TestSafeNextStaysOnSite(t *testing.T) {
	base, _ := url.Parse("https://battleship.example.org/")
	rapid.Check(t, func(t *rapid.T) {
		in := rapid.OneOf(
			rapid.String(),
			rapid.StringOf(rapid.SampledFrom([]rune{'/', '\\', ':', '.', '@', '?', '#', '%', 'a', 'e', 'v', 'l', '\t', '\n', ' '})),
			rapid.Map(rapid.String(), func(s string) string { return "/" + s }),
		).Draw(t, "next")
		checkSafeNext(t, base, in)
	})
}

// checkSafeNext checks that safeNext(in) is in or "/", and that a browser
// following it stays on base's site and out of login.
func checkSafeNext(t interface{ Fatalf(string, ...any) }, base *url.URL, in string) {
	out := safeNext(in)
	if !strings.HasPrefix(out, "/") || strings.HasPrefix(out, "//") || strings.ContainsAny(out, "\\\r\n\t\x00") {
		t.Fatalf("safeNext(%q) = %q", in, out)
	}
	ref, err := url.Parse(out)
	if err != nil {
		t.Fatalf("safeNext(%q) = %q, which doesn't parse: %v", in, out, err)
	}
	dest := base.ResolveReference(ref)
	if dest.Scheme != base.Scheme || dest.Host != base.Host {
		t.Fatalf("safeNext(%q) = %q leads to %s", in, out, dest)
	}
	// The server's mux then cleans the path, redirecting //auth to /auth.
	if p := path.Clean(dest.Path); p == "/auth" || strings.HasPrefix(p, "/auth/") {
		t.Fatalf("safeNext(%q) = %q leads back into login", in, out)
	}
	if out != "/" && out != in {
		t.Fatalf("safeNext(%q) = %q; it may only keep next or replace it with /", in, out)
	}
}

func TestPropInterstitialStaysLocal(t *testing.T) {
	hostile := rapid.OneOf(rapid.String(), rapid.SampledFrom([]string{
		"//evil.example", "javascript:alert(1)", ";url=https://evil.example", "\r\n", "%2F%2Fevil", "\\", "finish", "",
	}))
	rapid.Check(t, func(t *rapid.T) {
		route := rapid.SampledFrom([]string{"/auth/proxmox", "/auth/proxmox/callback"}).Draw(t, "route")
		r := httptest.NewRequest(http.MethodGet, route, nil)
		if rapid.Bool().Draw(t, "raw query") {
			r.URL.RawQuery = hostile.Draw(t, "query")
		} else {
			q := url.Values{}
			for range rapid.IntRange(0, 3).Draw(t, "keys") {
				q.Add(hostile.Draw(t, "key"), hostile.Draw(t, "value"))
			}
			r.URL.RawQuery = q.Encode()
		}
		if mode := rapid.SampledFrom([]string{"", "navigate", "cors", "no-cors", "other"}).Draw(t, "mode"); mode != "" {
			r.Header.Set("Sec-Fetch-Mode", mode)
		}
		var shown *Page
		s := &Service{render: func(_ http.ResponseWriter, _ *http.Request, p Page) { shown = &p }}
		if !s.interstitialFirst(httptest.NewRecorder(), r) {
			if shown != nil {
				t.Fatalf("rendered %+v without saying so", shown)
			}
			return
		}
		if shown == nil {
			t.Fatal("said it showed the interstitial but rendered nothing")
		}
		u := LocalURL(shown.Refresh)
		if u == nil || u.Path != route || u.Query().Get("finish") != "1" || shown.Link != "" {
			t.Fatalf("interstitial for %s?%s goes on to %q (link %q)", route, r.URL.RawQuery, shown.Refresh, shown.Link)
		}
	})
}
