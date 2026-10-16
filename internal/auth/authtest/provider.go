package authtest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
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
// tokens, for end-to-end tests of the real login pages. Set makes it
// misbehave.
type Provider struct {
	// Now is the provider's clock for ID token times. Default time.Now; set
	// it to the app's clock when the test moves time.
	Now func() time.Time

	t        testing.TB
	srv      *httptest.Server
	key      *rsa.PrivateKey
	otherKey *rsa.PrivateKey // not in the JWKS
	clientID string
	secret   string

	mu        sync.Mutex
	current   string           // subject the authorization endpoint logs in
	users     map[string]User  // by subject, with defaults filled in
	codes     map[string]grant // authorization code -> login
	refresh   map[string]string
	access    map[string]string
	logins    int
	refreshes int
	requests  []url.Values // every token request's form
	revoked   []string     // refresh tokens revoked at /revoke
	idTokens  []string     // every ID token issued, in order
	b         Behavior
}

// Behavior is how the provider departs from a well-behaved one, which is
// the zero value.
type Behavior struct {
	ClientSecret       string                            // non-empty: the secret the token endpoint accepts instead
	NoRefreshToken     bool                              // token responses carry no refresh_token
	NoIDTokenOnRefresh bool                              // refresh responses carry no id_token
	NoEndSession       bool                              // discovery lists no end_session_endpoint
	NoRevocation       bool                              // discovery lists no revocation_endpoint
	RevokeStatus       int                               // non-zero: the revocation endpoint fails with this status
	AuthMethods        []string                          // non-nil: token_endpoint_auth_methods_supported in discovery
	RevokeAuthMethods  []string                          // non-nil: revocation_endpoint_auth_methods_supported in discovery
	RevokePostOnly     bool                              // the revocation endpoint refuses HTTP Basic client auth (401)
	RevokeDelay        time.Duration                     // the revocation endpoint answers after this long
	AdvertiseAlgs      []string                          // extra id_token_signing_alg_values_supported
	KeyID              string                            // the signing key's ID, default "authtest"; change it to rotate
	MutateIDToken      func(claims map[string]any)       // edits ID token claims before signing
	SignWithUnknownKey bool                              // sign ID tokens with a key not in the JWKS
	SignAlg            string                            // "none", "HS256-secret" (HMAC with the client secret) or "HS256-jwk" (HMAC with the public key)
	AuthorizeError     string                            // the authorization endpoint sends the browser back with this error
	OnRefresh          func(r *http.Request) (fail bool) // runs inside the refresh grant
	UserinfoSubject    string                            // overrides userinfo's sub
	TokenStatus        int                               // non-zero: the token endpoint fails with this status
	JWKSStatus         int                               // non-zero: the JWKS endpoint fails with this status
	UserinfoStatus     int                               // non-zero: userinfo fails with this status
	FailBody           string                            // with a failing status: the raw body sent
	FailContentType    string                            // with FailBody: its Content-Type, default text/html
	DropConnections    bool                              // the token endpoint closes connections without answering
	TokenEcho          int                               // non-zero: the token endpoint fails with this status and a body echoing the request
	TokenEchoJSON      bool                              // with TokenEcho: the echo is inside an OAuth invalid_grant error
}

// EchoMarker is in every body the token endpoint echoes (Behavior.TokenEcho).
const EchoMarker = "ECHOBODY"

type grant struct {
	subject, nonce, challenge, redirectURI string
	offline                                bool
}

// providerKeys are generated once per test binary; RSA key generation is
// slow. The second is the unknown key.
var providerKeys = sync.OnceValues(func() ([2]*rsa.PrivateKey, error) {
	var keys [2]*rsa.PrivateKey
	for i := range keys {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return keys, err
		}
		keys[i] = k
	}
	return keys, nil
})

// NewProvider starts a Provider and points cfg's [oidc] section at it with
// a test client ID and secret. The provider stops when the test ends.
func NewProvider(t testing.TB, cfg *config.Config) *Provider {
	t.Helper()
	keys, err := providerKeys()
	if err != nil {
		t.Fatalf("authtest: generating signing keys: %v", err)
	}
	p := &Provider{
		Now: time.Now, t: t, key: keys[0], otherKey: keys[1],
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
	mux.HandleFunc("POST /revoke", p.revoke)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	cfg.OIDC.Issuer = p.Issuer()
	cfg.OIDC.ClientID = p.clientID
	cfg.OIDC.ClientSecret = p.secret
	return p
}

// Issuer is the provider's issuer URL. Its endpoints are below it.
func (p *Provider) Issuer() string { return p.srv.URL + "/" }

// SignIn makes u the person the next login signs in, with Login's
// defaults. Signing in an existing subject replaces their details, which
// sessions see at their next refresh.
func (p *Provider) SignIn(u User) {
	p.t.Helper()
	u.defaults()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users[u.Subject] = u
	p.current = u.Subject
}

// Set changes the provider's Behavior.
func (p *Provider) Set(fn func(*Behavior)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(&p.b)
}

// RevokeAll invalidates every refresh token, as disabling a user would.
func (p *Provider) RevokeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.refresh)
}

