package pods

import (
	"context"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/proxmox"
)

// freePieces are pieces of free text hostile to the notes: battleship
// lines, block headers and items, control and invisible characters, CRLF.
var freePieces = []string{
	"battleship: ip=10.50.0.1", "\nbattleship: base=x-v1 source=7", "  battleship: os=windows", "\u200bbattleship: ip=10.50.0.2",
	detailsHeader, "\n" + detailsHeader + "\n", " " + detailsHeader + " ", "\u200b" + detailsHeader, "**details**",
	"- OS: Debian 12", "\n- Login: root / pw", "\n- Colour: blue",
	"\r\n", "\r", "\n", "\n\n", "\t", " ", "\x00", "\x1b", "\x7f", "\u0085", "\u00a0", "\u200b", "\u200e", "\u202e", "\u2028", "\u2066", "\ufeff",
	"\xff", "Battleship: x", "x battleship: y", "Patched monthly.", "<b>x</b>", "é",
}

// drawFree draws free text from freePieces and arbitrary text, cleaned as
// a form cleans it or not.
func drawFree(t *rapid.T) string {
	piece := rapid.OneOf(rapid.SampledFrom(freePieces), rapid.SampledFrom(freePieces), rapid.StringMatching(`[A-Za-z0-9 .:/-]{0,12}`), rapid.String())
	s := strings.Join(rapid.SliceOfN(piece, 0, 6).Draw(t, "free pieces"), "")
	if rapid.Bool().Draw(t, "cleaned") {
		s = CleanFreeNotes(s)
	}
	return s
}

// freeOK is the rule for free text, written apart from CheckFreeNotes:
// valid UTF-8 of letters, marks, numbers, punctuation, symbols, spaces and
// newlines; at most MaxFreeNotes characters; trimmed as CleanFreeNotes
// leaves it; and no line that, less its spaces, starts "battleship:" or is
// the block's header.
func freeOK(s string) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxFreeNotes || s != CleanFreeNotes(s) {
		return false
	}
	for _, r := range s {
		if r != '\n' && r != ' ' && !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S) {
			return false
		}
	}
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "battleship:") || t == "**Details**" {
			return false
		}
	}
	return true
}

// shipLines are desc's battleship lines as people never see them
// (UserNotes drops them): a line that, less format characters and spaces,
// starts "battleship:".
func shipLines(desc string) []string {
	var out []string
	for _, line := range strings.Split(desc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, line)), "battleship:") {
			out = append(out, line)
		}
	}
	return out
}

// headers counts desc's lines that head a details block.
func headers(desc string) int {
	n := 0
	for _, line := range strings.Split(desc, "\n") {
		if isHeader(line) {
			n++
		}
	}
	return n
}

// Whatever free text is asked for, WithFreeNotes refuses it, leaving the
// notes as they were, just when the rule refuses it; and what it writes
// keeps every battleship line exactly and in order, has exactly the one
// block the details make, and reads back as exactly those details and that
// text.
func TestPropFreeNotesCannotForgeLines(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d := rapid.SampledFrom([]ImageDetails{{}, winDetails, {OS: "Debian 12"}, {User: "root"}, {OS: "a\nbattleship: ip=1"}}).Draw(t, "details")
		prior := rapid.SampledFrom([]string{"", "free text", winBlock + "\n\nPatched.", "intro\n\n" + winBlock + "\n\noutro",
			"**Details**\n- Colour: blue\n\nkept?", "**Details**\n- OS: a\n**Details**\n- OS: b", proseNotes, "a\r\nb\r\n"}).Draw(t, "prior")
		line := rapid.SampledFrom([]string{"", "\nbattleship: source=150", "\nbattleship: ip=192.168.100.10 os=linux base=debian-12-v1\r",
			"\n  battleship: base=x-v1", "\n\u200bbattleship: hidden=1\nbattleship: source=2"}).Draw(t, "line")
		desc := prior + line
		if rapid.Bool().Draw(t, "line first") {
			desc = strings.TrimPrefix(line, "\n") + "\n" + prior
		}
		free := drawFree(t)

		out, err := WithFreeNotes(desc, d, free)
		if want := freeOK(free) && CheckDetails(d) == nil; (err == nil) != want {
			t.Fatalf("WithFreeNotes(%q) err %v; the rule accepts it: %v", free, err, want)
		}
		if err != nil {
			if out != desc {
				t.Fatalf("refused, but changed the notes to %q", out)
			}
			return
		}
		if got, want := shipLines(out), shipLines(desc); !slices.Equal(got, want) {
			t.Fatalf("battleship lines %q, want %q, in %q", got, want, out)
		}
		n, has := ParseNotes(out)
		if n0, has0 := ParseNotes(desc); n != n0 || has != has0 {
			t.Fatalf("ParseNotes = %+v %v, was %+v %v", n, has, n0, has0)
		}
		if want := min(1, len(d.block())); headers(out) != want || len(blocks(strings.Split(out, "\n"))) != want {
			t.Fatalf("%d headers in %q, want %d", headers(out), out, want)
		}
		if got, f, err := ParseDetails(out); err != nil || got != d || f != free {
			t.Fatalf("ParseDetails(%q) = %+v, %q, %v", out, got, f, err)
		}
		if got, f, err := EditableNotes(out); err != nil || got != d || f != free {
			t.Fatalf("EditableNotes(%q) = %+v, %q, %v", out, got, f, err)
		}
		if again, _ := WithFreeNotes(out, d, free); again != out {
			t.Fatalf("writing again changed %q to %q", out, again)
		}
	})
}

