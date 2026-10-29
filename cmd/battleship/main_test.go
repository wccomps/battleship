package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

const testSealKey = "a seal key for the cmd tests, long enough"

// fakeAPI is the shared fake cluster on n1, each VM with one disk, with
// deletes that fail as tests set. Its fields are guarded by the fake's Mu.
type fakeAPI struct {
	*podstest.Fake
	deleteErr map[int]error // fails every DeleteVM of these VMIDs
}

func newFakeAPI(vms ...proxmox.VM) *fakeAPI {
	f := &fakeAPI{Fake: podstest.New("n1"), deleteErr: map[int]error{}}
	for _, vm := range vms {
		f.Add(vm, map[string]string{"scsi0": fmt.Sprintf("competitions:%d/vm-%d-disk-0.qcow2", vm.VMID, vm.VMID)})
	}
	f.Gate = f.hold
	return f
}

// hold is the fake's Gate: it fails deletes in deleteErr.
func (f *fakeAPI) hold(_ context.Context, key string) (func(), error) {
	id, ok := strings.CutPrefix(key, "delete:")
	if !ok {
		return nil, nil
	}
	vmid, _ := strconv.Atoi(id)
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return nil, f.deleteErr[vmid]
}

// isWorkerID reports whether id is a jobs.NewWorkerID(prefix) from this
// process.
func isWorkerID(id, prefix string) bool {
	host, _ := os.Hostname()
	rest, ok := strings.CutPrefix(id, fmt.Sprintf("%s-%s-%d-", prefix, host, os.Getpid()))
	return ok && regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(rest)
}

// syncBuffer is safe to read while a command writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds, or fails the test after 15s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// proxmox.ca_file reaches the client: it verifies Proxmox's certificate
// against the bundle.
func TestProxmoxCAFileVerifiesTheCluster(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "pve-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "battleship.toml")
	toml := "[proxmox]\nurl = \"" + srv.URL + "\"\nca_file = \"" + ca + "\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newCluster(cfg.Proxmox, nil).Bind(func() proxmox.Credential { return proxmox.TicketCredential("u@pve", "PVE:u", "csrf", time.Now()) }, nil).ClusterVMs(context.Background()); err != nil {
		t.Errorf("with proxmox.ca_file: %v", err)
	}
}
