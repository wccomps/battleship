package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/wccomps/battleship/internal/config"
)

// Provider is an OpenID provider on httptest whose authorization endpoint
// logs in whoever SignIn named last, with no form. Unlike StubProvider it
// runs the whole flow, including PKCE, userinfo and rotating refresh
// tokens, for end-to-end tests of the real login pages.
type Provider struct {
	// Now is the provider's clock for ID token times. Default time.Now; set
	// it to the app's clock when the test moves time.
	Now func() time.Time

	t        testing.TB
	srv      *httptest.Server
	key      *rsa.PrivateKey
	clientID string
	secret   string

	mu      sync.Mutex
	current string           // subject the authorization endpoint logs in
	users   map[string]User  // by subject, with Groups filled in
	codes   map[string]grant // authorization code -> login
	refresh map[string]string
	access  map[string]string
	logins  int
}

type grant struct {
	subject, nonce, challenge, redirectURI string
	offline                                bool
}

// providerKey is generated once per test binary; RSA key generation is slow.
var providerKey = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

// NewProvider starts a Provider and points cfg's [oidc] section at it with
// a test client ID and secret. The provider stops when the test ends.
func NewProvider(t testing.TB, cfg *config.Config) *Provider {
	t.Helper()
	key, err := providerKey()
	if err != nil {
		t.Fatalf("authtest: generating a signing key: %v", err)
	}
	p := &Provider{
		Now: time.Now, t: t, key: key,
		clientID: "battleship-e2e", secret: "authtest-provider-secret",
		users: map[string]User{}, codes: map[string]grant{},
		refresh: map[string]string{}, access: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	mux.HandleFunc("GET /end-session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("signed out of the test provider"))
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	cfg.OIDC.Issuer = p.Issuer()
	cfg.OIDC.ClientID = p.clientID
	cfg.OIDC.ClientSecret = p.secret
	return p
}

// Issuer is the provider's issuer URL.
func (p *Provider) Issuer() string { return p.srv.URL + "/" }

// SignIn makes u the person the next login signs in, with Login's
// defaults. Signing in an existing subject replaces their details, which
// sessions see at their next refresh.
func (p *Provider) SignIn(u User) {
	p.t.Helper()
	u.defaults()
	u.Groups = slices.Clone(u.Groups)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users[u.Subject] = u
	p.current = u.Subject
}

// Logins is how many authorization codes the provider has exchanged.
func (p *Provider) Logins() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logins
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                p.Issuer(),
		"authorization_endpoint":                p.srv.URL + "/authorize",
		"token_endpoint":                        p.srv.URL + "/token",
		"jwks_uri":                              p.srv.URL + "/jwks",
		"userinfo_endpoint":                     p.srv.URL + "/userinfo",
		"end_session_endpoint":                  p.srv.URL + "/end-session",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &p.key.PublicKey, KeyID: "authtest", Algorithm: "RS256", Use: "sig",
	}}})
}

// authorize logs in the current user and sends the browser back with a
// code, as a provider does once its user has signed in.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("client_id") != p.clientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		!slices.Contains(strings.Fields(q.Get("scope")), "openid") {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	if p.current == "" {
		p.mu.Unlock()
		http.Error(w, "authtest: nobody to sign in; call SignIn first", http.StatusForbidden)
		return
	}
	code := randomToken()
	p.codes[code] = grant{
		subject: p.current, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		redirectURI: q.Get("redirect_uri"), offline: slices.Contains(strings.Fields(q.Get("scope")), "offline_access"),
	}
	p.mu.Unlock()
	params := back.Query()
	params.Set("code", code)
	params.Set("state", q.Get("state"))
	back.RawQuery = params.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	if id, secret, ok := r.BasicAuth(); !ok || id != p.clientID || secret != p.secret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		g, ok := p.codes[r.PostForm.Get("code")]
		delete(p.codes, r.PostForm.Get("code")) // single use
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || g.redirectURI != r.PostForm.Get("redirect_uri") ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		p.logins++
		p.issue(w, g.subject, g.nonce, g.offline)
	case "refresh_token":
		old := r.PostForm.Get("refresh_token")
		subject, ok := p.refresh[old]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		delete(p.refresh, old) // rotation: each refresh token works once
		p.issue(w, subject, "", true)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

// issue answers a token request for subject. The caller holds p.mu.
func (p *Provider) issue(w http.ResponseWriter, subject, nonce string, offline bool) {
	u := p.users[subject]
	access := "at-" + randomToken()
	p.access[access] = subject
	now := p.Now()
	atHash := sha256.Sum256([]byte(access))
	claims := map[string]any{
		"iss": p.Issuer(), "sub": subject, "aud": p.clientID,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"name": u.Name, "email": u.Email, "groups": u.Groups, "preferred_username": u.username(),
		"at_hash": base64.RawURLEncoding.EncodeToString(atHash[:16]),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	idToken, err := p.sign(claims)
	if err != nil {
		p.t.Errorf("authtest: signing an ID token: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	resp := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300, "id_token": idToken}
	if offline {
		rt := "rt-" + randomToken()
		p.refresh[rt] = subject
		resp["refresh_token"] = rt
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Provider) sign(claims map[string]any) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: p.key, KeyID: "authtest"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return jws.CompactSerialize()
}

func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	defer p.mu.Unlock()
	subject, ok := p.access[tok]
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	u := p.users[subject]
	writeJSON(w, http.StatusOK, map[string]any{"sub": subject, "name": u.Name, "email": u.Email, "groups": u.Groups, "preferred_username": u.username()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
