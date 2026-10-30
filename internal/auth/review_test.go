package auth_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// pastInterstitial gets target, which must answer with the interstitial,
// then what the interstitial goes on to, without following redirects.
func (b *browser) pastInterstitial(target string) response {
	b.h.t.Helper()
	page := b.do(http.MethodGet, target, nil, nil)
	to := refreshOf(page.body)
	if page.StatusCode != http.StatusOK || !strings.Contains(to, "finish=1") {
		b.h.t.Fatalf("%s = %d refreshing to %q, want the interstitial", target, page.StatusCode, to)
	}
	return b.do(http.MethodGet, to, nil, nil)
}

// proxmoxCallback walks one tab through /auth/proxmox and the identity
// provider, and returns the callback URL its interstitial goes on to.
func (b *browser) proxmoxCallback(next string) *url.URL {
	b.h.t.Helper()
	start := b.pastInterstitial("/auth/proxmox?next=" + url.QueryEscape(next))
	if start.StatusCode != http.StatusFound {
		b.h.t.Fatalf("/auth/proxmox = %d %s", start.StatusCode, start.body)
	}
	idp := b.do(http.MethodGet, start.Header.Get("Location"), nil, nil)
	// The callback first shows the interstitial, which goes on to it again.
	page := b.do(http.MethodGet, idp.Header.Get("Location"), nil, nil)
	to := refreshOf(page.body)
	if page.StatusCode != http.StatusOK || !strings.HasPrefix(to, "/auth/proxmox/callback?") {
		b.h.t.Fatalf("callback = %d refreshing to %q, want the interstitial", page.StatusCode, to)
	}
	cb, err := url.Parse(to)
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
	b.dropTicket()
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
	b.dropTicket()
	h.pve.SignIn(pveUser(lena))
	resp := b.get("/private")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(resp.body, "lena@auth.example.org") {
		t.Fatalf("got %d: %s", resp.StatusCode, resp.body)
	}
	if c, ok := b.cookieTicket(); ok {
		t.Fatalf("alice's session holds %v", c)
	}
}

// With Proxmox's clock hours off, sign-in still works: a ticket's age
// counts from when battleship got it.
func TestProxmoxSignInWithSkewedClocksWorks(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice)
	b.dropTicket()
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
	c, ok := b.cookieTicket()
	if !ok || !c.Issued.Equal(h.clock.Now()) {
		t.Fatalf("ticket issued %v, want battleship's now %v", c.Issued, h.clock.Now())
	}
}
