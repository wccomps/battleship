package pods

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods/podstest"
)

// Run any of these with go test -fuzz <name> ./internal/pods.

// FuzzGuestCommand explores guest values, read as a stored plan holds them,
// for one GuestCommand accepts that isn't a plain name, address or key
// quoted into the script.
func FuzzGuestCommand(f *testing.F) {
	f.Add(true, "teak", "kilo.alpha", "192.168.100.10", 24, "192.168.100.2", "192.168.100.2", "BC:24:11:00:00:01", podstest.DefaultSSHCAKey)
	f.Add(false, "dc", "a.b", "10.0.0.5", 32, "10.0.0.1", "10.0.0.1", "bc:24:11:aa:bb:cc", "")
	f.Add(false, "te'ak", "kilo.alpha;id", "::ffff:1.2.3.4", 0, "1.2.3", "fe80::1%eth0", "BC-24-11-00-00-01", podstest.DefaultSSHCAKey+" x")
	f.Fuzz(func(t *testing.T, windows bool, hostname, domain, ip string, prefix int, gateway, dns, mac, key string) {
		g := Guest{OS: ImageLinux, Hostname: hostname, Domain: domain, Prefix: prefix, OpsCAKey: key}
		if windows {
			g.OS = ImageWindows
		}
		// A stored plan holds addresses as AddrText writes them.
		for _, a := range []string{ip, gateway, dns} {
			if AddrText(parsedIP(a)) != a && parsedIP(a).IsValid() {
				return
			}
		}
		if g, ok := storedGuest(t, g, ip, gateway, dns); ok {
			checkGuest(t, g, mac)
		}
	})
}

// FuzzParseHostSpec checks ParseHostSpec against its model on any text.
func FuzzParseHostSpec(f *testing.F) {
	for _, s := range []string{"dc=win2022", "dc=win2022:10", "dc=WIN 2022", "dc=x-v10000", "dc=a=b", "dc=win2022:+10", " teak = debian-12 : 20 "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { checkHostSpec(t, s) })
}

// FuzzParseTeams checks that any spec ParseTeams takes is sorted, unique,
// two-digit teams 00-99 that FormatTeams writes back to the same set.
func FuzzParseTeams(f *testing.F) {
	for _, s := range []string{"7", "1-32", "1,3,5", "1-3,7", " 0 - 99 ", "5-1", "1,,2", "007", "1-", "99999999999999999999", "١"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		teams, err := ParseTeams(spec)
		if err != nil {
			return
		}
		if len(teams) == 0 || !slices.IsSorted(teams) || len(slices.Compact(slices.Clone(teams))) != len(teams) {
			t.Fatalf("ParseTeams(%q) = %q", spec, teams)
		}
		for _, team := range teams {
			if n, err := strconv.Atoi(team); err != nil || n < 0 || n > 99 || FormatTeam(n) != team {
				t.Fatalf("ParseTeams(%q) has team %q", spec, team)
			}
		}
		if again, err := ParseTeams(FormatTeams(teams)); err != nil || !slices.Equal(again, teams) {
			t.Fatalf("ParseTeams(FormatTeams(%q)) = %q, %v", teams, again, err)
		}
	})
}

// FuzzUserNotes checks that what people wrote never carries a line that
// reads as battleship's own, and that battleship's line, as Checked leaves
// it, written into any description reads back the same.
func FuzzUserNotes(f *testing.F) {
	for _, s := range notesPieces {
		f.Add(s, "192.168.100.10", true, "router")
	}
	f.Fuzz(func(t *testing.T, desc, ip string, windows bool, dns string) {
		for _, line := range strings.Split(UserNotes(desc), "\n") {
			if isNotesLine(line) {
				t.Fatalf("UserNotes(%q) keeps %q", desc, line)
			}
		}
		n := Notes{IP: parsedIP(ip), OS: ImageLinux, DNS: dns}
		if windows {
			n.OS = ImageWindows
		}
		n = n.Checked(lanOf("192.168.100"))
		with := WithNotes(desc, n)
		if got, ok := ParseNotes(with); !ok || got != n {
			t.Fatalf("ParseNotes(WithNotes(%q, %+v)) = %+v, %v", desc, n, got, ok)
		}
	})
}
