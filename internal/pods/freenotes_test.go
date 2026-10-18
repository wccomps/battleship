package pods

import (
	"context"
	"encoding/json"
	"fmt"
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
// valid UTF-8 with no control character but LF (C0, DEL, C1), no
// bidirectional control, and no space but U+0020; at most MaxFreeNotes
// characters; no space or newline at the start or end, and no space at a
// line's end; and no line that, less its format characters and spaces,
// starts "battleship:" or is the block's header. Format, private-use and
// unassigned characters are text like any other.
func freeOK(s string) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxFreeNotes || strings.Trim(s, " \n") != s {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\n' || r == ' ':
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return false
		case r == 0x061c || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			return false
		case unicode.In(r, unicode.Zs, unicode.Zl, unicode.Zp):
			return false
		}
	}
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(noFormat(line))
		if strings.HasSuffix(line, " ") || strings.HasPrefix(t, "battleship:") || t == "**Details**" {
			return false
		}
	}
	return true
}

// noFormat is s less its format characters (Cf).
func noFormat(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// writeFree is apply of details and free text.
func writeFree(desc string, d ImageDetails, free string) (string, error) {
	return ImageNotes{ImageDetails: d, Free: &free}.apply(desc)
}

// shipLines are desc's battleship lines as people never see them
// (UserNotes drops them): a line that, less format characters and spaces,
// starts "battleship:".
func shipLines(desc string) []string {
	var out []string
	for _, line := range strings.Split(desc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(noFormat(line)), "battleship:") {
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

// Whatever free text is asked for, apply refuses it, leaving the
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
			t.Fatalf("apply(%q) err %v; the rule accepts it: %v", free, err, want)
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
		if got, _, err := ParseDetails(out); err != nil || got != d {
			t.Fatalf("ParseDetails(%q) = %+v, %v", out, got, err)
		}
		if w := readImageNotes(out); w != (NotesRead{Details: d, Free: free}) {
			t.Fatalf("readImageNotes(%q) = %+v", out, w)
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
		if checkFreeNotes(c) == nil && editText(c) != c {
			t.Fatalf("Edit notes reads %q back as %q", c, editText(c))
		}
	})
}

// checkFreeNotes refuses only what can hide what notes say, or pass for
// Battleship's lines, and says which.
func TestCheckFreeNotes(t *testing.T) {
	for _, ok := range []string{"", "Patched monthly.\n\nRebooted.", "x battleship: y", "Battleship: x", "**details**", "- OS: Debian 12",
		"<b>x</b> & \u00e9", strings.Repeat("\u00e9", MaxFreeNotes), "a\u200bb", "Face \U0001FAE9", "private \ue000 \U000F0000",
		"zwnj \u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645", "soft hy\u00adphen", "\U0001F468\u200d\U0001F469", "bom \ufeff"} {
		if err := checkFreeNotes(ok); err != nil {
			t.Errorf("checkFreeNotes(%q) = %v", ok, err)
		}
	}
	for s, why := range map[string]string{
		"a\nbattleship: ip=10.50.0.1":       "Battleship's own line",
		"a\n  battleship:":                  "Battleship's own line",
		"a\n\u200bbattleship: ip=10.50.0.1": "Battleship's own line",
		"a\n\u2060\u00adbattleship: x":      "Battleship's own line",
		"**Details**\n- OS: x":              "heads the details",
		"a\n **Details**":                   "heads the details",
		"a\n\u200c**Details**\ufeff":        "heads the details",
		"a\r\nb":                            "a control character (U+000D)",
		"a\tb":                              "a control character (U+0009)",
		"a\x00b":                            "a control character (U+0000)",
		"a\x1b[2Jb":                         "a control character (U+001B)",
		"a\u0085b":                          "a control character (U+0085)",
		"a\u202eb":                          "a bidirectional control (U+202E)",
		"a\u2066b":                          "a bidirectional control (U+2066)",
		"a\u200fb":                          "a bidirectional control (U+200F)",
		"a\u00a0b":                          "plain spaces",
		"\xff":                              "UTF-8",
		strings.Repeat("x", MaxFreeNotes+1): "too long",
		"\nx":                               "blank lines at the start",
		"  hello":                           "spaces or blank lines at the start",
		"x  \ny":                            "spaces ending a line",
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
		"a\vb\fc\u0085d":               "a\nb\nc\nd",
		"a\u200bb\u0085":               "a\u200bb",
	} {
		if got := CleanFreeNotes(in); got != want {
			t.Errorf("CleanFreeNotes(%q) = %q, want %q", in, got, want)
		}
	}
}

// An unreadable block is editable: no details, the text outside it, and
// the block as it reads, which the details replace.
func TestReadNotesUnreadable(t *testing.T) {
	desc := "intro\n\n**Details**\n- Colour: blue\nrun on\n\noutro\nbattleship: source=1"
	want := NotesRead{Unreadable: true, Free: "intro\n\noutro", Block: "**Details**\n- Colour: blue\nrun on"}
	if w := readImageNotes(desc); w != want {
		t.Errorf("readImageNotes = %+v", w)
	}
	out, err := writeFree(desc, ImageDetails{OS: "Debian 12"}, want.Free)
	if err != nil || out != "**Details**\n- OS: Debian 12\n\nintro\n\noutro\n\nbattleship: source=1" {
		t.Errorf("apply = %q, %v", out, err)
	}
	two := "**Details**\n- OS: a\n\nmid\n\n**Details**\n- OS: b"
	if w := readImageNotes(two); !w.Unreadable || w.Free != "mid" || w.Block != "**Details**\n- OS: a\n\n**Details**\n- OS: b" {
		t.Errorf("readImageNotes over two blocks = %+v", w)
	}
	long := strings.Repeat("x", maxUserNotes+10)
	if w := readImageNotes(long); w.Free != long {
		t.Errorf("readImageNotes caps the text at %d characters", len(w.Free))
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
	plan, err := p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{ImageDetails: winDetails, Free: &notes}})
	if err != nil {
		t.Fatal(err)
	}
	it := plan.Items[0]
	if it.Blocked != "" || it.Details.Free == nil || *it.Details.Free != notes || it.DetailsWas.Free != "Patched." {
		t.Errorf("item = %+v, was %+v", it, it.DetailsWas)
	}
	same := "Patched."
	for _, n := range []*string{&same, nil} {
		plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{ImageDetails: winDetails, Free: n}})
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
	big := strings.Repeat("\u00e9", MaxFreeNotes) // 8000 bytes, but 4000 characters, as Proxmox counts them
	plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{ImageDetails: winDetails, Free: &big}})
	if plan.Items[0].Blocked != "" {
		t.Errorf("notes Proxmox keeps: blocked %q", plan.Items[0].Blocked)
	}
	f.VMs[8001].Config["description"] = "Patched.\n" + strings.Repeat("\u00e9", maxDescription-20) + "\nbattleship: source=150"
	plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{ImageDetails: winDetails}})
	if !strings.Contains(plan.Items[0].Blocked, "characters; Proxmox keeps at most 8192") {
		t.Errorf("notes too long for Proxmox: %+v", plan.Items[0].Blocked)
	}
	bad := "x\nbattleship: ip=10.50.0.1"
	if _, err := p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{ImageDetails: winDetails, Free: &bad}}); err == nil {
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
		posted := CleanFreeNotes(browserPost(readImageNotes(desc).Free))
		n := ImageNotes{ImageDetails: ImageDetails{OS: "Debian 13"}, Free: &posted}
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

// A UTF-8 description is held to what Proxmox keeps, counted in characters as
// Proxmox counts them, not bytes.
func TestCheckDescriptionCountsCharacters(t *testing.T) {
	for desc, ok := range map[string]bool{
		strings.Repeat("x", maxDescription):            true,
		strings.Repeat("x", maxDescription+1):          false,
		strings.Repeat("\u00e9", maxDescription):       true,
		strings.Repeat("\U0001F468", maxDescription):   true,
		strings.Repeat("\u00e9", maxDescription+1):     false,
		strings.Repeat("\U0001F468", maxDescription+1): false,
	} {
		if err := CheckDescription(desc); (err == nil) != ok {
			t.Errorf("CheckDescription(%d bytes, %d characters) = %v", len(desc), utf8.RuneCountInString(desc), err)
		} else if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("would be %d characters", maxDescription+1)) {
			t.Errorf("CheckDescription = %v", err)
		}
	}
}