// ExpireCodes invalidates every authorization code not yet exchanged.
func (p *Provider) ExpireCodes() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.codes)
}

// RefreshTokens are the refresh tokens the provider would accept now.
func (p *Provider) RefreshTokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for tok := range p.refresh {
		out = append(out, tok)
	}
	return out
}

// TokenRequests are the forms of every request to the token endpoint.
func (p *Provider) TokenRequests() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

// Logins is how many authorization codes the provider has exchanged.
func (p *Provider) Logins() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logins
}

// Refreshes is how many refresh grants the provider has accepted.
func (p *Provider) Refreshes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refreshes
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"issuer":                                p.Issuer(),
		"authorization_endpoint":                p.srv.URL + "/authorize",
		"token_endpoint":                        p.srv.URL + "/token",
		"jwks_uri":                              p.srv.URL + "/jwks",
		"userinfo_endpoint":                     p.srv.URL + "/userinfo",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
	}
	p.mu.Lock()
	doc["id_token_signing_alg_values_supported"] = append([]string{"RS256"}, p.b.AdvertiseAlgs...)
	if p.b.AuthMethods != nil {
		doc["token_endpoint_auth_methods_supported"] = p.b.AuthMethods
	}
	if p.b.RevokeAuthMethods != nil {
		doc["revocation_endpoint_auth_methods_supported"] = p.b.RevokeAuthMethods
	}
	if !p.b.NoEndSession {
		doc["end_session_endpoint"] = p.srv.URL + "/end-session"
	}
	if !p.b.NoRevocation {
		doc["revocation_endpoint"] = p.srv.URL + "/revoke"
	}
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, doc)
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.b.JWKSStatus != 0 {
		if !p.writeFailBody(w, p.b.JWKSStatus) {
			http.Error(w, "unavailable", p.b.JWKSStatus)
		}
		return
	}
	writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &p.key.PublicKey, KeyID: p.keyID(), Algorithm: "RS256", Use: "sig",
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
	params := back.Query()
	params.Set("state", q.Get("state"))
	p.mu.Lock()
	switch {
	case p.b.AuthorizeError != "":
		params.Set("error", p.b.AuthorizeError)
		params.Set("error_description", "the user said no")
	case p.current == "":
		p.mu.Unlock()
		http.Error(w, "authtest: nobody to sign in; call SignIn first", http.StatusForbidden)
		return
	default:
		code := randomToken()
		p.codes[code] = grant{
			subject: p.current, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
			redirectURI: q.Get("redirect_uri"), offline: slices.Contains(strings.Fields(q.Get("scope")), "offline_access"),
		}
		params.Set("code", code)
	}
	p.mu.Unlock()
	back.RawQuery = params.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// revoke revokes a refresh token for the client (RFC 7009): it stops
// working at the token endpoint.
func (p *Provider) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	id, secret, basic := r.BasicAuth()
	if !basic {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	p.mu.Lock()
	delay := p.b.RevokeDelay
	p.mu.Unlock()
	time.Sleep(delay)
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case basic && p.b.RevokePostOnly:
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	case p.b.RevokeStatus != 0:
		writeJSON(w, p.b.RevokeStatus, map[string]string{"error": "server_error"})
		return
	case id != p.clientID || secret != p.clientSecret():
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	token := r.PostForm.Get("token")
	if _, ok := p.refresh[token]; ok {
		delete(p.refresh, token)
		p.revoked = append(p.revoked, token)
	}
	w.WriteHeader(http.StatusOK)
}

// Revoked is the refresh tokens the client revoked, in order.
func (p *Provider) Revoked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.revoked)
}

// IDTokens is every ID token issued, in order.
func (p *Provider) IDTokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.idTokens)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, r.PostForm)
	switch {
	case p.b.DropConnections:
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			p.t.Errorf("authtest: hijacking: %v", err)
			return
		}
		_ = conn.Close()
		return
	case p.b.TokenEcho != 0:
		p.writeEcho(w, r, secret)
		return
	case p.b.TokenStatus != 0:
		if !p.writeFailBody(w, p.b.TokenStatus) {
			writeJSON(w, p.b.TokenStatus, map[string]string{"error": "server_error"})
		}
		return
	}
	if id != p.clientID || secret != p.clientSecret() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
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
		p.issue(w, g.subject, g.nonce, g.offline, true)
	case "refresh_token":
		old := r.PostForm.Get("refresh_token")
		subject, ok := p.refresh[old]
		if ok && p.b.OnRefresh != nil && p.b.OnRefresh(r) {
			ok = false
		}
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "refresh token is not valid"})
			return
		}
		delete(p.refresh, old) // rotation: each refresh token works once
		p.refreshes++
		p.issue(w, subject, "", true, !p.b.NoIDTokenOnRefresh)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

