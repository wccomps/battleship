package main

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// Refusals and outages from auth and web log at Warn, so they stand out
// from routine logins and submits; the component prefix is dropped.
func TestLogfForLevels(t *testing.T) {
	for _, c := range []struct {
		component, line, level string
	}{
		{"auth", "auth: login: subject=%q", "INFO"},
		{"auth", "auth: logout: subject=%q", "INFO"},
		{"auth", "auth: session expired: subject=%q", "INFO"},
		{"auth", "auth: proxmox login: subject=%q user=\"jdoe@auth.example.org\" node=192.0.2.123:8006", "INFO"},
		{"auth", "auth: proxmox login failed: subject=%q", "WARN"},
		{"auth", "auth: proxmox login not started: subject=%q: the session has no username", "WARN"},
		{"auth", "auth: proxmox ticket refused at renewal: subject=%q", "WARN"},
		{"auth", "auth: csrf rejected: subject=%q POST", "WARN"},
		{"auth", "auth: login failed: no login cookie %q", "WARN"},
		{"auth", "auth: refresh rejected: subject=%q", "WARN"},
		{"auth", "auth: refresh failed: subject=%q", "WARN"},
		{"auth", "auth: reading session: %q", "WARN"},
		{"web", "web: ready%.0q", "INFO"},
		{"web", "web: ready again%.0q", "INFO"},
		{"web", "web: job 7 submitted: subject=%q", "INFO"},
		{"web", "web: cancel requested: job=7 subject=%q", "INFO"},
		{"web", "web: confirm refused: subject=%q", "WARN"},
		{"web", "web: not ready: database: %q", "WARN"},
		{"web", "web: storing a preview: %q", "WARN"},
		{"web", "web: listing jobs for the event stream: %q", "WARN"},
		{"status", "status: poll failed: %q", "INFO"}, // the status package keeps its levels
		{"proxmox", "proxmox: failing over from 192.0.2.123:8006 to 192.0.2.124:8006: %q", "WARN"},
	} {
		var buf strings.Builder
		logfFor(slog.New(slog.NewJSONHandler(&buf, nil)), c.component)(c.line, "x")
		var rec map[string]any
		if err := json.Unmarshal([]byte(buf.String()), &rec); err != nil {
			t.Fatalf("%s: log line isn't JSON: %q", c.line, buf.String())
		}
		if rec["level"] != c.level || rec["component"] != c.component || strings.HasPrefix(rec["msg"].(string), c.component+": ") {
			t.Errorf("%s: logged %v, want level %s, component %s and no prefix", c.line, rec, c.level, c.component)
		}
	}
}
