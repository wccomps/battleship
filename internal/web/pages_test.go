package web

import (
	"encoding/base64"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// /access redirects to the grid: who may use battleship is Authentik's call.
func TestAccessPageIsGone(t *testing.T) {
	h := newHarness(t)
	rec := h.get(nil, "/access")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("GET /access = %d to %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAuthPagesUseTheLayout(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	// A form post without the CSRF token is refused by auth, through the
	// web layout.
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(op.Cookie)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /auth/logout without CSRF = %d, want 403", rec.Code)
	}
	contains(t, "CSRF refusal", rec.Body.String(),
		"<h1>Request refused</h1>", `<link rel="stylesheet" href="/static/app.css?v=`, `class="deny"`)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	// Any auth page renders through the layout, escaped.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	h.srv.AuthPage(w, r, auth.Page{Status: http.StatusBadGateway, Title: "Authentik didn't answer", Message: "Try again <soon>.", Link: "/auth/login", LinkText: "Try again"})
	if w.Code != http.StatusBadGateway {
		t.Errorf("AuthPage status = %d", w.Code)
	}
	contains(t, "AuthPage", w.Body.String(), "<h1>Authentik didn&#39;t answer</h1>", "Try again &lt;soon&gt;.", `<a class="btn" href="/auth/login">Try again</a>`)

	// HEAD gets headers only.
	w = httptest.NewRecorder()
	h.srv.AuthPage(w, httptest.NewRequest(http.MethodHead, "/", nil), auth.Page{Status: http.StatusUnauthorized, Title: "x"})
	if w.Code != http.StatusUnauthorized || w.Body.Len() != 0 {
		t.Errorf("HEAD AuthPage = %d with %d bytes", w.Code, w.Body.Len())
	}
}

func TestSentence(t *testing.T) {
	for in, want := range map[string]string{
		"":                          "",
		"deploying pods needs lead": "Deploying pods needs lead.",
		"Already fine.":             "Already fine.",
		"ask a lead to add you to:": "Ask a lead to add you to:",
		"  élan vital ":             "Élan vital.",
		"is it?":                    "Is it?",
	} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNotFound(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/nope", "/vm", "/static/missing.js"} {
		rec := h.get(nil, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
			continue
		}
		contains(t, path, rec.Body.String(), "<h1>Miss.</h1>")
	}
}

func TestStaticAssets(t *testing.T) {
	h := newHarness(t)
	for path, ctype := range map[string]string{
		"/static/app.js":      "text/javascript; charset=utf-8",
		"/static/app.css":     "text/css; charset=utf-8",
		"/static/favicon.svg": "image/svg+xml",
	} {
		rec := h.get(nil, path)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ctype || rec.Body.Len() == 0 {
			t.Errorf("GET %s = %d %q (%d bytes)", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", path, got)
		}
		v := assetURL(strings.TrimPrefix(path, "/static/"))
		rec = h.get(nil, v)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("GET %s = %d, Cache-Control %q", v, rec.Code, rec.Header().Get("Cache-Control"))
		}
		stale := path + "?v=old"
		if rec = h.get(nil, stale); rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache for an old version", stale, rec.Header().Get("Cache-Control"))
		}
	}
	// Pages link the icon; /favicon.ico, which browsers fetch unprompted,
	// redirects to it, logged in or not.
	contains(t, "error page", h.get(nil, "/no-such-page").Body.String(),
		`<link rel="icon" type="image/svg+xml" href="`+assetURL("favicon.svg")+`">`)
	if rec := h.get(nil, "/favicon.ico"); rec.Code != http.StatusFound || rec.Header().Get("Location") != assetURL("favicon.svg") {
		t.Errorf("GET /favicon.ico = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	// The assets are small: no framework.
	for _, name := range []string{"app.js", "app.css"} {
		b, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 32<<10 {
			t.Errorf("%s is %d bytes, want at most 32 KiB", name, len(b))
		}
	}
	// The fonts are served from here, linked from the stylesheet by
	// version, and cached for good; the CSP allows nothing else.
	css := h.get(nil, assetURL("app.css")).Body.String()
	for _, name := range []string{"plex-sans-400.woff2", "plex-sans-600.woff2", "plex-mono-400.woff2"} {
		v := assetURL(name)
		contains(t, "app.css", css, "url("+v+")")
		rec := h.get(nil, v)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "font/woff2" ||
			rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || rec.Body.Len() < 10<<10 {
			t.Errorf("GET %s = %d %q %q (%d bytes)", v, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"), rec.Body.Len())
		}
	}
	lacks(t, "app.css", css, "googleapis", "http:", "https:")
}

func TestFlash(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	w := httptest.NewRecorder()
	h.srv.setFlash(w, flashInfo, "You already confirmed this preview: it is job 7.")
	var flash *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == flashCookie {
			flash = c
		}
	}
	if flash == nil || !flash.HttpOnly || flash.Path != "/" || flash.SameSite != http.SameSiteLaxMode || flash.Secure {
		t.Fatalf("flash cookie = %+v", flash)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	op.Apply(req)
	req.AddCookie(flash)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	contains(t, "page after the flash", rec.Body.String(),
		`<div class="flash" role="status"><p class="warn note">`, "You already confirmed this preview: it is job 7.")
	cleared := false
	for _, c := range rec.Result().Cookies() {
		cleared = cleared || (c.Name == flashCookie && c.MaxAge < 0)
	}
	if !cleared {
		t.Error("the flash cookie was not cleared after showing it")
	}

	// A garbled or forged cookie shows nothing.
	for _, value := range []string{
		url.QueryEscape("<script>"),
		"evil." + base64.RawURLEncoding.EncodeToString([]byte("hi")),
		"info.!!!",
		"info.",
	} {
		req = httptest.NewRequest(http.MethodGet, "/", nil)
		op.Apply(req)
		req.AddCookie(&http.Cookie{Name: flashCookie, Value: value})
		rec = httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		lacks(t, "page with the flash cookie "+value, rec.Body.String(), `class="flash`, "<script>")
	}
}

// On https the flash cookie is Secure and __Host- prefixed, like the
// session's, so a network attacker can't plant or read one over http.
func TestFlashCookieOnHTTPS(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Web.BaseURL = "https://battleship.test" })
	h.poll()
	op := h.login(asOperator)
	ok := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Action: "start"}, byOperator)
	h.start(ok)
	h.finish(ok, store.Outcome{Status: store.StatusSucceeded})
	rec := h.post(&op, "/logs/"+itoa(ok)+"/retry", url.Values{})
	var flash *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if strings.HasSuffix(c.Name, "battleship_flash") {
			flash = c
		}
	}
	if flash == nil || flash.Name != "__Host-battleship_flash" || !flash.Secure || !flash.HttpOnly || flash.Path != "/" || flash.Domain != "" {
		t.Fatalf("flash cookie on https = %+v, want __Host-battleship_flash, Secure, HttpOnly, Path=/", flash)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	op.Apply(req)
	req.AddCookie(flash)
	rec = httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	contains(t, "page after the flash", rec.Body.String(), "can be retried")
	cleared := false
	for _, c := range rec.Result().Cookies() {
		cleared = cleared || (c.Name == "__Host-battleship_flash" && c.MaxAge < 0 && c.Secure)
	}
	if !cleared {
		t.Error("the __Host- flash cookie was not cleared after showing it")
	}
	// The plain name means nothing on https.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	op.Apply(req)
	req.AddCookie(&http.Cookie{Name: "battleship_flash", Value: flash.Value})
	rec = httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	lacks(t, "page with a plain flash cookie on https", rec.Body.String(), `class="flash`)
}

