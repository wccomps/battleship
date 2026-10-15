package pods

import (
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
