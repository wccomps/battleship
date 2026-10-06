package web

import (
	"bytes"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
)

// hostileStrings are text that breaks pages that don't escape it: markup,
// attribute and URL breakouts, template syntax, line breaks of every
// kind, NUL and invalid UTF-8, mixed with names that look like the real
// thing.
var hostileStrings = []string{
	`<script>alert(1)</script>`, `"><img src=x onerror=alert(1)>`, `' onmouseover='alert(1)`,
	`javascript:alert(1)`, ` JaVaScRiPt:alert(1)`, `</template><script>x()</script>`, `</div></main>`,
	`{{.CSRF}}`, `<!--`, `-->`, `<svg onload=alert(1)>`, `<style>*{}</style>`, `style="x"`,
	"a\rb", "a\r\nb", "a\nb", "  ", "\x00", "\xff\xfe", "&amp;&lt;", "data:text/html,<b>",
	"team01-dc", "*.kilo.alpha", "01-03", "", " ", "lead", "operator", "#ZgotmplZ",
}

// hostile draws a string built of hostile pieces and arbitrary text.
func hostile(t *rapid.T, label string) string {
	piece := rapid.OneOf(
		rapid.SampledFrom(hostileStrings),
		rapid.StringMatching(`[a-z0-9<>"'&=/:;. -]{0,12}`),
		rapid.String(),
	)
	return strings.Join(rapid.SliceOfN(piece, 1, 3).Draw(t, label), "")
}

// fillHostile sets every settable field of v, recursively: strings to
// hostile ones, numbers small (sizes come from them), slices of up to 3,
// pointers set or nil. Fields that are always set by the app are fixed up
// by the callers.
func fillHostile(t *rapid.T, v reflect.Value, path string) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(hostile(t, path))
	case reflect.Bool:
		v.SetBool(rapid.Bool().Draw(t, path))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(rapid.IntRange(-2, 30).Draw(t, path)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(rapid.IntRange(0, 30).Draw(t, path)))
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return // raw JSON: the views never show it
		}
		n := rapid.IntRange(0, 3).Draw(t, path+" len")
		s := reflect.MakeSlice(v.Type(), n, n)
		for i := range n {
			fillHostile(t, s.Index(i), path+"[]")
		}
		v.Set(s)
	case reflect.Pointer:
		if rapid.Bool().Draw(t, path+" set") {
			p := reflect.New(v.Type().Elem())
			fillHostile(t, p.Elem(), path)
			v.Set(p)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Field(i); f.CanSet() {
				fillHostile(t, f, path+"."+v.Type().Field(i).Name)
			}
		}
	}
}

// drawHostile draws a T with every field hostile.
func drawHostile[T any](t *rapid.T, label string) T {
	var x T
	fillHostile(t, reflect.ValueOf(&x).Elem(), label)
	return x
}

// pageData draws the data of page, as hostile as the app could make it:
// any text, but the shapes the handlers always give (a reset form has its
// picker, an operation is one of the four).
func pageData(t *rapid.T, page string) any {
	switch page {
	case "grid":
		return drawHostile[gridPageView](t, "grid")
	case "vm":
		return drawHostile[cellPage](t, "vm")
	case "preview":
		p := drawHostile[previewPage](t, "preview")
		p.Op = rapid.SampledFrom(operations).Draw(t, "op")
		return p
	case "job":
		return drawHostile[jobView](t, "job")
	case "jobs":
		return drawHostile[jobsList](t, "jobs")
	case "opform":
		p := drawHostile[formPage](t, "opform")
		p.Op = rapid.SampledFrom(operations).Draw(t, "op")
		if p.Op.Kind == pods.KindReset && p.Picker == nil {
			p.Picker = &snapshotPicker{}
		}
		return p
	case "message":
		return drawHostile[message](t, "message")
	}
	t.Fatalf("no data for page %q", page)
	return nil
}

// Every page, in the layout and in the panel, renders whatever text the
// cluster, the jobs and the users put in it: without error, as markup
// that nests and closes, with all of that text escaped (no new tags or
// attributes), and with nothing the CSP would refuse.
func TestPropPagesEscapeHostileText(t *testing.T) {
	names := slices.Sorted(maps.Keys(pages))
	rapid.Check(t, func(t *rapid.T) {
		page := rapid.SampledFrom(names).Draw(t, "page")
		v := drawHostile[view](t, "view")
		v.Data = pageData(t, page)
		root := rapid.SampledFrom([]string{"layout", "panel"}).Draw(t, "root")
		var buf bytes.Buffer
		if err := pages[page].ExecuteTemplate(&buf, root, v); err != nil {
			t.Fatalf("rendering %s as %s: %v", page, root, err)
		}
		tags, err := checkMarkup(buf.String())
		if err != nil {
			t.Fatalf("%s as %s: %v", page, root, err)
		}
		scripts := 0
		for _, tag := range tags {
			if tag.Name == "script" {
				scripts++
			}
		}
		if want := map[string]int{"layout": 1, "panel": 0}[root]; scripts != want {
			t.Fatalf("%s as %s has %d scripts, want %d", page, root, scripts, want)
		}
	})
}

// The pieces the event streams send are as safe as the pages.
func TestPropFragmentsEscapeHostileText(t *testing.T) {
	type frag struct {
		page, name string
		data       func(*rapid.T) any
	}
	frags := []frag{
		{"grid", "grid-live", func(t *rapid.T) any { return drawHostile[gridView](t, "grid") }},
		{"grid", "grid-status", func(t *rapid.T) any { return drawHostile[gridView](t, "grid") }},
		{"grid", "grid-counts", func(t *rapid.T) any { return drawHostile[gridView](t, "grid") }},
		{"grid", "grid-cell", func(t *rapid.T) any { return drawHostile[cellView](t, "cell") }},
		{"job", "job-head", func(t *rapid.T) any { return drawHostile[jobView](t, "job") }},
		{"job", "job-actions", func(t *rapid.T) any { return drawHostile[jobView](t, "job") }},
		{"job", "job-items", func(t *rapid.T) any { return drawHostile[jobView](t, "job") }},
		{"job", "job-log-lines", func(t *rapid.T) any { return drawHostile[[]eventView](t, "events") }},
		{"jobs", "jobs-table", func(t *rapid.T) any { return drawHostile[jobsList](t, "jobs") }},
	}
	rapid.Check(t, func(t *rapid.T) {
		f := rapid.SampledFrom(frags).Draw(t, "fragment")
		h, err := fragment(f.page, f.name, f.data(t))
		if err != nil {
			t.Fatalf("rendering %s: %v", f.name, err)
		}
		tags, err := checkMarkup(piece(h))
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		for _, tag := range tags {
			if tag.Name == "script" {
				t.Fatalf("%s has a script", f.name)
			}
		}
	})
}