// A long message is cut at a rune boundary, so the page never shows half
// a character.
func TestFlashTruncatesWholeRunes(t *testing.T) {
	msg := strings.Repeat("a", flashMaxLen-1) + "é and more"
	s := &Server{}
	w := httptest.NewRecorder()
	s.setFlash(w, flashError, msg)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	f := s.takeFlash(httptest.NewRecorder(), r)
	if f == nil {
		t.Fatal("no flash after truncating")
	}
	if !utf8.ValidString(f.Message) || f.Message != strings.Repeat("a", flashMaxLen-1) {
		t.Errorf("truncated flash = %q (%d bytes), want the 499 a's", f.Message, len(f.Message))
	}
}

func TestTimeWords(t *testing.T) {
	now := time.Date(2026, 10, 3, 14, 3, 5, 0, time.UTC)
	for _, tc := range []struct {
		t     time.Time
		clock string
	}{
		{now, "14:03:05 UTC"},
		{now.Add(-40 * time.Second), "14:02:25 UTC"},
		{now.Add(-3*time.Minute - 59*time.Second), "13:59:06 UTC"},
		{now.Add(-15 * time.Hour), "Oct 2 23:03:05 UTC"},
		{now.In(time.FixedZone("PDT", -7*3600)), "14:03:05 UTC"},
	} {
		if got := clockTime(now, tc.t); got != tc.clock {
			t.Errorf("clockTime(%v) = %q, want %q", tc.t, got, tc.clock)
		}
	}
	for d, want := range map[time.Duration]string{
		5 * time.Second:         "5 seconds",
		time.Second:             "1 second",
		90 * time.Second:        "90 seconds",
		time.Minute:             "1 minute",
		2 * time.Minute:         "2 minutes",
		12 * time.Hour:          "12 hours",
		1500 * time.Millisecond: "1.5s",
	} {
		if got := spoken(d); got != want {
			t.Errorf("spoken(%v) = %q, want %q", d, got, want)
		}
	}
}

