package pods

import (
	"context"
	"encoding/json"
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

// freeOK is the rule for free text, written apart from checkFreeNotes:
// valid UTF-8 of letters, marks, numbers, punctuation, symbols, spaces and
// newlines; at most MaxFreeNotes characters; no space or newline at the
// start or end, and no space at a line's end; and no line that, less its
// spaces, starts "battleship:" or is the block's header.
func freeOK(s string) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxFreeNotes || strings.Trim(s, " \n") != s {
		return false
	}
	for _, r := range s {
		if r != '\n' && r != ' ' && !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S) {
			return false
		}
	}
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasSuffix(line, " ") || strings.HasPrefix(t, "battleship:") || t == "**Details**" {
			return false
		}
	}
	return true
}

// writeFree is Apply of details and free text.
func writeFree(desc string, d ImageDetails, free string) (string, error) {
	return ImageNotes{ImageDetails: d, Free: &free}.Apply(desc)
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

// Whatever free text is asked for, Apply refuses it, leaving the
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

		out, err := writeFree(desc, d, free)
		if want := freeOK(free) && CheckDetails(d) == nil; (err == nil) != want {
			t.Fatalf("Apply(%q) err %v; the rule accepts it: %v", free, err, want)
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
		if w := EditStart(out); w != (DetailsWas{Details: d, Notes: free}) {
			t.Fatalf("EditStart(%q) = %+v", out, w)
		}
		if again, _ := writeFree(out, d, free); again != out {
			t.Fatalf("writing again changed %q to %q", out, again)
		}
	})
}

// CleanFreeNotes is what a form posts as the notes keep it, once: no tab,
// CR, line or paragraph separator, or space other than U+0020 is
// left, nor a space ending a line, nor space or newlines at either end.
func TestPropCleanFreeNotes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := strings.Join(rapid.SliceOfN(rapid.OneOf(rapid.SampledFrom(freePieces), rapid.String()), 0, 6).Draw(t, "pieces"), "")
		c := CleanFreeNotes(s)
		if CleanFreeNotes(c) != c {
			t.Fatalf("CleanFreeNotes(%q) = %q, again %q", s, c, CleanFreeNotes(c))
		}
		if strings.ContainsFunc(c, func(r rune) bool {
			return r == '\t' || r == '\r' || r == '\u2028' || r == '\u2029' || (r != ' ' && unicode.Is(unicode.Zs, r))
		}) || strings.Contains(c, " \n") || strings.TrimSpace(c) != c {
			t.Fatalf("CleanFreeNotes(%q) = %q", s, c)
		}
		if checkFreeNotes(c) == nil && userText(c) != c {
			t.Fatalf("UserNotes reads %q back as %q", c, userText(c))
		}
	})
}

// checkFreeNotes says why it refuses text.
func TestCheckFreeNotes(t *testing.T) {
	for _, ok := range []string{"", "Patched monthly.\n\nRebooted.", "x battleship: y", "Battleship: x", "**details**", "- OS: Debian 12",
		"<b>x</b> & é", strings.Repeat("é", MaxFreeNotes)} {
		if err := checkFreeNotes(ok); err != nil {
			t.Errorf("checkFreeNotes(%q) = %v", ok, err)
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
		if err := checkFreeNotes(s); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("checkFreeNotes(%.20q) = %v, want %q", s, err, why)
		}
	}
	for in, want := range map[string]string{
		"\r\n  a  \r\n\r\nb\t\r\n\r\n": "a\n\nb",
		"User\tpass":                   "User    pass",
		"a\u00a0b\u3000c\u2007d":       "a b c d",
		"a\u2028b\u2029c\rd":           "a\nb\nc\nd",
		"a\u200bb\u0085":               "a\u200bb",
	} {
		if got := CleanFreeNotes(in); got != want {
			t.Errorf("CleanFreeNotes(%q) = %q, want %q", in, got, want)
		}
	}
}

