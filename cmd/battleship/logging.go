package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
)

// newLogger returns battleship serve's structured logger, writing format ("text"
// or "json") to w.
func newLogger(w io.Writer, format string) (*slog.Logger, error) {
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, nil)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, nil)), nil
	}
	return nil, fmt.Errorf("-log-format must be text or json, not %q", format)
}

// routine lists, per component, the line prefixes of normal use. Other auth,
// web and proxmox lines are refusals or outages and log at Warn; other
// components log at Info.
var routine = map[string][]string{
	"auth": {"login: ", "logout: ", "session expired: ", "proxmox login: "},
	"web":  {"ready", "job ", "cancel requested: "},
	// Every proxmox line is a failover away from a node that failed.
	"proxmox": {},
}

// logfFor adapts l to the packages' printf-style Logf, moving their
// "<component>: " prefix into an attribute. The packages never pass secrets.
func logfFor(l *slog.Logger, component string) func(format string, args ...any) {
	l = l.With("component", component)
	prefix := component + ": "
	normal, graded := routine[component]
	return func(format string, args ...any) {
		msg := strings.TrimPrefix(fmt.Sprintf(format, args...), prefix)
		level := slog.LevelInfo
		if graded && !slices.ContainsFunc(normal, func(p string) bool { return strings.HasPrefix(msg, p) }) {
			level = slog.LevelWarn
		}
		l.Log(context.Background(), level, msg)
	}
}
