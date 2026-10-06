package web

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/auth"
)

// contentSecurityPolicy allows only this app's own scripts, styles and
// images: no inline script or style. Forms may post here and follow the
// logout redirect to the identity provider (issuerOrigin).
func contentSecurityPolicy(issuerOrigin string) string {
	return "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self' " + issuerOrigin
}

// secure sets the security headers on every response.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", s.csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Content-Type-Options", "nosniff")
		if isHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type peerKey struct{}

// peer is where a request came from, after forwarded headers from trusted
// proxies are applied.
type peer struct {
	ip    string
	https bool
}

// forwardedHeaders are the proxy headers forwarded reads and then removes,
// so no handler can trust them by mistake.
var forwardedHeaders = []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded", "X-Real-Ip"}

// forwarded works out each request's client address and scheme. Only a peer
// inside trusted may speak for the client: X-Forwarded-For is read from the
// right, skipping trusted proxies, and the first other address is the
// client; X-Forwarded-Proto's last value is the scheme the client used.
// Everyone else's forwarded headers are ignored. Handlers read the result
// with clientIP and isHTTPS.
func forwarded(trusted []netip.Prefix, next http.Handler) http.Handler {
	isTrusted := func(a netip.Addr) bool {
		a = a.Unmap()
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		p := peer{ip: host, https: r.TLS != nil}
		if addr, err := netip.ParseAddr(host); err == nil && isTrusted(addr) {
			p.ip = addr.Unmap().String()
			hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
			for i := len(hops) - 1; i >= 0; i-- {
				hop := strings.TrimSpace(hops[i])
				if hop == "" {
					continue
				}
				a, err := netip.ParseAddr(hop)
				if err != nil {
					break // can't vouch for anything further left
				}
				p.ip = a.Unmap().String()
				if !isTrusted(a) {
					break
				}
			}
			if protos := r.Header.Values("X-Forwarded-Proto"); len(protos) > 0 {
				all := strings.Split(protos[len(protos)-1], ",")
				p.https = strings.EqualFold(strings.TrimSpace(all[len(all)-1]), "https")
			}
		}
		for _, k := range forwardedHeaders {
			r.Header.Del(k)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKey{}, p)))
	})
}

// clientIP is the client's address as forwarded worked it out, or the
// connection's peer outside forwarded.
func clientIP(r *http.Request) string {
	if p, ok := r.Context().Value(peerKey{}).(peer); ok {
		return p.ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isHTTPS reports whether the client reached the app over HTTPS, directly or
// through a trusted proxy.
func isHTTPS(r *http.Request) bool {
	if p, ok := r.Context().Value(peerKey{}).(peer); ok {
		return p.https
	}
	return r.TLS != nil
}

// logRequests logs requests for looking back at what happened during an
// event. With an access log, every request but the health probes gets a
// record naming the user (auth.TrackSubject). Without one, Logf gets a
// line, with the client's address, for each request that could change
// something (anything but GET and HEAD) and each server error. Neither
// logs query strings or cookies.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		ctx, subject := auth.TrackSubject(r.Context())
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(ctx))
		code := sw.status
		if code == 0 {
			code = http.StatusOK
		}
		if probe[r.URL.Path] {
			return // readyz logs its own changes; probes come every few seconds
		}
		took := s.now().Sub(start)
		if s.access != nil {
			s.access.LogAttrs(ctx, slog.LevelInfo, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", code),
				slog.Duration("duration", took),
				slog.String("client", clientIP(r)),
				slog.String("user", subject()))
			return
		}
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || code >= 500 {
			s.logf("web: %s %q %d from %s in %s", r.Method, r.URL.Path, code, clientIP(r), took.Round(time.Millisecond))
		}
	})
}

// probe are the health check paths, which the request log skips.
var probe = map[string]bool{"/healthz": true, "/readyz": true}

// statusWriter records the status of a response. Unwrap lets
// http.ResponseController reach the connection, for flushing event streams.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
