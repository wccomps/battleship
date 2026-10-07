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

// HashToken is the SHA-256 hex ID under which a secret token is stored, so
// a database leak hands out no live sessions or confirmable previews.
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

// expired reports whether sess has ended at now. It uses the current
// config so a redeploy with shorter limits applies to existing sessions.
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
// activity, saving a write per request at a small cost to the idle window.
func touchEvery(w config.Web) time.Duration {
	return min(time.Minute, w.SessionIdle/10)
}

// lookup finds the request's session, or errNoSession.
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

// hostPrefix makes browsers accept the cookie only Secure, Path=/ and
// without Domain, so a subdomain or plain-http page can't plant it.
const hostPrefix = "__Host-"

// cookieNames are the names (and the login cookie's path) the service uses.
type cookieNames struct {
	session, login, loginPath string
}

// namesFor chooses cookie names: __Host- prefixed for https, plain names
// (login cookie on /auth/) for development over http.
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

// SessionCookieName is the session cookie's name for this web.base_url.
func SessionCookieName(baseURL string) string { return namesFor(SecureBaseURL(baseURL)).session }
