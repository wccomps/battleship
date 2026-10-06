package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/seal"
	"github.com/wccomps/battleship/internal/store"
)

const (
	// SessionCookie names the cookie that carries a session token. The
	// database keeps only the token's hash (see HashToken). With an https
	// web.base_url the cookie is "__Host-" + SessionCookie; see
	// SessionCookieName.
	SessionCookie = "battleship_session"
	// CSRFField is the form field that carries a session's CSRF token on
	// state-changing requests.
	CSRFField = "csrf"

	// loginCookie holds a login in progress (state, nonce, PKCE verifier and
	// next), sealed, for loginTTL. With an https web.base_url it is __Host-
	// prefixed, like the session cookie (see namesFor).
	loginCookie = "battleship_oidc"
	loginTTL    = 10 * time.Minute
	// providerTimeout bounds each exchange with the identity provider.
	providerTimeout = 15 * time.Second
)

// ErrNoStore is returned by NewService without a session store.
var ErrNoStore = errors.New("auth: a session store is required")

// Options configure a Service. Proxmox is required.
type Options struct {
	// Now is the clock for session expiry, refresh timing and ID token
	// expiry. Default time.Now.
	Now func() time.Time
	// Logf receives security events: logins, logouts, expiries, refresh
	// failures, refused Proxmox sign-ins and CSRF rejections. It never
	// receives tokens or cookies. Default log.Printf.
	Logf func(format string, args ...any)
	// Proxmox signs users in to Proxmox and renews their tickets; it is
	// required. It holds no credential of its own.
	Proxmox ProxmoxLogin
}

// Service logs users in through the identity provider (OpenID Connect),
// keeps their sessions in the store, and guards handlers.
type Service struct {
	cfg           config.Config
	st            *store.Store
	now           func() time.Time
	logf          func(format string, args ...any)
	render        func(w http.ResponseWriter, r *http.Request, p Page)
	client        *http.Client
	provider      *oidc.Provider
	verifier      *oidc.IDTokenVerifier
	oauth         oauth2.Config
	endSessionURL string      // provider logout URL; "" if discovery lists none
	secure        bool        // cookies get Secure (base_url is https)
	names         cookieNames // cookie names, from namesFor(secure)
	origin        string      // base_url's origin, for the Origin check
	loginKey      seal.Key    // seals the login cookie (loginstate.go)
	refreshKey    seal.Key    // seals refresh tokens at rest (refreshtoken.go)
	ticketKey     seal.Key    // seals Proxmox tickets at rest (ticket.go)
	pve           ProxmoxLogin
}

// SetRender makes render draw the pages auth shows itself (login errors,
// 401 and 403) in the web layer's layout. The web server, built after the
// service, calls it before serving; until then auth answers in plain text.
func (s *Service) SetRender(render func(w http.ResponseWriter, r *http.Request, p Page)) {
	s.render = render
}

// plainPage is the render a service has until SetRender: the page's title
// and message as text.
func plainPage(w http.ResponseWriter, _ *http.Request, p Page) {
	http.Error(w, p.Title+": "+p.Message, p.Status)
}

// NewService checks the config battleship serve needs (config.RequireWeb) and
// runs OIDC discovery against oidc.issuer, so a wrong issuer or an
// unreachable provider fails at startup. cfg is expected to have passed
// config.Load's validation.
func NewService(ctx context.Context, cfg config.Config, st *store.Store, opts Options) (*Service, error) {
	if st == nil {
		return nil, ErrNoStore
	}
	if err := cfg.RequireWeb(); err != nil {
		return nil, err
	}
	w := cfg.Web
	if w.SessionIdle <= 0 || w.SessionMax < w.SessionIdle || w.SessionRefresh <= 0 {
		return nil, errors.New("web.session_idle, session_max and session_refresh must be set; load the config with config.Load")
	}
	base, err := url.Parse(w.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("web.base_url must be an http(s) URL with a host")
	}
	if opts.Proxmox == nil {
		return nil, errors.New("auth: a Proxmox client is required for the Proxmox sign-in")
	}

	s := &Service{
		cfg:        cfg,
		st:         st,
		now:        opts.Now,
		logf:       opts.Logf,
		render:     plainPage,
		client:     &http.Client{Timeout: providerTimeout},
		secure:     SecureBaseURL(w.BaseURL),
		names:      namesFor(SecureBaseURL(w.BaseURL)),
		origin:     normalOrigin(base),
		loginKey:   seal.NewKey(cfg.OIDC.ClientSecret, loginStatePurpose),
		refreshKey: seal.NewKey(cfg.OIDC.ClientSecret, refreshTokenPurpose),
		ticketKey:  ticketKey(cfg),
		pve:        opts.Proxmox,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = log.Printf
	}

	dctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	provider, err := oidc.NewProvider(oidc.ClientContext(dctx, s.client), cfg.OIDC.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc.issuer %s: discovery failed: %w", cfg.OIDC.Issuer, err)
	}
	var meta struct {
		EndSession  string   `json:"end_session_endpoint"`
		AuthMethods []string `json:"token_endpoint_auth_methods_supported"`
		JWKSURI     string   `json:"jwks_uri"`
		Algorithms  []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := provider.Claims(&meta); err != nil {
		return nil, fmt.Errorf("oidc.issuer %s: reading discovery: %w", cfg.OIDC.Issuer, err)
	}
	if u, err := url.Parse(meta.EndSession); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		s.endSessionURL = meta.EndSession
	}
	endpoint := provider.Endpoint()
	switch {
	case slices.Contains(meta.AuthMethods, "client_secret_basic"):
		endpoint.AuthStyle = oauth2.AuthStyleInHeader
	case slices.Contains(meta.AuthMethods, "client_secret_post"):
		endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	s.provider = provider
	var algs []string // what go-oidc supports of what the provider offers; empty means RS256
	for _, a := range meta.Algorithms {
		if slices.Contains(signingAlgs, a) {
			algs = append(algs, a)
		}
	}
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), s.client), meta.JWKSURI)
	s.verifier = oidc.NewVerifier(cfg.OIDC.Issuer, keySet{keys}, &oidc.Config{
		ClientID: cfg.OIDC.ClientID, Now: s.now, SupportedSigningAlgs: algs,
	})
	s.oauth = oauth2.Config{
		ClientID:     cfg.OIDC.ClientID,
		ClientSecret: cfg.OIDC.ClientSecret,
		Endpoint:     endpoint,
		RedirectURL:  w.BaseURL + "/auth/callback",
		Scopes:       slices.Clone(cfg.OIDC.Scopes),
	}
	return s, nil
}

// signingAlgs are the ID token algorithms go-oidc can verify.
var signingAlgs = []string{
	oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.PS256, oidc.PS384, oidc.PS512, oidc.EdDSA,
}

// Routes registers the login flow: GET /auth/login, GET /auth/callback,
// POST /auth/logout, and the Proxmox step, GET /auth/proxmox and GET
// /auth/proxmox/callback.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", s.login)
	mux.HandleFunc("GET /auth/callback", s.callback)
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("GET /auth/proxmox", s.proxmoxStart)
	mux.HandleFunc("GET "+proxmoxCallbackPath, s.proxmoxCallback)
}

// providerContext bounds a call to the identity provider and makes it use
// the service's HTTP client.
func (s *Service) providerContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, providerTimeout)
	return oidc.ClientContext(ctx, s.client), cancel
}

// normalOrigin is u's origin as browsers send it in the Origin header:
// lowercase scheme and host, default port dropped.
func normalOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		host += ":" + port
	}
	return scheme + "://" + host
}
