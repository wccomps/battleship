package auth_test

import (
	"html"
	"net/http"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// isErrorCodeModel is whether e is an OAuth error code worth repeating: one
// to 64 lower-case ASCII letters and underscores.
func isErrorCodeModel(e string) bool {
	if len(e) < 1 || len(e) > 64 {
		return false
	}
	for i := 0; i < len(e); i++ {
		if (e[i] < 'a' || e[i] > 'z') && e[i] != '_' {
			return false
		}
	}
	return true
}

// A provider's error is repeated on the page only when it is a plain code;
// anything else shows as unknown_error, and the login still fails.
func TestPropCallbackErrorEcho(t *testing.T) {
	h := newHarness(t)
	codes := []string{"access_denied", "a", "_", "server_error", "A", "Access_Denied", "access-denied", "access denied", "a1", "é", "<b>x</b>", "a\n", " a", "a ",
		strings.Repeat("a", 64), strings.Repeat("a", 65), strings.Repeat("_", 64), "javascript:alert(1)", "%41", "a&b", `a"b`}
	check := func(t interface{ Fatalf(string, ...any) }, e string) {
		b := h.browser()
		cb := b.startLogin(alice, "/")
		q := cb.Query()
		q.Del("code")
		q.Set("error", e)
		cb.RawQuery = q.Encode()
		resp := b.do(http.MethodGet, cb.String(), nil, nil)
		want := e
		if !isErrorCodeModel(e) {
			want = "unknown_error"
		}
		if e == "" {
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("empty error: status %d", resp.StatusCode)
			}
			return
		}
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(resp.body, "("+want+")") {
			t.Fatalf("error=%q: %d, want 403 showing (%s)\n%s", e, resp.StatusCode, want, resp.body)
		}
		if !isErrorCodeModel(e) && strings.Contains(resp.body, html.EscapeString(e)) && len(e) > 3 {
			t.Fatalf("error=%q repeated on the page", e)
		}
	}
	for _, e := range codes {
		check(t, e)
	}
	rapid.Check(t, func(t *rapid.T) {
		e := rapid.OneOf(rapid.StringMatching(`[a-z_]{1,70}`), rapid.StringMatching(`[a-zA-Z_ -]{1,10}`), rapid.String(), rapid.SampledFrom(codes)).Draw(t, "error")
		check(t, e)
	})
}
