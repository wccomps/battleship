package auth

import (
	"net/url"
	"path"
	"strings"
)

// maxNext bounds the next URL a login carries in its cookie.
const maxNext = 2048

// safeNext returns next if it is a clean path on this site outside /auth, else
// "/", so login is neither an open redirect nor a loop.
func safeNext(next string) string {
	// http.Redirect cleans everything before the query, a fragment included.
	if p, _, _ := strings.Cut(next, "?"); len(next) > maxNext || strings.Contains(p, "#") {
		return "/"
	}
	u := LocalURL(next)
	if u == nil {
		return "/"
	}
	// u.Path is decoded, so this refuses dot and empty segments in any spelling.
	if c := path.Clean(u.Path); u.Path != c && (c == "/" || u.Path != c+"/") {
		return "/"
	}
	if first, _, _ := strings.Cut(u.Path[1:], "/"); first == "auth" {
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
