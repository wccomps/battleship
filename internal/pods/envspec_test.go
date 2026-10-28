package pods

import (
	"net/netip"
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

// parsedIP is s as ParseNotes reads an ip= value: the zero Addr if it
// isn't an address.
func parsedIP(s string) netip.Addr {
	a, _ := netip.ParseAddr(s)
	return a
}

func TestNotesLine(t *testing.T) {
	desc := "Domain controller.\nbattleship: ip=192.168.100.10 os=windows base=win2022-v3 dns=router\nkeep me"
	n, ok := ParseNotes(desc)
	if !ok || n != (Notes{IP: netip.MustParseAddr("192.168.100.10"), OS: "windows", Base: "win2022-v3", DNS: "router"}) {
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
		checkUserNotes(t, strings.Join(rapid.SliceOfN(piece, 0, 40).Draw(t, "pieces"), ""))
	})
}

func checkUserNotes(t interface{ Fatalf(string, ...any) }, desc string) {
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

// notesPieces build descriptions: battleship lines, near misses of them,
// line breaks and what people write.
var notesPieces = []string{"battleship: ip=192.168.100.10 os=linux", "  battleship: base=gw-v1", "\tbattleship:x", "Battleship: ip=1",
	"battleship ip=1", "xbattleship:", "\u200bbattleship: ip=1", "battleship:\u2028ip=1\u0085os=x", "battleship:  ip=a=b\tdns=\u00a0x",
	"\n", "\r\n", "\r", "  ", "\n\n", "Login: root", "pw: hunter2 "}

// oneLine reports whether s is fields joined by single spaces, with no
// line break or other space Fields would split on.
func oneLine(s string) bool { return strings.Join(strings.Fields(s), " ") == s }

// The battleship line written into any description reads back the same,
// writing it twice changes nothing, and people's notes around it are kept.
func TestPropNotesLineRoundTrip(t *testing.T) {
	n := Notes{IP: netip.MustParseAddr("192.168.100.10"), OS: "linux", Base: "a=b", DNS: "router"}
	for _, a := range notesPieces {
		for _, b := range notesPieces {
			with := WithNotes(a+b, n)
			if got, ok := ParseNotes(with); !ok || got != n || WithNotes(with, n) != with || UserNotes(with) != UserNotes(a+b) {
				t.Errorf("WithNotes(%q) = %q, reads %+v", a+b, with, got)
			}
		}
	}
	value := rapid.StringMatching(`[A-Za-z0-9._:=-]{0,12}`)
	rapid.Check(t, func(t *rapid.T) {
		piece := rapid.OneOf(rapid.SampledFrom(notesPieces), rapid.SampledFrom(notesPieces), rapid.String())
		desc := strings.Join(rapid.SliceOfN(piece, 0, 8).Draw(t, "pieces"), "")
		if old, ok := ParseNotes(desc); ok && !oneLine(old.String()) {
			t.Fatalf("ParseNotes(%q) = %+v, written %q", desc, old, old.String())
		}
		if c := (Notes{IP: parsedIP(editedNoteValue(t)), OS: editedNoteValue(t), Base: value.Draw(t, "base"), DNS: editedNoteValue(t)}).Checked(lanOf("192.168.100")); !oneLine(c.String()) {
			t.Fatalf("Checked notes written %q", c.String())
		}
		n := Notes{IP: parsedIP(value.Draw(t, "ip")), OS: value.Draw(t, "os"), Base: value.Draw(t, "base"), DNS: value.Draw(t, "dns")}
		checkNotesLine(t, desc, n)
		cur := map[string]string{"description": desc, "tags": rapid.SampledFrom([]string{"", "dev", "Dev;kilo.alpha", "kilo.alpha,dev", " dev "}).Draw(t, "tags")}
		tags := rapid.SampledFrom([]string{"", "dev", "dev;kilo.alpha", "kilo.alpha;router"}).Draw(t, "new tags")
		for k, v := range LabelChanges(cur, tags, &n) {
			if k == "delete" {
				delete(cur, v)
			} else {
				cur[k] = v
			}
		}
		if again := LabelChanges(cur, tags, &n); len(again) != 0 {
			t.Fatalf("LabelChanges after applying them: %q", again)
		}
	})
}

func checkNotesLine(t interface{ Fatalf(string, ...any) }, desc string, n Notes) {
	with := WithNotes(desc, n)
	if got, ok := ParseNotes(with); !ok || got != n {
		t.Fatalf("ParseNotes(WithNotes(%q, %+v)) = %+v, %v", desc, n, got, ok)
	}
	if again := WithNotes(with, n); again != with {
		t.Fatalf("WithNotes twice: %q, then %q", with, again)
	}
	if UserNotes(with) != UserNotes(desc) {
		t.Fatalf("UserNotes(WithNotes(%q)) = %q, want %q", desc, UserNotes(with), UserNotes(desc))
	}
}

// editedNoteValue draws a note value as people might edit it: spaces, line breaks
// and script characters around address-like text.
func editedNoteValue(t *rapid.T) string {
	return rapid.SampledFrom([]string{"", "10", "192.168.100.10", "192.168.100.10\n", "dc\u2028x", "router", "windows", "a b", "x\u0085"}).Draw(t, "hostile")
}
