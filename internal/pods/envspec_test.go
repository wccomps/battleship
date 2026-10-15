package pods

import (
	"strconv"
	"strings"
	"testing"
	"unicode"

	"pgregory.net/rapid"
)

func TestParseHostSpec(t *testing.T) {
	for in, want := range map[string]HostSpec{
		"teak=debian-12":       {Host: "teak", Family: "debian-12"},
		" oak = win2022 : 10 ": {Host: "oak", Family: "win2022", Octet: 10},
	} {
		got, err := ParseHostSpec(in)
		if err != nil || got != want {
			t.Errorf("ParseHostSpec(%q) = %+v, %v", in, got, err)
		}
		if again, _ := ParseHostSpec(got.String()); again != got {
			t.Errorf("String() doesn't round-trip: %q", got.String())
		}
	}
	for _, bad := range []string{"", "teak", "=x", "teak=", "Teak=x", "teak=x:3", "teak=x:255", "teak=x:a", "te.ak=x"} {
		if _, err := ParseHostSpec(bad); err == nil {
			t.Errorf("ParseHostSpec(%q) accepted", bad)
		}
	}
}

func TestCheckDomain(t *testing.T) {
	for _, ok := range []string{"kilo.alpha", "a.b.c", "x-1.y2"} {
		if err := CheckDomain(ok); err != nil {
			t.Errorf("CheckDomain(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "kilo", "Kilo.alpha", "kilo..alpha", ".kilo.alpha", "kilo.alpha.", "ki_lo.alpha"} {
		if CheckDomain(bad) == nil {
			t.Errorf("CheckDomain(%q) accepted", bad)
		}
	}
}

func TestNotesLine(t *testing.T) {
	desc := "Domain controller.\nbattleship: ip=192.168.100.10 os=windows base=win2022-v3 dns=router\nkeep me"
	n, ok := ParseNotes(desc)
	if !ok || n != (Notes{IP: "192.168.100.10", OS: "windows", Base: "win2022-v3", DNS: "router"}) {
		t.Fatalf("ParseNotes = %+v, %v", n, ok)
	}
	n.DNS = "teak"
	got := WithNotes(desc, n)
	want := "Domain controller.\nbattleship: ip=192.168.100.10 os=windows base=win2022-v3 dns=teak\nkeep me"
	if got != want {
		t.Errorf("WithNotes = %q", got)
	}
	if WithNotes("", Notes{Base: "gw-v1", DNS: "router"}) != "battleship: base=gw-v1 dns=router" {
		t.Error("WithNotes on empty notes")
	}
	if WithNotes("hello", Notes{DNS: "router"}) != "hello\nbattleship: dns=router" {
		t.Error("WithNotes appends a line")
	}
	if _, ok := ParseNotes("nothing here"); ok {
		t.Error("ParseNotes found a line in plain notes")
	}
}

func TestPickOctetIsStableAndAvoidsUsed(t *testing.T) {
	used := map[int]bool{}
	a, ok := PickOctet("kilo.alpha", "teak", used)
	if !ok || a < FirstHostOctet || a > LastHostOctet {
		t.Fatalf("PickOctet = %d, %v", a, ok)
	}
	if b, _ := PickOctet("kilo.alpha", "teak", used); b != a {
		t.Error("PickOctet isn't deterministic")
	}
	used[a] = true
	if c, _ := PickOctet("kilo.alpha", "teak", used); c == a {
		t.Error("PickOctet picked a used octet")
	}
	for o := FirstHostOctet; o <= LastHostOctet; o++ {
		used[o] = true
	}
	if _, ok := PickOctet("kilo.alpha", "teak", used); ok {
		t.Error("PickOctet found an octet in a full subnet")
	}
}

func TestUserNotes(t *testing.T) {
	for _, c := range []struct{ desc, want string }{
		{"", ""},
		{"battleship: ip=192.168.100.10 base=deb-v1", ""},
		{"Login: root / pw\nbattleship: ip=192.168.100.10\n", "Login: root / pw"},
		{"  battleship: reboot\nDebian 13\r\n\nLogin: root  \n", "Debian 13\n\nLogin: root"},
		{"<script>alert(1)</script>", "<script>alert(1)</script>"}, // pages escape it
		{"Login: \u202eroot\u200b / pw\ufeff", "Login: root / pw"}, // format characters dropped
	} {
		if got := UserNotes(c.desc); got != c.want {
			t.Errorf("UserNotes(%q) = %q, want %q", c.desc, got, c.want)
		}
	}
	long := strings.Repeat("é", maxUserNotes+5)
	if got := []rune(UserNotes(long)); len(got) != maxUserNotes+1 || got[maxUserNotes] != '…' {
		t.Errorf("long notes keep %d runes, want %d and an ellipsis", len(got), maxUserNotes+1)
	}
}

// UserNotes never keeps battleship's lines or format characters, stays within
// its cap, and cleaning cleaned notes changes nothing.
func TestPropUserNotes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		piece := rapid.OneOf(rapid.String(), rapid.SampledFrom([]string{
			"battleship: ip=192.168.100.10", "  battleship: reboot", "\n", "\r\n", "\u202e", "\u200b", "\ufeff", "Login: root",
		}))
		desc := strings.Join(rapid.SliceOfN(piece, 0, 40).Draw(t, "pieces"), "")
		got := UserNotes(desc)
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), notesPrefix) {
				t.Fatalf("kept a battleship line: %q", line)
			}
		}
		if strings.ContainsFunc(got, func(r rune) bool { return unicode.Is(unicode.Cf, r) }) {
			t.Fatalf("kept a format character: %q", got)
		}
		if n := len([]rune(got)); n > maxUserNotes+1 {
			t.Fatalf("kept %d runes", n)
		}
		if len([]rune(got)) <= maxUserNotes && UserNotes(got) != got {
			t.Fatalf("not idempotent: %q -> %q", got, UserNotes(got))
		}
	})
}

