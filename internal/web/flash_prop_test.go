package web

import (
	"encoding/base64"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"pgregory.net/rapid"
)

// cutAt is the longest prefix of valid UTF-8 s that is at most n bytes,
// built rune by rune.
func cutAt(s string, n int) string {
	size := 0
	for _, r := range s {
		w := utf8.RuneLen(r)
		if size+w > n {
			break
		}
		size += w
	}
	return s[:size]
}

// flashRunes are runes of each encoded length, for texts that put a
// multi-byte character across a cut.
var flashRunes = []string{"a", "é", "€", "\U0001F600", "\n", "<", "\u0000"}

// drawFlashText draws valid UTF-8 near the length limit, ending in
// characters of 1 to 4 bytes so that the cut falls inside one.
func drawFlashText(t *rapid.T) string {
	pad := rapid.IntRange(flashMaxLen-8, flashMaxLen+8).Draw(t, "pad")
	text := strings.Repeat("a", pad)
	text = text[:pad-rapid.IntRange(0, 4).Draw(t, "back")%(pad+1)]
	return text + strings.Join(rapid.SliceOfN(rapid.SampledFrom(flashRunes), 0, 6).Draw(t, "tail"), "")
}

// truncateRunes keeps the longest whole-rune prefix within n bytes.
func TestPropTruncateRunes(t *testing.T) {
	check := func(t interface{ Fatalf(string, ...any) }, s string, n int) {
		got := truncateRunes(s, n)
		if want := cutAt(s, n); got != want {
			t.Fatalf("truncateRunes(%q, %d) = %q, want %q", s, n, got, want)
		}
		if len(got) > n || !strings.HasPrefix(s, got) || !utf8.ValidString(got) {
			t.Fatalf("truncateRunes(%q, %d) = %q", s, n, got)
		}
	}
	for n := 0; n < 12; n++ {
		for _, s := range []string{"", "abc", "ééé", "a€b", "\U0001F600\U0001F600", "a\U0001F600"} {
			check(t, s, n)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		s := strings.Join(rapid.SliceOfN(rapid.SampledFrom(flashRunes), 0, 10).Draw(t, "s"), "")
		check(t, s, rapid.IntRange(0, 24).Draw(t, "n"))
	})
}

func flashRequest(s *Server, value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: s.flashCookieName(), Value: value})
	return r
}

// clearsFlash is whether w sets the flash cookie to be deleted.
func clearsFlash(s *Server, w *httptest.ResponseRecorder) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == s.flashCookieName() && c.MaxAge < 0 && c.Value == "" {
			return true
		}
	}
	return false
}

// A message set as a flash comes back as itself, cut to the limit at a
// character boundary, with its kind; an empty one comes back as nothing.
func TestPropFlashRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := &Server{secureCookies: rapid.Bool().Draw(t, "secure")}
		kind := rapid.SampledFrom([]string{flashInfo, flashSuccess, flashError}).Draw(t, "kind")
		msg := rapid.OneOf(rapid.Custom(drawFlashText), rapid.String()).Draw(t, "msg")
		w := httptest.NewRecorder()
		s.setFlash(w, kind, msg)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		out := httptest.NewRecorder()
		got := s.takeFlash(out, r)
		want := cutAt(msg, flashMaxLen)
		if !utf8.ValidString(msg) || want == "" {
			if got != nil {
				t.Fatalf("flash of %q came back as %+v", msg, got)
			}
		} else if got == nil || got.Kind != kind || got.Message != want {
			t.Fatalf("flash of %q (%d bytes) came back as %+v, want %q", msg, len(msg), got, want)
		}
		if !clearsFlash(s, out) {
			t.Fatalf("flash %q not cleared", msg)
		}
	})
}

