package pods

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ImageDetails are a base image's build facts. Proxmox keeps no key/value
// data on a VM, so they live in its notes as a block Battleship writes
// (ImageNotes.Changes) and reads (ParseDetails), a Markdown list that also reads
// cleanly in Proxmox's notes panel:
//
//	**Details**
//	- OS: Debian 12
//	- Login: debian / Changeme1!
//
// The fields and their order are fixed. A line with any other key makes
// the block unreadable rather than an extra field: the block is
// Battleship's, a field it doesn't know couldn't be edited or shown as
// anything but text, and a rewrite would drop it unseen.
type ImageDetails struct {
	OS          string `json:"os,omitempty"`
	User        string `json:"user,omitempty"`     // the Login before " / "
	Password    string `json:"password,omitempty"` // the Login after it
	Built       string `json:"built,omitempty"`
	Generalized string `json:"generalized,omitempty"`
	Installed   string `json:"installed,omitempty"`
	Licence     string `json:"licence,omitempty"`
}

const (
	detailsHeader = "**Details**"
	loginSep      = " / "
	// MaxDetail is the longest detail value, in characters.
	MaxDetail = 200
)

// DetailKeys are the block's keys, in its order.
var DetailKeys = []string{"OS", "Login", "Built", "Generalized", "Installed", "Licence"}

// Login is the Login value: "user / password", or the user alone.
func (d ImageDetails) Login() string {
	if d.Password == "" {
		return d.User
	}
	return d.User + loginSep + d.Password
}

// Value is the value of key, one of DetailKeys.
func (d ImageDetails) Value(key string) string {
	switch key {
	case "OS":
		return d.OS
	case "Login":
		return d.Login()
	case "Built":
		return d.Built
	case "Generalized":
		return d.Generalized
	case "Installed":
		return d.Installed
	case "Licence":
		return d.Licence
	}
	return ""
}

// set sets key's value; Login is split at its first " / ".
func (d *ImageDetails) set(key, v string) {
	switch key {
	case "OS":
		d.OS = v
	case "Login":
		user, pass, _ := strings.Cut(v, loginSep)
		d.User, d.Password = strings.TrimSpace(user), strings.TrimSpace(pass)
	case "Built":
		d.Built = v
	case "Generalized":
		d.Generalized = v
	case "Installed":
		d.Installed = v
	case "Licence":
		d.Licence = v
	}
}

// Empty reports whether no field is set.
func (d ImageDetails) Empty() bool { return d == ImageDetails{} }

// Trimmed is d with each value's surrounding spaces removed, as forms post
// them.
func (d ImageDetails) Trimmed() ImageDetails {
	for _, f := range []*string{&d.OS, &d.User, &d.Password, &d.Built, &d.Generalized, &d.Installed, &d.Licence} {
		*f = strings.TrimSpace(*f)
	}
	return d
}

// checkDetail checks one value: printable text on one line (no control,
// format or invisible characters), trimmed, at most MaxDetail characters.
func checkDetail(name, v string) error {
	switch {
	case !utf8.ValidString(v) || strings.ContainsFunc(v, func(r rune) bool { return !unicode.IsPrint(r) }):
		return fmt.Errorf("%s must be printable text on one line", name)
	case v != strings.TrimSpace(v):
		return fmt.Errorf("%s must not start or end with spaces", name)
	case utf8.RuneCountInString(v) > MaxDetail:
		return fmt.Errorf("%s is too long: at most %d characters", name, MaxDetail)
	}
	return nil
}