// keptRunes are characters notes may hold that editing keeps as they are:
// text, emoji Unicode added after Go's tables (U+1FAE9, U+1FADF), private
// use, and format characters such as ZWNJ, ZWJ, the soft hyphen and the
// word joiner.
var keptRunes = []rune{'a', 'Z', '7', '.', '-', '/', '<', '&', 'é', '́', 'م', '中', '\U0001F468', '\U0001FAE9', '\U0001FADF',
	'\ue000', '\uf8ff', '\U000F0000', '\u200c', '\u200d', '\u00ad', '\u200b', '\u2060', '\ufeff'}

// formatRunes are format characters that could hide a forged line.
var formatRunes = []rune{'\u200b', '\u200c', '\u200d', '\u00ad', '\u2060', '\ufeff'}

// keptLine draws a line of words of keptRunes.
func keptLine(t *rapid.T, label string) string {
	word := rapid.StringOfN(rapid.SampledFrom(keptRunes), 1, 6, -1)
	return strings.Join(rapid.SliceOfN(word, 1, 4).Draw(t, label), " ")
}

// For notes of keptLine lines, an Edit notes changing one line of the free
// text writes that line and leaves every other line byte for byte as it
// was; a changed line that forges a battleship line or a details header,
// hidden behind format characters, is refused, the notes left alone.
func TestPropEditKeepsUntouchedLines(t *testing.T) {
	ctx := context.Background()
	rapid.Check(t, func(t *rapid.T) {
		lines := []string{keptLine(t, "first")}
		for _, blank := range rapid.SliceOfN(rapid.Bool(), 0, 4).Draw(t, "middle blank") {
			if blank {
				lines = append(lines, "")
			} else {
				lines = append(lines, keptLine(t, "middle"))
			}
		}
		lines = append(lines, keptLine(t, "last"))
		d := rapid.SampledFrom([]ImageDetails{{}, winDetails}).Draw(t, "details")
		desc := strings.Join(lines, "\n") + "\nbattleship: source=1"
		if !d.Empty() {
			desc = d.block() + "\n\n" + desc
		}

		i := rapid.IntRange(0, len(lines)).Draw(t, "line edited")
		edited := keptLine(t, "edit")
		forged := rapid.Bool().Draw(t, "forged")
		if forged {
			hide := rapid.StringOfN(rapid.SampledFrom(formatRunes), 1, 3, -1).Draw(t, "hide")
			edited = hide + rapid.SampledFrom([]string{"battleship: ip=10.50.0.9", detailsHeader}).Draw(t, "forgery")
		}
		want := slices.Clone(lines)
		form := strings.Split(readImageNotes(desc).Free, "\n")
		if i == len(lines) {
			want, form = append(want, edited), append(form, edited)
		} else {
			if lines[i] == edited {
				edited += "x"
			}
			want[i], form[i] = edited, edited
		}

		f := imageFake()
		f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v2", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"description": desc})
		posted := CleanFreeNotes(browserPost(strings.Join(form, "\n")))
		plan, err := NewPlanner(f, envConfig()).ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{ImageDetails: d, Free: &posted}})
		if forged {
			if err == nil {
				t.Fatalf("planned the forged line %q", edited)
			}
			return
		}
		if err != nil || plan.Items[0].Blocked != "" {
			t.Fatalf("edit refused: %v %q", err, plan.Items[0].Blocked)
		}
		ch, err := plan.Items[0].Details.Changes(f.VMs[8001].Config)
		wantDesc := strings.Join(want, "\n") + "\n\nbattleship: source=1"
		if !d.Empty() {
			wantDesc = d.block() + "\n\n" + wantDesc
		}
		if err != nil || ch["description"] != wantDesc {
			t.Fatalf("writes %q, %v; want %q", ch["description"], err, wantDesc)
		}
	})
}

