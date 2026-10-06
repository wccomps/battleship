package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Item 9: the worker, like serve, holds no Proxmox token: one in its
// environment is the old service token, or a user's own, which a worker
// must never act as.
func TestWorkerRefusesAProxmoxToken(t *testing.T) {
	for _, k := range []string{"BATTLESHIP_PROXMOX_TOKEN_ID", "BATTLESHIP_PROXMOX_TOKEN_SECRET"} {
		t.Run(k, func(t *testing.T) {
			e := newEnv(t, false, "")
			withStore(t, e)
			if k == "BATTLESHIP_PROXMOX_TOKEN_ID" {
				t.Setenv("BATTLESHIP_PROXMOX_TOKEN_SECRET", "")
			} else {
				t.Setenv("BATTLESHIP_PROXMOX_TOKEN_ID", "")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // a worker that starts runs until stopped
			defer cancel()
			if code := run(ctx, []string{"worker", "-config", e.cfg}, e.d); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			if !strings.Contains(e.stderr.String(), k+" is set") {
				t.Fatalf("stderr = %q", e.stderr.String())
			}
		})
	}
}

// Item 11: a process whose seal key isn't the one the database's jobs were
// sealed with refuses to start.
func TestWorkerRefusesAnotherSealKey(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	withStore(t, e)
	if code := e.run("teardown", "-teams", "7", "-yes", "-queue"); code != 0 { // first use: stores the key check
		t.Fatalf("queue: exit %d: %s", code, e.stderr.String())
	}
	t.Setenv("BATTLESHIP_PROXMOX_TOKEN_ID", "")
	t.Setenv("BATTLESHIP_PROXMOX_TOKEN_SECRET", "")
	t.Setenv("BATTLESHIP_SEAL_KEY", "a different seal key for the cmd tests, long")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if code := run(ctx, []string{"worker", "-config", e.cfg}, e.d); code != 1 || !strings.Contains(e.stderr.String(), "database.seal_key") {
		t.Fatalf("exit %d, stderr %q", code, e.stderr.String())
	}
}

// Item 22: the worker, like serve, refuses to send people's credentials to
// a Proxmox whose certificate it doesn't check.
func TestWorkerRefusesUncheckedProxmox(t *testing.T) {
	e := newEnv(t, false, "")
	withStore(t, e)
	t.Setenv("BATTLESHIP_PROXMOX_TOKEN_ID", "")
	t.Setenv("BATTLESHIP_PROXMOX_TOKEN_SECRET", "")
	path := filepath.Join(t.TempDir(), "battleship.toml")
	if err := os.WriteFile(path, []byte("[proxmox]\nurl = \"https://pve.example:8006\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if code := run(ctx, []string{"worker", "-config", path}, e.d); code != 1 || !strings.Contains(e.stderr.String(), "proxmox.ca_file is required") {
		t.Fatalf("exit %d, stderr %q", code, e.stderr.String())
	}
}

// Item 22: serve warns when told not to check Proxmox's certificate.
func TestServeWarnsWhenProxmoxIsUnchecked(t *testing.T) {
	e := startServe(t)
	if recs := e.records("Proxmox's certificate is not checked"); len(recs) != 1 || recs[0]["level"] != "WARN" {
		t.Fatalf("records = %v", recs)
	}
}
