package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The templates follow one convention per kind of control, so people can tell
// at a glance what is clickable and what it does:
//
//   - an action (a link to an operation's form, or a submit) is a button;
//   - actions are never text links joined by separators;
//   - a disclosure toggle with visible text is a .disc summary;
//   - a table with buttons in its rows turns into cards on phones, so the
//     buttons are never scrolled off-screen, and keeps them in a td.act
//     sized to them;
//   - links in text keep their underline, tables included.
func TestTemplatesFollowControlConventions(t *testing.T) {
	var (
		anchorRE  = regexp.MustCompile(`<a\b[^>]*>`)
		hrefRE    = regexp.MustCompile(`href="([^"{]*)`)
		fullRE    = regexp.MustCompile(`href="([^"]*)"`)
		classRE   = regexp.MustCompile(`class="([^"]*)"`)
		summaryRE = regexp.MustCompile(`<summary\b[^>]*>`)
		joinedRE  = regexp.MustCompile(`</a>\s*(·|\||/|,)\s*<a\b`)
		tableRE   = regexp.MustCompile(`(?s)<table\b[^>]*>.*?</table>`)
	)
	isButton := func(tag string) bool {
		m := classRE.FindStringSubmatch(tag)
		return m != nil && strings.Contains(" "+m[1]+" ", " btn ")
	}
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	for _, name := range files {
		b, _ := fs.ReadFile(templateFS, name)
		src := string(b)
		for _, tag := range anchorRE.FindAllString(src, -1) {
			m := hrefRE.FindStringSubmatch(tag)
			if m == nil {
				continue
			}
			full := fullRE.FindStringSubmatch(tag)[1]
			for _, op := range operations {
				// An environment's form: /environments/{{.Domain}}/add.
				before, after, onEnv := strings.Cut(op.Path, "{domain}")
				envForm := onEnv && strings.HasPrefix(full, before) && strings.Contains(full+"?", after+"?")
				if (m[1] == op.Path || strings.HasPrefix(m[1], op.Path+"?") || envForm) && !isButton(tag) {
					t.Errorf("%s: %s links to the %s form but isn't a button", name, tag, op.Title)
				}
			}
		}
		for _, j := range joinedRE.FindAllString(src, -1) {
			t.Errorf("%s: actions joined as text links: %q", name, j)
		}
		for _, tag := range summaryRE.FindAllString(src, -1) {
			if strings.Contains(tag, "aria-label=") {
				continue // an icon that opens a menu, not a disclosure
			}
			if m := classRE.FindStringSubmatch(tag); m == nil || !strings.Contains(" "+m[1]+" ", " disc ") {
				t.Errorf("%s: %s is a disclosure toggle without class disc", name, tag)
			}
		}
		for _, table := range tableRE.FindAllString(src, -1) {
			body, _, _ := strings.Cut(table[strings.Index(table, "<tbody")+1:], "</tbody>")
			if strings.Contains(body, `class="btn`) && !strings.Contains(table[:strings.Index(table, ">")], "cards") {
				t.Errorf("%s: a table with buttons in its rows isn't .tbl.cards: %.80s", name, table)
			}
			if strings.Count(body, `<div class="acts">`) != strings.Count(body, `<td class="act">`) {
				t.Errorf("%s: row buttons outside a td.act: %.80s", name, table)
			}
		}
	}
	css, _ := fs.ReadFile(staticFS, "static/app.css")
	if regexp.MustCompile(`\.tbl a\{[^}]*text-decoration:none`).Match(css) {
		t.Error("app.css takes the underline off links in tables")
	}
}
