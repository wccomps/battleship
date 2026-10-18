package pods

import (
	"context"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/proxmox"
)

// proseNotes are a base image's notes as people wrote them before the
// details block, one paragraph.
const proseNotes = "Base image build: Windows Server 2022 Standard Evaluation (Desktop Experience), built 2026-10-14 from windows_server_2022.iso. " +
	"Generalized (sysprep /oobe). Login: Administrator / Template-Password-1. QEMU guest agent + VirtIO drivers installed. " +
	"Eval licence (servers): 180 days from first boot of each clone."

var winDetails = ImageDetails{
	OS: "Windows Server 2022 Standard Evaluation (Desktop Experience)", User: "Administrator", Password: "Template-Password-1",
	Built: "2026-10-14 from windows_server_2022.iso", Generalized: "sysprep /oobe", Installed: "QEMU guest agent, VirtIO drivers",
	Licence: "180-day evaluation from each clone's first boot",
}

const winBlock = "**Details**\n" +
	"- OS: Windows Server 2022 Standard Evaluation (Desktop Experience)\n" +
	"- Login: Administrator / Template-Password-1\n" +
	"- Built: 2026-10-14 from windows_server_2022.iso\n" +
	"- Generalized: sysprep /oobe\n" +
	"- Installed: QEMU guest agent, VirtIO drivers\n" +
	"- Licence: 180-day evaluation from each clone's first boot"

// The block is written as a Markdown list in the fixed order, first in the
// notes, and reads back as the same details, the free text and the
// battleship line apart and unchanged.
func TestDetailsRoundTrip(t *testing.T) {
	desc := "Patched monthly.\nbattleship: source=150"
	got, err := writeDetails(desc, winDetails)
	if err != nil {
		t.Fatal(err)
	}
	if want := winBlock + "\n\n" + desc; got != want {
		t.Fatalf("WithDetails =\n%s\nwant\n%s", got, want)
	}
	d, free, err := ParseDetails(got)
	if err != nil || d != winDetails || free != "Patched monthly." {
		t.Errorf("ParseDetails = %+v, %q, %v", d, free, err)
	}
	if n, _ := ParseNotes(got); n.Source != "150" {
		t.Errorf("battleship line = %+v", n)
	}

	// Rewritten in place: only the block's lines change.
	edited := winDetails
	edited.Password, edited.Installed = "Template-Password-2", ""
	again, err := writeDetails(got, edited)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(strings.Replace(got, "Template-Password-1", "Template-Password-2", 1), "- Installed: QEMU guest agent, VirtIO drivers\n", "", 1)
	if again != want {
		t.Errorf("rewrite =\n%s\nwant\n%s", again, want)
	}
	// Empty details take the block out, and the gap after it.
	if out, _ := writeDetails(got, ImageDetails{}); out != desc {
		t.Errorf("removing the block = %q, want %q", out, desc)
	}
	// Nothing to write, nothing written.
	for _, d := range []string{"", "notes", "battleship: base=gw-v1"} {
		if out, _ := writeDetails(d, ImageDetails{}); out != d {
			t.Errorf("Apply(%q, empty) = %q", d, out)
		}
	}
	if out, _ := writeDetails("  \n", ImageDetails{OS: "Debian 12"}); out != "**Details**\n- OS: Debian 12" {
		t.Errorf("into blank notes = %q", out)
	}
	// A block in the middle of the notes is read and rewritten there.
	mid := "intro\n\n**Details**\n- OS: Debian 12\n\noutro\nbattleship: base=debian-12-v1"
	if d, free, err := ParseDetails(mid); err != nil || d != (ImageDetails{OS: "Debian 12"}) || free != "intro\n\noutro" {
		t.Errorf("mid-notes block = %+v, %q, %v", d, free, err)
	}
	if out, _ := writeDetails(mid, ImageDetails{OS: "Debian 13"}); out != strings.Replace(mid, "12\n", "13\n", 1) {
		t.Errorf("mid-notes rewrite = %q", out)
	}
	// A login without a password, and Proxmox's CRLFs and trailing spaces.
	if d, _, err := ParseDetails("**Details**\r\n- Login: root  \r\n"); err != nil || d != (ImageDetails{User: "root"}) {
		t.Errorf("CRLF block = %+v, %v", d, err)
	}
}

