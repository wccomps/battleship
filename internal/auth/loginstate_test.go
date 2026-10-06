package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/seal"
)

func TestLoginStateRoundTrip(t *testing.T) {
	key := seal.NewKey("client-secret", "test")
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	in := loginState{State: "s", Nonce: "n", Verifier: "v", Next: "/jobs", Issued: now.Unix()}
	sealed, err := sealLoginState(key, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{`"s"`, "/jobs", "Verifier"} {
		if strings.Contains(sealed, secret) {
			t.Errorf("sealed state shows %s: %s", secret, sealed)
		}
	}
	out, err := openLoginState(key, sealed, now.Add(9*time.Minute))
	if err != nil || out != in {
		t.Fatalf("open = %+v, %v; want %+v", out, err, in)
	}
	// Sealing twice gives different values (random nonce).
	again, _ := sealLoginState(key, in)
	if again == sealed {
		t.Error("two seals of the same state are equal")
	}
}

func TestLoginStateRejects(t *testing.T) {
	key := seal.NewKey("client-secret", "test")
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	sealed, err := sealLoginState(key, loginState{State: "s", Nonce: "n", Verifier: "v", Next: "/", Issued: now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	flip := []byte(sealed)
	flip[len(flip)/2] ^= 1
	cases := map[string]struct {
		key   seal.Key
		value string
		at    time.Time
	}{
		"expired":         {key, sealed, now.Add(loginTTL + time.Second)},
		"from the future": {key, sealed, now.Add(-2 * time.Minute)},
		"tampered":        {key, string(flip), now},
		"other key":       {seal.NewKey("other-secret", "test"), sealed, now},
		"other purpose":   {seal.NewKey("client-secret", "other"), sealed, now},
		"empty":           {key, "", now},
		"not base64":      {key, "***", now},
		"truncated":       {key, sealed[:10], now},
	}
	for name, tc := range cases {
		if _, err := openLoginState(tc.key, tc.value, tc.at); err == nil {
			t.Errorf("%s: opened, want an error", name)
		}
	}
}
