package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// originCase is an Origin header and whether it names the site's origin,
// https://battleship.example.org.
type originCase struct {
	header string
	same   bool
}

var siteOrigins = []originCase{
	{"https://battleship.example.org", true},
	{"HTTPS://Battleship.Example.ORG", true},
	{"https://battleship.example.org:443", true},
	{"https://battleship.example.org:", true},
	{"https://battleship.example.org/path", true},
	{"https://battleship.example.org?x=1", true},
	{"https://user@battleship.example.org", true},
	{"https://battleship.example.org.", false},
	{"https://battleship.example.org:80", false},
	{"https://battleship.example.org:8443", false},
	{"https://battleship.example.org:0443", false},
	{"http://battleship.example.org", false},
	{"http://battleship.example.org:443", false},
	{"wss://battleship.example.org", false},
	{"null", false},
	{"https://battleship.example.org@evil.example", false},
	{"https://battleship.example.org.evil.example", false},
	{"https://evil.example/battleship.example.org", false},
	{"https://xn--battleship-9za.example.org", false},
	{"https://battleship\u00edp.example.org", false},
	{"https://[::1]", false},
	{"https://[::1]:443", false},
	{" https://battleship.example.org", false},
	{"//battleship.example.org", false},
	{"battleship.example.org", false},
}

// csrfRequest is a form request to /private carrying token in its body or
// query, with an Origin header if origin isn't nil.
func csrfRequest(method, token string, inBody bool, origin *string) *http.Request {
	form := url.Values{CSRFField: {token}}.Encode()
	target := "/private"
	var body *strings.Reader
	if inBody {
		body = strings.NewReader(form)
	} else {
		target += "?" + form
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != nil {
		r.Header.Set("Origin", *origin)
	}
	return r
}

// With the session's token in the body of a request Go reads a form from,
// the Origin alone decides.
func TestCSRFOrigins(t *testing.T) {
	base, _ := url.Parse("https://battleship.example.org")
	s := &Service{origin: normalOrigin(base)}
	const session = "c2Vzc2lvbi1jc3JmLXRva2VuLWZvci10ZXN0cw"
	for _, o := range siteOrigins {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
			if got := s.csrfProblem(csrfRequest(method, session, true, &o.header), session) == ""; got != o.same {
				t.Errorf("%s with Origin %q: passed %v, want %v", method, o.header, got, o.same)
			}
		}
	}
}

// A request passes the CSRF check exactly when its method is safe, or its
// form body carries the session's token and any Origin is the site's own.
func TestPropCSRFModel(t *testing.T) {
	base, _ := url.Parse("https://battleship.example.org")
	s := &Service{origin: normalOrigin(base)}
	const session = "c2Vzc2lvbi1jc3JmLXRva2VuLWZvci10ZXN0cw"
	rapid.Check(t, func(t *rapid.T) {
		method := rapid.SampledFrom([]string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}).Draw(t, "method")
		want := rapid.SampledFrom([]string{session, ""}).Draw(t, "session token")
		token := rapid.OneOf(
			rapid.Just(session), rapid.Just(""), rapid.Just(session[:20]), rapid.Just(session+"x"),
			rapid.Custom(func(t *rapid.T) string {
				i := rapid.IntRange(0, len(session)-1).Draw(t, "at")
				c := rapid.ByteRange('0', 'z').Filter(func(b byte) bool { return b != session[i] }).Draw(t, "byte")
				return session[:i] + string(c) + session[i+1:]
			}),
		).Draw(t, "token")
		inBody := rapid.Bool().Draw(t, "in body")
		absent := rapid.Bool().Draw(t, "no origin")
		known := rapid.Bool().Draw(t, "known origin")
		origin := originCase{header: rapid.String().Draw(t, "any origin")}
		if known {
			origin = rapid.SampledFrom(siteOrigins).Draw(t, "origin")
		}

		var header *string
		if !absent {
			header = &origin.header
		}
		r := csrfRequest(method, token, inBody, header)
		got := s.csrfProblem(r, want) == ""

		safe := method == http.MethodGet || method == http.MethodHead
		// Go reads form bodies only for POST, PUT and PATCH.
		sent := inBody && method != http.MethodDelete && token == want && want != ""
		switch {
		case absent || origin.header == "":
			origin.same = true // no Origin: the token decides
		case !known:
			if got && !safe && !sameSite(origin.header) {
				t.Fatalf("%s with Origin %q passed", method, origin.header)
			}
			return
		}
		if wantOK := safe || sent && origin.same; got != wantOK {
			t.Fatalf("%s token %q (session %q, in body %v) Origin %q (absent %v): passed %v, want %v",
				method, token, want, inBody, origin.header, absent, got, wantOK)
		}
	})
}

// sameSite reads o as a browser's Origin of https://battleship.example.org.
func sameSite(o string) bool {
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "battleship.example.org") &&
		(u.Port() == "" || u.Port() == "443")
}
