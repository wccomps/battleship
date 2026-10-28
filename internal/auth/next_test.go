package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
		"/jobs/../help":                 "/",
		"/x%2Fy/../auth/login":          "/",
		"/a%2Fb%2Fc/../auth/login":      "/",
		"/x//%2e%2e/auth/login":         "/",
		"/x/%2E%2e/auth/login":          "/",
		"/x/.%2e/auth":                  "/",
		"/%61uth/login":                 "/",
		"/jobs/":                        "/jobs/",
		"/x%2F..%2F..%2Fauth/login":     "/",
		"/x#/../auth/login":             "/",
		"/x#/../../auth/proxmox?next=/": "/",
		"/jobs#x":                       "/",
		"/jobs?x=/../auth#y/../z":       "/jobs?x=/../auth#y/../z",
		"/auth.x/y":                     "/auth.x/y",
		"/%2F%2Fevil.example":           "/",
		"/%2F":                          "/",
		"/authors":                      "/authors",
		"/" + strings.Repeat("a", 2048): "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// A clean path outside /auth, with any query, is kept as it is.
func TestPropSafeNextKeepsCleanPaths(t *testing.T) {
	segment := rapid.OneOf(rapid.StringMatching(`[a-z0-9][a-z0-9._~-]{0,8}`), rapid.SampledFrom([]string{"authors", "auth.x", "%41uth", "01"}))
	rapid.Check(t, func(t *rapid.T) {
		segs := rapid.SliceOfN(segment, 1, 4).Filter(func(s []string) bool { return s[0] != "auth" }).Draw(t, "segments")
		in := "/" + strings.Join(segs, "/") + rapid.SampledFrom([]string{"", "/"}).Draw(t, "slash") +
			rapid.SampledFrom([]string{"", "?", "?page=2", "?x=/../auth#y/../z", "?next=/auth/login"}).Draw(t, "query")
		if got := safeNext(in); got != in {
			t.Fatalf("safeNext(%q) = %q, want it kept", in, got)
		}
	})
}

// TestSafeNextStaysOnSite checks that whatever next a login is given, the
// browser is sent somewhere on this site that no auth route serves.
func TestSafeNextStaysOnSite(t *testing.T) {
	segment := rapid.SampledFrom([]string{"x", "", ".", "..", ".%2e", "%2E%2e", "x%2Fy", "x%2F..", "auth", "\\", "@",
		"x%2Fy/..", "auth//..", "/%2e%2e", "x#"})
	tail := rapid.SampledFrom([]string{"auth", "%61uth", "auth/login", "x"})
	segmented := rapid.Custom(func(t *rapid.T) string {
		return "/" + strings.Join(append(rapid.SliceOfN(segment, 0, 4).Draw(t, "segments"), tail.Draw(t, "tail")), "/")
	})
	rapid.Check(t, func(t *rapid.T) {
		in := rapid.OneOf(
			rapid.String(),
			rapid.StringOf(rapid.SampledFrom([]rune{'/', '\\', ':', '.', '@', '?', '#', '%', 'a', 'e', 'v', 'l', '\t', '\n', ' '})),
			rapid.Map(rapid.String(), func(s string) string { return "/" + s }),
			segmented, segmented, segmented, segmented,
		).Draw(t, "next")
		in += rapid.SampledFrom([]string{"", "?next=/auth/login", "#x", "#/../auth/login"}).Draw(t, "suffix")
		checkSafeNext(t, in)
	})
}

var authMux = func() *http.ServeMux {
	mux := http.NewServeMux()
	(&Service{}).Routes(mux)
	mux.HandleFunc("/", http.NotFound)
	return mux
}()

func checkSafeNext(t interface{ Fatalf(string, ...any) }, in string) {
	out := safeNext(in)
	if out != "/" && out != in {
		t.Fatalf("safeNext(%q) = %q; it may only keep next or replace it with /", in, out)
	}
	w := httptest.NewRecorder()
	http.Redirect(w, httptest.NewRequest(http.MethodGet, "/auth/login", nil), out, http.StatusSeeOther)
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") || strings.ContainsAny(loc, "\\\r\n\t\x00") {
		t.Fatalf("safeNext(%q) = %q, sent as Location %q", in, out, loc)
	}
	u, err := url.ParseRequestURI(browserPath(loc))
	if err != nil {
		t.Fatalf("safeNext(%q) = %q, sent as Location %q: %v", in, out, loc, err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.URL = u
	if _, pattern := authMux.Handler(r); pattern != "/" {
		t.Fatalf("safeNext(%q) = %q, sent as Location %q, leads to %s", in, out, loc, pattern)
	}
}

// browserPath trims loc and resolves its dot segments, %2e spellings included,
// as WHATWG URL parsing does.
func browserPath(loc string) string {
	loc = strings.Trim(loc, " ")
	p := loc[:strings.IndexAny(loc+"?", "?#")]
	segs := strings.Split(p[1:], "/")
	var stack []string
	for i, seg := range segs {
		last := i == len(segs)-1
		switch strings.ToLower(seg) {
		case "..", ".%2e", "%2e.", "%2e%2e":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if last {
				stack = append(stack, "")
			}
		case ".", "%2e":
			if last {
				stack = append(stack, "")
			}
		default:
			stack = append(stack, seg)
		}
	}
	return "/" + strings.Join(stack, "/")
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