// An unreadable block is editable: no details, the text outside it, and
// the block as it reads, which the details replace.
func TestEditStartUnreadable(t *testing.T) {
	desc := "intro\n\n**Details**\n- Colour: blue\nrun on\n\noutro\nbattleship: source=1"
	want := DetailsWas{Unreadable: true, Notes: "intro\n\noutro", Block: "**Details**\n- Colour: blue\nrun on"}
	if w := EditStart(desc); w != want {
		t.Errorf("EditStart = %+v", w)
	}
	out, err := writeFree(desc, ImageDetails{OS: "Debian 12"}, want.Notes)
	if err != nil || out != "**Details**\n- OS: Debian 12\n\nintro\n\noutro\n\nbattleship: source=1" {
		t.Errorf("Apply = %q, %v", out, err)
	}
	two := "**Details**\n- OS: a\n\nmid\n\n**Details**\n- OS: b"
	if w := EditStart(two); !w.Unreadable || w.Notes != "mid" || w.Block != "**Details**\n- OS: a\n\n**Details**\n- OS: b" {
		t.Errorf("EditStart over two blocks = %+v", w)
	}
	long := strings.Repeat("x", maxUserNotes+10)
	if w := EditStart(long); w.Notes != long {
		t.Errorf("EditStart caps the text at %d characters", len(w.Notes))
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
	plan, err := p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{winDetails, &notes}})
	if err != nil {
		t.Fatal(err)
	}
	it := plan.Items[0]
	if it.Blocked != "" || it.Details.Free == nil || *it.Details.Free != notes || it.DetailsWas.Notes != "Patched." {
		t.Errorf("item = %+v, was %+v", it, it.DetailsWas)
	}
	same := "Patched."
	for _, n := range []*string{&same, nil} {
		plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{winDetails, n}})
		if !strings.Contains(plan.Items[0].Blocked, "already say this") {
			t.Errorf("unchanged notes %v: %+v", n, plan.Items[0])
		}
	}
	debian := ImageNotes{ImageDetails: ImageDetails{OS: "Debian 12"}}
	plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &debian})
	if it := plan.Items[0]; it.Details.Free != nil || it.Blocked != "" {
		t.Errorf("without notes: %+v", it)
	}
	ch, _ := debian.Changes(f.VMs[8001].Config)
	if want := "**Details**\n- OS: Debian 12\n\nPatched.\nbattleship: source=150"; ch["description"] != want {
		t.Errorf("without notes, writes %q", ch["description"])
	}
	big := strings.Repeat("é", MaxFreeNotes) // 8000 bytes
	plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{winDetails, &big}})
	if !strings.Contains(plan.Items[0].Blocked, "Proxmox keeps at most 8192") {
		t.Errorf("notes too big for Proxmox: %+v", plan.Items[0].Blocked)
	}
	bad := "x\nbattleship: ip=10.50.0.1"
	if _, err := p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{winDetails, &bad}}); err == nil {
		t.Error("planned notes with a battleship line")
	}
}

// browserPost is text as a form posts it back from a textarea: line breaks
// as CRLF.
func browserPost(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\n", "\r\n")
}

// outsideBlocks is desc's lines outside every details block, each with the
// blank line after it, joined: what a details-only write leaves as it was.
func outsideBlocks(desc string) string {
	lines := strings.Split(desc, "\n")
	return strings.Join(without(lines, blocks(lines)), "\n")
}

// Notes that free text written by Edit notes couldn't hold (a tab, NBSP, a
// line separator, more than MaxFreeNotes characters) don't stop an edit of
// the details: the form's text sent back unchanged is kept exactly.
func TestEditKeepsUnchangedNotes(t *testing.T) {
	ctx := context.Background()
	for _, free := range []string{
		"User\tpass", "Login:\u00a0admin", "line\u2028sep", strings.Repeat("x", MaxFreeNotes+1),
		"Family \U0001F468\u200d\U0001F469\u200d\U0001F467 ok", "private \ue000 use", "bell \x07 here",
	} {
		desc := winBlock + "\n\n" + free + "\nbattleship: source=1"
		f := imageFake()
		f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v2", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"description": desc})
		posted := CleanFreeNotes(browserPost(EditStart(desc).Notes))
		n := ImageNotes{ImageDetails{OS: "Debian 13"}, &posted}
		plan, err := NewPlanner(f, envConfig()).ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &n})
		if err != nil {
			t.Errorf("%.20q: %v", free, err)
			continue
		}
		it := plan.Items[0]
		ch, err := it.Details.Changes(f.VMs[8001].Config)
		if it.Blocked != "" || err != nil || outsideBlocks(ch["description"]) != outsideBlocks(desc) {
			t.Errorf("%.20q: blocked %q, %v, writes %q", free, it.Blocked, err, ch["description"])
		}
	}
}

// Notes with free text keep the JSON their details alone had, and read
// back from it with none.
func TestPropImageNotesJSON(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		val := rapid.OneOf(rapid.Just(""), rapid.String(), rapid.SampledFrom([]string{"Debian 12", "a / b", "\"q\"", "<&>"}))
		d := ImageDetails{OS: val.Draw(t, "os"), User: val.Draw(t, "user"), Password: val.Draw(t, "password"), Built: val.Draw(t, "built"),
			Generalized: val.Draw(t, "generalized"), Installed: val.Draw(t, "installed"), Licence: val.Draw(t, "licence")}
		was, _ := json.Marshal(d)
		now, _ := json.Marshal(ImageNotes{ImageDetails: d})
		if string(now) != string(was) {
			t.Fatalf("ImageNotes JSON %s, details alone %s", now, was)
		}
		var back ImageNotes
		if err := json.Unmarshal(was, &back); err != nil || back.Free != nil {
			t.Fatalf("read back %+v, %v", back, err)
		}
		var dBack ImageDetails
		_ = json.Unmarshal(was, &dBack)
		if back.ImageDetails != dBack {
			t.Fatalf("read back %+v, want %+v", back.ImageDetails, dBack)
		}
	})
}

// Every write of notes is held to what Proxmox keeps, so the details step
// fails rather than send a description Proxmox refuses, whatever planned
// it: a register's copy of long notes, too.
func TestApplyRefusesNotesTooLong(t *testing.T) {
	desc := strings.Repeat("x", maxNotesBytes-10)
	n := ImageNotes{ImageDetails: ImageDetails{OS: "Debian 12"}}
	if ch, err := n.Changes(map[string]string{"description": desc}); err == nil || ch != nil || !strings.Contains(err.Error(), "at most 8192") {
		t.Errorf("Changes = %v, %v", ch, err)
	}
	if out, err := n.Apply(""); err != nil || len(out) > maxNotesBytes {
		t.Errorf("Apply = %q, %v", out, err)
	}
}
