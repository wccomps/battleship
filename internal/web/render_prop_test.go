package web

import (
	"bytes"
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/pods"
)

// hostileStrings break pages that don't escape them, mixed with names that
// look real.
var hostileStrings = []string{
	`<script>alert(1)</script>`, `"><img src=x onerror=alert(1)>`, `' onmouseover='alert(1)`,
	`javascript:alert(1)`, ` JaVaScRiPt:alert(1)`, `</template><script>x()</script>`, `</div></main>`,
	`{{.CSRF}}`, `<!--`, `-->`, `<svg onload=alert(1)>`, `<style>*{}</style>`, `style="x"`,
	"a\rb", "a\r\nb", "a\nb", "\u2028\u2029", "\x00", "\xff\xfe", "&amp;&lt;", "data:text/html,<b>",
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

// fillHostile recursively fills v with hostile strings and small values
// (sizes derive from numbers); callers fix up fields the app always sets.
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

// pageData draws hostile data for page, in the shapes handlers always give.
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
	case "catalog":
		p := drawHostile[catalogView](t, "catalog")
		p.Show = rapid.SampledFrom([]string{"images", "environments"}).Draw(t, "show")
		return p
	case "teamless":
		p := drawHostile[catalogPage](t, "teamless")
		p.Op = rapid.SampledFrom(slices.DeleteFunc(slices.Clone(operations), func(o operation) bool { return !o.Kind.Teamless() })).Draw(t, "op")
		return p
	}
	t.Fatalf("no data for page %q", page)
	return nil
}

// Every page, in layout and panel, renders hostile text without error, with
// it escaped, in well-formed markup the CSP accepts.
func TestPropPagesEscapeHostileText(t *testing.T) {
	names := slices.Sorted(maps.Keys(pages))
	rapid.Check(t, func(t *rapid.T) {
		page := rapid.SampledFrom(names).Draw(t, "page")
		v := drawHostile[view](t, "view")
		v.refresh = rapid.OneOf(rapid.Just("/x"), offSiteURL).Draw(t, "refresh")
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
		if err := refreshesOnSite(tags); err != nil {
			t.Fatalf("%s as %s: %v", page, root, err)
		}
	})
}

// offSiteURL draws addresses that leave the site, or nearly do, mixed with
// local paths that carry hostile text.
var offSiteURL = rapid.OneOf(
	rapid.SampledFrom([]string{"//evil.example", "/\\evil.example", "/\tevil", "javascript:alert(1)", "https://evil.example/",
		"http:/evil.example", " /x", "/x;url=//evil.example", "/x\"><script>", "/%2F%2Fevil.example", "/auth/proxmox?finish=1&code=a"}),
	rapid.Map(rapid.String(), func(s string) string { return "/" + s }),
	rapid.String(),
)

// siteBase is where the test pages are served.
var siteBase = &url.URL{Scheme: "https", Host: "battleship.example.org", Path: "/"}

// staysOnSite reports whether a browser on siteBase following u stays on
// its origin. Browsers drop tabs and newlines and read \ as /.
func staysOnSite(u string) bool {
	if !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") ||
		strings.ContainsFunc(u, func(r rune) bool { return r == '\\' || r < 0x20 || r == 0x7f }) {
		return false
	}
	ref, err := url.Parse(u)
	if err != nil {
		return false
	}
	dest := siteBase.ResolveReference(ref)
	return dest.Scheme == siteBase.Scheme && dest.Host == siteBase.Host
}

// refreshesOnSite checks that every meta refresh in tags goes to a path
// on this site.
func refreshesOnSite(tags []markupTag) error {
	for _, tag := range tags {
		if tag.Name != "meta" || !strings.EqualFold(tag.Attrs["http-equiv"], "refresh") {
			continue
		}
		content := html.UnescapeString(tag.Attrs["content"])
		to, ok := strings.CutPrefix(content, "0;url=")
		if !ok || !staysOnSite(to) {
			return fmt.Errorf("meta refresh %q leaves the site", content)
		}
	}
	return nil
}

// Auth pages, whatever link and refresh they are given, offer and go on
// only to paths on this site.
func TestPropAuthPageGoesOnLocally(t *testing.T) {
	s := &Server{}
	rapid.Check(t, func(t *rapid.T) {
		p := drawHostile[auth.Page](t, "page")
		p.Status = http.StatusOK
		p.Link = rapid.OneOf(rapid.Just(p.Link), offSiteURL).Draw(t, "link")
		p.Refresh = rapid.OneOf(rapid.Just(p.Refresh), offSiteURL).Draw(t, "refresh")
		w := httptest.NewRecorder()
		s.AuthPage(w, httptest.NewRequest(http.MethodGet, "/", nil), p)
		tags, err := checkMarkup(w.Body.String())
		if err != nil {
			t.Fatal(err)
		}
		if err := refreshesOnSite(tags); err != nil {
			t.Fatal(err)
		}
		for _, tag := range tags {
			if href, ok := tag.Attrs["href"]; ok && tag.Attrs["class"] == "btn" && !staysOnSite(html.UnescapeString(href)) {
				t.Fatalf("links to %q", href)
			}
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
