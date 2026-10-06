package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/store"
)

// tokenLen is the length of a 32-byte token in unpadded base64url.
const tokenLen = 43

// errNoSession means the request carries no usable session cookie, or its
// session is gone.
var errNoSession = errors.New("no session")

// randomToken returns 32 random bytes as unpadded base64url.
func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails; it crashes the program instead
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is the ID under which a secret token is stored: its SHA-256
// hex. Sessions (keyed by the cookie value) and the web app's previews
// (keyed by their nonce) store only this, so a database leak hands out
// neither live sessions nor confirmable previews.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// wellFormed reports whether a cookie value could be a token randomToken
// made, so junk never reaches the database.
func wellFormed(token string) bool {
	if len(token) != tokenLen {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil
}

// expired reports whether sess has ended at now under the current config:
// idle for session_idle, older than session_max, or past its stored expiry.
// Using the current config means a redeploy with shorter limits applies to
// existing sessions.
func expired(w config.Web, sess store.Session, now time.Time) bool {
	return !now.Before(sess.ExpiresAt) ||
		!now.Before(sess.CreatedAt.Add(w.SessionMax)) ||
		!now.Before(sess.LastSeen.Add(w.SessionIdle))
}

// expiresAt is when a session created at created and last seen at seen
// ends: session_idle after seen, but never after session_max from created.
func expiresAt(w config.Web, created, seen time.Time) time.Time {
	idle, abs := seen.Add(w.SessionIdle), created.Add(w.SessionMax)
	if abs.Before(idle) {
		return abs
	}
	return idle
}

// touchEvery is how stale last_seen may get before a request records
// activity: a tenth of the idle window, at most a minute. Skipping the
// write in between costs at most that much of the idle window.
func touchEvery(w config.Web) time.Duration {
	return min(time.Minute, w.SessionIdle/10)
}

// lookup finds the request's session. It returns errNoSession when there is
// no well-formed cookie or no such session, and other errors for database
// failures.
func (s *Service) lookup(r *http.Request) (store.Session, error) {
	c, err := r.Cookie(s.names.session)
	if err != nil || !wellFormed(c.Value) {
		return store.Session{}, errNoSession
	}
	sess, err := s.st.Session(r.Context(), HashToken(c.Value))
	if errors.Is(err, store.ErrSessionNotFound) {
		return store.Session{}, errNoSession
	}
	return sess, err
}

func (s *Service) hasSessionCookie(r *http.Request) bool {
	_, err := r.Cookie(s.names.session)
	return err == nil
}

// setSessionCookie gives the browser the session token for the time left.
func (s *Service) setSessionCookie(w http.ResponseWriter, token string, left time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.names.session,
		Value:    token,
		Path:     "/",
		MaxAge:   max(1, int(math.Ceil(left.Seconds()))), // 0 would mean "no Max-Age"
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.names.session, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// noStore keeps responses that set or clear cookies out of caches.
func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }

// hostPrefix is the cookie name prefix browsers accept only on a Secure
// cookie with Path=/ and no Domain, so a subdomain or a plain-http page
// can't plant or overwrite the cookie.
const hostPrefix = "__Host-"

// cookieNames are the names (and the login cookie's path) the service uses.
type cookieNames struct {
	session, login, loginPath string
}

// namesFor is the one place cookie names are chosen: with an https
// base_url (secure) the cookies take the __Host- prefix, which needs
// Path=/; plain http, for development, keeps the plain names and the
// login cookie stays on /auth/.
func namesFor(secure bool) cookieNames {
	if secure {
		return cookieNames{session: hostPrefix + SessionCookie, login: hostPrefix + loginCookie, loginPath: "/"}
	}
	return cookieNames{session: SessionCookie, login: loginCookie, loginPath: "/auth/"}
}

// SecureBaseURL reports whether base_url is https, which makes cookies
// Secure and __Host- prefixed.
func SecureBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	return err == nil && u.Scheme == "https" // Parse lowercases the scheme
}

// SessionCookieName is the name of the session cookie for a service with
// this web.base_url: SessionCookie with the __Host- prefix when it is
// https, else SessionCookie.
func SessionCookieName(baseURL string) string { return namesFor(SecureBaseURL(baseURL)).session }
