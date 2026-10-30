package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/seal"
	"github.com/wccomps/battleship/internal/store"
)

// One box bound to the session, so its login time can't be moved.
const cookiePurpose = "proxmox ticket cookie v1"

func cookieKey(cfg config.Config) seal.Key { return seal.NewKey(cfg.OIDC.ClientSecret, cookiePurpose) }

type ticketBox struct {
	User    string    `json:"u"`
	Ticket  string    `json:"t"`
	CSRF    string    `json:"c"`
	Issued  time.Time `json:"i"`
	LoginAt time.Time `json:"l"`
}

func sealTicketCookie(k seal.Key, sessionID string, cred proxmox.Credential) string {
	plain, err := json.Marshal(ticketBox{User: cred.User, Ticket: cred.Ticket, CSRF: cred.CSRF, Issued: cred.Issued, LoginAt: cred.LoginAt})
	if err != nil {
		panic(err) // its times come from the clock or a cookie that decoded
	}
	return k.Seal(sessionID, plain)
}

func (s *Service) openTicketCookie(sessionID, value string) (proxmox.Credential, bool) {
	plain, err := s.cookieKey.Open(sessionID, value)
	if err != nil {
		return proxmox.Credential{}, false
	}
	var b ticketBox
	if json.Unmarshal(plain, &b) != nil {
		return proxmox.Credential{}, false
	}
	cred := proxmox.TicketCredential(b.User, b.Ticket, b.CSRF, b.Issued)
	cred.LoginAt = b.LoginAt
	return cred, cred.Usable()
}

// ticketCookie carries cred for session sessionID until it lapses.
func ticketCookie(k seal.Key, name string, secure bool, sessionID string, cred proxmox.Credential, maxAge time.Duration, now time.Time) *http.Cookie {
	return siteCookie(name, sealTicketCookie(k, sessionID, cred), secure, cookieMaxAge(cred.LapsesAt(maxAge).Sub(now)))
}

// SignedInCookies are the cookies a browser holds at now after logging in
// with session token and signing in to Proxmox with cred (no ticket cookie
// unless cred is usable).
func SignedInCookies(cfg config.Config, token string, cred proxmox.Credential, now time.Time) []*http.Cookie {
	secure := SecureBaseURL(cfg.Web.BaseURL)
	names := namesFor(secure)
	out := []*http.Cookie{siteCookie(names.session, token, secure, cookieMaxAge(expiresAt(cfg.Web, now, now).Sub(now)))}
	if !cred.Usable() {
		return out
	}
	return append(out, ticketCookie(cookieKey(cfg), names.ticket, secure, HashToken(token), cred, cfg.Proxmox.TicketMaxAge, now))
}

// loadTicket is the session's ticket from its ticket cookie, if it opens
// and hasn't lapsed at now.
func (s *Service) loadTicket(r *http.Request, sess store.Session, now time.Time) (proxmox.Credential, bool) {
	c, err := r.Cookie(s.names.ticket)
	if err != nil {
		return proxmox.Credential{}, false
	}
	cred, ok := s.openTicketCookie(sess.ID, c.Value)
	return cred, ok && !cred.Lapsed(now, s.cfg.Proxmox.TicketMaxAge)
}

// setTicketCookie gives the browser cred for sess until it lapses.
func (s *Service) setTicketCookie(w http.ResponseWriter, sess store.Session, cred proxmox.Credential, now time.Time) {
	http.SetCookie(w, ticketCookie(s.cookieKey, s.names.ticket, s.secure, sess.ID, cred, s.cfg.Proxmox.TicketMaxAge, now))
}

func (s *Service) clearTicketCookie(w http.ResponseWriter) {
	http.SetCookie(w, siteCookie(s.names.ticket, "", s.secure, -1))
}

// ProxmoxLogin is what the service needs of Proxmox to sign a user in to
// it and keep their ticket alive; *proxmox.Client is one.
type ProxmoxLogin interface {
	OpenIDAuthURL(ctx context.Context, realm, redirectURL string) (authURL, endpoint string, err error)
	OpenIDLogin(ctx context.Context, endpoint, code, state, redirectURL string, now time.Time) (proxmox.Credential, error)
	RenewTicket(ctx context.Context, cred proxmox.Credential, now time.Time) (proxmox.Credential, error)
}

type credKey struct{}

// ProxmoxCredential returns the user's Proxmox ticket from RequireUser;
// every Proxmox call for the request uses it.
func ProxmoxCredential(ctx context.Context) (proxmox.Credential, bool) {
	c, ok := ctx.Value(credKey{}).(proxmox.Credential)
	return c, ok
}

// WithProxmoxCredential returns ctx carrying cred, as RequireUser does, for
// tests of handlers that need one.
func WithProxmoxCredential(ctx context.Context, cred proxmox.Credential) context.Context {
	return context.WithValue(ctx, credKey{}, cred)
}
