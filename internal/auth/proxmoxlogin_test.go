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

// pveTicket is the browser's session's Proxmox ticket, opened.
func (b *browser) pveTicket() (proxmox.Credential, bool) {
	b.h.t.Helper()
	sess := b.session()
	return auth.OpenSessionTicket(b.h.svc, sess)
}

func TestLoginSignsInToProxmoxToo(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	resp := b.login(alice)
	if !strings.Contains(resp.body, "pve=alice@auth.example.org") {
		t.Fatalf("/private saw %q, want alice's Proxmox credential", resp.body)
	}
	sess := b.session()
	cred, ok := b.pveTicket()
	if !ok || cred.User != "alice@auth.example.org" || !h.pve.Accepts(cred) {
		t.Fatalf("session ticket = %v, %v", cred, ok)
	}
	if strings.Contains(sess.PVE.Ticket, cred.Ticket) || strings.Contains(sess.PVE.CSRF, cred.CSRF) {
		t.Fatal("the ticket or CSRF token is stored in the clear")
	}
	if !cred.LoginAt.Equal(h.clock.Now()) {
		t.Fatalf("login time %v, want %v", cred.LoginAt, h.clock.Now())
	}
	if !strings.Contains(h.logs.String(), `auth: proxmox login: subject="sub-alice" user="alice@auth.example.org"`) {
		t.Errorf("logs: %s", h.logs)
	}
}

// The sealed ticket opens only for its own session.
func TestSessionTicketIsBoundToItsSession(t *testing.T) {
	h := newHarness(t)
	a, l := h.browser(), h.browser()
	a.login(alice)
	l.login(lena)
	as, ls := a.session(), l.session()
	ls.PVE = as.PVE // alice's sealed ticket copied into lena's row
	if c, ok := auth.OpenSessionTicket(h.svc, ls); ok {
		t.Fatalf("alice's ticket opened in lena's session: %v", c)
	}
}

func TestSessionWithoutATicketGoesThroughProxmox(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	if err := h.st.ClearSessionTicket(ctx, b.session().ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
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
	if err := h.st.ClearSessionTicket(ctx, b.session().ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	h.pve.SignIn(pveUser(alice))
	start := b.do(http.MethodGet, "/auth/proxmox?next=/private", nil, nil)
	if start.StatusCode != http.StatusFound {
		t.Fatalf("/auth/proxmox = %d %s", start.StatusCode, start.body)
	}
	idp := b.do(http.MethodGet, start.Header.Get("Location"), nil, nil)
	cb, _ := url.Parse(idp.Header.Get("Location"))
	q := cb.Query()
	q.Set("state", "forged"+q.Get("state"))
	cb.RawQuery = q.Encode()
	resp := b.do(http.MethodGet, cb.String(), nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback with another state: %d, want 400", resp.StatusCode)
	}
	if _, ok := b.pveTicket(); ok {
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
	if err := h.st.ClearSessionTicket(ctx, b.session().ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
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

func TestTicketRenewedOnUse(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Web.SessionIdle, c.Web.SessionRefresh, c.Web.SessionMax = 3*time.Hour, 2*time.Hour, 24*time.Hour
	})
	b := h.browser()
	b.login(alice)
	before, _ := b.pveTicket()
	h.clock.Advance(30 * time.Minute)
	b.get("/private")
	if _, renews, _ := h.pve.Counts(); renews != 0 {
		t.Fatal("renewed a 30-minute-old ticket")
	}
	h.clock.Advance(31 * time.Minute)
	if resp := b.get("/private"); resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, resp.body)
	}
	after, _ := b.pveTicket()
	if _, renews, _ := h.pve.Counts(); renews != 1 || after.Ticket == before.Ticket || !h.pve.Accepts(after) {
		t.Fatalf("renewals %d; ticket changed %v", renews, after.Ticket != before.Ticket)
	}
	if !after.LoginAt.Equal(before.LoginAt) {
		t.Fatal("renewal moved the login time")
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
		if _, ok := b.pveTicket(); ok {
			t.Fatal("the refused ticket was kept")
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
	if err := h.st.UpdateSessionGroups(ctx, sess.ID, store.SessionUpdate{Name: sess.Name, Email: sess.Email, Groups: sess.Groups,
		RefreshToken: sess.RefreshToken, RefreshedAt: sess.RefreshedAt}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.ClearSessionTicket(ctx, sess.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
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
