package web

import (
	"context"
	"io"
	"net/http"
	"time"
)

// pingTimeout bounds the readiness check's database ping.
const pingTimeout = 2 * time.Second

// readiness is the last readiness answer: unknown until the first check.
type readiness int

const (
	readyUnknown readiness = iota
	readyYes
	readyNo
)

// healthz is the liveness probe: the process answers HTTP.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	plain(w, r, http.StatusOK, "ok")
}

// readyz is the readiness probe: 200 while the database answers, 503 if not
// or while draining. It skips Proxmox: battleship has no credential of its
// own, and all replicas share Proxmox, so failing them all would hide the job
// pages and cancel when most needed. The probe is unauthenticated, so the
// body names what failed but never how.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		plain(w, r, http.StatusServiceUnavailable, "shutting down")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pingTimeout)
	defer cancel()
	dbErr := s.st.Ping(ctx)

	s.readyMu.Lock()
	prev := s.ready
	if dbErr != nil {
		s.ready = readyNo
	} else {
		s.ready = readyYes
	}
	s.readyMu.Unlock()

	if dbErr != nil {
		if prev != readyNo {
			s.logf("web: not ready: database: %v", dbErr)
		}
		plain(w, r, http.StatusServiceUnavailable, "database: unreachable")
		return
	}
	switch prev {
	case readyNo:
		s.logf("web: ready again")
	case readyUnknown:
		s.logf("web: ready")
	}
	plain(w, r, http.StatusOK, "ready")
}

// plain writes a short text answer that is never cached.
func plain(w http.ResponseWriter, r *http.Request, status int, text string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, text+"\n")
	}
}