// A block that isn't as Battleship writes it is unreadable, not guessed
// at: the free text is then all of the user notes, block and all.
func TestDetailsUnreadable(t *testing.T) {
	for name, desc := range map[string]string{
		"unknown key":      "**Details**\n- OS: Debian 12\n- Colour: blue",
		"out of order":     "**Details**\n- Login: root / pw\n- OS: Debian 12",
		"twice":            "**Details**\n- OS: Debian 12\n- OS: Debian 13",
		"not a list item":  "**Details**\nOS: Debian 12",
		"run-on line":      "**Details**\n- OS: Debian\n12",
		"empty value":      "**Details**\n- OS:",
		"lower-case key":   "**Details**\n- os: Debian 12",
		"two blocks":       "**Details**\n- OS: a\n\n**Details**\n- OS: b",
		"tab in value":     "**Details**\n- OS: Debian\t12",
		"long value":       "**Details**\n- OS: " + strings.Repeat("x", MaxDetail+1),
		"user ends with /": "**Details**\n- Login: root /",
	} {
		d, free, err := ParseDetails(desc + "\nbattleship: base=x-v1")
		if err != ErrDetailsUnreadable || !d.Empty() || free != UserNotes(desc) {
			t.Errorf("%s: ParseDetails = %+v, %q, %v", name, d, free, err)
		}
	}
	// Rewriting one makes it readable again, keeping the free text.
	out, err := writeDetails("intro\n\n**Details**\n- Colour: blue\n\n**Details**\n- OS: b\noutro\n\nend", ImageDetails{OS: "Debian 12"})
	if err != nil || out != "intro\n\n**Details**\n- OS: Debian 12\n\nend" {
		t.Errorf("rewriting unreadable blocks = %q, %v", out, err)
	}
	out, err = writeDetails("**Details**\n- OS: a\n**Details**\n- OS: b\n\nfree", ImageDetails{OS: "c"})
	if d, free, perr := ParseDetails(out); err != nil || perr != nil || d.OS != "c" || free != "free" {
		t.Errorf("rewriting adjacent blocks = %q (%+v %q %v)", out, d, free, perr)
	}
}

// CheckDetails refuses what would break the block or not read back.
func TestCheckDetails(t *testing.T) {
	if err := CheckDetails(winDetails); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]ImageDetails{
		"newline":         {OS: "a\nbattleship: ip=1.2.3.4"},
		"carriage return": {Built: "a\rb"},
		"line separator":  {OS: "a\u2028b"},
		"zero width":      {OS: "a\u200bb"},
		"bidi override":   {Licence: "a\u202eb"},
		"nbsp":            {OS: "a\u00a0b"},
		"invalid utf-8":   {OS: "\xff"},
		"untrimmed":       {OS: " a"},
		"too long":        {Installed: strings.Repeat("é", MaxDetail+1)},
		"user with sep":   {User: "a / b", Password: "c"},
		"user ends in /":  {User: "a /"},
		"password alone":  {Password: "pw"},
	} {
		if CheckDetails(d) == nil {
			t.Errorf("%s: CheckDetails(%+v) accepted", name, d)
		}
		if out, err := writeDetails("notes", d); err == nil || out != "notes" {
			t.Errorf("%s: WithDetails wrote %q", name, out)
		}
	}
}

// Register keeps a source's block, and from notes written as prose takes
// the login, build and OS, which are plainly said; an unreadable block
// gives none.
func TestRegisterDetails(t *testing.T) {
	d, ok := RegisterDetails(proseNotes + "\nbattleship: source=150")
	want := ImageDetails{OS: winDetails.OS, User: "Administrator", Password: "Template-Password-1", Built: winDetails.Built}
	if !ok || d != want {
		t.Errorf("from prose = %+v, %v\nwant %+v", d, ok, want)
	}
	if d, ok := RegisterDetails(winBlock + "\n\n" + proseNotes); !ok || d != winDetails {
		t.Errorf("from a block = %+v, %v", d, ok)
	}
	if d, ok := RegisterDetails("**Details**\n- Colour: blue\n\n" + proseNotes); ok || !d.Empty() {
		t.Errorf("from an unreadable block = %+v, %v", d, ok)
	}
	for _, desc := range []string{"", "root / opnsense\nLAN .2", "Login: admin", "built yesterday"} {
		if d, ok := RegisterDetails(desc); !ok || !d.Empty() {
			t.Errorf("RegisterDetails(%q) = %+v, %v; nothing is plainly said", desc, d, ok)
		}
	}
	if d, _ := RegisterDetails("login: root / pw\u202e1."); d.Password != "pw1" {
		t.Errorf("format characters kept: %+v", d)
	}
}

