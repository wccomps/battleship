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

// The session row's copy of the Proxmox ticket and CSRF token is sealed like
// its refresh token (see refreshtoken.go), each bound to the session and its
// column.
const ticketPurpose = "proxmox ticket v1"

func ticketKey(cfg config.Config) seal.Key { return seal.NewKey(cfg.OIDC.ClientSecret, ticketPurpose) }

// One box bound to the session, so unlike the row's columns its login time
// can't be moved.
const cookiePurpose = "proxmox ticket cookie v1"

func cookieKey(cfg config.Config) seal.Key { return seal.NewKey(cfg.OIDC.ClientSecret, cookiePurpose) }

type ticketBox struct {
	User    string    `json:"u"`
	Ticket  string    `json:"t"`
	CSRF    string    `json:"c"`
	Issued  time.Time `json:"i"`
	LoginAt time.Time `json:"l"`
}

// sealTicketCookie fails only for a time outside years 0-9999, which only a
// session row's columns could hold.
func sealTicketCookie(k seal.Key, sessionID string, cred proxmox.Credential) (string, error) {
	plain, err := json.Marshal(ticketBox{User: cred.User, Ticket: cred.Ticket, CSRF: cred.CSRF, Issued: cred.Issued, LoginAt: cred.LoginAt})
	if err != nil {
		return "", err
	}
	return k.Seal(sessionID, plain), nil
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
func ticketCookie(k seal.Key, name string, secure bool, sessionID string, cred proxmox.Credential, maxAge time.Duration, now time.Time) (*http.Cookie, error) {
	v, err := sealTicketCookie(k, sessionID, cred)
	if err != nil {
		return nil, err
	}
	return siteCookie(name, v, secure, cookieMaxAge(cred.LapsesAt(maxAge).Sub(now))), nil
}

// SignedInCookies are the cookies a browser holds at now after logging in
// with session token and signing in to Proxmox with cred (no ticket cookie
// unless cred is usable).
func SignedInCookies(cfg config.Config, token string, cred proxmox.Credential, now time.Time) ([]*http.Cookie, error) {
	secure := SecureBaseURL(cfg.Web.BaseURL)
	names := namesFor(secure)
	out := []*http.Cookie{siteCookie(names.session, token, secure, cookieMaxAge(expiresAt(cfg.Web, now, now).Sub(now)))}
	if !cred.Usable() {
		return out, nil
	}
	c, err := ticketCookie(cookieKey(cfg), names.ticket, secure, HashToken(token), cred, cfg.Proxmox.TicketMaxAge, now)
	if err != nil {
		return nil, err
	}
	return append(out, c), nil
}

// loadTicket is the session's ticket if it opens and hasn't lapsed at now:
// the ticket cookie's, else the session row's copy (inCookie false), kept
// for the previous release (deploy/README.md).
func (s *Service) loadTicket(r *http.Request, sess store.Session, now time.Time) (cred proxmox.Credential, inCookie, ok bool) {
	if c, err := r.Cookie(s.names.ticket); err == nil {
		cred, inCookie = s.openTicketCookie(sess.ID, c.Value)
	}
	if !inCookie || cred.Lapsed(now, s.cfg.Proxmox.TicketMaxAge) {
		if cred, ok = s.openTicket(sess); !ok {
			return cred, false, false
		}
		inCookie = false
	}
	return cred, inCookie, !cred.Lapsed(now, s.cfg.Proxmox.TicketMaxAge)
}

// setTicketCookie gives the browser cred for sess until it lapses.
func (s *Service) setTicketCookie(w http.ResponseWriter, sess store.Session, cred proxmox.Credential, now time.Time) {
	c, err := ticketCookie(s.cookieKey, s.names.ticket, s.secure, sess.ID, cred, s.cfg.Proxmox.TicketMaxAge, now)
	if err != nil {
		s.logf("auth: sealing the proxmox ticket of subject=%q: %v", sess.Subject, err)
		return
	}
	http.SetCookie(w, c)
}

// saveRenewedTicket gives the browser a renewed ticket, and the session row
// as RenewSessionTicket allows.
func (s *Service) saveRenewedTicket(ctx context.Context, w http.ResponseWriter, sess store.Session, cred proxmox.Credential, now time.Time) {
	s.setTicketCookie(w, sess, cred, now)
	if err := s.st.RenewSessionTicket(ctx, sess.ID, SealSessionTicket(s.cfg, sess.ID, cred)); err != nil {
		s.logf("auth: saving the renewed proxmox ticket of subject=%q: %v", sess.Subject, err)
	}
}

// dropTicket drops a ticket Proxmox refused from the browser and from the
// session row, unless the row holds another one.
func (s *Service) dropTicket(ctx context.Context, w http.ResponseWriter, sess store.Session, cred proxmox.Credential) {
	if err := s.st.ClearSessionTicket(ctx, sess.ID, cred.Issued); err != nil {
		s.logf("auth: clearing the Proxmox ticket of subject=%q: %v", sess.Subject, err)
	}
	http.SetCookie(w, siteCookie(s.names.ticket, "", s.secure, -1))
}

// ProxmoxLogin is what the service needs of Proxmox to sign a user in to
// it and keep their ticket alive; *proxmox.Client is one.
type ProxmoxLogin interface {
	OpenIDAuthURL(ctx context.Context, realm, redirectURL string) (authURL, endpoint string, err error)
	OpenIDLogin(ctx context.Context, endpoint, code, state, redirectURL string, now time.Time) (proxmox.Credential, error)
	RenewTicket(ctx context.Context, cred proxmox.Credential, now time.Time) (proxmox.Credential, error)
}

// SealSessionTicket seals cred as session sessionID's row stores it.
func SealSessionTicket(cfg config.Config, sessionID string, cred proxmox.Credential) store.SessionTicket {
	k := ticketKey(cfg)
	return store.SessionTicket{
		User:     cred.User,
		Ticket:   k.Seal(sessionID+"\x00ticket", []byte(cred.Ticket)),
		CSRF:     k.Seal(sessionID+"\x00csrf", []byte(cred.CSRF)),
		IssuedAt: cred.Issued,
		LoginAt:  cred.LoginAt,
	}
}

// openTicket opens the session row's ticket; ok is false if it has none or
// it doesn't open.
func (s *Service) openTicket(sess store.Session) (proxmox.Credential, bool) {
	t := sess.PVE
	if t.User == "" || t.Ticket == "" || t.IssuedAt.IsZero() {
		return proxmox.Credential{}, false
	}
	ticket, err := s.ticketKey.Open(sess.ID+"\x00ticket", t.Ticket)
	if err != nil {
		return proxmox.Credential{}, false
	}
	csrf, err := s.ticketKey.Open(sess.ID+"\x00csrf", t.CSRF)
	if err != nil {
		return proxmox.Credential{}, false
	}
	c := proxmox.TicketCredential(t.User, string(ticket), string(csrf), t.IssuedAt)
	c.LoginAt = t.LoginAt
	return c, true
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
