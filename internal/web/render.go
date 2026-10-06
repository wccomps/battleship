package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/pods"
)

//go:embed templates/*.html
var templateFS embed.FS

// Page templates. Each page file defines "content"; it is parsed on top of
// layout.html and the shared partials (partials.html), and executed as
// "layout". A page is looked up by its file name without ".html".
var pages = parsePages()

func parsePages() map[string]*template.Template {
	base := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html"))
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		panic(err)
	}
	out := map[string]*template.Template{}
	for _, name := range names {
		file := path.Base(name)
		if file == "layout.html" || file == "partials.html" {
			continue
		}
		t := template.Must(template.Must(base.Clone()).ParseFS(templateFS, name))
		out[strings.TrimSuffix(file, ".html")] = t
	}
	return out
}

// funcs are the helpers templates may call. Times are formatted in Go
// (clockTime, minuteTime, shortTime), against the server's clock, before
// they reach a template.
var funcs = template.FuncMap{"asset": assetURL, "names": keepNames, "icon": icon, "opIcon": opIcon, "needs": needs}

// nameRE matches what reads as a name in prose: a run of letters, digits,
// '_' and '*' joined by '-' or '.', as VM, pool, snapshot, template and
// set names are (team03-web, pool-03, dc.kilo.alpha.tpl,
// *.kilo.alpha). keepNames skips the ones without a letter or '*'.
var nameRE = regexp.MustCompile(`[A-Za-z0-9_*]+(?:[-.][A-Za-z0-9_*]+)+`)

// keepNames escapes s for HTML with each name in it in a span of class
// "name", which the stylesheet never breaks across lines: a browser may
// otherwise break a name at a hyphen.
func keepNames(s string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range nameRE.FindAllStringIndex(s, -1) {
		if !strings.ContainsFunc(s[m[0]:m[1]], func(r rune) bool { return r == '*' || unicode.IsLetter(r) }) {
			continue // a number or a team range, such as 01-03
		}
		b.WriteString(template.HTMLEscapeString(s[last:m[0]]))
		b.WriteString(`<span class="name">`)
		b.WriteString(template.HTMLEscapeString(s[m[0]:m[1]]))
		b.WriteString(`</span>`)
		last = m[1]
	}
	b.WriteString(template.HTMLEscapeString(s[last:]))
	return template.HTML(b.String())
}

// clockTime formats t for people: the time of day in UTC, e.g.
// "14:03:05 UTC", or with the date when t isn't on the same day as now.
func clockTime(now, t time.Time) string {
	return dayTime(now, t, "15:04:05 UTC", "Jan 2 15:04:05 UTC")
}

// shortTime formats t compactly: "14:03:05", or "Jan 2 14:03" on
// another day than now. Times are UTC, as clockTime says in full; pages
// put that in a title.
func shortTime(now, t time.Time) string { return dayTime(now, t, "15:04:05", "Jan 2 15:04") }

// minuteTime is shortTime to the minute: "14:03", or "Jan 2 14:03".
func minuteTime(now, t time.Time) string { return dayTime(now, t, "15:04", "Jan 2 15:04") }

func dayTime(now, t time.Time, today, otherDay string) string {
	t, now = t.UTC(), now.UTC()
	if t.YearDay() != now.YearDay() || t.Year() != now.Year() {
		return t.Format(otherDay)
	}
	return t.Format(today)
}

// banner is a problem a page shows above its content: a title and a few
// sentences. Level is "warn" or "bad".
type banner struct {
	Level string
	Title string
	Text  []string
}

