package config

import (
	"strings"
	"testing"
)

func TestLoadReadsProxmoxURLs(t *testing.T) {
	path := writeFile(t, `
[proxmox]
urls = ["https://192.0.2.123:8006", "https://192.0.2.124:8006/"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Proxmox.Endpoints()
	if len(got) != 2 || got[0] != "https://192.0.2.123:8006" || got[1] != "https://192.0.2.124:8006/" {
		t.Errorf("Endpoints = %q", got)
	}
	if s := cfg.Proxmox.String(); !strings.Contains(s, "192.0.2.124") {
		t.Errorf("String = %s, want the URLs", s)
	}
}

func TestSingleURLIsOneEndpoint(t *testing.T) {
	p := Proxmox{URL: "https://pve:8006"}
	if got := p.Endpoints(); len(got) != 1 || got[0] != "https://pve:8006" {
		t.Errorf("Endpoints = %q", got)
	}
}

func TestValidateProxmoxURLs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		url     string
		urls    []string
		wantErr string
	}{
		{"urls only", "", []string{"https://a:8006", "http://b:8006"}, ""},
		{"both", "https://a:8006", []string{"https://b:8006"}, "proxmox.url and proxmox.urls can't both be set"},
		{"neither", "", nil, "proxmox.url or proxmox.urls is required"},
		{"empty list", "", []string{}, "proxmox.url or proxmox.urls is required"},
		{"bad scheme", "", []string{"https://a:8006", "ftp://b"}, "proxmox.urls[1] must be an http(s) URL with a host"},
		{"no host", "", []string{"https://"}, "proxmox.urls[0] must be an http(s) URL with a host"},
		{"duplicate", "", []string{"https://a:8006", "https://a:8006/"}, "proxmox.urls[1] repeats"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Proxmox.URL, cfg.Proxmox.URLs = tc.url, tc.urls
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate: %v, want %q", err, tc.wantErr)
			}
		})
	}
}
