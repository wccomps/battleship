package config

import (
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	cfg := Default()
	cfg.Proxmox.URL = "https://pve:8006"
	cfg.Proxmox.InsecureSkipVerify = false
	return cfg
}

// webConfig is a config battleship serve accepts.
func webConfig() Config {
	cfg := validConfig()
	cfg.Web.BaseURL = "https://battleship.example.org"
	cfg.OIDC.ClientID = "battleship"
	cfg.OIDC.ClientSecret = "oidc-secret"
	return cfg
}

func TestWebDefaults(t *testing.T) {
	cfg := Default()
	want := Web{
		Listen:          ":8080",
		Workers:         2,
		SessionIdle:     90 * time.Minute,
		SessionMax:      12 * time.Hour,
		SessionRefresh:  5 * time.Minute,
		StatusPoll:      5 * time.Second,
		DriftScan:       2 * time.Minute,
		ShutdownTimeout: 15 * time.Minute,
	}
	if !reflect.DeepEqual(cfg.Web, want) {
		t.Errorf("Web = %+v, want %+v", cfg.Web, want)
	}
	wantOIDC := OIDC{
		Issuer:      "https://auth.example.org/application/o/battleship/",
		Scopes:      []string{"openid", "profile", "email", "groups", "offline_access"},
		GroupsClaim: "groups",
	}
	if !reflect.DeepEqual(cfg.OIDC, wantOIDC) {
		t.Errorf("OIDC = %#v, want %#v", cfg.OIDC, wantOIDC)
	}
	if err := validConfig().Validate(); err != nil {
		t.Errorf("defaults don't validate: %v", err)
	}
}

func TestLoadWebSections(t *testing.T) {
	path := writeFile(t, `
[proxmox]
url = "https://pve.example:8006"

[web]
listen = "127.0.0.1:9000"
base_url = "https://battleship.example.org/"
workers = 3
session_idle = "2h"
session_max = "8h"
session_refresh = "2m"
status_poll = "3s"
drift_scan = "5m"
shutdown_timeout = "15m"
trusted_proxies = ["10.0.0.0/8", "fd00::/8"]
templates = "*.kilo.alpha"

[oidc]
issuer = "https://auth.example.org/application/o/battleship/"
client_id = "battleship"
client_secret = "from-file"
scopes = ["openid", "groups", "offline_access"]
groups_claim = "roles"
`)
	t.Setenv("BATTLESHIP_OIDC_CLIENT_SECRET", "from-env")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantWeb := Web{
		Listen: "127.0.0.1:9000", BaseURL: "https://battleship.example.org", Workers: 3,
		SessionIdle: 2 * time.Hour, SessionMax: 8 * time.Hour, SessionRefresh: 2 * time.Minute,
		StatusPoll: 3 * time.Second, DriftScan: 5 * time.Minute, ShutdownTimeout: 15 * time.Minute,
		TrustedProxies: []string{"10.0.0.0/8", "fd00::/8"}, Templates: "*.kilo.alpha",
	}
	if !reflect.DeepEqual(cfg.Web, wantWeb) {
		t.Errorf("Web = %+v, want %+v", cfg.Web, wantWeb)
	}
	wantOIDC := OIDC{Issuer: "https://auth.example.org/application/o/battleship/", ClientID: "battleship", ClientSecret: "from-env",
		Scopes: []string{"openid", "groups", "offline_access"}, GroupsClaim: "roles"}
	if !reflect.DeepEqual(cfg.OIDC, wantOIDC) {
		t.Errorf("OIDC = %#v, want the env secret and file values", cfg.OIDC)
	}
	if err := cfg.RequireWeb(); err != nil {
		t.Errorf("RequireWeb: %v", err)
	}

	// The client ID can come from the environment too, next to the secret.
	t.Setenv("BATTLESHIP_OIDC_CLIENT_ID", "id-from-env")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OIDC.ClientID != "id-from-env" {
		t.Errorf("ClientID = %q, want BATTLESHIP_OIDC_CLIENT_ID's value", cfg.OIDC.ClientID)
	}
	prefixes, err := cfg.Web.TrustedProxyPrefixes()
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	if err != nil || !reflect.DeepEqual(prefixes, want) {
		t.Errorf("TrustedProxyPrefixes = %v, %v; want %v", prefixes, err, want)
	}
}

func TestWebValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"listen empty", func(c *Config) { c.Web.Listen = "" }, "web.listen must be host:port"},
		{"listen no port", func(c *Config) { c.Web.Listen = "localhost" }, "web.listen must be host:port"},
		{"base_url not http", func(c *Config) { c.Web.BaseURL = "ftp://battleship.example.org" }, "web.base_url must be an http(s) URL"},
		{"base_url no host", func(c *Config) { c.Web.BaseURL = "https://" }, "web.base_url must be an http(s) URL"},
		{"base_url path", func(c *Config) { c.Web.BaseURL = "https://example.org/battleship" }, "web.base_url must be an http(s) URL with a host and nothing after it"},
		{"base_url query", func(c *Config) { c.Web.BaseURL = "https://example.org?x=1" }, "nothing after it"},
		{"workers zero", func(c *Config) { c.Web.Workers = 0 }, "web.workers must be at least 1"},
		{"templates blank", func(c *Config) { c.Web.Templates = " \t" }, "web.templates must be a master pattern such as \"*.kilo.alpha\", or empty"},
		{"idle too short", func(c *Config) { c.Web.SessionIdle = 30 * time.Second }, "web.session_idle must be at least 1m"},
		{"max below idle", func(c *Config) { c.Web.SessionMax = 10 * time.Minute }, "web.session_max must be at least web.session_idle"},
		{"idle not past renewal", func(c *Config) { c.Web.SessionIdle = time.Hour }, "web.session_idle must be longer than proxmox.ticket_renew_after"},
		{"refresh too short", func(c *Config) { c.Web.SessionRefresh = time.Second }, "web.session_refresh must be at least 30s"},
		{"refresh past max", func(c *Config) { c.Web.SessionRefresh = 13 * time.Hour }, "web.session_refresh must be shorter than web.session_max"},
		{"status_poll too fast", func(c *Config) { c.Web.StatusPoll = 100 * time.Millisecond }, "web.status_poll must be at least 1s"},
		{"drift_scan too fast", func(c *Config) { c.Web.DriftScan = time.Second }, "web.drift_scan must be at least 10s"},
		{"shutdown_timeout too short", func(c *Config) { c.Web.ShutdownTimeout = 5 * time.Second }, "web.shutdown_timeout must be at least 10s"},
		{"trusted proxy not a CIDR", func(c *Config) { c.Web.TrustedProxies = []string{"10.0.0.1"} }, `web.trusted_proxies: "10.0.0.1" is not a CIDR (for one address, use 10.0.0.1/32)`},
		{"trusted proxy garbage", func(c *Config) { c.Web.TrustedProxies = []string{"proxy"} }, `web.trusted_proxies: "proxy" is not a CIDR`},
		{"issuer not a url", func(c *Config) { c.OIDC.Issuer = "auth.example.org" }, "oidc.issuer must be an http(s) URL with a host"},
		{"scopes without openid", func(c *Config) { c.OIDC.Scopes = []string{"profile", "groups"} }, `oidc.scopes must include "openid"`},
		{"groups_claim empty", func(c *Config) { c.OIDC.GroupsClaim = " " }, "oidc.groups_claim is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mut(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

// web.templates is optional.
func TestTemplatesDefaultAndValid(t *testing.T) {
	if got := Default().Web.Templates; got != "" {
		t.Errorf("default web.templates = %q, want empty", got)
	}
	for _, p := range []string{"", "*.kilo.alpha", "dc.kilo.alpha"} {
		cfg := validConfig()
		cfg.Web.Templates = p
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() with web.templates %q = %v", p, err)
		}
	}
}

func TestRequireWeb(t *testing.T) {
	if err := webConfig().RequireWeb(); err != nil {
		t.Fatalf("RequireWeb: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"base_url", func(c *Config) { c.Web.BaseURL = "" }, "web.base_url is required (e.g. https://battleship.example.org)"},
		{"issuer", func(c *Config) { c.OIDC.Issuer = "" }, "oidc.issuer is required"},
		{"client_id", func(c *Config) { c.OIDC.ClientID = " " }, "oidc.client_id (or BATTLESHIP_OIDC_CLIENT_ID) is required"},
		{"client_secret", func(c *Config) { c.OIDC.ClientSecret = "" }, "oidc.client_secret (or BATTLESHIP_OIDC_CLIENT_SECRET) is required"},
		{"offline_access", func(c *Config) { c.OIDC.Scopes = []string{"openid", "profile", "email", "groups"} },
			`oidc.scopes must include "offline_access"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := webConfig()
			tc.mut(&cfg)
			if err := cfg.RequireWeb(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("RequireWeb() = %v, want %q", err, tc.want)
			}
		})
	}
	// Every missing setting is reported at once.
	err := validConfig().RequireWeb()
	for _, want := range []string{"web.base_url", "oidc.client_id", "oidc.client_secret"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("RequireWeb() = %v, want it to mention %s", err, want)
		}
	}
}

func TestOIDCSecretIsRedacted(t *testing.T) {
	cfg := webConfig()
	cfg.OIDC.ClientSecret = "hunter2"
	s := fmt.Sprintf("%v %+v %#v %v %s", cfg, cfg, cfg, &cfg, cfg.OIDC)
	if strings.Contains(s, "hunter2") || strings.Contains(s, ":pw@") {
		t.Fatalf("client secret leaked: %s", s)
	}
	if !strings.Contains(s, "ClientSecret:[redacted]") || !strings.Contains(s, "ClientID:battleship") {
		t.Errorf("redacted OIDC missing: %s", s)
	}
}
