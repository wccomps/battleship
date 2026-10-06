package auth

import (
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
		if dest.Path == "/auth" || strings.HasPrefix(dest.Path, "/auth/") {
			t.Fatalf("safeNext(%q) = %q leads back into login", in, out)
		}
		if out != "/" && out != in {
			t.Fatalf("safeNext(%q) = %q; it may only keep next or replace it with /", in, out)
		}
	})
}
