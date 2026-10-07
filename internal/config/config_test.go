package config

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "battleship.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppliesDefaultsAndOverrides(t *testing.T) {
	path := writeFile(t, `
[proxmox]
url = "https://pve.example:8006"

[retry]
round_pause = "5s"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retry.RoundPause != 5*time.Second {
		t.Errorf("RoundPause = %v, want 5s", cfg.Retry.RoundPause)
	}
	if cfg.Naming.VMName != "team{team}-{host}" || cfg.Deploy.SnapshotName != "initial" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoadRejectsMissingFields(t *testing.T) {
	path := writeFile(t, `
[naming]
vm_name = "team-{host}"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded, want validation error")
	}
	for _, want := range []string{"proxmox.url", "naming.vm_name must contain {team}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../battleship.example.toml")
	if err != nil {
		t.Fatalf("battleship.example.toml: %v", err)
	}
	if cfg.Proxmox.URL == "" || cfg.Concurrency.Workers != 20 || cfg.Concurrency.TemplateBuilds != 4 {
		t.Errorf("unexpected config: %+v", cfg)
	}
	// With the secret from the environment, the example is ready for battleship serve.
	t.Setenv("BATTLESHIP_OIDC_CLIENT_SECRET", "secret")
	if cfg, err = Load("../../battleship.example.toml"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.RequireWeb(); err != nil {
		t.Errorf("battleship.example.toml: RequireWeb: %v", err)
	}
	wantWeb := Default().Web
	wantWeb.BaseURL = "https://battleship.example.org"
	if !reflect.DeepEqual(cfg.Web, wantWeb) {
		t.Errorf("example [web] differs from the defaults it documents: %+v", cfg.Web)
	}
}

