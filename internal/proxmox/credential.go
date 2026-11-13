package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// TicketLifetime is how long Proxmox accepts a ticket after it was issued.
// A ticket can be renewed (RenewTicket) only while it is still valid.
const TicketLifetime = 2 * time.Hour

// ErrNoCredential is returned, without contacting Proxmox, by a call through
// a client with no credential.
var ErrNoCredential = errors.New("no Proxmox credential: every call must be made as a person (Client.As)")

// Credential is a person's login ticket, with its CSRF token. Its String and
// GoString never show secrets.
type Credential struct {
	// User is the Proxmox user, e.g. jdoe@auth.example.org.
	User   string
	Ticket string
	CSRF   string
	// Issued is when battleship asked for the ticket, by its own clock, so
	// Proxmox clock skew can't make it look younger than it is.
	Issued time.Time
	// LoginAt is when the login ran; renewals keep it. Renewal skips the IdP,
	// so proxmox.ticket_max_age caps renewals after it.
	LoginAt time.Time
}

// TicketCredential is a login ticket of user.
func TicketCredential(user, ticket, csrf string, issued time.Time) Credential {
	return Credential{User: user, Ticket: ticket, CSRF: csrf, Issued: issued}
}

// Usable reports whether c holds a user and a ticket.
func (c Credential) Usable() bool { return c.User != "" && c.Ticket != "" }

// Expired reports whether the ticket is past TicketLifetime.
func (c Credential) Expired(now time.Time) bool {
	return !now.Before(c.Issued.Add(TicketLifetime))
}

// LapsesAt is when the ticket expires or its login turns maxAge old,
// whichever comes first.
func (c Credential) LapsesAt(maxAge time.Duration) time.Time {
	if l := c.LoginAt.Add(maxAge); l.Before(c.Issued.Add(TicketLifetime)) {
		return l
	}
	return c.Issued.Add(TicketLifetime)
}

// Lapsed reports whether the ticket expired or its login is maxAge old.
func (c Credential) Lapsed(now time.Time, maxAge time.Duration) bool {
	return !now.Before(c.LapsesAt(maxAge))
}

func (c Credential) String() string {
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
// GET) also needs the CSRF token.
func (c Credential) authorize(req *http.Request) {
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

// TagAccess is what the cluster's datacenter options (/cluster/options) say a
// user without Sys.Modify on / may set as a guest tag. The zero value is
// Proxmox's default ("free": any tag not in Registered).
type TagAccess struct {
	// Mode is user-tag-access's user-allow setting: "" or "free" (the
	// default), "existing", "list" or "none".
	Mode       string
	Allowed    []string
	Registered []string
}

// Allows reports whether a user without Sys.Modify on / may set tag.
func (t TagAccess) Allows(tag string) bool {
	if slices.ContainsFunc(t.Registered, func(r string) bool { return strings.EqualFold(r, tag) }) {
		return false
	}
	return t.Mode == "" || t.Mode == "free" ||
		slices.ContainsFunc(t.Allowed, func(a string) bool { return strings.EqualFold(a, tag) })
}

// TagAccess reads the cluster's tag policy for the caller from /cluster/options.
func (c *Client) TagAccess(ctx context.Context) (TagAccess, error) {
	var raw struct {
		UserTagAccess *struct {
			UserAllow string `json:"user-allow"`
		} `json:"user-tag-access"`
		Allowed    []string `json:"allowed-tags"`
		Registered []string `json:"registered-tags"`
	}
	if err := c.do(ctx, http.MethodGet, "/cluster/options", nil, &raw); err != nil {
		return TagAccess{}, err
	}
	var mode string
	if raw.UserTagAccess != nil {
		mode = raw.UserTagAccess.UserAllow
	}
	return TagAccess{Mode: mode, Allowed: raw.Allowed, Registered: raw.Registered}, nil
}
