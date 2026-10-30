package auth_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// cookieTicket is the browser's ticket cookie, opened for its session.
func (b *browser) cookieTicket() (proxmox.Credential, bool) {
	b.h.t.Helper()
	u, _ := url.Parse(b.h.app.URL)
	for _, c := range b.jar.Cookies(u) {
		if c.Name == auth.TicketCookie {
			return auth.OpenTicketCookie(b.h.svc, b.session().ID, c.Value)
		}
	}
	return proxmox.Credential{}, false
}

func (b *browser) setTicketCookie(v string) {
	u, _ := url.Parse(b.h.app.URL)
	b.jar.SetCookies(u, []*http.Cookie{{Name: auth.TicketCookie, Value: v, Path: "/"}})
}

func (b *browser) ticketCookieValue() string {
	u, _ := url.Parse(b.h.app.URL)
	for _, c := range b.jar.Cookies(u) {
		if c.Name == auth.TicketCookie {
			return c.Value
		}
	}
	return ""
}

func TestLoginSignsInToProxmoxToo(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	resp := b.login(alice)
	if !strings.Contains(resp.body, "pve=alice@auth.example.org") {
		t.Fatalf("/private saw %q, want alice's Proxmox credential", resp.body)
	}
	cred, ok := b.cookieTicket()
	if !ok || cred.User != "alice@auth.example.org" || !h.pve.Accepts(cred) {
		t.Fatalf("ticket cookie = %v, %v", cred, ok)
	}
	if !cred.LoginAt.Equal(h.clock.Now()) {
		t.Fatalf("login time %v, want %v", cred.LoginAt, h.clock.Now())
	}
	if v := b.ticketCookieValue(); strings.Contains(v, cred.Ticket) || strings.Contains(v, cred.CSRF) {
		t.Fatal("the ticket cookie carries the ticket in the clear")
	}
	if !strings.Contains(h.logs.String(), `auth: proxmox login: subject="sub-alice" user="alice@auth.example.org"`) {
		t.Errorf("logs: %s", h.logs)
	}
}

func TestSessionWithoutATicketGoesThroughProxmox(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	b.dropTicket()
	resp := b.do(http.MethodGet, "/private?x=1", nil, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/auth/proxmox?next="+url.QueryEscape("/private?x=1") {
		t.Fatalf("GET without a ticket: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp = b.postForm("/private", url.Values{"csrf": {b.session().CSRF}}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST without a ticket: %d, want 401", resp.StatusCode)
	}
	resp = b.do(http.MethodGet, "/private", nil, http.Header{"Accept": {"text/event-stream"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("event stream without a ticket: %d, want 401", resp.StatusCode)
	}
	// Following the redirect signs in to Proxmox again, silently.
	h.pve.SignIn(pveUser(alice))
	if resp := b.get("/private"); resp.StatusCode != http.StatusOK || !strings.Contains(resp.body, "pve=alice") {
		t.Fatalf("after the Proxmox step: %d %q", resp.StatusCode, resp.body)
	}
}

func TestProxmoxCallbackChecksState(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	b.dropTicket()
	h.pve.SignIn(pveUser(alice))
	start := b.pastInterstitial("/auth/proxmox?next=/private")
	if start.StatusCode != http.StatusFound {
		t.Fatalf("/auth/proxmox = %d %s", start.StatusCode, start.body)
	}
	idp := b.do(http.MethodGet, start.Header.Get("Location"), nil, nil)
	cb, _ := url.Parse(idp.Header.Get("Location"))
	q := cb.Query()
	q.Set("state", "forged"+q.Get("state"))
	q.Set("finish", "1") // past the interstitial
	cb.RawQuery = q.Encode()
	resp := b.do(http.MethodGet, cb.String(), nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback with another state: %d, want 400", resp.StatusCode)
	}
	if _, ok := b.cookieTicket(); ok {
		t.Fatal("a callback with the wrong state stored a ticket")
	}
	if logins, _, _ := h.pve.Counts(); logins != 1 {
		t.Fatalf("Proxmox saw %d logins, want only the first (the forged state never reaches it)", logins)
	}
}

func TestProxmoxSignInFailureSaysTryAgain(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	b.dropTicket()
	h.pve.Disable(pveUser(alice)) // Proxmox refuses the login with a bare 401
	h.pve.SignIn(pveUser(alice))
	resp := b.get("/private")
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(resp.body, "Proxmox sign-in failed; try again") {
		t.Fatalf("got %d: %s", resp.StatusCode, resp.body)
	}
	if !strings.Contains(h.logs.String(), "proxmox login failed") || !strings.Contains(h.logs.String(), h.pve.Endpoint()) {
		t.Errorf("the failure isn't logged with the node to check: %s", h.logs)
	}
}

// The ticket cookie opens only for its own session.
func TestTicketCookieIsBoundToItsSession(t *testing.T) {
	h := newHarness(t)
	a, l := h.browser(), h.browser()
	a.login(alice)
	l.login(lena)
	l.setTicketCookie(a.ticketCookieValue())
	if resp := l.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(resp.Header.Get("Location"), "/auth/proxmox?") {
		t.Fatalf("lena with alice's ticket cookie: %d to %q %s", resp.StatusCode, resp.Header.Get("Location"), resp.body)
	}
}

func TestTicketRenewedOnUse(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Web.SessionIdle, c.Web.SessionRefresh, c.Web.SessionMax = 3*time.Hour, 2*time.Hour, 24*time.Hour
	})
	b := h.browser()
	b.login(alice)
	before, _ := b.cookieTicket()
	h.clock.Advance(30 * time.Minute)
	b.get("/private")
	if _, renews, _ := h.pve.Counts(); renews != 0 {
		t.Fatal("renewed a 30-minute-old ticket")
	}
	h.clock.Advance(31 * time.Minute)
	if resp := b.get("/private"); resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, resp.body)
	}
	after, _ := b.cookieTicket()
	if _, renews, _ := h.pve.Counts(); renews != 1 || after.Ticket == before.Ticket || !h.pve.Accepts(after) {
		t.Fatalf("renewals %d; ticket changed %v", renews, after.Ticket != before.Ticket)
	}
	if !after.LoginAt.Equal(before.LoginAt) {
		t.Fatal("renewal moved the login time")
	}
	if inCookie, ok := b.cookieTicket(); !ok || inCookie.Ticket != after.Ticket {
		t.Fatalf("ticket cookie after renewal = %v, %v; want the renewed ticket", inCookie, ok)
	}
}