// view is what the layout needs, plus the page's own data.
type view struct {
	Title  string     // the page's name: the <title> and, usually, its <h1>
	Active string     // the nav entry to mark as the current page
	User   *auth.User // nil when nobody is logged in
	CSRF   string     // the session's CSRF token, for every form
	Nav    []navItem
	Lead   []navItem // the header's Deploy and Teardown buttons: the team-level operations the user may run
	// PVEUser is the Proxmox user the signed-in person acts as.
	PVEUser string
	Flash   *flash
	Data    any // the page's own data
	// Panel is set when the page is rendered for the grid's side panel:
	// its content only, without the layout (see render).
	Panel bool
	// Live, if set, shows the header's live dot in this state ("live",
	// "stale"): for pages that follow an event stream. The script turns
	// it "off" while the stream is down.
	Live string
	// Initial is the user's initial, for the account button.
	Initial string
}

// navItem is an entry of the top navigation.
type navItem struct {
	Key, Label, Href string
}

// nav is the main navigation: the grid, where VMs are powered and reset,
// and the logs of what ran. Pages call newView, which fills it in. The
// power and reset forms stay at their addresses, for links that fill them
// in (a job's "start again"), but aren't in the menu.
func nav(u *auth.User) []navItem {
	if u == nil {
		return nil
	}
	return []navItem{{"grid", "Grid", "/"}, {"logs", "Logs", logsPath}}
}

// leadOps is the operations that work on whole teams, deploy and
// teardown, those of them the user holds a privilege for somewhere.
func (s *Server) leadOps(ctx context.Context) []navItem {
	var items []navItem
	for _, op := range operations {
		if (op.Kind == pods.KindDeploy || op.Kind == pods.KindTeardown) && s.mayRun(ctx, op.Kind) {
			items = append(items, navItem{string(op.Kind), op.Title, op.Path})
		}
	}
	return items
}

// newView starts the view of a page for the request's user. It takes the
// flash message waiting for this browser, if any, and clears it.
func (s *Server) newView(w http.ResponseWriter, r *http.Request, title, active string, data any) view {
	ctx := r.Context()
	v := view{Title: title, Active: active, Data: data, CSRF: auth.CSRFToken(ctx), Panel: wantsPanel(r)}
	if u, ok := auth.UserFrom(ctx); ok {
		v.User = &u
		v.Initial = initial(u.Name + u.Email)
		// Only the layout shows these, and only a Proxmox ticket can read
		// the user's privileges.
		if cred, ok := auth.ProxmoxCredential(ctx); ok && !v.Panel {
			v.PVEUser, v.Lead = cred.User, s.leadOps(ctx)
		}
	}
	v.Nav = nav(v.User)
	v.Flash = s.takeFlash(w, r)
	return v
}

// panelHeader asks for a page's content without the layout, for the side
// panel; fetch keeps it across redirects. Panel responses carry it back, so
// the script can tell them from anything else (the login page).
const panelHeader = "X-Battleship-Panel"

// wantsPanel reports whether r asks for the panel's rendering.
func wantsPanel(r *http.Request) bool { return r.Header.Get(panelHeader) == "1" }

