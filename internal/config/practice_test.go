package config

import (
	"fmt"
	"strings"
	"testing"
)

// practiceConfig is a valid config with the session interface on.
func practiceConfig() Config {
	cfg := validConfig()
	cfg.Practice = Practice{
		Pods:      "51-59",
		Principal: "practice@pve",
		Listen:    ":8081",
		Key:       strings.Repeat("k", MinPracticeKey),
	}
	return cfg
}

func TestPracticeOffUnlessSet(t *testing.T) {
	if Default().Practice.Enabled() {
		t.Error("the session interface is on by default")
	}
	cfg, err := Load(writeFile(t, "[proxmox]\nurl = \"https://pve.example:8006\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Practice.Enabled() {
		t.Errorf("a config without [practice] turns the interface on: %v", cfg.Practice)
	}
	if cfg, err = Load("../../battleship.example.toml"); err != nil {
		t.Fatal(err)
	}
	if cfg.Practice.Enabled() {
		t.Errorf("battleship.example.toml turns the interface on: %v", cfg.Practice)
	}
}

func TestPracticeLoadsWithKeyFromEnv(t *testing.T) {
	path := writeFile(t, `
[proxmox]
url = "https://pve.example:8006"

[practice]
pods = "51-59"
principal = "practice@pve"
listen = "127.0.0.1:8081"
`)
	key := strings.Repeat("e", 40)
	t.Setenv("BATTLESHIP_PRACTICE_KEY", key)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Practice.Enabled() || cfg.Practice.Key != key {
		t.Fatalf("Practice = %#v, key from the environment = %t", cfg.Practice, cfg.Practice.Key == key)
	}
}

func TestPracticeValidation(t *testing.T) {
	onlyPods := func(c *Config) { c.Practice = Practice{Pods: "51-59"} }
	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"only pods: principal", onlyPods, "practice.principal is required"},
		{"only pods: listen", onlyPods, "practice.listen is required"},
		{"only pods: key", onlyPods, "practice.key (or BATTLESHIP_PRACTICE_KEY) is required"},
		{"only key", func(c *Config) { c.Practice = Practice{Key: strings.Repeat("k", 40)} }, "practice.pods is required"},
		{"blank pods", func(c *Config) { c.Practice.Pods = " " }, "practice.pods is required"},
		{"pods not a range", func(c *Config) { c.Practice.Pods = "51" }, `practice.pods: "51" is not a range`},
		{"pods backwards", func(c *Config) { c.Practice.Pods = "59-51" }, `practice.pods: "59-51" is not a range`},
		{"pods past 99", func(c *Config) { c.Practice.Pods = "51-120" }, "practice.pods must lie within 0-99"},
		{"principal without realm", func(c *Config) { c.Practice.Principal = "practice" }, "practice.principal must be a Proxmox user as name@realm"},
		{"principal without name", func(c *Config) { c.Practice.Principal = "@pve" }, "practice.principal must be a Proxmox user as name@realm"},
		{"principal with empty realm", func(c *Config) { c.Practice.Principal = "practice@" }, "practice.principal must be a Proxmox user as name@realm"},
		{"principal with a space", func(c *Config) { c.Practice.Principal = "practice @pve" }, "practice.principal must be a Proxmox user as name@realm"},
		{"principal in the OpenID realm", func(c *Config) { c.Practice.Principal = "practice@" + strings.ToUpper(c.Proxmox.Realm) }, "practice.principal must not be in proxmox.realm"},
		{"listen without port", func(c *Config) { c.Practice.Listen = "8081" }, "practice.listen must be host:port or :port"},
		{"short key", func(c *Config) { c.Practice.Key = "short" }, "practice.key must be at least 32 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := practiceConfig()
			tc.edit(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
	if err := practiceConfig().Validate(); err != nil {
		t.Fatalf("a full [practice] is refused: %v", err)
	}
}

func TestPracticeKeyIsRedacted(t *testing.T) {
	cfg := practiceConfig()
	cfg.Practice.Key = "hunter2-hunter2-hunter2-hunter2-hunter2"
	s := fmt.Sprintf("%v %+v %#v %v %s", cfg, cfg, cfg, &cfg, cfg.Practice)
	if strings.Contains(s, "hunter2") {
		t.Errorf("practice.key not redacted: %s", s)
	}
	if !strings.Contains(s, "Key:[redacted]") {
		t.Errorf("redaction marker missing: %s", s)
	}
}
