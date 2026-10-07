package auth

import (
	"context"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/seal"
	"github.com/wccomps/battleship/internal/store"
)

// A session's Proxmox ticket and CSRF token are sealed like its refresh
// token (see refreshtoken.go), each bound to the session and its column.
const ticketPurpose = "proxmox ticket v1"

func ticketKey(cfg config.Config) seal.Key { return seal.NewKey(cfg.OIDC.ClientSecret, ticketPurpose) }

// ProxmoxLogin is what the service needs of Proxmox to sign a user in to
// it and keep their ticket alive; *proxmox.Client is one.
type ProxmoxLogin interface {
	OpenIDAuthURL(ctx context.Context, realm, redirectURL string) (authURL, endpoint string, err error)
	OpenIDLogin(ctx context.Context, endpoint, code, state, redirectURL string, now time.Time) (proxmox.Credential, error)
	RenewTicket(ctx context.Context, cred proxmox.Credential, now time.Time) (proxmox.Credential, error)
}

// SealSessionTicket seals cred as session sessionID stores it. authtest
// uses it to log test users in with a ticket.
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

// openTicket opens sess's ticket; ok is false if it has none or it doesn't
// open.
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
