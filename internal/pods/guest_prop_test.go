package pods

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"pgregory.net/rapid"
)

// scriptTricks are pieces that would break out of a quoted value in sh or
// PowerShell, or slip past a check meant for ASCII names and addresses.
var scriptTricks = []string{
	"'", "\u2019", "\u2018", "\u201a", "\u201b", "`", "$(id)", "${x}", ";", "\n", "\r", "\x00", `"`, `\`, "|", "&",
	"\u0661\u0662\u0663", "\uff0e", "A", "-", ".", " ", "01.2.3.4", "1.2.3.4 ", "::ffff:1.2.3.4", "1.2.3", "+1", strings.Repeat("a", 64),
}

// guestValue draws a valid value, or, if bad, a hostile one: a trick,
// any text, or a valid value with tricks around it.
func guestValue(t *rapid.T, label string, bad bool, valid ...string) string {
	if !bad {
		return rapid.SampledFrom(valid).Draw(t, label)
	}
	trick := rapid.SampledFrom(scriptTricks)
	return rapid.OneOf(
		trick,
		rapid.String(),
		rapid.Custom(func(t *rapid.T) string {
			return trick.Draw(t, "before") + rapid.SampledFrom(valid).Draw(t, "valid") + trick.Draw(t, "after")
		}),
	).Draw(t, label)
}

// safeLabel is one DNS label of lower-case ASCII letters, digits and inner
// dashes, written out without regexps.
func safeLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := range len(s) {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func safeDomain(s string) bool {
	labels := strings.Split(s, ".")
	return len(labels) >= 2 && len(s) <= 200 && !slices.ContainsFunc(labels, func(l string) bool { return !safeLabel(l) })
}

// safeIPv4 is a dotted quad of ASCII decimals 0-255 without leading zeros.
func safeIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 || strconv.Itoa(n) != p {
			return false
		}
	}
	return true
}

func safeMAC(s string) bool {
	if len(s) != 17 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if i%3 == 2 {
			if c != ':' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// A guest script embeds only names and addresses, each in single quotes:
// whatever the values, either GuestCommand refuses them or the script is the
// template with safe values quoted in, and the command carries exactly it.
func TestPropGuestScriptsAreInjectionFree(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Most draws spoil a field or two; the rest must pass.
		fields := []string{"os", "hostname", "domain", "ip", "prefix", "gateway", "dns", "mac"}
		spoilt := rapid.SliceOfNDistinct(rapid.SampledFrom(fields), 0, 2, rapid.ID).Draw(t, "spoilt")
		bad := func(f string) bool { return slices.Contains(spoilt, f) }
		g := Guest{
			OS:       rapid.SampledFrom([]string{ImageWindows, ImageLinux}).Draw(t, "os"),
			Hostname: guestValue(t, "hostname", bad("hostname"), "teak", "dc", "web-1", "a", strings.Repeat("z", 63)),
			Domain:   guestValue(t, "domain", bad("domain"), "kilo.alpha", "a.b", "x-1.y.z"),
			IP:       guestValue(t, "ip", bad("ip"), "192.168.100.10", "10.0.0.5", "0.0.0.0"),
			Prefix:   rapid.IntRange(1, 32).Draw(t, "prefix"),
			Gateway:  guestValue(t, "gateway", bad("gateway"), "192.168.100.2"),
			DNS:      guestValue(t, "dns", bad("dns"), "192.168.100.2", "255.255.255.255"),
			DNSOnly:  rapid.Bool().Draw(t, "dns only"),
		}
		if bad("os") {
			g.OS = rapid.SampledFrom([]string{ImageOther, "", "Windows", "linux "}).Draw(t, "bad os")
		}
		if bad("prefix") {
			g.Prefix = rapid.SampledFrom([]int{-1, 0, 33, 40}).Draw(t, "bad prefix")
		}
		mac := guestValue(t, "mac", bad("mac"), "BC:24:11:00:00:01", "bc:24:11:aa:bb:cc")
		cmd, input, err := GuestCommand(g, mac)
		if err != nil {
			return
		}
		if (g.OS != ImageWindows && g.OS != ImageLinux) || !safeLabel(g.Hostname) || !safeDomain(g.Domain) || g.Prefix < 1 || g.Prefix > 32 ||
			!safeIPv4(g.IP) || !safeIPv4(g.Gateway) || !safeIPv4(g.DNS) || !safeMAC(mac) {
			t.Fatalf("accepted %+v with MAC %q", g, mac)
		}
		q := func(v string) string { return "'" + v + "'" }
		winMAC, linMAC := q(strings.ToUpper(strings.ReplaceAll(mac, ":", "-"))), q(strings.ToLower(mac))
		var want string
		switch {
		case g.DNSOnly && g.OS == ImageWindows:
			want = fmt.Sprintf(windowsDNSScript, winMAC, q(g.DNS), q(g.Domain))
		case g.DNSOnly:
			want = "MAC=" + linMAC + "\nDNS=" + q(g.DNS) + "\nDOMAIN=" + q(g.Domain) + "\n" + linuxFindIface + linuxDNSScript
		case g.OS == ImageWindows:
			want = fmt.Sprintf(windowsScript, winMAC, q(g.IP), g.Prefix, q(g.Gateway), q(g.DNS), q(g.Domain), q(g.Hostname), RebootMarker)
		default:
			want = "MAC=" + linMAC + "\nIP=" + q(g.IP) + "\nPREFIX=" + strconv.Itoa(g.Prefix) + "\nGW=" + q(g.Gateway) +
				"\nDNS=" + q(g.DNS) + "\nHOST=" + q(g.Hostname) + "\nDOMAIN=" + q(g.Domain) + "\n" + linuxFindIface + linuxScript
		}
		script, err := GuestScript(g, mac)
		if err != nil || script != want {
			t.Fatalf("GuestScript = %v, and differs from the template with the values quoted in", err)
		}
		if g.OS == ImageLinux {
			if !slices.Equal(cmd, []string{"/bin/sh", "-s"}) || input != script {
				t.Fatalf("command %q doesn't run the script on stdin", cmd)
			}
			return
		}
		head := []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand"}
		if len(cmd) != len(head)+1 || !slices.Equal(cmd[:len(head)], head) || input != "" {
			t.Fatalf("command = %q", cmd)
		}
		b, err := base64.StdEncoding.DecodeString(cmd[len(head)])
		if err != nil || len(b)%2 != 0 {
			t.Fatalf("encoded command: %v", err)
		}
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
		if got := string(utf16.Decode(u)); got != script {
			t.Fatal("the encoded command isn't the script")
		}
	})
}

// A host name or domain that passes its check is plain ASCII, with nothing
// a shell or PowerShell treats specially.
func TestPropNameChecksAdmitOnlyPlainASCII(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := strings.Join(rapid.SliceOfN(rapid.OneOf(rapid.SampledFrom(scriptTricks), rapid.SampledFrom([]string{"teak", "kilo", "."}), rapid.String()), 1, 4).Draw(t, "pieces"), "")
		for name, check := range map[string]func(string) error{"CheckHostname": CheckHostname, "CheckDomain": CheckDomain} {
			if check(s) != nil {
				continue
			}
			if strings.ContainsAny(s, "'\"$`;\n\r\x00\\ ") || strings.ContainsFunc(s, func(r rune) bool { return r >= 0x80 }) {
				t.Fatalf("%s accepted %q", name, s)
			}
		}
	})
}
