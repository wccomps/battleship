package auth

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/seal"
)

// A ticket cookie opens only as the exact box sealed for its own session.
func TestPropTicketCookieOpensOnlyForItsSession(t *testing.T) {
	s := &Service{cookieKey: seal.NewKey("secret", cookiePurpose)}
	issued := time.Unix(1_700_000_000, 0)
	cred := proxmox.TicketCredential("alice@auth.example.org", "PVE:alice@auth.example.org:6553F100::sig", "6553F100:csrf", issued)
	cred.LoginAt = issued.Add(-time.Hour)
	good := sealTicketCookie(s.cookieKey, "sess-a", cred)
	got, ok := s.openTicketCookie("sess-a", good)
	if !ok || got.User != cred.User || got.Ticket != cred.Ticket || got.CSRF != cred.CSRF || !got.Issued.Equal(cred.Issued) || !got.LoginAt.Equal(cred.LoginAt) {
		t.Fatalf("own cookie opened as %v, %v; want %v", got, ok, cred)
	}
	rapid.Check(t, func(t *rapid.T) {
		var v string
		switch rapid.IntRange(0, 4).Draw(t, "kind") {
		case 0: // one character of the real cookie changed
			i := rapid.IntRange(0, len(good)-1).Draw(t, "at")
			v = good[:i] + rapid.SampledFrom([]string{"A", "-", "_", "=", "%", " ", "\x00"}).Draw(t, "char") + good[i+1:]
		case 1:
			v = good[:rapid.IntRange(0, len(good)-1).Draw(t, "cut")]
		case 2:
			v = sealTicketCookie(s.cookieKey, rapid.StringMatching(`sess-[a-z]{1,3}`).Filter(func(id string) bool { return id != "sess-a" }).Draw(t, "session"), cred)
		case 3:
			v = s.cookieKey.Seal("sess-a", []byte(rapid.SampledFrom([]string{"", "{}", "null", `{"u":"alice"}`, "not json"}).Draw(t, "plain")))
		default:
			v = rapid.String().Draw(t, "any")
		}
		if v == good {
			return
		}
		if c, ok := s.openTicketCookie("sess-a", v); ok {
			t.Fatalf("cookie %q opened as %v", v, c)
		}
	})
}