// CheckDetails checks details before they are written: each value one line
// of printable text, and a login that reads back as the same user and
// password.
func CheckDetails(d ImageDetails) error {
	var errs []error
	for _, f := range []struct{ name, v string }{{"OS", d.OS}, {"the login's user", d.User}, {"the login's password", d.Password},
		{"Built", d.Built}, {"Generalized", d.Generalized}, {"Installed", d.Installed}, {"Licence", d.Licence}} {
		if err := checkDetail(f.name, f.v); err != nil {
			errs = append(errs, err)
		}
	}
	switch {
	case strings.Contains(d.User, loginSep) || strings.HasSuffix(d.User, " /"):
		errs = append(errs, errors.New(`the login's user must not contain " / ", which separates it from the password`))
	case d.User == "" && d.Password != "":
		errs = append(errs, errors.New("a login's password needs its user"))
	}
	if err := checkDetail("the login", d.Login()); err != nil && len(errs) == 0 {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// block is d's details block, "" when d is empty.
func (d ImageDetails) block() string {
	if d.Empty() {
		return ""
	}
	lines := []string{detailsHeader}
	for _, k := range DetailKeys {
		if v := d.Value(k); v != "" {
			lines = append(lines, "- "+k+": "+v)
		}
	}
	return strings.Join(lines, "\n")
}

// ErrDetailsUnreadable is ParseDetails's error for a details block that
// isn't as Battleship writes it, such as one edited by hand.
var ErrDetailsUnreadable = errors.New("the notes' details block is unreadable")

// dropFormat removes format characters (bidi overrides, zero widths), which
// could disguise what notes say.
func dropFormat(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// isHeader and endsBlock read a notes line as the block's bounds see it.
func isHeader(line string) bool { return strings.TrimSpace(dropFormat(line)) == detailsHeader }

func endsBlock(line string) bool {
	t := strings.TrimSpace(dropFormat(line))
	return t == "" || strings.HasPrefix(t, notesPrefix)
}

// blocks are the [start, end) line ranges of the details blocks in lines:
// each header and the lines after it, up to a blank line or the battleship
// line. A line run on after a list item belongs to it in Markdown, so it is
// the block's too.
func blocks(lines []string) [][2]int {
	var out [][2]int
	for i := 0; i < len(lines); i++ {
		if !isHeader(lines[i]) {
			continue
		}
		end := i + 1
		for end < len(lines) && !endsBlock(lines[end]) && !isHeader(lines[end]) {
			end++
		}
		out = append(out, [2]int{i, end})
		i = end - 1
	}
	return out
}

// parseBlock reads a block's lines after its header: "- Key: value", each
// key known, at most once and in order, each value valid.
func parseBlock(lines []string) (ImageDetails, error) {
	var d ImageDetails
	next := 0 // DetailKeys before this were passed
	for _, line := range lines {
		line = strings.TrimRight(dropFormat(line), " \t\r")
		rest, ok := strings.CutPrefix(line, "- ")
		if !ok {
			return ImageDetails{}, ErrDetailsUnreadable
		}
		key, v, ok := strings.Cut(rest, ":")
		v = strings.TrimSpace(v)
		i := slices.Index(DetailKeys[next:], key)
		if !ok || i < 0 || v == "" || checkDetail(key, v) != nil {
			return ImageDetails{}, ErrDetailsUnreadable
		}
		next += i + 1
		d.set(key, v)
	}
	if d.Login() != "" && CheckDetails(ImageDetails{User: d.User, Password: d.Password}) != nil {
		return ImageDetails{}, ErrDetailsUnreadable
	}
	return d, nil
}

// splitNotes is desc's lines, the ranges of its details blocks, and the
// details they hold: none without a block, and ErrDetailsUnreadable unless
// there is one block, as Battleship writes it.
func splitNotes(desc string) (lines []string, bs [][2]int, d ImageDetails, err error) {
	lines = strings.Split(desc, "\n")
	bs = blocks(lines)
	switch len(bs) {
	case 0:
		return lines, bs, ImageDetails{}, nil
	case 1:
		if d, err = parseBlock(lines[bs[0][0]+1 : bs[0][1]]); err == nil {
			return lines, bs, d, nil
		}
	}
	return lines, bs, ImageDetails{}, ErrDetailsUnreadable
}

// ParseDetails reads desc's details block and the free text around it,
// which is its user notes (UserNotes) less the block. Notes without a block
// have empty details. A block that isn't as Battleship writes it (two of
// them, an unknown key, a bad value) isn't guessed at: the error is
// ErrDetailsUnreadable and free is all of the user notes, block included,
// for a page to show as they are.
func ParseDetails(desc string) (d ImageDetails, free string, err error) {
	lines, bs, d, err := splitNotes(desc)
	if err != nil {
		return ImageDetails{}, UserNotes(desc), err
	}
	return d, UserNotes(strings.Join(without(lines, bs), "\n")), nil
}

// without is lines less the ranges bs, each with the blank line after it,
// so taking a block out leaves no extra gap.
func without(lines []string, bs [][2]int) []string {
	var out []string
	from := 0
	for _, b := range bs {
		out = append(out, lines[from:b[0]]...)
		from = b[1]
		if from < len(lines) && strings.TrimSpace(dropFormat(lines[from])) == "" {
			from++
		}
	}
	return append(out, lines[from:]...)
}

// withDetails is desc with its details block set to d's: written in place
// of the first block (any others, such as a hand-made second one, are
// taken out), else put first, before the notes people wrote; empty
// details take the block out. Every other line stays exactly as it was.
// d must pass CheckDetails (apply checks it), so no value ends the block.
func withDetails(desc string, d ImageDetails) string {
	lines := strings.Split(desc, "\n")
	bs := blocks(lines)
	block := d.block()
	if len(bs) == 0 {
		switch {
		case block == "":
			return desc
		case strings.TrimSpace(desc) == "":
			return block
		}
		return block + "\n\n" + desc
	}
	if block == "" {
		return strings.Join(without(lines, bs), "\n")
	}
	first := bs[0]
	rest := without(lines[first[1]:], shift(bs[1:], -first[1]))
	if len(rest) > 0 && !endsBlock(rest[0]) {
		rest = append([]string{""}, rest...) // a block taken out after this one took its blank line
	}
	out := append(slices.Clone(lines[:first[0]]), strings.Split(block, "\n")...)
	return strings.Join(append(out, rest...), "\n")
}

// shift moves ranges by n lines.
func shift(bs [][2]int, n int) [][2]int {
	out := make([][2]int, len(bs))
	for i, b := range bs {
		out[i] = [2]int{b[0] + n, b[1] + n}
	}
	return out
}

// ImageNotes are what a register or an Edit notes writes into an image's
// notes: details, and the free text beside them unless Free is nil, which
// keeps the free text there is. The details are embedded, so a value
// without free text has the JSON its details alone had.
type ImageNotes struct {
	ImageDetails
	Free *string `json:"free,omitempty"`
	// From marks the notes an Edit notes form opened on (NotesEdit.Edit):
	// changed since, the plan is blocked rather than write over them.
	From string `json:"from,omitempty"`
}

// check checks n before it is written: its details (CheckDetails), and its
// free text if any (checkFreeNotes).
func (n ImageNotes) check() error {
	err := CheckDetails(n.ImageDetails)
	if err == nil && n.Free != nil {
		err = checkFreeNotes(*n.Free)
	}
	return err
}

// apply is notes desc with n written into them, or desc and why check
// refuses n.
func (n ImageNotes) apply(desc string) (string, error) {
	if err := n.check(); err != nil {
		return desc, err
	}
	if n.Free != nil {
		return withFreeNotes(desc, n.ImageDetails, *n.Free), nil
	}
	return withDetails(desc, n.ImageDetails), nil
}

// Changes is the config change that gives cur notes n; nil if there is none.
func (n ImageNotes) Changes(cur map[string]string) (map[string]string, error) {
	desc, err := n.apply(cur["description"])
	if err != nil || desc == cur["description"] {
		return nil, err
	}
	return map[string]string{"description": desc}, nil
}

// against is n as Edit notes writes it over notes read as r: From is
// cleared, and free text that is r's once cleaned is kept as it is (Free
// nil), so a form sent back unchanged writes nothing there, even text
// checkFreeNotes would refuse. same says n changes nothing in r.
func (n ImageNotes) against(r NotesRead) (_ ImageNotes, same bool) {
	n.From = ""
	if n.Free != nil && CleanFreeNotes(*n.Free) == r.Free {
		n.Free = nil
	}
	return n, !r.Unreadable && n.ImageDetails == r.Details && n.Free == nil
}

// NotesRead is an image's notes as Edit notes reads them
// (readImageNotes): the details, and the free text beside them as editing
// writes it (editText). An unreadable details block gives no details:
// Unreadable is set, and Block holds the block's lines as pages show them.
// Plans store it (Item.DetailsWas); one stored before Edit notes wrote free
// text has only the details and Unreadable.
type NotesRead struct {
	Details    ImageDetails `json:"details"`
	Unreadable bool         `json:"unreadable,omitempty"`
	Free       string       `json:"notes,omitempty"`
	Block      string       `json:"block,omitempty"`
}

// readImageNotes reads notes desc as Edit notes edits them.
func readImageNotes(desc string) NotesRead {
	lines, bs, d, err := splitNotes(desc)
	r := NotesRead{Details: d, Free: editText(strings.Join(without(lines, bs), "\n"))}
	if err != nil {
		var in []string
		for _, b := range bs {
			in = append(in, strings.Join(lines[b[0]:b[1]], "\n"))
		}
		r.Unreadable, r.Block = true, userText(strings.Join(in, "\n\n"))
	}
	return r
}

const (
	// MaxFreeNotes is the longest free text Edit notes writes, in
	// characters: as much as pages show (UserNotes).
	MaxFreeNotes = maxUserNotes
	// maxDescription is qemu-server's 8192-character description limit
	// (Perl length() on decoded UTF-8).
	maxDescription = 8 * 1024
)

// CheckDescription checks a VM description before it is written: at most
// maxDescription characters, counted as Proxmox counts them.
func CheckDescription(desc string) error {
	if n := utf8.RuneCountInString(desc); n > maxDescription {
		return fmt.Errorf("would be %d characters; Proxmox keeps at most %d", n, maxDescription)
	}
	return nil
}

// blockLongNotes blocks it, unless it is already, when a step on it would
// write notes over desc that are more than Proxmox keeps (CheckDescription).
func (it *Item) blockLongNotes(desc string) {
	check := func(after string) {
		if err := CheckDescription(after); err != nil && after != desc && it.Blocked == "" {
			it.Blocked = "the notes of " + it.Name + " " + err.Error()
		}
		desc = after
	}
	if it.Notes != nil {
		check(WithNotes(desc, *it.Notes))
	}
	if it.Details != nil {
		after, _ := it.Details.apply(desc)
		check(after)
	}
}

// freeSpaces are what CleanFreeNotes turns into plain text: line breaks
// (CR, VT, FF, NEL, the line and paragraph separators) into LF, and a tab
// into four spaces.
var freeSpaces = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n",
	"\t", "    ")

// CleanFreeNotes is free text as Edit notes writes it: line breaks as LF
// (freeSpaces), tabs and other spaces (NBSP, U+3000) as spaces, no spaces
// at a line's end, no blank lines or spaces at the start or end. Forms post
// text through it.
func CleanFreeNotes(s string) string {
	s = strings.Map(func(r rune) rune {
		if r != ' ' && unicode.Is(unicode.Zs, r) {
			return ' '
		}
		return r
	}, freeSpaces.Replace(s))
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// hides reports whether r can hide what notes say: a control character
// (C0, DEL, C1; a line's LF aside) or a bidirectional control.
func hides(r rune) bool {
	return r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r))
}

// checkFreeNotes checks free text before it is written beside the details
// block. Only what can hide what the notes say (hides), or pass for
// Battleship's own lines, is refused.
func checkFreeNotes(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("the notes must be UTF-8 text")
	}
	if i := strings.IndexFunc(s, hides); i >= 0 {
		r, _ := utf8.DecodeRuneInString(s[i:])
		class := "a control character"
		if !unicode.IsControl(r) {
			class = "a bidirectional control"
		}
		return fmt.Errorf("the notes hold %s (U+%04X), which can hide what they say", class, r)
	}
	switch {
	case utf8.RuneCountInString(s) > MaxFreeNotes:
		return fmt.Errorf("the notes are too long: at most %d characters", MaxFreeNotes)
	case s != CleanFreeNotes(s):
		return errors.New("the notes must have plain spaces and line breaks, and no spaces or blank lines at the start or end, or spaces ending a line")
	}
	for _, line := range strings.Split(s, "\n") {
		switch {
		case isHeader(line):
			return fmt.Errorf("a line of the notes reads %s, which heads the details: use the fields", detailsHeader)
		case isNotesLine(line):
			return fmt.Errorf("a line of the notes starts %q, which is Battleship's own line", notesPrefix)
		}
	}
	return nil
}