// CleanFreeNotes is what a form posts as the notes keep it, once.
func TestPropCleanFreeNotes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := strings.Join(rapid.SliceOfN(rapid.OneOf(rapid.SampledFrom(freePieces), rapid.String()), 0, 6).Draw(t, "pieces"), "")
		c := CleanFreeNotes(s)
		if CleanFreeNotes(c) != c || strings.Contains(c, "\r\n") {
			t.Fatalf("CleanFreeNotes(%q) = %q, again %q", s, c, CleanFreeNotes(c))
		}
		if CheckFreeNotes(c) == nil && userText(c) != c {
			t.Fatalf("UserNotes reads %q back as %q", c, userText(c))
		}
	})
}

// CheckFreeNotes says why it refuses text.
func TestCheckFreeNotes(t *testing.T) {
	for _, ok := range []string{"", "Patched monthly.\n\nRebooted.", "x battleship: y", "Battleship: x", "**details**", "- OS: Debian 12",
		"<b>x</b> & é", strings.Repeat("é", MaxFreeNotes)} {
		if err := CheckFreeNotes(ok); err != nil {
			t.Errorf("CheckFreeNotes(%q) = %v", ok, err)
		}
	}
	for s, why := range map[string]string{
		"a\nbattleship: ip=10.50.0.1":       "Battleship's own line",
		"a\n  battleship:":                  "Battleship's own line",
		"**Details**\n- OS: x":              "heads the details",
		"a\n **Details**":                   "heads the details",
		"a\r\nb":                            "printable",
		"a\tb":                              "printable",
		"a\x00b":                            "printable",
		"a\u200bb":                          "printable",
		"a\u202eb":                          "printable",
		"a\u00a0b":                          "printable",
		"\xff":                              "printable",
		strings.Repeat("x", MaxFreeNotes+1): "too long",
		"\nx":                               "blank lines",
		"x  \ny":                            "end a line with spaces",
	} {
		if err := CheckFreeNotes(s); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("CheckFreeNotes(%.20q) = %v, want %q", s, err, why)
		}
	}
	if got := CleanFreeNotes("\r\n  a  \r\n\r\nb\t\r\n\r\n"); got != "a\n\nb" {
		t.Errorf("CleanFreeNotes = %q", got)
	}
}

// An unreadable block is editable: no details, and the text outside it.
func TestEditableNotesUnreadable(t *testing.T) {
	desc := "intro\n\n**Details**\n- Colour: blue\nrun on\n\noutro\nbattleship: source=1"
	d, free, err := EditableNotes(desc)
	if err != ErrDetailsUnreadable || !d.Empty() || free != "intro\n\noutro" {
		t.Errorf("EditableNotes = %+v, %q, %v", d, free, err)
	}
	out, err := WithFreeNotes(desc, ImageDetails{OS: "Debian 12"}, free)
	if err != nil || out != "**Details**\n- OS: Debian 12\n\nintro\n\noutro\n\nbattleship: source=1" {
		t.Errorf("WithFreeNotes = %q, %v", out, err)
	}
	long := strings.Repeat("x", maxUserNotes+10)
	if _, free, _ := EditableNotes(long); free != long {
		t.Errorf("EditableNotes caps the text at %d characters", len(free))
	}
}

// Edit notes writes free text when asked, saying what it was; without it
// (a job stored before Edit notes could write it) it keeps what is there.
func TestImageSetNotes(t *testing.T) {
	ctx := context.Background()
	f := imageFake()
	f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v2", Node: "alder", Template: true, Tags: "base;linux"},
		map[string]string{"description": winBlock + "\n\nPatched.\nbattleship: source=150"})
	p := NewPlanner(f, envConfig())
	notes := "Patched.\nRebooted."
	plan, err := p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &winDetails, Notes: &notes})
	if err != nil {
		t.Fatal(err)
	}
	it := plan.Items[0]
	if it.Blocked != "" || it.FreeNotes == nil || *it.FreeNotes != notes || it.DetailsWas.Notes != "Patched." {
		t.Errorf("item = %+v, was %+v", it, it.DetailsWas)
	}
	same := "Patched."
	for _, n := range []*string{&same, nil} {
		plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &winDetails, Notes: n})
		if !strings.Contains(plan.Items[0].Blocked, "already say this") {
			t.Errorf("unchanged notes %v: %+v", n, plan.Items[0])
		}
	}
	plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageDetails{OS: "Debian 12"}})
	if it := plan.Items[0]; it.FreeNotes != nil || it.Blocked != "" {
		t.Errorf("without notes: %+v", it)
	}
	ch, _ := DetailsChanges(f.VMs[8001].Config, ImageDetails{OS: "Debian 12"}, nil)
	if want := "**Details**\n- OS: Debian 12\n\nPatched.\nbattleship: source=150"; ch["description"] != want {
		t.Errorf("without notes, writes %q", ch["description"])
	}
	big := strings.Repeat("é", MaxFreeNotes) // 8000 bytes
	plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &winDetails, Notes: &big})
	if !strings.Contains(plan.Items[0].Blocked, "Proxmox keeps at most 8192") {
		t.Errorf("notes too big for Proxmox: %+v", plan.Items[0].Blocked)
	}
	bad := "x\nbattleship: ip=10.50.0.1"
	if _, err := p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &winDetails, Notes: &bad}); err == nil {
		t.Error("planned notes with a battleship line")
	}
}