// A line elsewhere in the notes that Edit notes's free text couldn't hold
// as it is doesn't stop an edit: a vertical tab, form feed or NEL is the
// line break it stands for, a control character is dropped, and visible
// text Go's tables don't know is kept.
func TestEditWithOddCharactersElsewhere(t *testing.T) {
	ctx := context.Background()
	for other, want := range map[string]string{
		"Face \U0001FAE9":   "Face \U0001FAE9",
		"Logo \uf8ff":       "Logo \uf8ff",
		"vt\vtab":           "vt\ntab",
		"ff\fpage":          "ff\npage",
		"c1 \u0085next":     "c1\nnext",
		"esc \x1b[0mred":    "esc [0mred",
		"bidi \u202eevil":   "bidi evil",
		"zwnj م\u200cx":     "zwnj م\u200cx",
		"soft hy\u00adphen": "soft hy\u00adphen",
	} {
		desc := winBlock + "\n\n" + other + "\nPatched.\nbattleship: source=1"
		f := imageFake()
		f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v2", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"description": desc})
		posted := CleanFreeNotes(browserPost(readImageNotes(desc).Free + "\nRebooted."))
		plan, err := NewPlanner(f, envConfig()).ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{ImageDetails: winDetails, Free: &posted}})
		if err != nil {
			t.Errorf("%q: edit refused: %v", other, err)
			continue
		}
		ch, err := plan.Items[0].Details.Changes(f.VMs[8001].Config)
		if w := winBlock + "\n\n" + want + "\nPatched.\nRebooted.\n\nbattleship: source=1"; plan.Items[0].Blocked != "" || err != nil || ch["description"] != w {
			t.Errorf("%q: blocked %q, %v, writes %q, want %q", other, plan.Items[0].Blocked, err, ch["description"], w)
		}
	}
}