// Whatever a browser sends as the flash cookie, takeFlash shows nothing or
// a non-empty, valid message of a known kind within the limit, and clears
// the cookie either way.
func TestPropFlashFromAnyCookie(t *testing.T) {
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	bodies := []func(*rapid.T) string{
		func(t *rapid.T) string { return enc([]byte(rapid.String().Draw(t, "text"))) },
		func(t *rapid.T) string { return enc(rapid.SliceOf(rapid.Byte()).Draw(t, "bytes")) },
		func(t *rapid.T) string {
			return enc([]byte(strings.Repeat("a", rapid.IntRange(flashMaxLen-2, flashMaxLen+2).Draw(t, "len"))))
		},
		func(t *rapid.T) string { return enc([]byte{0xc3}) },
		func(t *rapid.T) string { return base64.StdEncoding.EncodeToString([]byte("hi?>")) },
		func(t *rapid.T) string { return base64.StdEncoding.EncodeToString([]byte("hi")) },
		func(t *rapid.T) string { return "" },
		func(t *rapid.T) string { return rapid.StringMatching(`[A-Za-z0-9_.=-]{0,20}`).Draw(t, "raw") },
	}
	// Messages at the limit, as a browser could send them.
	s := &Server{}
	for _, n := range []int{flashMaxLen - 1, flashMaxLen, flashMaxLen + 1, flashMaxLen + 2} {
		got := s.takeFlash(httptest.NewRecorder(), flashRequest(s, flashInfo+"."+enc([]byte(strings.Repeat("a", n)))))
		if (got != nil) != (n <= flashMaxLen) {
			t.Errorf("a %d byte flash cookie came back as %+v", n, got)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		s := &Server{secureCookies: rapid.Bool().Draw(t, "secure")}
		kind := rapid.SampledFrom([]string{flashInfo, flashSuccess, flashError, "", "Info", "warning", "info ", "error."}).Draw(t, "kind")
		sep := rapid.SampledFrom([]string{".", ".", ".", "", ":", ".."}).Draw(t, "sep")
		value := kind + sep + rapid.SampledFrom(bodies).Draw(t, "body")(t)
		if rapid.IntRange(0, 5).Draw(t, "raw") == 0 {
			value = rapid.StringMatching(`[ -~]{0,40}`).Draw(t, "any value")
		}
		out := httptest.NewRecorder()
		got := s.takeFlash(out, flashRequest(s, value))
		if got != nil {
			if got.Kind != flashInfo && got.Kind != flashSuccess && got.Kind != flashError {
				t.Fatalf("%q: kind %q", value, got.Kind)
			}
			if got.Message == "" || len(got.Message) > flashMaxLen || !utf8.ValidString(got.Message) {
				t.Fatalf("%q: message %q", value, got.Message)
			}
		}
		// A request whose Cookie header parsed to our cookie must clear it.
		if r := flashRequest(s, value); func() bool { _, err := r.Cookie(s.flashCookieName()); return err == nil }() && !clearsFlash(s, out) {
			t.Fatalf("%q: cookie not cleared", value)
		}
	})
}

