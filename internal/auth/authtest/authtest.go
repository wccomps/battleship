// Package authtest logs test users in: Login writes sessions straight to the
// store, so web handler tests can use the real auth middleware, and Provider
// is a fake OIDC identity provider for tests of the login flow itself.
package authtest

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// User describes the session to create.
type User struct {
	Subject string // default "test-user"
	Name    string // default "Test user"
	Email   string // default "<subject>@example.org"
	// Username is the preferred_username; default the subject without
	// "sub-", or the Proxmox ticket's user's name.
	Username string
	// At is the login time on the auth.Service clock; default time.Now().
	// The fake refresh token isn't sealed, so the session ends at its first
	// refresh, web.session_refresh after At.
	At time.Time
	// Proxmox is the session's ticket; default a made-up one for PVEUser
	// that a Proxmox fake won't renew (use pvetest's for renewal).
	Proxmox proxmox.Credential
	// PVEUser names the default ticket's Proxmox user; default
	// <subject>@auth.example.org.
	PVEUser string
	// NoProxmox leaves the session without a Proxmox ticket, as between
	// the battleship login and the Proxmox sign-in.
	NoProxmox bool
}

// Session is a logged-in test browser.
type Session struct {
	Cookie *http.Cookie // the session cookie
	Ticket *http.Cookie // the Proxmox ticket cookie; nil with NoProxmox
	CSRF   string       // the session's CSRF token
}

// Cookies are the browser's cookies for the session.
func (s Session) Cookies() []*http.Cookie {
	if s.Ticket == nil {
		return []*http.Cookie{s.Cookie}
	}
	return []*http.Cookie{s.Cookie, s.Ticket}
}

// Apply adds the session's cookies to r and, unless GET or HEAD, the CSRF
// token to its form body, which must be form-encoded.
func (s Session) Apply(r *http.Request) {
	for _, c := range s.Cookies() {
		r.AddCookie(c)
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body) // an in-memory test body
		_ = r.Body.Close()
	}
	if len(body) > 0 {
		body = append(body, '&')
	}
	body = append(body, url.Values{auth.CSRFField: {s.CSRF}}.Encode()...)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
}

// Login stores a session for u, as a successful login would, and returns
// its cookies and CSRF token.
func Login(t testing.TB, st *store.Store, cfg config.Config, u User) Session {
	t.Helper()
	if u.At.IsZero() {
		u.At = time.Now()
	}
	u.defaults()
	token, csrf := random(), random()
	exp := u.At.Add(min(cfg.Web.SessionIdle, cfg.Web.SessionMax))
	err := st.CreateSession(t.Context(), store.Session{
		ID:           auth.HashToken(token),
		Subject:      u.Subject,
		Username:     u.username(),
		Name:         u.Name,
		Email:        u.Email,
		RefreshToken: "authtest-refresh-token",
		CSRF:         csrf,
		CreatedAt:    u.At,
		LastSeen:     u.At,
		RefreshedAt:  u.At,
		ExpiresAt:    exp,
	})
	if err != nil {
		t.Fatalf("authtest: creating session: %v", err)
	}
	var cred proxmox.Credential
	if !u.NoProxmox {
		cred = u.Proxmox
		if !cred.Usable() {
			user := u.PVEUser
			if user == "" {
				user = u.username() + "@auth.example.org"
			}
			cred = proxmox.TicketCredential(user, fmt.Sprintf("PVE:%s:%08X::authtest-%s", user, u.At.Unix(), random()), "authtest-csrf", u.At)
		}
		if cred.LoginAt.IsZero() {
			cred.LoginAt = cred.Issued
		}
	}
	cookies, err := auth.SignedInCookies(cfg, token, cred, u.At)
	if err != nil {
		t.Fatalf("authtest: %v", err)
	}
	sess := Session{Cookie: cookies[0], CSRF: csrf}
	if len(cookies) > 1 {
		sess.Ticket = cookies[1]
	}
	return sess
}

// username is u's preferred_username.
func (u User) username() string {
	if u.Username != "" {
		return u.Username
	}
	if name, _, ok := strings.Cut(u.Proxmox.User, "@"); ok {
		return name
	}
	return strings.TrimPrefix(u.Subject, "sub-")
}

// defaults fills in what u leaves out.
func (u *User) defaults() {
	if u.Subject == "" {
		u.Subject = "test-user"
	}
	if u.Name == "" {
		u.Name = "Test user"
	}
	if u.Email == "" {
		u.Email = u.Subject + "@example.org"
	}
}

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
