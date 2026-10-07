package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// TicketLifetime is how long Proxmox accepts a ticket after it was issued.
// A ticket can be renewed (RenewTicket) only while it is still valid.
const TicketLifetime = 2 * time.Hour

// ErrNoCredential is returned, without contacting Proxmox, by a call through
// a client with no credential.
var ErrNoCredential = errors.New("no Proxmox credential: every call must be made as a person (Client.As)")

// Credential is a person's login ticket (with CSRF token) or API token.
// Its String and GoString never show secrets.
type Credential struct {
	// User is the Proxmox user, e.g. jdoe@auth.example.org; for a token,
	// the token ID, e.g. jdoe@auth.example.org!cli.
	User   string
	Ticket string
	CSRF   string
	// Issued is when battleship asked for the ticket, by its own clock, so
	// Proxmox clock skew can't make it look younger than it is.
	Issued time.Time
	// LoginAt is when the login ran; renewals keep it. Renewal skips the IdP,
	// so proxmox.ticket_max_age caps renewals after it.
	LoginAt     time.Time
	TokenSecret string
}

// TicketCredential is a login ticket of user.
func TicketCredential(user, ticket, csrf string, issued time.Time) Credential {
	return Credential{User: user, Ticket: ticket, CSRF: csrf, Issued: issued}
}

// TokenCredential is an API token: id is user@realm!name.
func TokenCredential(id, secret string) Credential {
	return Credential{User: id, TokenSecret: secret}
}

// IsToken reports whether c is an API token rather than a ticket.
func (c Credential) IsToken() bool { return c.TokenSecret != "" }

// Usable reports whether c holds a ticket or a token secret, and a user.
func (c Credential) Usable() bool { return c.User != "" && (c.Ticket != "" || c.TokenSecret != "") }

// Expired reports whether a ticket is past TicketLifetime. Proxmox enforces
// token expiry.
func (c Credential) Expired(now time.Time) bool {
	return !c.IsToken() && !now.Before(c.Issued.Add(TicketLifetime))
}

// Lapsed reports whether a ticket expired or its login is maxAge old.
// Tokens never lapse here.
func (c Credential) Lapsed(now time.Time, maxAge time.Duration) bool {
	return !c.IsToken() && (c.Expired(now) || !now.Before(c.LoginAt.Add(maxAge)))
}

func (c Credential) String() string {
	if c.IsToken() {
		return fmt.Sprintf("{token %s [redacted]}", c.User)
	}
	return fmt.Sprintf("{ticket %s issued %s [redacted]}", c.User, c.Issued.UTC().Format(time.RFC3339))
}

// GoString redacts like String, for %#v.
func (c Credential) GoString() string { return "proxmox.Credential" + c.String() }

// Format makes every verb, %+v included, print String.
func (c Credential) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = f.Write([]byte(c.GoString()))
		return
	}
	_, _ = f.Write([]byte(c.String()))
}

// authorize sets the request's credential headers. A write (anything but
// GET) with a ticket also needs the CSRF token.
func (c Credential) authorize(req *http.Request) {
	if c.IsToken() {
		req.Header.Set("Authorization", "PVEAPIToken="+c.User+"="+c.TokenSecret)
		return
	}
	req.AddCookie(&http.Cookie{Name: "PVEAuthCookie", Value: url.QueryEscape(c.Ticket)})
	if req.Method != http.MethodGet {
		req.Header.Set("CSRFPreventionToken", c.CSRF)
	}
}

// ticketAnswer is what /access/ticket and /access/openid/login return.
type ticketAnswer struct {
	Username string `json:"username"`
	Ticket   string `json:"ticket"`
	CSRF     string `json:"CSRFPreventionToken"`
}

func (a ticketAnswer) credential(what string, asked time.Time) (Credential, error) {
	if a.Username == "" || a.Ticket == "" || a.CSRF == "" {
		return Credential{}, fmt.Errorf("%s: Proxmox returned no ticket", what)
	}
	return TicketCredential(a.Username, a.Ticket, a.CSRF, asked), nil
}

// RenewTicket exchanges a valid ticket for a fresh one with the same LoginAt;
// Proxmox accepts the current ticket as the password.
func (c *Client) RenewTicket(ctx context.Context, cred Credential, now time.Time) (Credential, error) {
	if cred.IsToken() {
		return Credential{}, errors.New("an API token is not renewed")
	}
	if !cred.Usable() {
		return Credential{}, ErrNoCredential
	}
	var a ticketAnswer
	if err := c.anonymous().do(ctx, http.MethodPost, "/access/ticket",
		url.Values{"username": {cred.User}, "password": {cred.Ticket}}, &a); err != nil {
		return Credential{}, err
	}
	renewed, err := a.credential("renewing the ticket", now)
	renewed.LoginAt = cred.LoginAt
	return renewed, err
}

// RenewalRefused reports whether a RenewTicket error is Proxmox refusing the
// ticket (401 or 403) rather than a failure to ask.
func RenewalRefused(err error) bool { return IsLapsed(err) || IsForbidden(err) }

// OpenIDAuthURL starts a Proxmox OpenID login, returning the IdP URL and the
// endpoint that started it. Proxmox keeps login state on that node's disk,
// so OpenIDLogin must go to the same endpoint.
func (c *Client) OpenIDAuthURL(ctx context.Context, realm, redirectURL string) (authURL, endpoint string, err error) {
	endpoint, err = c.anonymous().doServed(ctx, http.MethodPost, "/access/openid/auth-url",
		url.Values{"realm": {realm}, "redirect-url": {redirectURL}}, &authURL)
	if err != nil {
		return "", "", err
	}
	if authURL == "" {
		return "", "", errors.New("starting the Proxmox login: Proxmox returned no URL")
	}
	return authURL, endpoint, nil
}

// OpenIDLogin finishes a login at the endpoint that started it. Proxmox
// answers any failure with a bare 401.
func (c *Client) OpenIDLogin(ctx context.Context, endpoint, code, state, redirectURL string, now time.Time) (Credential, error) {
	var a ticketAnswer
	if err := c.anonymous().doPinned(ctx, endpoint, http.MethodPost, "/access/openid/login",
		url.Values{"code": {code}, "state": {state}, "redirect-url": {redirectURL}}, &a); err != nil {
		return Credential{}, err
	}
	cred, err := a.credential("finishing the Proxmox login", now)
	cred.LoginAt = cred.Issued
	return cred, err
}

// Permissions maps ACL path -> privilege -> whether it propagates.
type Permissions map[string]map[string]bool

// Permissions reads the caller's effective privileges.
func (c *Client) Permissions(ctx context.Context) (Permissions, error) {
	return c.permissions(ctx, nil)
}

func (c *Client) permissions(ctx context.Context, q url.Values) (Permissions, error) {
	var raw map[string]map[string]int
	if err := c.do(ctx, http.MethodGet, "/access/permissions", q, &raw); err != nil {
		return nil, err
	}
	out := make(Permissions, len(raw))
	for path, privs := range raw {
		m := make(map[string]bool, len(privs))
		for p, prop := range privs {
			m[p] = prop != 0
		}
		out[path] = m
	}
	return out, nil
}

// PermissionsAt reads the caller's privileges at one path as Proxmox
// resolves them; they can't be inferred from parents, since inheritance
// replaces rather than merges and pool privileges don't propagate. The map
// value is whether the privilege propagates.
func (c *Client) PermissionsAt(ctx context.Context, path string) (map[string]bool, error) {
	perms, err := c.permissions(ctx, url.Values{"path": {path}})
	if err != nil {
		return nil, err
	}
	return perms[path], nil
}
