package web

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wccomps/battleship/internal/config"
)

// lockedBuffer is a bytes.Buffer safe for a logger and a test to share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// withAccessLog rebuilds h's app with an access log in JSON, and returns the
// records it writes.
func withAccessLog(t *testing.T, h *harness) func() []map[string]any {
	t.Helper()
	buf := &lockedBuffer{}
	srv, err := New(Deps{
		Config: h.cfg, Store: h.st, As: h.as, Credentials: h.creds, Auth: h.srv.auth, Views: h.views, Hub: h.hub,
		Clock: h.clock, Logf: h.logs.Logf,
		AccessLog: slog.New(slog.NewJSONHandler(buf, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.h = srv, srv.Handler()
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("access log line %q: %v", line, err)
			}
			out = append(out, rec)
		}
		return out
	}
}

func TestAccessLog(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Web.TrustedProxies = []string{"192.0.2.0/24"} })
	h.poll()
	records := withAccessLog(t, h)
	op := h.login(asOperator)

	h.get(&op, "/logs?before=12")
	h.get(nil, "/")
	h.get(nil, "/healthz")
	h.get(nil, "/readyz")
	h.get(nil, "/auth/callback?code=the-secret-code&state=the-state")
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", "198.51.100.23")
	req.AddCookie(op.Cookie)
	h.h.ServeHTTP(httptest.NewRecorder(), req)

	got := records()
	want := []struct {
		method, path string
		status       float64
		user, client string
	}{
		{"GET", "/logs", 200, "test-operator", "192.0.2.1"},
		{"GET", "/", 303, "", "192.0.2.1"},
		{"GET", "/auth/callback", 400, "", "192.0.2.1"},
		{"POST", "/auth/logout", 403, "test-operator", "198.51.100.23"},
	}
	if len(got) != len(want) {
		t.Fatalf("access log has %d records, want %d (no probes):\n%v", len(got), len(want), got)
	}
	for i, w := range want {
		r := got[i]
		if r["msg"] != "request" || r["method"] != w.method || r["path"] != w.path || r["status"] != w.status ||
			r["user"] != w.user || r["client"] != w.client {
			t.Errorf("record %d = %v, want %s %s %v user %q from %s", i, r, w.method, w.path, w.status, w.user, w.client)
		}
		if _, ok := r["duration"]; !ok {
			t.Errorf("record %d has no duration: %v", i, r)
		}
	}
	all := fmtRecords(got)
	for name, secret := range map[string]string{
		"callback code": "the-secret-code", "callback state": "the-state", "query": "before=12",
		"session cookie": op.Cookie.Value, "csrf token": op.CSRF,
	} {
		if strings.Contains(all, secret) {
			t.Errorf("access log contains the %s:\n%s", name, all)
		}
	}
	// The access log replaces the request lines in Logf.
	if strings.Contains(h.logs.String(), `web: POST "/auth/logout"`) {
		t.Errorf("logs = %q, want the request only in the access log", h.logs)
	}
}

func fmtRecords(recs []map[string]any) string {
	b, _ := json.Marshal(recs)
	return string(b)
}

func TestDrain(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	if rec := h.get(nil, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("/readyz before draining = %d", rec.Code)
	}
	h.srv.Drain()
	h.srv.Drain() // idempotent
	if rec := h.get(nil, "/readyz"); rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != "shutting down" {
		t.Errorf("/readyz while draining = %d %q, want 503 shutting down", rec.Code, rec.Body)
	}
	// Everything else keeps working until the server stops.
	if rec := h.get(nil, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("/healthz while draining = %d, want 200", rec.Code)
	}
	if rec := h.get(&op, "/"); rec.Code != http.StatusOK {
		t.Errorf("grid while draining = %d, want 200", rec.Code)
	}
}
