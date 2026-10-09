package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Web configures battleship serve: the web app and its job workers.
type Web struct {
	Listen  string `toml:"listen"`   // address to serve HTTP on, e.g. ":8080"
	BaseURL string `toml:"base_url"` // public URL, e.g. https://battleship.example.org; required for serve
	Workers int    `toml:"workers"`  // job workers per process (per replica)
	// Templates is a master pattern, e.g. "*.kilo.alpha", whose hosts the grid
	// shows even before every team has them. Empty shows only hosts with VMs.
	Templates string `toml:"templates"`
	// SessionIdle and SessionMax end a session after idle time and after
	// login. Every SessionRefresh it re-checks the user's groups with the IdP.
	SessionIdle    time.Duration `toml:"session_idle"`
	SessionMax     time.Duration `toml:"session_max"`
	SessionRefresh time.Duration `toml:"session_refresh"`
	StatusPoll     time.Duration `toml:"status_poll"` // how often the grid reads the cluster's VMs
	DriftScan      time.Duration `toml:"drift_scan"`  // how often every team VM's config and snapshots are checked
	// ShutdownTimeout is how long serve waits for running jobs on stop. Keep
	// it above apply.StopBudget (14m) and below terminationGracePeriodSeconds.
	ShutdownTimeout time.Duration `toml:"shutdown_timeout"`
	// TrustedProxies are the CIDRs of reverse proxies whose X-Forwarded-For
	// and X-Forwarded-Proto headers are believed. Empty trusts none.
	TrustedProxies []string `toml:"trusted_proxies"`
}

