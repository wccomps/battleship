// Package authtest logs test users in without an identity provider, by
// writing sessions straight to the store, so web handler tests can use the
// real auth middleware.
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
	// Username is the preferred_username, whose Proxmox user is
	// <username>@auth.example.org; default the subject without "sub-", or
	// the Proxmox ticket's user's name.
	Username string
	Groups   []string
	// At is the login time on the clock the auth.Service uses; default
	// time.Now(). The session won't be refreshed for web.session_refresh
	// after At (the fake refresh token isn't sealed, so that refresh is
	// rejected, ending it).
	At time.Time
	// Proxmox is the session's Proxmox ticket; default a made-up ticket of
	// PVEUser issued at At, which a Proxmox fake won't renew. Pass one from
	// pvetest for sessions that must renew theirs.
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
	Cookie *http.Cookie // the session cookie, named by auth.SessionCookieName
	CSRF   string       // the session's CSRF token
}

// Apply adds the session cookie to r and, for anything but GET and HEAD,
// the CSRF token in the auth.CSRFField field of its form body, as a
// browser's form post would. r's body, if any, must be form-encoded.
func (s Session) Apply(r *http.Request) {
	r.AddCookie(s.Cookie)
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
// its cookie and CSRF token.
func Login(t testing.TB, st *store.Store, cfg config.Config, u User) Session {
	t.Helper()
	if u.At.IsZero() {
		u.At = time.Now()
	}
	u.defaults()
	groups := u.Groups
	token, csrf := random(), random()
	exp := u.At.Add(min(cfg.Web.SessionIdle, cfg.Web.SessionMax))
	err := st.CreateSession(t.Context(), store.Session{
		ID:           auth.HashToken(token),
		Subject:      u.Subject,
		Username:     u.username(),
		Name:         u.Name,
		Email:        u.Email,
		Groups:       groups,
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
	if !u.NoProxmox {
		cred := u.Proxmox
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
		id := auth.HashToken(token)
		if err := st.SetSessionTicket(t.Context(), id, time.Time{}, auth.SealSessionTicket(cfg, id, cred)); err != nil {
			t.Fatalf("authtest: storing the Proxmox ticket: %v", err)
		}
	}
	name := auth.SessionCookieName(cfg.Web.BaseURL)
	return Session{
		Cookie: &http.Cookie{
			Name: name, Value: token, Path: "/",
			HttpOnly: true, Secure: name != auth.SessionCookie, SameSite: http.SameSiteLaxMode,
		},
		CSRF: csrf,
	}
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
	if u.Groups == nil {
		u.Groups = []string{}
	}
}

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