func TestTeardownAndDeleteDefaults(t *testing.T) {
	d := Default()
	if d.Concurrency.Deletes != 4 || d.Teardown.ShutdownTimeout != 60*time.Second {
		t.Errorf("deletes = %d, teardown.shutdown_timeout = %v; want 4 and 60s", d.Concurrency.Deletes, d.Teardown.ShutdownTimeout)
	}
	path := writeFile(t, `
[proxmox]
url = "https://pve.example:8006"

[concurrency]
deletes = 2

[teardown]
shutdown_timeout = "90s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.Deletes != 2 || cfg.Teardown.ShutdownTimeout != 90*time.Second {
		t.Errorf("deletes = %d, teardown.shutdown_timeout = %v; want 2 and 90s", cfg.Concurrency.Deletes, cfg.Teardown.ShutdownTimeout)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := writeFile(t, `
[proxmox]
url = "https://pve.example:8006"

[concurrency]
clones_per_nodes = 5
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded, want error for unknown keys")
	}
	if !strings.Contains(err.Error(), "concurrency.clones_per_nodes") {
		t.Errorf("error %q missing 'concurrency.clones_per_nodes'", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/battleship.toml")
	if err == nil {
		t.Fatal("Load succeeded, want error for missing file")
	}
	if !strings.Contains(err.Error(), "/nonexistent/battleship.toml") {
		t.Errorf("error %q missing path", err)
	}
}

func TestLoadMalformedTOML(t *testing.T) {
	path := writeFile(t, `
[proxmox]
url =
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded, want error for malformed TOML")
	}
}

func TestValidateRanges(t *testing.T) {
	baseURL := "https://pve:8006"

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "valid config",
			mutate:  func(c *Config) {},
			wantErr: "",
		},
		{
			name: "url not a url",
			mutate: func(c *Config) {
				c.Proxmox.URL = "not a url"
			},
			wantErr: "proxmox.url must be an http(s) URL with a host",
		},
		{
			name: "url with the API path",
			mutate: func(c *Config) {
				c.Proxmox.URL = "https://192.0.2.123:8006/api2/json"
			},
			wantErr: "proxmox.url must be an http(s) URL with a host and nothing after it",
		},
		{
			name: "urls with the API path",
			mutate: func(c *Config) {
				c.Proxmox.URL, c.Proxmox.URLs = "", []string{"https://192.0.2.121:8006", "https://192.0.2.123:8006/api2/json"}
			},
			wantErr: "proxmox.urls[1] must be an http(s) URL with a host and nothing after it",
		},
		{
			name: "workers zero",
			mutate: func(c *Config) {
				c.Concurrency.Workers = 0
			},
			wantErr: "concurrency.workers must be at least 1",
		},
		{
			name: "clones_per_node zero",
			mutate: func(c *Config) {
				c.Concurrency.ClonesPerNode = 0
			},
			wantErr: "concurrency.clones_per_node must be at least 1",
		},
		{
			name: "config_calls zero",
			mutate: func(c *Config) {
				c.Concurrency.ConfigCalls = 0
			},
			wantErr: "concurrency.config_calls must be at least 1",
		},
		{
			name: "template_builds zero",
			mutate: func(c *Config) {
				c.Concurrency.TemplateBuilds = 0
			},
			wantErr: "concurrency.template_builds must be at least 1",
		},
		{
			name: "deletes zero",
			mutate: func(c *Config) {
				c.Concurrency.Deletes = 0
			},
			wantErr: "concurrency.deletes must be at least 1",
		},
		{
			name: "teardown shutdown_timeout too short",
			mutate: func(c *Config) {
				c.Teardown.ShutdownTimeout = 500 * time.Millisecond
			},
			wantErr: "teardown.shutdown_timeout must be at least 1s",
		},
		{
			name: "clone_vmid_team_stride zero",
			mutate: func(c *Config) {
				c.Naming.CloneVMIDTeamStride = 0
			},
			wantErr: "naming.clone_vmid_team_stride must be at least 100 (clone VMIDs use template VMID % 100)",
		},
		{
			name: "clone_vmid_team_stride too small",
			mutate: func(c *Config) {
				c.Naming.CloneVMIDTeamStride = 99
			},
			wantErr: "naming.clone_vmid_team_stride must be at least 100 (clone VMIDs use template VMID % 100)",
		},
		{
			name: "storage empty",
			mutate: func(c *Config) {
				c.Deploy.Storage = ""
			},
			wantErr: "deploy.storage is required",
		},
		{
			name: "snapshot_name empty",
			mutate: func(c *Config) {
				c.Deploy.SnapshotName = ""
			},
			wantErr: "deploy.snapshot_name is required",
		},
		{
			name: "disk_mbps_rd zero",
			mutate: func(c *Config) {
				c.Deploy.DiskMBpsRead = 0
			},
			wantErr: "deploy.disk_mbps_rd must be at least 1",
		},
		{
			name: "disk_mbps_wr zero",
			mutate: func(c *Config) {
				c.Deploy.DiskMBpsWrite = 0
			},
			wantErr: "deploy.disk_mbps_wr must be at least 1",
		},
		{
			name: "task_poll too small",
			mutate: func(c *Config) {
				c.Retry.TaskPoll = 5 * time.Nanosecond
			},
			wantErr: "retry.task_poll must be at least 100ms",
		},
		{
			name: "initial_backoff too small",
			mutate: func(c *Config) {
				c.Retry.InitialBackoff = 5 * time.Millisecond
			},
			wantErr: "retry.initial_backoff must be at least 100ms",
		},
		{
			name: "max_backoff below initial_backoff",
			mutate: func(c *Config) {
				c.Retry.InitialBackoff = 200 * time.Millisecond
				c.Retry.MaxBackoff = 100 * time.Millisecond
			},
			wantErr: "retry.max_backoff must be at least retry.initial_backoff",
		},
		{
			name: "rounds negative",
			mutate: func(c *Config) {
				c.Retry.Rounds = -1
			},
			wantErr: "retry.rounds must not be negative",
		},
		{
			name: "round_pause negative",
			mutate: func(c *Config) {
				c.Retry.RoundPause = -1 * time.Second
			},
			wantErr: "retry.round_pause must not be negative",
		},
		{
			name: "template_suffix empty",
			mutate: func(c *Config) {
				c.Naming.TemplateSuffix = ""
			},
			wantErr: "naming.template_suffix is required",
		},
		{
			name: "master_tag empty",
			mutate: func(c *Config) {
				c.Deploy.MasterTag = ""
			},
			wantErr: "deploy.master_tag is required",
		},
		{
			name: "master_tag only whitespace",
			mutate: func(c *Config) {
				c.Deploy.MasterTag = "   "
			},
			wantErr: "deploy.master_tag is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Proxmox.URL = baseURL
			tt.mutate(&cfg)

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Errorf("Validate: nil, want error containing %q", tt.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q missing %q", err, tt.wantErr)
			}
		})
	}
}

func TestSecretIsRedacted(t *testing.T) {
	cfg := Config{
		Proxmox:  Proxmox{URL: "https://pve:8006"},
		Database: Database{URL: "postgres://u:pw@db/x", SealKey: "hunter2-hunter2-hunter2-hunter2-hunter2"},
	}

	s := fmt.Sprintf("%v %+v %#v %v", cfg, cfg, cfg, &cfg)
	if strings.Contains(s, "hunter2") || strings.Contains(s, ":pw@") {
		t.Errorf("secret not redacted: %s", s)
	}
	if !strings.Contains(s, "[redacted]") {
		t.Errorf("redaction marker missing: %s", s)
	}
}

func TestTransientPatternsReplaceDefaults(t *testing.T) {
	path := writeFile(t, `
[proxmox]
url = "https://pve.example:8006"

[retry]
transient_patterns = ["only"]
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Retry.TransientPatterns) != 1 || cfg.Retry.TransientPatterns[0] != "only" {
		t.Errorf("TransientPatterns = %v, want [\"only\"]", cfg.Retry.TransientPatterns)
	}
}

func TestJobsDefaultsAndDatabaseEnv(t *testing.T) {
	path := writeFile(t, `
[proxmox]
url = "https://pve.example:8006"
`)
	t.Setenv("BATTLESHIP_DATABASE_URL", "postgres://battleship:pw@db:5432/battleship")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.URL != "postgres://battleship:pw@db:5432/battleship" {
		t.Errorf("Database.URL = %q, want env value", cfg.Database.URL)
	}
	if cfg.Jobs.Heartbeat != 10*time.Second || cfg.Jobs.StaleAfter != 60*time.Second || cfg.Jobs.Poll != 2*time.Second {
		t.Errorf("Jobs = %+v, want 10s/60s/2s defaults", cfg.Jobs)
	}
	if err := cfg.RequireDatabase(); err != nil {
		t.Errorf("RequireDatabase: %v", err)
	}
}

func TestDatabasePasswordIsRedacted(t *testing.T) {
	cfg := Default()
	cfg.Database.URL = "postgres://battleship:hunter2@db:5432/battleship?sslmode=disable"
	s := fmt.Sprintf("%v %+v %#v %v", cfg, cfg, cfg, &cfg)
	if strings.Contains(s, "hunter2") || strings.Contains(s, ":pw@") {
		t.Fatalf("password leaked: %s", s)
	}
	if !strings.Contains(s, "battleship:redacted@db:5432") {
		t.Errorf("redacted URL missing: %s", s)
	}
}

func TestJobsValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"heartbeat too short", func(c *Config) { c.Jobs.Heartbeat = 500 * time.Millisecond }, "jobs.heartbeat must be at least 1s"},
		{"stale too soon", func(c *Config) { c.Jobs.StaleAfter = 45 * time.Second }, "jobs.stale_after must be at least 5 times jobs.heartbeat"},
		{"poll too fast", func(c *Config) { c.Jobs.Poll = time.Millisecond }, "jobs.poll must be at least 100ms"},
		{"cancel grace negative", func(c *Config) { c.Jobs.CancelGrace = -time.Second }, "jobs.cancel_grace must be between 0s and 5m"},
		{"cancel grace too long", func(c *Config) { c.Jobs.CancelGrace = 10 * time.Minute }, "jobs.cancel_grace must be between 0s and 5m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Proxmox.URL, cfg.Proxmox.InsecureSkipVerify = "https://pve:8006", false
			tc.mut(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRequireDatabase(t *testing.T) {
	for url, want := range map[string]string{
		"":                                  "database.url (or BATTLESHIP_DATABASE_URL) is required",
		"   ":                               "database.url (or BATTLESHIP_DATABASE_URL) is required",
		"mysql://db/battleship":             "must be a postgres:// URL",
		"postgres:///battleship":            "must be a postgres:// URL",
		"postgresql://u@db:5432/battleship": "",
	} {
		cfg := Default()
		cfg.Database.URL = url
		err := cfg.RequireDatabase()
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("RequireDatabase(%q) = %v, want %q", url, err, want)
		}
	}
}

// writeCAFile writes a PEM bundle holding a test server's certificate.
func writeCAFile(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	srv.Close()
	path := filepath.Join(t.TempDir(), "pve-ca.pem")
	body := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const proxmoxTOML = `
[proxmox]
url = "https://pve.example:8006"
`

func TestLoadProxmoxCAFile(t *testing.T) {
	ca := writeCAFile(t)

	// ca_file alone turns verification on, with the bundle as the roots.
	cfg, err := Load(writeFile(t, proxmoxTOML+"ca_file = \""+ca+"\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxmox.CAFile != ca || cfg.Proxmox.InsecureSkipVerify || cfg.Proxmox.RootCAs == nil {
		t.Errorf("proxmox = %v (RootCAs %v), want the CA file, verification on and the pool loaded", cfg.Proxmox, cfg.Proxmox.RootCAs)
	}
	if _, err := Load(writeFile(t, proxmoxTOML+"ca_file = \""+ca+"\"\ninsecure_skip_verify = false\n")); err != nil {
		t.Errorf("ca_file with insecure_skip_verify = false: %v", err)
	}
	// Without it, verification uses system CAs; serve and worker refuse that.
	cfg, err = Load(writeFile(t, proxmoxTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxmox.InsecureSkipVerify || cfg.Proxmox.RootCAs != nil {
		t.Errorf("proxmox = %v, want verification on and no pool", cfg.Proxmox)
	}

	notPEM := filepath.Join(t.TempDir(), "not.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, toml, want string
	}{
		{"both set", "ca_file = \"" + ca + "\"\ninsecure_skip_verify = true\n",
			"proxmox.ca_file and proxmox.insecure_skip_verify = true can't both be set"},
		{"missing file", "ca_file = \"" + filepath.Join(t.TempDir(), "nope.pem") + "\"\n", "proxmox.ca_file: open "},
		{"no certificates", "ca_file = \"" + notPEM + "\"\n", "proxmox.ca_file: " + notPEM + " holds no PEM certificates"},
	} {
		_, err := Load(writeFile(t, proxmoxTOML+c.toml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Load = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestDatabaseMaxConns(t *testing.T) {
	if got := Default().Database.MaxConns; got != 16 {
		t.Errorf("default database.max_conns = %d, want 16", got)
	}
	path := writeFile(t, `
[proxmox]
url = "https://pve.example:8006"

[database]
max_conns = 24
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.MaxConns != 24 {
		t.Errorf("database.max_conns = %d, want 24 from the file", cfg.Database.MaxConns)
	}
	for n, ok := range map[int]bool{4: true, 16: true, 3: false, 0: false, -1: false} {
		cfg := Default()
		cfg.Proxmox.URL, cfg.Proxmox.InsecureSkipVerify = "https://pve:8006", false
		cfg.Database.MaxConns = n
		err := cfg.Validate()
		if ok && err != nil || !ok && (err == nil || !strings.Contains(err.Error(), "database.max_conns must be at least 4")) {
			t.Errorf("Validate with max_conns %d = %v", n, err)
		}
	}
	cfg.Database.URL = "postgres://battleship:hunter2@db:5432/battleship"
	if s := fmt.Sprint(cfg.Database); !strings.Contains(s, "MaxConns:24") || strings.Contains(s, "hunter2") {
		t.Errorf("Database prints as %s", s)
	}
}

func TestBaselinePatterns(t *testing.T) {
	if got := Default().Deploy.BaselinePatterns; !reflect.DeepEqual(got, []string{"fresh_clone_*"}) {
		t.Errorf("default baseline_patterns = %v, want [fresh_clone_*]", got)
	}
	load := func(patterns string) (Config, error) {
		t.Helper()
		path := writeFile(t, `
[proxmox]
url = "https://pve.example:8006"

[deploy]
baseline_patterns = `+patterns+"\n")
		return Load(path)
	}
	cfg, err := load(`[]`)
	if err != nil || len(cfg.Deploy.BaselinePatterns) != 0 {
		t.Errorf("empty list: %v, %v; want no patterns and no error", cfg.Deploy.BaselinePatterns, err)
	}
	cfg, err = load(`["golden-?", "base_[0-9]*"]`)
	if err != nil || !reflect.DeepEqual(cfg.Deploy.BaselinePatterns, []string{"golden-?", "base_[0-9]*"}) {
		t.Errorf("custom list: %v, %v", cfg.Deploy.BaselinePatterns, err)
	}
	for _, bad := range []string{`["fresh_[clone"]`, `[""]`, `["  "]`} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), "deploy.baseline_patterns") {
			t.Errorf("%s: err = %v, want a deploy.baseline_patterns error", bad, err)
		}
	}
}