// TrustedProxyPrefixes parses TrustedProxies.
func (w Web) TrustedProxyPrefixes() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range w.TrustedProxies {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			msg := fmt.Sprintf("web.trusted_proxies: %q is not a CIDR", s)
			if addr, aerr := netip.ParseAddr(strings.TrimSpace(s)); aerr == nil {
				msg += fmt.Sprintf(" (for one address, use %s/%d)", addr, addr.BitLen())
			}
			return nil, errors.New(msg)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// OIDC configures login through the identity provider (Authentik).
type OIDC struct {
	Issuer       string   `toml:"issuer"`        // overridden by BATTLESHIP_OIDC_ISSUER
	ClientID     string   `toml:"client_id"`     // overridden by BATTLESHIP_OIDC_CLIENT_ID
	ClientSecret string   `toml:"client_secret"` // overridden by BATTLESHIP_OIDC_CLIENT_SECRET
	Scopes       []string `toml:"scopes"`
	GroupsClaim  string   `toml:"groups_claim"` // the ID token or userinfo claim that lists the user's groups
}

// String redacts the client secret so a config can be logged safely.
func (o OIDC) String() string {
	secret := ""
	if o.ClientSecret != "" {
		secret = "[redacted]"
	}
	return fmt.Sprintf("{Issuer:%s ClientID:%s ClientSecret:%s Scopes:%v GroupsClaim:%s}",
		o.Issuer, o.ClientID, secret, o.Scopes, o.GroupsClaim)
}

// GoString redacts the client secret for %#v.
func (o OIDC) GoString() string { return "config.OIDC" + o.String() }

func defaultWeb() Web {
	return Web{
		Listen:          ":8080",
		Workers:         2,
		SessionIdle:     90 * time.Minute, // longer than proxmox.ticket_renew_after's 1h
		SessionMax:      12 * time.Hour,
		SessionRefresh:  5 * time.Minute,
		StatusPoll:      5 * time.Second,
		DriftScan:       2 * time.Minute,
		ShutdownTimeout: 15 * time.Minute,
	}
}

func defaultOIDC() OIDC {
	return OIDC{
		Issuer:      "https://auth.example.org/application/o/battleship/",
		Scopes:      []string{"openid", "profile", "email", "groups", "offline_access"},
		GroupsClaim: "groups",
	}
}

// validateWeb checks the [web] and [oidc] values that have defaults.
func (c Config) validateWeb() []error {
	var errs []error
	w := c.Web
	if _, port, err := net.SplitHostPort(w.Listen); err != nil || port == "" {
		errs = append(errs, fmt.Errorf("web.listen must be host:port or :port, not %q", w.Listen))
	}
	if w.BaseURL != "" {
		u, err := url.Parse(w.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			errs = append(errs, errors.New("web.base_url must be an http(s) URL with a host and nothing after it, e.g. https://battleship.example.org"))
		}
	}
	if w.Workers < 1 {
		errs = append(errs, errors.New("web.workers must be at least 1"))
	}
	if w.Templates != "" && strings.TrimSpace(w.Templates) == "" {
		errs = append(errs, errors.New(`web.templates must be a master pattern such as "*.kilo.alpha", or empty`))
	}
	if w.SessionIdle < time.Minute {
		errs = append(errs, errors.New("web.session_idle must be at least 1m"))
	}
	// The idle limit shouldn't log people out before a ticket renews on use.
	if w.SessionIdle <= c.Proxmox.TicketRenewAfter {
		errs = append(errs, errors.New("web.session_idle must be longer than proxmox.ticket_renew_after, so a session outlasts a break longer than the wait before its ticket renews"))
	}
	if w.SessionMax < w.SessionIdle {
		errs = append(errs, errors.New("web.session_max must be at least web.session_idle"))
	}
	if w.SessionRefresh < 30*time.Second {
		errs = append(errs, errors.New("web.session_refresh must be at least 30s"))
	} else if w.SessionRefresh >= w.SessionMax {
		errs = append(errs, errors.New("web.session_refresh must be shorter than web.session_max, or group changes would never apply"))
	}
	if w.StatusPoll < time.Second {
		errs = append(errs, errors.New("web.status_poll must be at least 1s"))
	}
	if w.DriftScan < 10*time.Second {
		errs = append(errs, errors.New("web.drift_scan must be at least 10s"))
	}
	if w.ShutdownTimeout < 10*time.Second {
		errs = append(errs, errors.New("web.shutdown_timeout must be at least 10s (keep it above the 14m running jobs may take to stop)"))
	}
	if _, err := w.TrustedProxyPrefixes(); err != nil {
		errs = append(errs, err)
	}

	o := c.OIDC
	if o.Issuer != "" {
		u, err := url.Parse(o.Issuer)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("oidc.issuer must be an http(s) URL with a host"))
		}
	}
	if !slices.Contains(o.Scopes, "openid") {
		errs = append(errs, errors.New(`oidc.scopes must include "openid"`))
	}
	if strings.TrimSpace(o.GroupsClaim) == "" {
		errs = append(errs, errors.New("oidc.groups_claim is required"))
	}

	return errs
}

// RequireWeb reports an error for each serve-only setting without a default,
// including the offline_access scope.
func (c Config) RequireWeb() error {
	var errs []error
	if strings.TrimSpace(c.Web.BaseURL) == "" {
		errs = append(errs, errors.New("web.base_url is required (e.g. https://battleship.example.org)"))
	}
	if strings.TrimSpace(c.OIDC.Issuer) == "" {
		errs = append(errs, errors.New("oidc.issuer (or BATTLESHIP_OIDC_ISSUER) is required"))
	}
	if strings.TrimSpace(c.OIDC.ClientID) == "" {
		errs = append(errs, errors.New("oidc.client_id (or BATTLESHIP_OIDC_CLIENT_ID) is required"))
	}
	if c.OIDC.ClientSecret == "" {
		errs = append(errs, errors.New("oidc.client_secret (or BATTLESHIP_OIDC_CLIENT_SECRET) is required"))
	}
	if !slices.Contains(c.OIDC.Scopes, "offline_access") {
		errs = append(errs, errors.New(`oidc.scopes must include "offline_access": without it the identity provider issues no refresh token, and sessions can't re-check the user's groups`))
	}
	return errors.Join(errs...)
}
