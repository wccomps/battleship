package pods

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf16"

	"pgregory.net/rapid"
)

// scriptTricks are pieces that would break out of a quoted value in sh or
// PowerShell, or slip past a check meant for ASCII names and addresses.
var scriptTricks = []string{
	"'", "\u2019", "\u2018", "\u201a", "\u201b", "`", "$(id)", "${x}", ";", "\n", "\r", "\x00", `"`, `\`, "|", "&",
	"\u0661\u0662\u0663", "\uff0e", "A", "-", ".", " ", "01.2.3.4", "1.2.3.4 ", "::ffff:1.2.3.4", "1.2.3", "+1", strings.Repeat("a", 64),
}

// nearMisses are values one slip away from valid value v of field: what a
// lax check of that field would let through.
func nearMisses(field, v string) []string {
	switch field {
	case "ip", "gateway", "dns":
		dot := strings.LastIndex(v, ".")
		digits := func(zero rune) string {
			return strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return zero + r - '0'
				}
				return r
			}, v)
		}
		return []string{"0" + v, "::ffff:" + v, v + " ", " " + v, v + ".", v + ".1", "+" + v, "-" + v, v[:dot], v[:dot+1] + "256",
			v[:dot+1] + "0" + v[dot+1:], digits('\u0660'), digits('\uff10'), strings.Replace(v, ".", "\uff0e", 1)}
	case "hostname", "domain":
		first, _, _ := strings.Cut(v, ".")
		return []string{strings.ToUpper(v), "-" + v, v + "-", v + ".", "." + v, strings.Replace(v, ".", "..", 1), strings.Replace(v, ".", "-.", 1),
			v + "_x", first + strings.Repeat("a", 64-len(first)) + strings.TrimPrefix(v, first), " " + v, v + "\n"}
	case "mac":
		return []string{strings.ReplaceAll(v, ":", "-"), v[:len(v)-1], v + ":00", v[:len(v)-1] + "g", " " + v, v + " ", v + "\n"}
	}
	panic("no near misses for " + field)
}

// guestValue draws a valid value of field, or, if bad, a hostile one: a near
// miss, a trick or a valid value with tricks around it, or (rarely) any text.
func guestValue(t *rapid.T, field string, bad bool, valid ...string) string {
	if !bad {
		return rapid.SampledFrom(valid).Draw(t, field)
	}
	trick := rapid.SampledFrom(scriptTricks)
	near := rapid.Custom(func(t *rapid.T) string {
		return rapid.SampledFrom(nearMisses(field, rapid.SampledFrom(valid).Draw(t, "valid"))).Draw(t, "near miss")
	})
	return rapid.OneOf(
		near, near, near, near,
		trick,
		rapid.Custom(func(t *rapid.T) string {
			return trick.Draw(t, "before") + rapid.SampledFrom(valid).Draw(t, "valid") + trick.Draw(t, "after")
		}),
		rapid.String(),
	).Draw(t, field)
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

// storedGuest is g with its addresses read from text, as a stored plan's
// are; false if a plan with them can't be read.
func storedGuest(t interface{ Fatalf(string, ...any) }, g Guest, ip, gateway, dns string) (Guest, bool) {
	b, err := json.Marshal(map[string]string{"ip": ip, "gateway": gateway, "dns": dns})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if json.Unmarshal(b, &g) != nil {
		return Guest{}, false
	}
	if AddrText(g.IP) != ip || AddrText(g.Gateway) != gateway || AddrText(g.DNS) != dns {
		t.Fatalf("read addresses %q, %q, %q as %+v", ip, gateway, dns, g)
	}
	return g, true
}

// checkGuest checks that GuestCommand either refuses g with mac or accepts
// only safe values, quoted into the template, in a command that carries
// exactly the script.
func checkGuest(t interface {
	Fatal(...any)
	Fatalf(string, ...any)
}, g Guest, mac string) {
	cmd, input, err := GuestCommand(g, mac)
	if err != nil {
		return
	}
	ip, gw, dns := AddrText(g.IP), AddrText(g.Gateway), AddrText(g.DNS)
	if (g.OS != ImageWindows && g.OS != ImageLinux) || !safeLabel(g.Hostname) || !safeDomain(g.Domain) || g.Prefix < 1 || g.Prefix > 32 ||
		!safeIPv4(ip) || !safeIPv4(gw) || !safeIPv4(dns) || !safeMAC(mac) {
		t.Fatalf("accepted %+v with MAC %q", g, mac)
	}
	q := func(v string) string { return "'" + v + "'" }
	winMAC, linMAC := q(strings.ToUpper(strings.ReplaceAll(mac, ":", "-"))), q(strings.ToLower(mac))
	var want string
	if g.OS == ImageWindows {
		want = fmt.Sprintf(windowsScript, winMAC, q(ip), g.Prefix, q(gw), q(dns), q(g.Domain), q(g.Hostname), RebootMarker)
	} else {
		want = "MAC=" + linMAC + "\nIP=" + q(ip) + "\nPREFIX=" + strconv.Itoa(g.Prefix) + "\nGW=" + q(gw) +
			"\nDNS=" + q(dns) + "\nHOST=" + q(g.Hostname) + "\nDOMAIN=" + q(g.Domain) + "\n" + linuxFindIface + linuxScript
	}
	script, err := GuestScript(g, mac)
	if err != nil || script != want {
		t.Fatalf("GuestScript = %v, and differs from the template with the values quoted in", err)
	}
	if g.OS == ImageLinux {
		// The command is fixed text; every value travels in the script on
		// stdin, never in argv.
		if !slices.Equal(cmd, []string{"/bin/sh", "-c", linuxLocked}) || input != script {
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
}

// Every near miss and trick, alone, before or after a valid value or inside
// it, in each field of a guest of either OS, is refused or safe. Addresses
// come as a stored plan holds them.
func TestGuestTricks(t *testing.T) {
	for _, os := range []string{ImageWindows, ImageLinux} {
		for _, field := range []string{"hostname", "domain", "ip", "gateway", "dns", "mac"} {
			g := Guest{OS: os, Hostname: "teak", Domain: "kilo.alpha", Prefix: 24}
			ip, gw, dns, mac := "192.168.100.10", "192.168.100.2", "192.168.100.2", "BC:24:11:00:00:01"
			v := map[string]*string{"hostname": &g.Hostname, "domain": &g.Domain, "ip": &ip, "gateway": &gw, "dns": &dns, "mac": &mac}[field]
			valid := *v
			for _, trick := range append(nearMisses(field, valid), scriptTricks...) {
				half := len(valid) / 2
				for _, placed := range []string{trick, trick + valid, valid + trick, valid[:half] + trick + valid[half:]} {
					*v = placed
					if h, ok := storedGuest(t, g, ip, gw, dns); ok {
						checkGuest(t, h, mac)
					}
				}
			}
		}
	}
}

// A guest script embeds only names and addresses, each in single quotes:
// whatever the values, either GuestCommand refuses them or the script is the
// template with safe values quoted in, and the command carries exactly it.
// Addresses come as a stored plan holds them.
func TestPropGuestScriptsAreInjectionFree(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Most draws spoil exactly one field; the rest must pass.
		spoilt := rapid.SampledFrom([]string{"none", "os", "hostname", "domain", "ip", "prefix", "gateway", "dns", "mac"}).Draw(t, "spoilt")
		bad := func(f string) bool { return f == spoilt }
		g := Guest{
			OS:       rapid.SampledFrom([]string{ImageWindows, ImageLinux}).Draw(t, "os"),
			Hostname: guestValue(t, "hostname", bad("hostname"), "teak", "dc", "web-1", "a", strings.Repeat("z", 63)),
			Domain:   guestValue(t, "domain", bad("domain"), "kilo.alpha", "a.b", "x-1.y.z"),
			Prefix:   rapid.IntRange(1, 32).Draw(t, "prefix"),
		}
		g, ok := storedGuest(t, g, guestValue(t, "ip", bad("ip"), "192.168.100.10", "10.0.0.5", "0.0.0.0"),
			guestValue(t, "gateway", bad("gateway"), "192.168.100.2"), guestValue(t, "dns", bad("dns"), "192.168.100.2", "255.255.255.255"))
		if !ok {
			return
		}
		if bad("os") {
			g.OS = rapid.SampledFrom([]string{ImageOther, "", "Windows", "linux "}).Draw(t, "bad os")
		}
		if bad("prefix") {
			g.Prefix = rapid.SampledFrom([]int{-1, 0, 33, 40}).Draw(t, "bad prefix")
		}
		mac := guestValue(t, "mac", bad("mac"), "BC:24:11:00:00:01", "bc:24:11:aa:bb:cc")
		checkGuest(t, g, mac)
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

// Setup has finished exactly when the setup state read prints
// IMAGE_STATE_COMPLETE, in any case, or nothing, between blank space and
// byte order marks; any other state, or more output, means it hasn't.
func TestPropSetupComplete(t *testing.T) {
	states := []string{"IMAGE_STATE_COMPLETE", "image_state_complete", "IMAGE_STATE_UNDEPLOYABLE", "IMAGE_STATE_SPECIALIZE_RESEAL_TO_OOBE",
		"IMAGE_STATE_GENERALIZE_RESEAL_TO_OOBE", "IMAGE_STATE_COMPLETE_X", "IMAGE STATE COMPLETE", "COMPLETE", ""}
	blank := []string{"", " ", "\r\n", "\n", "\t", "\ufeff"}
	rapid.Check(t, func(t *rapid.T) {
		lines := rapid.SliceOfN(rapid.OneOf(rapid.SampledFrom(states), rapid.String()), 0, 2).Draw(t, "lines")
		out := rapid.SampledFrom(blank).Draw(t, "lead") + strings.Join(lines, "\r\n") + rapid.SampledFrom(blank).Draw(t, "trail")
		var said []string
		for _, f := range strings.FieldsFunc(out, func(r rune) bool { return r == '\r' || r == '\n' }) {
			if f = strings.TrimFunc(f, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' }); f != "" {
				said = append(said, f)
			}
		}
		want := len(said) == 0 || len(said) == 1 && strings.EqualFold(said[0], "IMAGE_STATE_COMPLETE")
		if got := SetupComplete(out); got != want {
			t.Fatalf("SetupComplete(%q) = %v, want %v", out, got, want)
		}
	})
}

// The setup state read names the registry value it prints, and only reads.
func TestSetupStateCommand(t *testing.T) {
	cmd := strings.Join(SetupStateCommand, " ")
	if !strings.Contains(cmd, `HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Setup\State`) || !strings.Contains(cmd, "ImageState") ||
		!strings.Contains(cmd, "Get-ItemProperty") || strings.ContainsAny(cmd, ";|") || strings.Contains(cmd, "Set-") {
		t.Errorf("command = %q", cmd)
	}
}
