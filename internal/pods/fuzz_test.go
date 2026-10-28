package pods

import (
	"testing"

	"github.com/wccomps/battleship/internal/pods/podstest"
)

func FuzzGuestCommand(f *testing.F) {
	f.Add(true, "teak", "kilo.alpha", "192.168.100.10", 24, "192.168.100.2", "192.168.100.2", "BC:24:11:00:00:01", podstest.DefaultSSHCAKey)
	f.Add(false, "dc", "a.b", "10.0.0.5", 32, "10.0.0.1", "10.0.0.1", "bc:24:11:aa:bb:cc", "")
	f.Add(false, "te'ak", "kilo.alpha;id", "::ffff:1.2.3.4", 0, "1.2.3", "fe80::1%eth0", "BC-24-11-00-00-01", podstest.DefaultSSHCAKey+" x")
	f.Fuzz(func(t *testing.T, windows bool, hostname, domain, ip string, prefix int, gateway, dns, mac, key string) {
		g := Guest{OS: ImageLinux, Hostname: hostname, Domain: domain, Prefix: prefix, OpsCAKey: key}
		if windows {
			g.OS = ImageWindows
		}
		if g, ok := storedGuest(g, ip, gateway, dns); ok {
			checkGuest(t, g, mac)
		}
	})
}

func FuzzParseHostSpec(f *testing.F) {
	for _, s := range []string{"dc=win2022", "dc=win2022:10", "dc=WIN 2022", "dc=x-v10000", "dc=a=b", "dc=win2022:+10", " teak = debian-12 : 20 "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { checkHostSpec(t, s) })
}

func FuzzUserNotes(f *testing.F) {
	for _, s := range notesPieces {
		f.Add(s, "192.168.100.10", true, "router")
	}
	f.Fuzz(func(t *testing.T, desc, ip string, windows bool, dns string) {
		checkUserNotes(t, desc)
		n := Notes{IP: parsedIP(ip), OS: ImageLinux, DNS: dns}
		if windows {
			n.OS = ImageWindows
		}
		checkNotesLine(t, desc, n.Checked(lanOf("192.168.100")))
	})
}