// issue answers a token request for subject. The caller holds p.mu.
func (p *Provider) issue(w http.ResponseWriter, subject, nonce string, offline, withIDToken bool) {
	u := p.users[subject]
	access := "at-" + randomToken()
	p.access[access] = subject
	resp := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300}
	if withIDToken {
		now := p.Now()
		atHash := sha256.Sum256([]byte(access))
		claims := map[string]any{
			"iss": p.Issuer(), "sub": subject, "aud": p.clientID,
			"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
			"name": u.Name, "email": u.Email, "preferred_username": u.username(),
			"at_hash": base64.RawURLEncoding.EncodeToString(atHash[:16]),
		}
		if nonce != "" {
			claims["nonce"] = nonce
		}
		if p.b.MutateIDToken != nil {
			p.b.MutateIDToken(claims)
		}
		idToken, err := p.sign(claims)
		if err != nil {
			p.t.Errorf("authtest: signing an ID token: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		resp["id_token"] = idToken
		p.idTokens = append(p.idTokens, idToken)
	}
	if offline && !p.b.NoRefreshToken {
		rt := "rt-" + randomToken()
		p.refresh[rt] = subject
		resp["refresh_token"] = rt
	}
	writeJSON(w, http.StatusOK, resp)
}

// clientSecret is the secret the token endpoint accepts. The caller holds
// p.mu.
func (p *Provider) clientSecret() string {
	if p.b.ClientSecret != "" {
		return p.b.ClientSecret
	}
	return p.secret
}

// keyID is the signing key's ID. The caller holds p.mu.
func (p *Provider) keyID() string {
	if p.b.KeyID != "" {
		return p.b.KeyID
	}
	return "authtest"
}

// sign signs an ID token. The caller holds p.mu.
func (p *Provider) sign(claims map[string]any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	if p.b.SignAlg != "" {
		return p.signUnsafe(payload)
	}
	key := p.key
	if p.b.SignWithUnknownKey {
		key = p.otherKey
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: p.keyID()}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return jws.CompactSerialize()
}

// signUnsafe makes the ID tokens an attacker would try: alg none, or HS256
// keyed with the client secret or public key. The caller holds p.mu.
func (p *Provider) signUnsafe(payload []byte) (string, error) {
	enc := base64.RawURLEncoding.EncodeToString
	alg := "HS256"
	if p.b.SignAlg == "none" {
		alg = "none"
	}
	header, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": p.keyID()})
	input := enc(header) + "." + enc(payload)
	var key []byte
	switch p.b.SignAlg {
	case "none":
		return input + ".", nil
	case "HS256-secret":
		key = []byte(p.clientSecret())
	case "HS256-jwk":
		var err error
		key, err = json.Marshal(jose.JSONWebKey{Key: &p.key.PublicKey, KeyID: p.keyID(), Algorithm: "RS256", Use: "sig"})
		if err != nil {
			return "", err
		}
	default:
		p.t.Fatalf("authtest: unknown SignAlg %q", p.b.SignAlg)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(input))
	return input + "." + enc(m.Sum(nil)), nil
}

func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.b.UserinfoStatus != 0 {
		if !p.writeFailBody(w, p.b.UserinfoStatus) {
			writeJSON(w, p.b.UserinfoStatus, map[string]string{"error": "unavailable"})
		}
		return
	}
	subject, ok := p.access[tok]
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	u := p.users[subject]
	info := map[string]any{"sub": subject, "name": u.Name, "email": u.Email, "preferred_username": u.username()}
	if p.b.UserinfoSubject != "" {
		info["sub"] = p.b.UserinfoSubject
	}
	writeJSON(w, http.StatusOK, info)
}

// writeFailBody writes FailBody raw with status, as a proxy or WAF page
// would, and reports whether there was one. The caller holds p.mu.
func (p *Provider) writeFailBody(w http.ResponseWriter, status int) bool {
	if p.b.FailBody == "" {
		return false
	}
	ct := p.b.FailContentType
	if ct == "" {
		ct = "text/html"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, p.b.FailBody)
	return true
}

// writeEcho answers a token request with an error body that repeats what
// was sent, secret included, as a careless proxy or provider might. The
// caller holds p.mu.
func (p *Provider) writeEcho(w http.ResponseWriter, r *http.Request, secret string) {
	echo := EchoMarker + " secret=" + secret + " form=" + r.PostForm.Encode()
	if p.b.TokenEchoJSON {
		writeJSON(w, p.b.TokenEcho, map[string]string{"error": "invalid_grant", "error_description": "denied", "echo": echo})
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(p.b.TokenEcho)
	_, _ = io.WriteString(w, "<html>"+echo+"</html>")
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