// What editing reads from any notes is free text checkFreeNotes takes, or
// too long for it: a form that changes nothing else can always be saved.
func TestPropEditTextIsWritable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := strings.Join(rapid.SliceOfN(rapid.OneOf(rapid.SampledFrom(freePieces), rapid.String()), 0, 8).Draw(t, "pieces"), "")
		r := readImageNotes(s).Free
		if err := checkFreeNotes(r); err != nil && utf8.RuneCountInString(r) <= MaxFreeNotes {
			t.Fatalf("read %q from %q, which checkFreeNotes refuses: %v", r, s, err)
		}
		if editText(r) != r {
			t.Fatalf("read %q, again %q", r, editText(r))
		}
	})
}

// An Edit notes form changed only the OS, but Bob added a line after it
// opened: its free text, sent back as the form showed it, would remove
// his line, so the plan is blocked; from notes that didn't change, it
// isn't. A version registered since is other notes, too.
func TestEditNotesRefusesNotesChangedSinceTheForm(t *testing.T) {
	ctx := context.Background()
	start := winBlock + "\n\nPatched.\nbattleship: source=1"
	f := imageFake()
	f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v2", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"description": start})
	p := NewPlanner(f, envConfig())
	at, err := p.NotesToEdit(ctx, "debian-12")
	if err != nil {
		t.Fatal(err)
	}
	form := at.Edit()
	form.OS = "Windows Server 2025"
	plan := func() Item {
		t.Helper()
		posted := CleanFreeNotes(browserPost(*form.Free))
		n := form
		n.Free = &posted
		plan, err := p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &n})
		if err != nil {
			t.Fatal(err)
		}
		return plan.Items[0]
	}
	if it := plan(); it.Blocked != "" {
		t.Fatalf("unchanged notes: blocked %q", it.Blocked)
	}
	f.VMs[8001].Config["description"] = winBlock + "\n\nPatched.\nDO NOT DEPLOY\nbattleship: source=1"
	if it := plan(); !strings.Contains(it.Blocked, "changed since you opened this form") {
		t.Errorf("notes changed since the form: blocked %q", it.Blocked)
	}
	f.VMs[8001].Config["description"] = start
	f.Add(proxmox.VM{VMID: 8002, Name: "debian-12-v3", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"description": start})
	if it := plan(); it.Name != "debian-12-v3" || !strings.Contains(it.Blocked, "changed since you opened this form") {
		t.Errorf("a version registered since the form: %s blocked %q", it.Name, it.Blocked)
	}
}
