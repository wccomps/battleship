package pvetest_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
)

var bg = context.Background()

// noRedirects is a browser that stops at each redirect.
var noRedirects = &http.Client{
	Transport:     http.DefaultTransport.(*http.Transport).Clone(),
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func init() {
	noRedirects.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // test server
}

func TestOpenIDLoginRoundTrip(t *testing.T) {
	pve := pvetest.New(t)
	pve.AddUser("alice@auth.example.org", nil)
	pve.SignIn("alice@auth.example.org")
	c := pve.Client()
	authURL, endpoint, err := c.OpenIDAuthURL(bg, pvetest.Realm, "https://rk.test/auth/proxmox/callback")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noRedirects.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	if back.Host != "rk.test" || back.Query().Get("code") == "" {
		t.Fatalf("the identity provider sent the browser to %s", back)
	}
	q := back.Query()
	cred, err := c.OpenIDLogin(bg, endpoint, q.Get("code"), q.Get("state"), "https://rk.test/auth/proxmox/callback", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cred.User != "alice@auth.example.org" || !pve.Accepts(cred) {
		t.Fatalf("login gave %v", cred)
	}
	// The state is single use.
	if _, err := c.OpenIDLogin(bg, endpoint, q.Get("code"), q.Get("state"), "https://rk.test/auth/proxmox/callback", time.Now()); !proxmox.IsLapsed(err) {
		t.Fatalf("a second login with one state: %v, want 401", err)
	}
}

func TestTicketsExpireRenewAndDie(t *testing.T) {
	pve := pvetest.New(t)
	now := time.Now()
	pve.SetClock(func() time.Time { return now })
	pve.AddUser("alice@auth.example.org", nil)
	cred := pve.Ticket("alice@auth.example.org")
	c := pve.Client()

	now = now.Add(time.Hour)
	renewed, err := c.RenewTicket(bg, cred, time.Now())
	if err != nil || !pve.Accepts(renewed) {
		t.Fatalf("renewing a valid ticket: %v", err)
	}
	now = now.Add(time.Hour + time.Second)
	if pve.Accepts(cred) {
		t.Fatal("a ticket outlived 2h")
	}
	if _, err := c.RenewTicket(bg, cred, time.Now()); !proxmox.IsLapsed(err) {
		t.Fatalf("renewing an expired ticket: %v, want 401", err)
	}
	if !pve.Accepts(renewed) {
		t.Fatal("the renewed ticket died with the old one")
	}
	pve.Disable("alice@auth.example.org")
	if _, err := c.As(renewed).Permissions(bg); !proxmox.IsLapsed(err) {
		t.Fatalf("a disabled user's ticket: %v, want 401", err)
	}
}

func TestPermissionsResolveLikeProxmox(t *testing.T) {
	pve := pvetest.New(t)
	pve.AddUser("op@auth.example.org", map[string]map[string]bool{
		"/vms":       {"VM.Audit": true, "VM.PowerMgmt": true},
		"/vms/10101": {"VM.Audit": false}, // replaces, doesn't add
	})
	api := pve.Client().As(pve.Ticket("op@auth.example.org"))
	all, err := api.Permissions(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := all["/vms"]["VM.PowerMgmt"]; !ok {
		t.Fatalf("permissions = %v", all)
	}
	for path, want := range map[string]bool{"/vms/10102": true, "/vms/10101": false} {
		privs, err := api.PermissionsAt(bg, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := privs["VM.PowerMgmt"]; ok != want {
			t.Errorf("VM.PowerMgmt on %s = %v, want %v", path, ok, want)
		}
	}
}

// Item 22: a client given the cluster CA (here the fake's certificate)
// verifies Proxmox; one without it refuses to talk to it.
func TestClientVerifiesTheFakeWithItsCA(t *testing.T) {
	pve := pvetest.New(t)
	verified := proxmox.New(proxmox.Options{URLs: []string{pve.URL()}, RootCAs: pve.CertPool()})
	if _, _, err := verified.OpenIDAuthURL(bg, pvetest.Realm, "https://rk.test/cb"); err != nil {
		t.Fatalf("with the CA: %v", err)
	}
	unverified := proxmox.New(proxmox.Options{URLs: []string{pve.URL()}})
	if _, _, err := unverified.OpenIDAuthURL(bg, pvetest.Realm, "https://rk.test/cb"); err == nil {
		t.Fatal("a client without the CA talked to Proxmox")
	}
}
