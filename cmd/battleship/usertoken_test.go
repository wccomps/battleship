package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
)

const testSealKey = "a seal key for the cmd tests, long enough"

func TestCLIRefusesWithoutTheUsersOwnToken(t *testing.T) {
	for _, unset := range []string{"BATTLESHIP_PROXMOX_TOKEN_ID", "BATTLESHIP_PROXMOX_TOKEN_SECRET"} {
		t.Run(unset, func(t *testing.T) {
			e := newEnv(t, false, "", vm(10701, "team07-dc"))
			t.Setenv(unset, "")
			if code := e.run("teardown", "-teams", "7"); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			if !strings.Contains(e.stderr.String(), "BATTLESHIP_PROXMOX_TOKEN_ID") || !strings.Contains(e.stderr.String(), "your own") {
				t.Fatalf("stderr = %q", e.stderr.String())
			}
		})
	}
}

func TestQueuedJobCarriesTheUsersToken(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	st := withStore(t, e)
	if code := e.run("teardown", "-teams", "7", "-yes", "-queue"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, e.stderr.String())
	}
	list, _ := st.Jobs(context.Background(), 1)
	jc, err := st.JobCredential(context.Background(), list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Database.SealKey = testSealKey
	creds, _ := jobs.NewCredentials(cfg)
	cred, err := creds.Open(jc)
	if err != nil {
		t.Fatal(err)
	}
	if !cred.IsToken() || cred.User != "tester@auth.example.org!cli" || cred.TokenSecret != "secret" {
		t.Fatalf("the job carries %v", cred)
	}
}

func TestQueueNeedsASealKey(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	withStore(t, e)
	t.Setenv("BATTLESHIP_SEAL_KEY", "")
	if code := e.run("teardown", "-teams", "7", "-yes", "-queue"); code != 1 || !strings.Contains(e.stderr.String(), "seal_key") {
		t.Fatalf("exit %d, stderr %q", code, e.stderr.String())
	}
}

// The real client sends the user's token, and nothing else.
func TestCLICallsProxmoxWithTheUsersToken(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization")+"|"+r.Header.Get("Cookie"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"type":"node","name":"n1","ip":"10.1.1.1","online":1}]}`)
	}))
	t.Cleanup(srv.Close)
	e := newEnv(t, false, "")
	path := filepath.Join(t.TempDir(), "battleship.toml")
	if err := os.WriteFile(path, []byte("[proxmox]\nurl = \""+srv.URL+"\"\ninsecure_skip_verify = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.cfg = path
	e.d.newAPI = newProxmoxAPI
	if code := e.run("nodes"); code != 0 {
		t.Fatalf("exit %d: %s", code, e.stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) == 0 {
		t.Fatal("no request")
	}
	for _, a := range auths {
		if a != "PVEAPIToken=tester@auth.example.org!cli=secret|" {
			t.Fatalf("request carried %q", a)
		}
	}
}