// detailValue draws values hostile to the block: newlines, list markers,
// battleship lines, headers and invisible characters among plain text.
func detailValue(t *rapid.T, label string) string {
	piece := rapid.OneOf(
		rapid.SampledFrom([]string{"\n", "\r\n", "- ", "battleship: ip=10.50.0.1", "\nbattleship: base=x-v1", detailsHeader, "\n" + detailsHeader + "\n",
			" / ", "/", ":", " ", "\t", "\u2028", "\u200b", "\u202e", "\u00a0", "\xff", "Template-Password-1", "Debian 12", "- OS: x"}),
		rapid.StringMatching(`[A-Za-z0-9 ./:-]{0,10}`),
		rapid.String(),
	)
	return strings.Join(rapid.SliceOfN(piece, 0, 4).Draw(t, label), "")
}

// Whatever details are asked for, WithDetails either refuses them, leaving
// the notes as they were, or writes a block that reads back as exactly
// those details, with the free text and the battleship line unchanged and
// no line of its own beyond the block's.
func TestPropDetailsCannotForgeLines(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d := ImageDetails{OS: detailValue(t, "os"), User: detailValue(t, "user"), Password: detailValue(t, "password"),
			Built: detailValue(t, "built"), Generalized: detailValue(t, "generalized"), Installed: detailValue(t, "installed"),
			Licence: detailValue(t, "licence")}
		if rapid.Bool().Draw(t, "trimmed") {
			d = d.Trimmed()
		}
		prior := rapid.SampledFrom([]string{"", "free text", "Login: root / pw\nmore", winBlock, "intro\n\n" + winBlock + "\n\noutro",
			proseNotes}).Draw(t, "prior")
		line := rapid.SampledFrom([]string{"", "\nbattleship: source=150", "\nbattleship: ip=192.168.100.10 os=linux base=debian-12-v1"}).Draw(t, "line")
		desc := prior + line
		_, freeBefore, err := ParseDetails(desc)
		if err != nil {
			t.Fatalf("prior notes unreadable: %v", err)
		}
		notesBefore, hadLine := ParseNotes(desc)
		out, err := writeDetails(desc, d)
		if err != nil {
			if out != desc {
				t.Fatalf("refused, but changed the notes to %q", out)
			}
			if CheckDetails(d) == nil {
				t.Fatalf("refused valid details: %v", err)
			}
			return
		}
		got, free, err := ParseDetails(out)
		if err != nil || got != d || free != freeBefore {
			t.Fatalf("wrote %q, read back %+v, %q, %v; want %+v, %q", out, got, free, err, d, freeBefore)
		}
		if n, has := ParseNotes(out); n != notesBefore || has != hadLine || notesLines(out) != notesLines(desc) {
			t.Fatalf("battleship line changed: %q", out)
		}
		lines := 0
		for _, k := range DetailKeys {
			if d.Value(k) != "" {
				lines++
			}
		}
		if lines > 0 {
			lines++ // the header
		}
		if strings.Count("\n"+out, "\n- ") > strings.Count("\n"+desc, "\n- ")+lines || len(blocks(strings.Split(out, "\n"))) != min(lines, 1) {
			t.Fatalf("wrote more lines than the block's: %q", out)
		}
		if again, _ := writeDetails(out, d); again != out {
			t.Fatalf("writing again changed %q to %q", out, again)
		}
	})
}

