package auth_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Through RequireUser, a request the CSRF check refuses gets the 403 page
// and never reaches the handler; one it passes does.
func TestPropCSRFRefusalsRunNothing(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	good := b.csrf()
	origin := h.app.URL
	u, _ := url.Parse(origin)
	rapid.Check(t, func(t *rapid.T) {
		method := rapid.SampledFrom([]string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}).Draw(t, "method")
		token := rapid.SampledFrom([]string{good, "", good[:20], flipFirst(good), good + "x"}).Draw(t, "token")
		type originCase struct {
			header string
			same   bool
		}
		o := rapid.SampledFrom([]originCase{
			{"", true}, {origin, true}, {strings.ToUpper(u.Scheme) + "://" + u.Host, true}, {origin + "/x", true},
			{"https://" + u.Host, false}, {"http://" + u.Hostname() + ":1", false}, {"http://localhost:" + u.Port(), false},
			{"null", false}, {origin + ".evil.example", false},
		}).Draw(t, "origin")
		header := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
		if o.header != "" {
			header.Set("Origin", o.header)
		}
		resp := b.do(method, "/private", strings.NewReader(url.Values{"csrf": {token}}.Encode()), header)
		safe := method == http.MethodGet || method == http.MethodHead
		// Go parses form bodies only for POST, PUT and PATCH.
		ok := safe || method != http.MethodDelete && token == good && o.same
		ran := strings.Contains(resp.body, " ok: user=")
		switch {
		case ok && (resp.StatusCode != http.StatusOK || method != http.MethodHead && !ran):
			t.Fatalf("%s token %q Origin %q = %d %q, want the handler", method, token, o.header, resp.StatusCode, resp.body)
		case !ok && (resp.StatusCode != http.StatusForbidden || ran || !strings.Contains(resp.body, "Request refused")):
			t.Fatalf("%s token %q Origin %q = %d %q, want the 403 page", method, token, o.header, resp.StatusCode, resp.body)
		}
	})
}
