package auth

import "testing"

func FuzzSafeNext(f *testing.F) {
	for _, s := range []string{"/", "/grid?x=1#y", "//evil.example", "/\\evil.example", "/%2e%2e/auth/login", "/auth/../grid",
		"https://evil.example/", "/a/../../auth", "/\t/evil", "/x@evil.example", "javascript:alert(1)", "/x%2Fy/../auth/login", "/%61uth", "/x#/../auth/login", "/x/..//auth/login", "/auth ", "/%2F"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) { checkSafeNext(t, in) })
}
