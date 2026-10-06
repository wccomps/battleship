package authtest_test

import (
	"context"
	"testing"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func TestStubProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Web.BaseURL = "http://battleship.test"
	authtest.StubProvider(t, &cfg)
	if cfg.OIDC.Issuer == "" || cfg.OIDC.ClientID == "" || cfg.OIDC.ClientSecret == "" {
		t.Fatalf("oidc = %v, want issuer, client ID and secret set", cfg.OIDC)
	}
	if err := cfg.RequireWeb(); err != nil {
		t.Fatalf("RequireWeb: %v", err)
	}
	st := storetest.New(t)
	svc, err := auth.NewService(context.Background(), cfg, st, auth.Options{Proxmox: pvetest.New(t).Client()})
	if err != nil {
		t.Fatalf("NewService against the stub provider: %v", err)
	}
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
}