// Template errors reach the server's log (Deps.Logf), not the process's
// default logger.
func TestRenderLogsThroughDeps(t *testing.T) {
	h := newHarness(t)
	pages["test-broken"] = template.Must(template.New("layout").Parse(`{{define "layout"}}{{.Data.NoSuchField}}{{end}}`))
	t.Cleanup(func() { delete(pages, "test-broken") })
	serve := func(page string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /x", func(w http.ResponseWriter, r *http.Request) {
			h.srv.render(w, r, http.StatusOK, page, view{Data: struct{}{}})
		})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		return rec
	}
	if rec := serve("test-broken"); rec.Code != http.StatusInternalServerError {
		t.Errorf("broken template = %d, want 500", rec.Code)
	}
	if rec := serve("no-such-page"); rec.Code != http.StatusInternalServerError {
		t.Errorf("missing template = %d, want 500", rec.Code)
	}
	contains(t, "logs", h.logs.String(), "web: rendering test-broken:", "NoSuchField", `web: no page template "no-such-page"`)
}

// The connection state needs its own live region: screen readers don't
// announce the live dot's pop-over inside a closed <details>.
func TestConnectionNoteStaysInPage(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	for _, path := range []string{"/", "/logs"} {
		body := h.get(&op, path).Body.String()
		contains(t, path, body, `<span class="sr" id="live-note" role="status"></span>`, `<details class="live" id="live">`)
	}
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "app.js", string(js), `$("live-note")`)
	// A stream refused with 401 makes the script check the stream the way
	// EventSource asks for it, and reload; three errors in a row do too.
	contains(t, "app.js", string(js), `Accept: "text/event-stream"`, "r.status === 401", "++errors < 3")
}

// keepNames keeps names whole and escapes everything.
func TestKeepNames(t *testing.T) {
	for in, want := range map[string]string{
		"Reset team 01 to snapshot before-scoring": `Reset team 01 to snapshot <span class="name">before-scoring</span>`,
		`in pool "wrong-pool", expected "pool-03"`: `in pool &#34;<span class="name">wrong-pool</span>&#34;, expected &#34;<span class="name">pool-03</span>&#34;`,
		"Deploy teams 01-03 from *.kilo.alpha.":    `Deploy teams 01-03 from <span class="name">*.kilo.alpha</span>.`,
		"<b>team01-dc</b>":                         `&lt;b&gt;<span class="name">team01-dc</span>&lt;/b&gt;`,
	} {
		if got := string(keepNames(in)); got != want {
			t.Errorf("keepNames(%q) = %s, want %s", in, got, want)
		}
	}
}

// A succeeded deploy or teardown says what it did in the game's words,
// leaving out VMs that were already gone; other outcomes say nothing extra.
func TestFleetHeadline(t *testing.T) {
	done := []store.Item{{Status: store.ItemDone}, {Status: store.ItemDone}, {Status: store.ItemDone}}
	for _, c := range []struct {
		kind, status string
		sum          *summaryView
		want         string
	}{
		{"deploy", store.StatusSucceeded, nil, "Fleet deployed: 3 VMs"},
		{"teardown", store.StatusSucceeded, &summaryView{AlreadyGone: []string{"team01-dc", "team01-web"}}, "Sunk: 1 VM"},
		{"teardown", store.StatusSucceeded, &summaryView{AlreadyGone: []string{"a", "b", "c"}}, ""},
		{"teardown", store.StatusCompletedWithFailures, nil, ""},
		{"reset", store.StatusSucceeded, nil, ""},
	} {
		if got := fleetHeadline(c.kind, c.status, done, c.sum); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.kind, c.status, got, c.want)
		}
	}
}

// An item's step strip; a done item's unreported steps weren't needed,
// unless it recorded no outcomes at all (older jobs).
func TestItemSteps(t *testing.T) {
	planned := []pods.Step{pods.StepClone, pods.StepNetwork, pods.StepCDROM, pods.StepStart}
	states := func(it store.Item) string {
		var out []string
		for _, s := range itemSteps(planned, it) {
			out = append(out, s.State)
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct {
		it   store.Item
		want string
	}{
		{store.Item{Status: store.ItemPending}, "wait wait wait wait"},
		{store.Item{Status: store.ItemRunning, Steps: map[string]string{"clone": "done", "network": "done"}}, "done done run wait"},
		{store.Item{Status: store.ItemFailed, Steps: map[string]string{"clone": "done", "network": "failed"}}, "done fail wait wait"},
		{store.Item{Status: store.ItemDone, Steps: map[string]string{"clone": "done", "network": "done", "cdrom": "skipped", "start": "done"}}, "done done skip done"},
		{store.Item{Status: store.ItemDone, Steps: map[string]string{"start": "skipped"}}, "skip skip skip skip"},
		{store.Item{Status: store.ItemDone}, "done done done done"},
		{store.Item{Status: store.ItemInterrupted, Steps: map[string]string{"clone": "interrupted"}}, "int wait wait wait"},
	} {
		if got := states(c.it); got != c.want {
			t.Errorf("%s %v: %q, want %q", c.it.Status, c.it.Steps, got, c.want)
		}
	}
}
