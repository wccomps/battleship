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

// A client verifies Proxmox with the CA and refuses it without.
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

// GET /pools shows a pool's comment only with Pool.Audit on it; PUT /pools
// sets it only with Pool.Allocate, keeping it on another's refusal.
func TestPoolCommentsFollowPoolPrivileges(t *testing.T) {
	pve := pvetest.New(t)
	pve.SetPool("dev-01", "lab [env kilo.alpha]")
	pve.SetPool("dev-02", "")
	pve.AddUser("aud@auth.example.org", map[string]map[string]bool{"/pool/dev-01": {"Pool.Audit": true}})
	pve.AddUser("alloc@auth.example.org", map[string]map[string]bool{"/pool/dev-02": {"Pool.Allocate": true}})
	aud := pve.Client().As(pve.Ticket("aud@auth.example.org"))
	alloc := pve.Client().As(pve.Ticket("alloc@auth.example.org"))

	pools, err := aud.Pools(bg)
	if err != nil || len(pools) != 1 || pools[0] != (proxmox.Pool{ID: "dev-01", Comment: "lab [env kilo.alpha]"}) {
		t.Fatalf("Pool.Audit on dev-01 lists %+v, %v", pools, err)
	}
	if pools, err = alloc.Pools(bg); err != nil || len(pools) != 0 {
		t.Fatalf("without Pool.Audit: %+v, %v; want no pools", pools, err)
	}
	if err := aud.SetPoolComment(bg, "dev-01", "x"); !proxmox.IsForbidden(err) || pve.PoolComment("dev-01") != "lab [env kilo.alpha]" {
		t.Fatalf("Pool.Audit only: %v, comment %q", err, pve.PoolComment("dev-01"))
	}
	if err := alloc.SetPoolComment(bg, "dev-02", "[env lima.alpha]"); err != nil || pve.PoolComment("dev-02") != "[env lima.alpha]" {
		t.Fatalf("Pool.Allocate: %v, comment %q", err, pve.PoolComment("dev-02"))
	}
}
