package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/config"
)

func TestAuthtestLogin(t *testing.T) {
	h := newHarness(t)
	serve := func(s authtest.Session, method, target string, withCSRF bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		if withCSRF {
			s.Apply(req)
		} else {
			req.AddCookie(s.Cookie)
		}
		rec := httptest.NewRecorder()
		h.mux.Load().ServeHTTP(rec, req)
		return rec
	}

	lead := authtest.Login(t, h.st, h.cfg, authtest.User{Name: "Lena", Groups: []string{"leads"}, At: h.clock.Now()})
	if rec := serve(lead, http.MethodGet, "/private", true); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "user=Lena groups=leads pve=test-user@auth.example.org") {
		t.Errorf("/private = %d %s", rec.Code, rec.Body)
	}
	if rec := serve(lead, http.MethodPost, "/private", true); rec.Code != http.StatusOK {
		t.Errorf("POST with Apply = %d %s", rec.Code, rec.Body)
	}
	if rec := serve(lead, http.MethodPost, "/private", false); rec.Code != http.StatusForbidden {
		t.Errorf("POST without the CSRF token = %d, want 403", rec.Code)
	}
	if lead.Cookie.Name != auth.SessionCookie || lead.CSRF == "" {
		t.Errorf("session = %+v", lead)
	}
	sess, err := h.st.Session(ctx, auth.HashToken(lead.Cookie.Value))
	if err != nil || sess.CSRF != lead.CSRF {
		t.Fatalf("stored session = %+v, %v", sess, err)
	}

	// Without a Proxmox ticket, the session goes through the Proxmox sign-in.
	bare := authtest.Login(t, h.st, h.cfg, authtest.User{NoProxmox: true, At: h.clock.Now()})
	if rec := serve(bare, http.MethodGet, "/private", true); rec.Code != http.StatusSeeOther ||
		!strings.HasPrefix(rec.Header().Get("Location"), "/auth/proxmox?") {
		t.Errorf("without a ticket: %d to %q", rec.Code, rec.Header().Get("Location"))
	}

	// Sessions last session_refresh without contacting the provider.
	h.clock.Advance(5*time.Minute - time.Second)
	if rec := serve(lead, http.MethodGet, "/private", true); rec.Code != http.StatusOK {
		t.Errorf("/private just before refresh = %d", rec.Code)
	}
}

func TestAuthtestDefaultsToNow(t *testing.T) {
	h := newHarness(t)
	s := authtest.Login(t, h.st, h.cfg, authtest.User{})
	sess, err := h.st.Session(ctx, auth.HashToken(s.Cookie.Value))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(sess.CreatedAt); d < 0 || d > time.Minute {
		t.Errorf("CreatedAt = %v, want about now", sess.CreatedAt)
	}
}

// With an https base URL, Login's cookie has the name RequireUser reads.
func TestAuthtestLoginHTTPS(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Web.BaseURL = "https://battleship.example.org" })
	s := authtest.Login(t, h.st, h.cfg, authtest.User{At: h.clock.Now()})
	if s.Cookie.Name != "__Host-battleship_session" || !s.Cookie.Secure || s.Cookie.Path != "/" {
		t.Errorf("cookie = %+v, want a Secure __Host-battleship_session with Path /", s.Cookie)
	}
	req := httptest.NewRequest(http.MethodGet, "/private", nil)
	s.Apply(req)
	rec := httptest.NewRecorder()
	h.mux.Load().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/private = %d %s", rec.Code, rec.Body)
	}
}