func TestLapsedTicketSendsBackThroughProxmox(t *testing.T) {
	long := func(c *config.Config) {
		c.Web.SessionIdle, c.Web.SessionRefresh, c.Web.SessionMax = 5*time.Hour, 4*time.Hour, 24*time.Hour
	}
	t.Run("expired", func(t *testing.T) {
		h := newHarness(t, long)
		b := h.browser()
		b.login(alice)
		h.clock.Advance(2*time.Hour + time.Minute)
		if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusSeeOther ||
			!strings.HasPrefix(resp.Header.Get("Location"), "/auth/proxmox?") {
			t.Fatalf("expired ticket: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
		}
		if _, renews, _ := h.pve.Counts(); renews != 0 {
			t.Fatal("tried to renew an expired ticket")
		}
	})
	t.Run("refused at renewal", func(t *testing.T) {
		h := newHarness(t, long)
		b := h.browser()
		b.login(alice)
		h.clock.Advance(61 * time.Minute)
		h.pve.Disable(pveUser(alice))
		if resp := b.do(http.MethodGet, "/private", nil, nil); resp.StatusCode != http.StatusSeeOther ||
			!strings.HasPrefix(resp.Header.Get("Location"), "/auth/proxmox?") {
			t.Fatalf("refused renewal: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
		}
		if _, ok := b.cookieTicket(); ok {
			t.Fatal("the refused ticket's cookie was kept")
		}
	})
	t.Run("past ticket_max_age", func(t *testing.T) {
		h := newHarness(t, long, func(c *config.Config) { c.Proxmox.TicketMaxAge = 90 * time.Minute })
		b := h.browser()
		b.login(alice)
		h.clock.Advance(61 * time.Minute)
		b.get("/private") // renewed
		h.clock.Advance(30 * time.Minute)
		resp := b.do(http.MethodGet, "/private", nil, nil)
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/auth/proxmox?") {
			t.Fatalf("past max age: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
		}
		// A fresh Proxmox login, which re-reads the Authentik groups.
		h.pve.SignIn(pveUser(alice))
		logins, _, _ := h.pve.Counts()
		if resp := b.get("/private"); resp.StatusCode != http.StatusOK {
			t.Fatalf("%d %s", resp.StatusCode, resp.body)
		}
		if n, _, _ := h.pve.Counts(); n != logins+1 {
			t.Fatal("no fresh Proxmox login")
		}
	})
}

// A session without a recorded username can't start the Proxmox sign-in:
// it is asked to log in again, which records the username.
func TestSessionWithoutAUsernameLogsInAgain(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	sess := b.session()
	if err := h.st.UpdateSession(ctx, sess.ID, store.SessionUpdate{Name: sess.Name, Email: sess.Email,
		RefreshToken: sess.RefreshToken, RefreshedAt: sess.RefreshedAt}); err != nil {
		t.Fatal(err)
	}
	b.dropTicket()
	resp := b.get("/private")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(resp.body, "Log in again") || !strings.Contains(resp.body, "/auth/login?next=%2Fprivate") {
		t.Fatalf("without a username: %d %q", resp.StatusCode, resp.body)
	}
	h.pve.SignIn(pveUser(alice))
	b.login(alice)
	if resp := b.get("/private"); resp.StatusCode != http.StatusOK || !strings.Contains(resp.body, "pve=alice") {
		t.Fatalf("after logging in again: %d %q\nlogs: %s", resp.StatusCode, resp.body, h.logs)
	}
}

// The callback's first request only shows the interstitial: nothing is
// signed in until it goes on with finish=1, so reloading it is harmless.
func TestProxmoxCallbackShowsInterstitialFirst(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	b.dropTicket()
	h.pve.SignIn(pveUser(alice))
	start := b.pastInterstitial("/auth/proxmox?next=/private")
	idp := b.do(http.MethodGet, start.Header.Get("Location"), nil, nil)
	cb := idp.Header.Get("Location")
	var to string
	for range 2 { // shown, then reloaded
		page := b.do(http.MethodGet, cb, nil, nil)
		to = refreshOf(page.body)
		if page.StatusCode != http.StatusOK || !strings.Contains(page.body, "Signing in to Proxmox") {
			t.Fatalf("callback = %d: %s", page.StatusCode, page.body)
		}
		if _, ok := b.cookieTicket(); ok {
			t.Fatal("the interstitial signed in to Proxmox")
		}
	}
	u, err := url.Parse(to)
	if err != nil {
		t.Fatal(err)
	}
	q, orig := u.Query(), mustQuery(t, cb)
	if u.Path != "/auth/proxmox/callback" || q.Get("finish") != "1" || q.Get("code") != orig.Get("code") || q.Get("state") != orig.Get("state") {
		t.Fatalf("interstitial goes on to %q, want the callback with its code, state and finish=1", to)
	}
	resp := b.do(http.MethodGet, to, nil, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/private" {
		t.Fatalf("finish = %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), resp.body)
	}
	if _, ok := b.cookieTicket(); !ok {
		t.Fatal("finishing stored no ticket")
	}
}

// An error from the identity provider has no code, so it skips the
// interstitial and says what happened at once.
func TestProxmoxCallbackErrorSkipsInterstitial(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	b.dropTicket()
	start := b.pastInterstitial("/auth/proxmox?next=/private")
	u, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp := b.do(http.MethodGet, "/auth/proxmox/callback?error=access_denied&state="+url.QueryEscape(u.Query().Get("state")), nil, nil)
	if resp.StatusCode != http.StatusForbidden || refreshOf(resp.body) != "" {
		t.Fatalf("error callback = %d: %s", resp.StatusCode, resp.body)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// /auth/proxmox's first request only shows the interstitial: it asks
// Proxmox for nothing until it goes on with finish=1.
func TestProxmoxStartShowsInterstitialFirst(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	b.dropTicket()
	page := b.do(http.MethodGet, "/auth/proxmox?next=/private", nil, nil)
	if page.StatusCode != http.StatusOK || refreshOf(page.body) != "/auth/proxmox?finish=1&next=%2Fprivate" {
		t.Fatalf("/auth/proxmox = %d refreshing to %q", page.StatusCode, refreshOf(page.body))
	}
	if b.session().PVELoginState != "" {
		t.Fatal("the interstitial started a Proxmox sign-in")
	}
	start := b.do(http.MethodGet, refreshOf(page.body), nil, nil)
	if start.StatusCode != http.StatusFound || b.session().PVELoginState == "" {
		t.Fatalf("finish = %d: %s", start.StatusCode, start.body)
	}
}

// A script's request can't show a page, so it skips the interstitial.
func TestScriptRequestsSkipInterstitial(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	b.dropTicket()
	for mode, want := range map[string]int{"cors": http.StatusFound, "same-origin": http.StatusFound, "navigate": http.StatusOK} {
		resp := b.do(http.MethodGet, "/auth/proxmox?next=/private", nil, http.Header{"Sec-Fetch-Mode": {mode}})
		if resp.StatusCode != want {
			t.Errorf("Sec-Fetch-Mode %s: /auth/proxmox = %d, want %d", mode, resp.StatusCode, want)
		}
	}
}
