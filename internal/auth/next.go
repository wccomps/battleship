package auth

import (
	"net/url"
	"strings"
)

// maxNext bounds the next URL a login carries in its cookie.
const maxNext = 2048

// safeNext returns next if it is a path on this site outside /auth/, else
// "/", so login is neither an open redirect nor a loop.
func safeNext(next string) string {
	if len(next) > maxNext || LocalURL(next) == nil {
		return "/"
	}
	// Browsers resolve dot segments (any %2e spelling) and the mux cleans empty
	// ones, so no page a browser reached has either.
	p := next[:strings.IndexAny(next+"?", "?#")]
	segs := strings.Split(p[1:], "/")
	for i, seg := range segs {
		if seg == "" && i < len(segs)-1 || isDotSegment(seg) {
			return "/"
		}
	}
	// The mux routes on each segment unescaped, so /%61uth is /auth.
	if first, err := url.PathUnescape(segs[0]); err != nil || first == "auth" {
		return "/"
	}
	return next
}

func isDotSegment(s string) bool {
	switch strings.ToLower(s) {
	case ".", "%2e", "..", ".%2e", "%2e.", "%2e%2e":
		return true
	}
	return false
}

// LocalURL parses s if it is a path on this site, else returns nil: the
// only addresses a page may send the browser on to.
func LocalURL(s string) *url.URL {
	if s == "" || s[0] != '/' {
		return nil
	}
	if len(s) > 1 && (s[1] == '/' || s[1] == '\\') {
		return nil
	}
	for _, c := range s {
		if c == '\\' || c < 0x20 || c == 0x7f {
			return nil
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return nil
	}
	return u
}