// A register writes the details it is given, else those of its source's
// notes, after the label step; empty details write none, so the copy keeps
// its source's notes as they are.
func TestImageRegisterDetails(t *testing.T) {
	ctx := context.Background()
	f := imageFake()
	f.VMs[150].Config["description"] = proseNotes
	p := NewPlanner(f, envConfig())
	want := []Step{StepClone, StepLabel, StepDetails, StepCDROM, StepConvert, StepRetire}
	plan, err := p.ImageRegister(ctx, ImageRequest{VMID: 150, Family: "debian-12"})
	if it := plan.Items[0]; err != nil || !slices.Equal(it.Steps, want) || it.Details == nil || it.Details.User != "Administrator" ||
		it.Source.Notes != proseNotes || it.Source.Unreadable {
		t.Errorf("from prose: %+v, %v", it, err)
	}
	given := ImageNotes{ImageDetails: ImageDetails{OS: "Debian 12"}}
	plan, _ = p.ImageRegister(ctx, ImageRequest{VMID: 150, Family: "debian-12", Details: &given})
	if it := plan.Items[0]; !slices.Equal(it.Steps, want) || *it.Details != given {
		t.Errorf("given: %+v", it)
	}
	for _, c := range []struct {
		name, desc string
		d          *ImageNotes
	}{{"empty", winBlock, &ImageNotes{}}, {"unreadable source", "**Details**\n- Colour: blue", nil}} {
		f.VMs[150].Config["description"] = c.desc
		plan, _ = p.ImageRegister(ctx, ImageRequest{VMID: 150, Family: "debian-12", Details: c.d})
		if it := plan.Items[0]; slices.Contains(it.Steps, StepDetails) || it.Details != nil {
			t.Errorf("%s: %+v", c.name, it)
		}
	}
	if !plan.Items[0].Source.Unreadable {
		t.Errorf("an unreadable source isn't said to be: %+v", plan.Items[0].Source)
	}
	if _, err := p.ImageRegister(ctx, ImageRequest{VMID: 150, Family: "debian-12", Details: &ImageNotes{ImageDetails: ImageDetails{OS: "a\nb"}}}); err == nil {
		t.Error("register took details that would break the block")
	}
}

// Edit notes without free text writes the newest version's details only,
// saying what they were; the same details again are nothing to do.
func TestImageSetDetails(t *testing.T) {
	ctx := context.Background()
	f := imageFake()
	f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v2", Node: "alder", Template: true, Tags: "base;linux"},
		map[string]string{"description": winBlock + "\nbattleship: source=150"})
	f.Add(proxmox.VM{VMID: 8002, Name: "debian-12-v3", Node: "alder", Template: true, Tags: "base;linux;deprecated"}, nil)
	p := NewPlanner(f, envConfig())
	d := ImageNotes{ImageDetails: ImageDetails{OS: "Debian 12"}}
	plan, err := p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &d})
	if err != nil {
		t.Fatal(err)
	}
	it := plan.Items[0]
	if plan.Kind != KindImageDetails || len(plan.Items) != 1 || it.VMID != 8001 || !slices.Equal(it.Steps, []Step{StepDetails}) ||
		*it.Details != d || it.DetailsWas == nil || it.DetailsWas.Details != winDetails || it.Blocked != "" ||
		!slices.Equal(plan.Locks, []string{"family:debian-12"}) {
		t.Errorf("plan = %+v, item %+v", plan, it)
	}
	plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &ImageNotes{ImageDetails: winDetails}})
	if !strings.Contains(plan.Items[0].Blocked, "already") {
		t.Errorf("unchanged details: %+v", plan.Items[0])
	}
	f.VMs[8001].Config["description"] = "**Details**\n- Colour: blue"
	plan, _ = p.ImageSetDetails(ctx, ImageRequest{Family: "debian-12", Details: &d})
	if w := plan.Items[0].DetailsWas; plan.Items[0].Blocked != "" || w == nil || !w.Unreadable {
		t.Errorf("over an unreadable block: %+v", plan.Items[0])
	}
	for _, req := range []ImageRequest{{Family: "debian-12"}, {Family: "debian-12", Details: &ImageNotes{ImageDetails: ImageDetails{User: "a / b", Password: "c"}}}, {Family: "nope", Details: &d}} {
		if _, err := p.ImageSetDetails(ctx, req); err == nil {
			t.Errorf("planned %+v", req)
		}
	}
}

// writeDetails is Apply of details alone.
func writeDetails(desc string, d ImageDetails) (string, error) {
	return ImageNotes{ImageDetails: d}.apply(desc)
}

// notesLines counts desc's battleship lines.
func notesLines(desc string) int {
	n := 0
	for _, line := range strings.Split(desc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), notesPrefix) {
			n++
		}
	}
	return n
}
