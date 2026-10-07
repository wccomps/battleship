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
	if next == "" || len(next) > maxNext || next[0] != '/' {
		return "/"
	}
	if len(next) > 1 && (next[1] == '/' || next[1] == '\\') {
		return "/"
	}
	for _, c := range next {
		if c == '\\' || c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "/"
	}
	// Browsers resolve dot segments (including %2e) before requesting.
	if p := path.Clean("/" + u.Path); p == "/auth" || strings.HasPrefix(p, "/auth/") {
		return "/"
	}
	return next
}