// isNotesLine reports whether line, less format characters and spaces,
// starts as a battleship line: UserNotes hides such a line, and ParseNotes
// may read it.
func isNotesLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(dropFormat(line)), notesPrefix)
}

// editText is text s as Edit notes edits it: cleaned (CleanFreeNotes),
// without characters that hide (hides) or lines that read as Battleship's
// own. Format characters (Cf) are kept, so editing another line does not
// strip them.
func editText(s string) string {
	s = strings.Map(func(r rune) rune {
		if hides(r) {
			return -1
		}
		return r
	}, freeSpaces.Replace(s))
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if !isNotesLine(line) && !isHeader(line) {
			keep = append(keep, line)
		}
	}
	return CleanFreeNotes(strings.Join(keep, "\n"))
}

// withFreeNotes is notes desc rewritten as details d's block, then free
// text free, then each battleship line of desc exactly as it was, a blank
// line between each part. People never see those lines, and
// checkFreeNotes lets no line of free read as one, or as a block's header.
func withFreeNotes(desc string, d ImageDetails, free string) string {
	var kept []string
	for _, line := range strings.Split(desc, "\n") {
		if isNotesLine(line) {
			kept = append(kept, line)
		}
	}
	var parts []string
	for _, p := range []string{d.block(), free, strings.Join(kept, "\n")} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Prose patterns RegisterDetails reads: "Login: user / password", "built
// 2026-10-14 from x.iso" and "Base image build: <OS>, built ...". Each ends
// at the sentence's end.
var (
	proseLogin = regexp.MustCompile(`(?i)\blogin:\s*(\S+) / (\S+?)\.?(?:\s|$)`)
	proseBuilt = regexp.MustCompile(`(?i)\bbuilt (\d{4}-\d{2}-\d{2}(?: from \S+?)?)\.?(?:[\s,;]|$)`)
	proseOS    = regexp.MustCompile(`(?i)\bbase image build:\s*([^,\n]+?)(?:,|\.\s|\.?$|\n)`)
)

// RegisterDetails are the details a register writes into the new image,
// from its source's notes desc: their block when they have a readable one;
// else, from notes written as prose, the login, build date and OS where
// they are plainly said, for the person registering to check. A value that
// wouldn't pass CheckDetails is left out. ok is false when the notes have
// a block that is unreadable: the copy keeps it as it is.
func RegisterDetails(desc string) (d ImageDetails, ok bool) {
	d, free, err := ParseDetails(desc)
	switch {
	case err != nil:
		return ImageDetails{}, false
	case !d.Empty():
		return d, true
	}
	if m := proseLogin.FindStringSubmatch(free); m != nil && CheckDetails(ImageDetails{User: m[1], Password: m[2]}) == nil {
		d.User, d.Password = m[1], m[2]
	}
	if m := proseBuilt.FindStringSubmatch(free); m != nil && checkDetail("Built", m[1]) == nil {
		d.Built = m[1]
	}
	if m := proseOS.FindStringSubmatch(free); m != nil {
		if os := strings.TrimSpace(m[1]); checkDetail("OS", os) == nil {
			d.OS = os
		}
	}
	return d, true
}
