package auth_test

import (
	"net/http"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
)

// bindingNames are usernames and Proxmox user names that are equal, equal
// but for case, or near: an "@" inside, Unicode case folding.
var bindingNames = []string{"alice", "Alice", "ALICE", "alic", "alice2", "a@b", "A@B", "a", "\u212aate", "kate", "\u017fam", "sam",
	"stra\u00dfe", "strasse", "\u0130", "i"}

// bindingRealms are what follows the user name in a Proxmox user ID.
var bindingRealms = []string{pvetest.Realm, strings.ToUpper(pvetest.Realm), "pve", pvetest.Realm + ".evil", "b@" + pvetest.Realm}

// The Proxmox sign-in is kept only for the session's own user,
// exactly <username>@<realm>; a session never holds another's ticket, nor
// one whose name differs only in case or by Unicode case folding.
func TestPropProxmoxSignInIsTheSessionsUser(t *testing.T) {
	h := newHarness(t)
	check := func(t interface{ Fatalf(string, ...any) }, username string, pveNames []string) {
		want := username + "@" + pvetest.Realm
		b := h.browser()
		h.idp.SignIn(authtest.User{Subject: "sub-" + username, Username: username})
		for i, name := range pveNames {
			h.pve.AddUser(name, nil)
			h.pve.SignIn(name)
			target := "/auth/login?next=/private"
			if i > 0 {
				target = "/auth/proxmox?next=/private"
			}
			resp := b.get(target)
			accepted := resp.StatusCode == http.StatusOK && resp.Request.URL.Path == "/private"
			if accepted != (name == want) {
				t.Fatalf("session of %q, Proxmox signed in %q: %d at %s, want accepted %v", username, name, resp.StatusCode, resp.Request.URL, !accepted)
			}
			if cred, ok := b.cookieTicket(); ok && cred.User != want {
				t.Fatalf("session of %q holds %q's ticket", username, cred.User)
			}
		}
	}
	for _, u := range bindingNames {
		check(t, u, []string{u + "@" + pvetest.Realm, strings.ToUpper(u) + "@" + pvetest.Realm, u + "@pve", "x" + u + "@" + pvetest.Realm})
	}
	rapid.Check(t, func(t *rapid.T) {
		username := rapid.SampledFrom(bindingNames).Draw(t, "username")
		pveName := rapid.Custom(func(t *rapid.T) string {
			return rapid.SampledFrom(bindingNames).Draw(t, "user") + "@" + rapid.SampledFrom(bindingRealms).Draw(t, "realm")
		})
		check(t, username, rapid.SliceOfN(pveName, 1, 3).Draw(t, "proxmox users"))
	})
}