// render writes page with v and status. The page is rendered in full before
// anything is written, so a template error becomes a plain 500, and is
// logged through Deps.Logf. For the side panel (v.Panel) it renders the
// same page's content without the layout: the same handler, data and
// template, so a preview or a job looks the same in the panel as on its
// own page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, v view) {
	t, ok := pages[page]
	if !ok {
		s.logf("web: no page template %q", page)
		http.Error(w, "internal error: no such page", http.StatusInternalServerError)
		return
	}
	root := "layout"
	if v.Panel {
		root = "panel"
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, root, v); err != nil {
		s.logf("web: rendering %s: %v", page, err)
		http.Error(w, "internal error rendering the page", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Vary", panelHeader)
	if v.Panel {
		h.Set(panelHeader, "1")
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = buf.WriteTo(w) // the client went away; nothing to do
	}
}

// fragment renders one named template of a page, e.g. a piece an event
// stream sends.
func fragment(page, name string, data any) (string, error) {
	t, ok := pages[page]
	if !ok {
		return "", fmt.Errorf("no page template %q", page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("rendering %s of %s: %w", name, page, err)
	}
	return buf.String(), nil
}

// message is the data of the message page: a short explanation and a
// link onwards.
type message struct {
	Status   int
	Message  string
	Link     string
	LinkText string
}

// AuthPage renders the pages auth shows itself (login and Proxmox sign-in
// errors, and 401/403 refusals) in this app's layout. New makes it the
// auth service's.
func (s *Server) AuthPage(w http.ResponseWriter, r *http.Request, p auth.Page) {
	v := s.newView(w, r, p.Title, "", message{Status: p.Status, Message: sentence(p.Message), Link: p.Link, LinkText: p.LinkText})
	s.render(w, r, p.Status, "message", v)
}

// sentence makes a message that may start in lower case, like auth's
// refusals, read as a sentence: capitalised, with a full stop.
func sentence(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(msg)
	msg = string(unicode.ToUpper(r)) + msg[n:]
	if !strings.ContainsAny(msg[len(msg)-1:], ".:!?") {
		msg += "."
	}
	return msg
}

// errorPage renders a problem as the message page.
func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, title, msg, link, linkText string) {
	s.AuthPage(w, r, auth.Page{Status: status, Title: title, Message: msg, Link: link, LinkText: linkText})
}

// notFound is the page for addresses the app doesn't serve.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusNotFound, "Miss.",
		"There's no page at this address. It may have been mistyped, or the link is out of date.", "/", "Go to the grid")
}

// opIcon is the icon of an operation kind, or of a power action.
func opIcon(kind string) template.HTML {
	switch {
	case pods.IsPowerAction(kind), kind == string(pods.KindReset), kind == string(pods.KindSnapshot),
		kind == string(pods.KindDeploy), kind == string(pods.KindTeardown):
		return icon(kind)
	case kind == string(pods.KindPower):
		return icon("shutdown")
	}
	return icon("reboot")
}

// Flash messages carry a one-line result across a redirect, e.g. "job 7
// submitted", in a short-lived cookie that the next page shows and clears.
// They hold nothing secret: a browser can only forge one for itself, and
// the text is escaped like any other. On https (web.base_url) the cookie is
// Secure and named with the __Host- prefix, like the session's, so nothing
// on plain http can set or read it.
const (
	flashCookie = "battleship_flash"
	flashMaxLen = 500 // bytes of message, cut at a rune boundary
)

const (
	flashInfo    = "info"
	flashSuccess = "success"
	flashError   = "error"
)

// flash is a message for the next page.
type flash struct {
	Kind    string // flashInfo, flashSuccess or flashError
	Message string
}

// flashCookieName is the flash cookie's name: __Host-battleship_flash on
// https.
func (s *Server) flashCookieName() string {
	if s.secureCookies {
		return "__Host-" + flashCookie
	}
	return flashCookie
}

// truncateRunes cuts s to at most n bytes without splitting a character.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// setFlash leaves a message for the next page this browser loads. Call it
// before writing the response, typically before a redirect.
func (s *Server) setFlash(w http.ResponseWriter, kind, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.flashCookieName(),
		Value:    kind + "." + base64.RawURLEncoding.EncodeToString([]byte(truncateRunes(msg, flashMaxLen))),
		Path:     "/",
		MaxAge:   60,
		Secure:   s.secureCookies,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// takeFlash returns the waiting message, if any, and clears it. A cookie
// that isn't a flash message is cleared and ignored.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	name := s.flashCookieName()
	c, err := r.Cookie(name)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Secure: s.secureCookies, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	kind, enc, ok := strings.Cut(c.Value, ".")
	if !ok || (kind != flashInfo && kind != flashSuccess && kind != flashError) {
		return nil
	}
	msg, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || len(msg) == 0 || len(msg) > flashMaxLen || !utf8.Valid(msg) {
		return nil
	}
	return &flash{Kind: kind, Message: string(msg)}
}
