package auth_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// proxmoxCallback walks one tab through /auth/proxmox and the identity
// provider, and returns the callback URL it would come back to.
func (b *browser) proxmoxCallback(next string) *url.URL {
	b.h.t.Helper()
	start := b.do(http.MethodGet, "/auth/proxmox?next="+url.QueryEscape(next), nil, nil)
	if start.StatusCode != http.StatusFound {
		b.h.t.Fatalf("/auth/proxmox = %d %s", start.StatusCode, start.body)
	}
	idp := b.do(http.MethodGet, start.Header.Get("Location"), nil, nil)
	cb, err := url.Parse(idp.Header.Get("Location"))
	if err != nil {
		b.h.t.Fatal(err)
	}
	return cb
}

// Two tabs sign in to Proxmox at once: once the newer one has a ticket,
// the older tab's stale callback just goes on.
func TestProxmoxSignInFromTwoTabs(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	if err := h.st.ClearSessionTicket(ctx, b.session().ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	h.pve.SignIn(pveUser(alice))
	first := b.proxmoxCallback("/logs")
	second := b.proxmoxCallback("/private")
	if resp := b.do(http.MethodGet, second.String(), nil, nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/private" {
		t.Fatalf("second tab: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp := b.do(http.MethodGet, first.String(), nil, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first tab, after the second signed in: %d %s", resp.StatusCode, resp.body)
	}
}

// A browser whose Authentik session is someone else's must not get that
// person's Proxmox ticket in this session.
func TestProxmoxSignInAsSomeoneElseIsRefused(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	if err := h.st.ClearSessionTicket(ctx, b.session().ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	h.pve.SignIn(pveUser(lena))
	resp := b.get("/private")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(resp.body, "lena@auth.example.org") {
		t.Fatalf("got %d: %s", resp.StatusCode, resp.body)
	}
	if c, ok := b.pveTicket(); ok {
		t.Fatalf("alice's session holds %v", c)
	}
}

// With Proxmox's clock hours off, sign-in still works: a ticket's age
// counts from when battleship got it.
func TestProxmoxSignInWithSkewedClocksWorks(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	if err := h.st.ClearSessionTicket(ctx, b.session().ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	h.pve.SetClock(func() time.Time { return h.clock.Now().Add(-3 * time.Hour) })
	h.pve.SignIn(pveUser(alice))
	cb := b.proxmoxCallback("/private")
	resp := b.do(http.MethodGet, cb.String(), nil, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/private" {
		t.Fatalf("callback = %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), resp.body)
	}
	if resp := b.get("/private"); resp.StatusCode != http.StatusOK {
		t.Fatalf("after the sign-in: %d %s", resp.StatusCode, resp.body)
	}
	c, ok := b.pveTicket()
	if !ok || !c.Issued.Equal(h.clock.Now()) {
		t.Fatalf("ticket issued %v, want battleship's now %v", c.Issued, h.clock.Now())
	}
}
