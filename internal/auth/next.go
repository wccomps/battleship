package auth

import (
	"net/url"
	"path"
	"strings"
)

// maxNext bounds the next URL a login carries in its cookie.
const maxNext = 2048

// safeNext returns next if it is a path on this site, else "/", so login
// isn't an open redirect. Paths under /auth/ are refused to avoid loops.
func safeNext(next string) string {
	u := LocalURL(next)
	if u == nil || len(next) > maxNext {
		return "/"
	}
	// Browsers resolve dot segments (including %2e) before requesting.
	if p := path.Clean("/" + u.Path); p == "/auth" || strings.HasPrefix(p, "/auth/") {
		return "/"
	}
	return next
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
