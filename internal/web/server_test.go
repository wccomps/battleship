package web

import (
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
)

func TestNewChecksDeps(t *testing.T) {
	h := newHarness(t)
	good := Deps{Config: h.cfg, Store: h.st, As: h.as, Credentials: h.creds, Auth: h.srv.auth, Views: h.views, Hub: h.hub}
	if _, err := New(good); err != nil {
		t.Fatalf("New with every dependency: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*Deps)
		want string
	}{
		{"no store", func(d *Deps) { d.Store = nil }, "a store"},
		{"no API", func(d *Deps) { d.As = nil }, "a Proxmox API"},
		{"no auth", func(d *Deps) { d.Auth = nil }, "an auth service"},
		{"no views", func(d *Deps) { d.Views = nil }, "status views"},
		{"no hub", func(d *Deps) { d.Hub = nil }, "a hub"},
		{"bad trusted proxy", func(d *Deps) { d.Config.Web.TrustedProxies = []string{"10.0.0.1"} }, "use 10.0.0.1/32"},
		{"bad issuer", func(d *Deps) { d.Config.OIDC.Issuer = "::" }, "oidc.issuer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := good
			d.Config = config.Default()
			d.Config.Web = h.cfg.Web
			d.Config.OIDC = h.cfg.OIDC
			tc.mut(&d)
			_, err := New(d)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("New = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}
