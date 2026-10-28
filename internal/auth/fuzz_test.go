package auth

import (
	"net/url"
	"testing"
)

// FuzzSafeNext explores next values for one that leaves the site or loops
// back into login.
func FuzzSafeNext(f *testing.F) {
	for _, s := range []string{"/", "/grid?x=1#y", "//evil.example", "/\\evil.example", "/%2e%2e/auth/login", "/auth/../grid",
		"https://evil.example/", "/a/../../auth", "/\t/evil", "/x@evil.example", "javascript:alert(1)", "/x%2Fy/../auth/login", "/%61uth"} {
		f.Add(s)
	}
	base, _ := url.Parse("https://battleship.example.org/")
	f.Fuzz(func(t *testing.T, in string) { checkSafeNext(t, base, in) })
}
