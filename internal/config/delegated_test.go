package config

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDelegatedAuthDefaults(t *testing.T) {
	c := Default()
	if c.Proxmox.Realm != "auth.example.org" || c.Proxmox.TicketRenewAfter != time.Hour || c.Proxmox.TicketMaxAge != 12*time.Hour {
		t.Fatalf("defaults: realm=%q renew_after=%v max_age=%v", c.Proxmox.Realm, c.Proxmox.TicketRenewAfter, c.Proxmox.TicketMaxAge)
	}
}

func TestDelegatedAuthValidation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"blank realm", func(c *Config) { c.Proxmox.Realm = " " }, "proxmox.realm is required"},
		{"renew too soon", func(c *Config) { c.Proxmox.TicketRenewAfter = time.Minute }, "proxmox.ticket_renew_after must be between 5m and 1h40m"},
		{"renew too late", func(c *Config) { c.Proxmox.TicketRenewAfter = 2 * time.Hour }, "proxmox.ticket_renew_after must be between 5m and 1h40m"},
		{"max age below renewal", func(c *Config) { c.Proxmox.TicketMaxAge = 30 * time.Minute }, "proxmox.ticket_max_age must be at least proxmox.ticket_renew_after"},
		{"short seal key", func(c *Config) { c.Database.SealKey = "short" }, "database.seal_key must be at least 32 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.edit(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSealKeyFromEnvAndRedacted(t *testing.T) {
	path := writeFile(t, "[proxmox]\nurl = \"https://pve.example:8006\"\n")
	key := strings.Repeat("k", 40)
	t.Setenv("BATTLESHIP_SEAL_KEY", key)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.SealKey != key {
		t.Fatalf("SealKey = %q", cfg.Database.SealKey)
	}
	if strings.Contains(cfg.Database.String(), key) {
		t.Fatalf("Database.String shows the seal key: %s", cfg.Database.String())
	}
	if err := cfg.RequireSealKey(); err != nil {
		t.Fatal(err)
	}
	cfg.Database.SealKey = ""
	if err := cfg.RequireSealKey(); err == nil || !strings.Contains(err.Error(), "BATTLESHIP_SEAL_KEY") {
		t.Fatalf("RequireSealKey without a key = %v", err)
	}
}

func TestLoadRefusesRemovedKeys(t *testing.T) {
	path := writeFile(t, `
[proxmox]
url = "https://pve.example:8006"
token_id = "battleship@pve!app"
token_secret = "x"

[roles]
operators = ["a"]
leads = ["b"]
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("a config with the removed service token loaded")
	}
	for _, want := range []string{"proxmox.token_id", "proxmox.token_secret", "roles.operators", "roles.leads", "removed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't name %q", err, want)
		}
	}
}

func TestLoadWithoutAToken(t *testing.T) {
	path := writeFile(t, "[proxmox]\nurl = \"https://pve.example:8006\"\n")
	if _, err := Load(path); err != nil {
		t.Fatalf("a config without a token: %v", err)
	}
}

// The shipped deployment configs load: none still sets a removed key.
func TestDeployConfigsLoad(t *testing.T) {
	ca := writeCAFile(t)
	for _, p := range []string{"../../deploy/k8s/base/battleship.toml", "../../deploy/k8s/overlays/example/battleship.toml"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.ReplaceAll(string(b), `"/config/pve-root-ca.pem"`, strconv.Quote(ca))
		if _, err := Load(writeFile(t, body)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// Item 22: Proxmox's certificate is checked by default; serve and worker
// (RequireTLSVerify) need the cluster CA (ca_file), or an explicit
// insecure_skip_verify = true.
func TestTLSVerifiedByDefault(t *testing.T) {
	if Default().Proxmox.InsecureSkipVerify {
		t.Fatal("insecure_skip_verify defaults to true")
	}
	cfg, err := Load(writeFile(t, "[proxmox]\nurl = \"https://pve.example:8006\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxmox.InsecureSkipVerify {
		t.Fatal("a config without insecure_skip_verify skips verification")
	}
	if err := cfg.RequireTLSVerify(); err == nil || !strings.Contains(err.Error(), "ca_file") || !strings.Contains(err.Error(), "pve-root-ca.pem") {
		t.Fatalf("RequireTLSVerify without ca_file = %v", err)
	}
	skip, err := Load(writeFile(t, "[proxmox]\nurl = \"https://pve.example:8006\"\ninsecure_skip_verify = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := skip.RequireTLSVerify(); err != nil {
		t.Fatalf("with insecure_skip_verify = true: %v", err)
	}
	ca := writeCAFile(t)
	withCA, err := Load(writeFile(t, "[proxmox]\nurl = \"https://pve.example:8006\"\nca_file = "+strconv.Quote(ca)+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := withCA.RequireTLSVerify(); err != nil || withCA.Proxmox.RootCAs == nil {
		t.Fatalf("with ca_file: %v", err)
	}
}

// web.teams is gone: the teams are those with VMs. An old config that sets
// it says so, not just "unknown key".
func TestLoadRefusesWebTeams(t *testing.T) {
	path := writeFile(t, "[proxmox]\nurl = \"https://pve.example:8006\"\n\n[web]\nteams = \"1-32\"\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "these settings were removed: web.teams. Battleship shows the teams that have VMs; delete the line") {
		t.Fatalf("Load with web.teams = %v", err)
	}
	if strings.Contains(err.Error(), "unknown keys") || strings.Contains(err.Error(), "Proxmox token") {
		t.Errorf("error %q also calls web.teams unknown or a token setting", err)
	}
}