// safeFamily is a base-image family written out without regexps: 1-40
// lower-case ASCII letters, digits and inner dashes, not ending in a
// version Battleship would add (-v1 to -v9999).
func safeFamily(f string) bool {
	if len(f) == 0 || len(f) > 40 || f[0] == '-' || f[len(f)-1] == '-' {
		return false
	}
	for i := range len(f) {
		if c := f[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	i := strings.LastIndex(f, "-v")
	if i < 0 || i == 0 {
		return true
	}
	v := f[i+2:]
	return !(len(v) >= 1 && len(v) <= 4 && v[0] != '0' && strings.Trim(v, "0123456789") == "")
}

// hostSpecParts are the pieces of "host=family:octet" near the edge of
// valid, keyed by part; the first of each is valid.
var hostSpecParts = map[string][]string{
	"host": {"teak", "dc", "a", "x-1", strings.Repeat("a", 63), "Teak", "-teak", "teak-", "te.ak", "te_ak", "", " ", strings.Repeat("a", 64),
		"te ak", "t\u0435ak", "teak\u00a0x"},
	"family": {"debian-12", "win2022", "gw", "x-v0", "x-v01", "x-v10000", strings.Repeat("a", 40), "WIN 2022", "win 2022", "Win2022", "win2022-v1",
		"x-v9999", "-x", "x-", "", " ", "debian_12", strings.Repeat("a", 41), "a=b", "debian-12\u00a0", "deb\u0131an"},
	"octet": {"", ":10", ": 10 ", ":4", ":254", ":+10", ":007", ":3", ":255", ":0", ":", ":a", "::10", ":1e1", ":\u0661\u0660", ":-4", ": "},
	"sep":   {"=", " = ", "==", "=:"},
}

// parseHostSpecModel is what ParseHostSpec should read from s.
func parseHostSpecModel(s string) (HostSpec, bool) {
	eq := strings.IndexByte(s, '=')
	if eq < 0 {
		return HostSpec{}, false
	}
	rest := s[eq+1:]
	h := HostSpec{Host: strings.TrimSpace(s[:eq])}
	octet, hasOctet := "", false
	if c := strings.IndexByte(rest, ':'); c >= 0 {
		rest, octet, hasOctet = rest[:c], strings.TrimSpace(rest[c+1:]), true
	}
	h.Family = strings.TrimSpace(rest)
	if !safeLabel(h.Host) || !safeFamily(h.Family) {
		return HostSpec{}, false
	}
	if hasOctet {
		n, err := strconv.ParseInt(octet, 10, 64)
		if err != nil || n < 4 || n > 254 {
			return HostSpec{}, false
		}
		h.Octet = int(n)
	}
	return h, true
}

func checkHostSpec(t interface{ Fatalf(string, ...any) }, s string) {
	got, err := ParseHostSpec(s)
	want, ok := parseHostSpecModel(s)
	if (err == nil) != ok || got != want {
		t.Fatalf("ParseHostSpec(%q) = %+v, %v; want %+v, ok %v", s, got, err, want, ok)
	}
	if err == nil {
		if again, err := ParseHostSpec(got.String()); err != nil || again != got {
			t.Fatalf("ParseHostSpec(%q) = %+v, %v", got.String(), again, err)
		}
	}
}

// A host spec parses to a host name, a family CheckFamily takes and an
// address octet in range, or not at all, and writes back the same.
func TestPropParseHostSpec(t *testing.T) {
	for _, s := range []string{"dc=WIN 2022", "dc=win2022-v1", "dc=x-v10000", "dc=a=b", "dc=win2022:+10", "dc=win2022:007", "dc==x", "dc=:10"} {
		checkHostSpec(t, s)
	}
	for part, values := range hostSpecParts {
		for _, v := range values {
			parts := map[string]string{"host": "teak", "family": "debian-12", "octet": ":10", "sep": "="}
			parts[part] = v
			checkHostSpec(t, parts["host"]+parts["sep"]+parts["family"]+parts["octet"])
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		// Spoil one part; the rest are valid.
		spoilt := rapid.SampledFrom([]string{"host", "family", "octet", "sep", "none", "any"}).Draw(t, "spoilt")
		if spoilt == "any" {
			checkHostSpec(t, rapid.String().Draw(t, "spec"))
			return
		}
		s := ""
		for _, part := range []string{"host", "sep", "family", "octet"} {
			values := hostSpecParts[part]
			if part != spoilt {
				values = values[:1]
				if part == "octet" {
					values = []string{"", ":10", ": 20 "}
				}
			}
			s += rapid.SampledFrom(values).Draw(t, part)
		}
		checkHostSpec(t, s)
	})
}
