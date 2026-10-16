package auth

import (
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/seal"
)

const base64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// tokenModel is a 32-byte token in canonical unpadded base64url: 43
// characters of the alphabet, the last carrying no stray low bits.
func tokenModel(s string) bool {
	if len(s) != 43 {
		return false
	}
	for i := range len(s) {
		if strings.IndexByte(base64URL, s[i]) < 0 {
			return false
		}
	}
	return strings.IndexByte(base64URL, s[42])%4 == 0
}

// tokenNearMiss draws a token one slip from one randomToken made.
func tokenNearMiss(t *rapid.T) string {
	tok := randomToken()
	i := rapid.IntRange(0, len(tok)-1).Draw(t, "at")
	switch rapid.IntRange(0, 5).Draw(t, "slip") {
	case 0:
		return tok
	case 1: // another character, in the alphabet or not
		c := rapid.OneOf(rapid.SampledFrom([]string{"+", "/", "=", " ", ".", "\x00", "\u00e9", "%"}), rapid.SampledFrom(strings.Split(base64URL, ""))).Draw(t, "char")
		return tok[:i] + c + tok[i+1:]
	case 2: // the last character, which must leave its low bits clear
		return tok[:42] + rapid.SampledFrom(strings.Split(base64URL, "")).Draw(t, "last")
	case 3:
		return tok[:i]
	case 4:
		return tok + rapid.SampledFrom([]string{"=", "A", "AAAA", "=="}).Draw(t, "extra")
	}
	return rapid.String().Draw(t, "any")
}

// Every token randomToken makes is well formed, and a cookie is well formed
// only if it is such a token.
func TestPropWellFormedCookies(t *testing.T) {
	tok := randomToken()
	last := strings.IndexByte(base64URL, tok[42])
	for _, s := range []string{tok, tok[:42] + string(base64URL[last+1]), tok[:42] + string(base64URL[last+3]), tok + "=", tok[:42],
		strings.Repeat("A", 43), strings.Repeat("A", 42) + "B", strings.Repeat("A", 42) + "E", strings.Repeat("_", 43)} {
		if wellFormed(s) != tokenModel(s) {
			t.Errorf("wellFormed(%q) = %v", s, wellFormed(s))
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		if tok := randomToken(); !wellFormed(tok) {
			t.Fatalf("wellFormed(%q) = false", tok)
		}
		if s := tokenNearMiss(t); wellFormed(s) != tokenModel(s) {
			t.Fatalf("wellFormed(%q) = %v", s, wellFormed(s))
		}
	})
}

// A sealed login state opens back from a fresh cookie and from nothing
// else: not after loginTTL, not from beyond clock skew ahead, not with a
// character changed, cut or added, and not under another key.
func TestPropLoginStateCookie(t *testing.T) {
	key := seal.NewKey("client-secret", "test")
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	// Every other last character, though base64 ignores its low bits.
	sealed, err := sealLoginState(key, loginState{State: "s", Nonce: "n", Verifier: "v", Next: "/", Issued: now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range base64URL {
		if other := sealed[:len(sealed)-1] + string(c); other != sealed {
			if _, err := openLoginState(key, other, now); err != errBadLoginCookie {
				t.Errorf("%q, changed from %q, opened: %v", other, sealed, err)
			}
		}
	}
	field := rapid.OneOf(rapid.StringMatching(`[A-Za-z0-9_-]{1,12}`), rapid.String().Filter(func(s string) bool { return s != "" }))
	rapid.Check(t, func(t *rapid.T) {
		st := loginState{State: field.Draw(t, "state"), Nonce: field.Draw(t, "nonce"), Verifier: field.Draw(t, "verifier"),
			Next: rapid.SampledFrom([]string{"/", "/jobs?after=3"}).Draw(t, "next")}
		age := rapid.OneOf(rapid.SampledFrom([]time.Duration{0, -clockSkew, -clockSkew - time.Second, loginTTL - time.Second, loginTTL,
			loginTTL + time.Second}), rapid.Map(rapid.Int64Range(-200, int64(loginTTL/time.Second)+200), func(s int64) time.Duration {
			return time.Duration(s) * time.Second
		})).Draw(t, "age")
		st.Issued = now.Add(-age).Unix()
		sealed, err := sealLoginState(key, st)
		if err != nil {
			t.Fatal(err)
		}
		got, err := openLoginState(key, sealed, now)
		if fresh := age >= -clockSkew && age < loginTTL; (err == nil) != fresh || fresh && got != st {
			t.Fatalf("opened %+v aged %v: %+v, %v", st, age, got, err)
		}
		other := sealed
		switch rapid.IntRange(0, 3).Draw(t, "spoil") {
		case 0:
			i := rapid.IntRange(0, len(sealed)-1).Draw(t, "at")
			c := rapid.SampledFrom(strings.Split(base64URL+"=:v", "")).Filter(func(c string) bool { return c != sealed[i:i+1] }).Draw(t, "char")
			other = sealed[:i] + c + sealed[i+1:]
		case 1:
			other = sealed[:rapid.IntRange(0, len(sealed)-1).Draw(t, "cut")]
		case 2:
			other = sealed + rapid.SampledFrom([]string{"A", "=", "AAAA"}).Draw(t, "extra")
		case 3:
			if _, err := openLoginState(seal.NewKey("other-secret", "test"), sealed, now); err != errBadLoginCookie {
				t.Fatalf("another key opened it: %v", err)
			}
			return
		}
		if _, err := openLoginState(key, other, now); err != errBadLoginCookie {
			t.Fatalf("%q, changed from %q, opened: %v", other, sealed, err)
		}
	})
}