// resumeModel reads a decimal the way the log stream's resume point is
// meant to be read: an optional sign, digits, a non-negative value that
// fits an int64.
func resumeModel(v string) (int64, bool) {
	neg := false
	d := v
	if strings.HasPrefix(d, "+") || strings.HasPrefix(d, "-") {
		neg, d = d[0] == '-', d[1:]
	}
	if d == "" || strings.Trim(d, "0123456789") != "" {
		return 0, false
	}
	var n uint64
	for _, c := range d {
		if n > (1<<63)/10 {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
		if n > 1<<63 {
			return 0, false
		}
	}
	if neg {
		return 0, n == 0
	}
	if n > 1<<63-1 {
		return 0, false
	}
	return int64(n), true
}

// numberTexts are numbers and the ways a number goes wrong.
var numberTexts = []string{
	"", "0", "1", "7", "42", "007", "-1", "-0", "+5", "+0", " 5", "5 ", "5\n", "0x10", "1_000", "1e3", "5.0", "٥", "５", "--5",
	"9223372036854775807", "9223372036854775808", "-9223372036854775808", "-9223372036854775809", "99999999999999999999", "abc", "5abc", "NaN",
}

func drawNumber(t *rapid.T, label string) string {
	return rapid.OneOf(rapid.SampledFrom(numberTexts), rapid.SampledFrom(numberTexts),
		rapid.Map(rapid.Int64(), func(n int64) string { return strconv.FormatInt(n, 10) }), rapid.String()).Draw(t, label)
}

func checkResume(t interface{ Fatalf(string, ...any) }, header, query string) {
	r := httptest.NewRequest(http.MethodGet, "/events/jobs/1?after="+url.QueryEscape(query), nil)
	r.Header.Set("Last-Event-ID", header)
	var want int64
	for _, v := range []string{header, query} {
		if n, ok := resumeModel(v); ok {
			want = n
			break
		}
	}
	if got := resumeFrom(r); got != want {
		t.Fatalf("resumeFrom(Last-Event-ID %q, after %q) = %d, want %d", header, query, got, want)
	}
}

// The resume point is the first of the header and ?after= that reads as a
// non-negative number, else 0.
func TestPropResumeFrom(t *testing.T) {
	for _, h := range numberTexts {
		for _, q := range numberTexts {
			checkResume(t, h, q)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		checkResume(t, drawNumber(t, "header"), drawNumber(t, "query"))
	})
}

// positiveModel is whether text reads as a decimal from 1 to the largest
// int64, with an optional plus sign and leading zeros.
func positiveModel(v string) bool {
	d := strings.TrimPrefix(v, "+")
	if d == "" || strings.Trim(d, "0123456789") != "" {
		return false
	}
	n, ok := resumeModel(d)
	return ok && n > 0
}

// A job ID in a path and ?before= are accepted exactly when they are
// positive decimals; anything else is a 404.
func TestPropJobIDAndBefore(t *testing.T) {
	h := newHarness(t)
	check := func(t interface{ Fatalf(string, ...any) }, v string) {
		want := positiveModel(v)
		r := httptest.NewRequest(http.MethodGet, "/logs/x", nil)
		r.SetPathValue("id", v)
		w := httptest.NewRecorder()
		if _, ok := h.srv.jobID(w, r); ok != want || (!ok && w.Code != http.StatusNotFound) {
			t.Fatalf("jobID(%q) ok = %v (status %d), want %v", v, ok, w.Code, want)
		}
		r = httptest.NewRequest(http.MethodGet, "/logs?before="+url.QueryEscape(v), nil)
		w = httptest.NewRecorder()
		h.srv.jobsPage(w, r)
		if v == "" {
			want = true // no ?before at all
		}
		if got := w.Code != http.StatusNotFound; got != want {
			t.Fatalf("?before=%q: status %d, want found = %v", v, w.Code, want)
		}
	}
	for _, v := range numberTexts {
		check(t, v)
	}
	rapid.Check(t, func(t *rapid.T) { check(t, drawNumber(t, "id")) })
}

// The cell page answers only for cells of the grid, and never reads
// Proxmox for anything else.
func TestPropCellPageOnlyForGridCells(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	teams := []string{"01", "02", "03", "04", "00", "1", "001", "", "0 1", "01 ", "+1", "١", "TEAM01", "01/dc", "%", "..", "."}
	hosts := []string{"dc", "web", "DC", "Web", "dc ", "d", "dcx", "xdc", "", "dc/web", "%2e", "..", ".", "é", "dc\n", "team01-dc", "01"}
	rapid.Check(t, func(t *rapid.T) {
		team, host := rapid.SampledFrom(teams).Draw(t, "team"), rapid.SampledFrom(hosts).Draw(t, "host")
		if rapid.IntRange(0, 5).Draw(t, "any") == 0 {
			team, host = rapid.StringMatching(`[0-9]{0,3}`).Draw(t, "any team"), rapid.StringMatching(`[a-zA-Z0-9.-]{0,5}`).Draw(t, "any host")
		}
		isCell := (team == "01" || team == "02" || team == "03") && (host == "dc" || host == "web")
		before, _, _ := h.api.readCounts()
		rec := h.get(&op, "/vm/"+url.PathEscape(team)+"/"+url.PathEscape(host))
		after, _, _ := h.api.readCounts()
		if isCell && rec.Code != http.StatusOK {
			t.Fatalf("GET /vm/%s/%s = %d", team, host, rec.Code)
		}
		if !isCell {
			if rec.Code == http.StatusOK {
				t.Fatalf("GET /vm/%q/%q = 200, not a cell", team, host)
			}
			if after != before {
				t.Fatalf("GET /vm/%q/%q read %d configs from Proxmox", team, host, after-before)
			}
		}
	})
}

var pickerOptionRE = regexp.MustCompile(`<input type="radio" name="snapshot" value="([^"]*)" required( checked)?>`)

// The reset picker offers deploy.snapshot_name first, then each snapshot of
// the VM once, keeps whatever was asked for, and marks that option (or
// deploy.snapshot_name when none was asked for).
func TestPropResetPickerOptions(t *testing.T) {
	h := newHarness(t)
	op := h.login(asOperator)
	names := []string{"initial", "Initial", "before-scoring", "round1", "round2", "a", "x-y_z"}
	rapid.Check(t, func(t *rapid.T) {
		snaps := rapid.SliceOfNDistinct(rapid.SampledFrom(names), 0, 6, rapid.ID[string]).Draw(t, "snapshots")
		selected := rapid.SampledFrom(append([]string{""}, append(slices.Clone(names), "gone", "a&b")...)).Draw(t, "selected")
		h.api.setSnapshots(10101, snaps...)
		h.poll()
		body := h.get(&op, "/reset?teams=1&hosts=dc&snapshot="+url.QueryEscape(selected)).Body.String()
		wantSelected := selected
		if wantSelected == "" {
			wantSelected = "initial"
		}
		var values []string
		checked := 0
		for _, m := range pickerOptionRE.FindAllStringSubmatch(body, -1) {
			values = append(values, html.UnescapeString(m[1]))
			if m[2] != "" {
				checked++
				if html.UnescapeString(m[1]) != wantSelected {
					t.Fatalf("checked option %q, selected %q", m[1], wantSelected)
				}
			}
		}
		want := []string{"initial"}
		for _, n := range snaps {
			if !slices.Contains(want, n) {
				want = append(want, n)
			}
		}
		if !slices.Contains(want, wantSelected) {
			want = append(want, wantSelected)
		}
		if !slices.Equal(values, want) || checked != 1 {
			t.Fatalf("snapshots %v, selected %q: options %q (%d checked), want %q", snaps, selected, values, checked, want)
		}
	})
}
