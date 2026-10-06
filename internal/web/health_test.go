package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func TestHealthz(t *testing.T) {
	h := newHarness(t)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := h.do(nil, method, "/healthz", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s /healthz = %d, want 200", method, rec.Code)
		}
	}
	// Liveness doesn't depend on Proxmox or the grid.
	h.api.setListErr(&proxmox.APIError{Status: 503, Message: "down"})
	_ = h.poller.Poll(t.Context())
	if rec := h.get(nil, "/healthz"); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Errorf("/healthz with Proxmox down = %d %q", rec.Code, rec.Body)
	}
}

// Readiness depends on the database only, and never calls Proxmox:
// battleship has no credential of its own to call it with, and Proxmox is
// shared by every replica, so its outage must not take them all out of the
// load balancer.
func TestReadyz(t *testing.T) {
	h := newHarness(t)
	h.api.setListErr(&proxmox.APIError{Status: 503, Message: "proxy down at 10.0.0.5"})
	before := len(h.usedCreds())
	for range 3 {
		if rec := h.get(nil, "/readyz"); rec.Code != http.StatusOK || rec.Body.String() != "ready\n" {
			t.Errorf("/readyz = %d %q, want 200 ready", rec.Code, rec.Body)
		}
	}
	if n := len(h.usedCreds()) - before; n != 0 {
		t.Errorf("readiness made %d Proxmox APIs", n)
	}
	// One line when it first became ready, however often the probe asks.
	if n := strings.Count(h.logs.String(), "web: ready"); n != 1 {
		t.Errorf("logs = %q, want one ready line", h.logs)
	}
	if strings.Contains(h.logs.String(), "/readyz") {
		t.Errorf("logs = %q, want no request lines for the probe", h.logs)
	}
}

func TestReadyzChecksTheDatabase(t *testing.T) {
	h := newHarness(t)
	h.poll()

	// A second store on the same database, closed: every ping fails.
	_, dbURL := storetest.NewWithURL(t)
	closed, err := store.Open(context.Background(), dbURL, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	srv, err := New(Deps{
		Config: h.cfg, Store: closed, As: h.as, Credentials: h.creds, Auth: h.srv.auth, Views: h.views, Hub: h.hub,
		Clock: h.clock, Logf: h.logs.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.h = srv.Handler()
	rec := h.get(nil, "/readyz")
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "database: unreachable\n" {
		t.Errorf("/readyz with the database down = %d %q, want 503 naming the database", rec.Code, rec.Body)
	}
	// The body doesn't describe the database; the log does.
	if strings.Contains(rec.Body.String(), "closed") || strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Errorf("/readyz body = %q, want no connection details", rec.Body)
	}
	if !strings.Contains(h.logs.String(), "web: not ready: database: pinging database:") {
		t.Errorf("logs = %q", h.logs)
	}
}
