package auth_test

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
)

// clock is the fake time shared by the app and the fake identity provider.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// idpUser is a person at the fake identity provider.
type idpUser struct {
	Sub, Name, Email string
	Groups           []string
}

type authCode struct {
	sub, nonce, challenge, redirectURI, scope string
}

// fakeIDP is an OpenID Connect provider on httptest: discovery, JWKS, the
// authorization endpoint (which logs in whoever is set as current), the
// token endpoint (authorization code with PKCE, and refresh with rotation),
// userinfo and end_session.
type fakeIDP struct {
	t        *testing.T
	srv      *httptest.Server
	clock    *clock
	key      *rsa.PrivateKey
	otherKey *rsa.PrivateKey // not in the JWKS
	clientID string
	secret   string

	mu      sync.Mutex
	current string              // subject that "logs in" at /authorize
	users   map[string]*idpUser // by subject
	codes   map[string]authCode
	refresh map[string]string // live refresh token -> subject
	access  map[string]string // access token -> subject

	// Knobs.
	noRefreshToken   bool                              // token responses carry no refresh_token
	noEndSession     bool                              // discovery lists no end_session_endpoint
	groupsInIDToken  bool                              // ID tokens carry the groups claim
	noIDTokenRefresh bool                              // refresh responses carry no id_token
	mutateIDToken    func(claims map[string]any)       // edits ID token claims before signing
	signWithOther    bool                              // sign ID tokens with a key not in the JWKS
	userinfoSub      string                            // overrides userinfo's sub
	authorizeError   string                            // /authorize redirects back with this error
	refreshCalls     int                               // successful refresh grants
	tokenRequests    []url.Values                      // every token request form
	onRefresh        func(r *http.Request) (fail bool) // runs inside the refresh grant
	tokenStatus      int                               // non-zero: the token endpoint fails with this status
	jwksStatus       int                               // non-zero: the JWKS endpoint fails with this status
	kid              string                            // key ID in the JWKS and ID tokens; change it to rotate
	dropConnections  bool                              // the token endpoint closes connections without answering
	userinfoStatus   int                               // non-zero: userinfo fails with this status
	failBody         string                            // with userinfoStatus, jwksStatus or tokenStatus: the raw body sent
	failContentType  string                            // with failBody: its Content-Type (default text/html)
	tokenEcho        int                               // non-zero: the token endpoint fails with this status and a body echoing the request
	tokenEchoJSON    bool                              // with tokenEcho: the echo is inside an OAuth invalid_grant error
	signAlg          string                            // "", "none", "HS256-secret" (HMAC with the client secret) or "HS256-jwk" (HMAC with the public key)
	advertiseAlgs    []string                          // extra id_token_signing_alg_values_supported
}

// echoMarker is in every echoed token endpoint body.
const echoMarker = "ECHOBODY"

// writeEcho answers a token request with an error body that repeats what
// was sent, secret included, as a careless proxy or provider might.
func (f *fakeIDP) writeEcho(w http.ResponseWriter, r *http.Request, secret string) {
	echo := echoMarker + " secret=" + secret + " form=" + r.PostForm.Encode()
	if f.tokenEchoJSON {
		writeJSON(w, f.tokenEcho, map[string]string{"error": "invalid_grant", "error_description": "denied", "echo": echo})
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(f.tokenEcho)
	_, _ = io.WriteString(w, "<html>"+echo+"</html>")
}

// writeFailBody writes failBody raw with status, as a proxy or WAF page
// would, and reports whether there was one. The caller holds f.mu.
func (f *fakeIDP) writeFailBody(w http.ResponseWriter, status int) bool {
	if f.failBody == "" {
		return false
	}
	ct := f.failContentType
	if ct == "" {
		ct = "text/html"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, f.failBody)
	return true
}

// signingKeys are generated once: RSA key generation is slow, and every
// fake provider can share them.
var signingKeys = sync.OnceValues(func() ([2]*rsa.PrivateKey, error) {
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

func newFakeIDP(t *testing.T, c *clock) *fakeIDP {
	t.Helper()
	keys, err := signingKeys()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIDP{
		t: t, clock: c, key: keys[0], otherKey: keys[1],
		clientID: "battleship", secret: "idp-client-secret",
		users: map[string]*idpUser{}, codes: map[string]authCode{},
		refresh: map[string]string{}, access: map[string]string{},
		groupsInIDToken: true, kid: "k1",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /jwks", f.jwks)
	mux.HandleFunc("GET /authorize", f.authorize)
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("GET /userinfo", f.userinfo)
	mux.HandleFunc("GET /end-session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("logged out of the identity provider"))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIDP) issuer() string { return f.srv.URL + "/" }

// setUser adds or replaces a user and makes them the one who logs in next.
func (f *fakeIDP) setUser(u idpUser) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := u
	cp.Groups = slices.Clone(u.Groups)
	f.users[u.Sub] = &cp
	f.current = u.Sub
}

// setGroups changes a user's groups, as an admin editing Authentik would.
func (f *fakeIDP) setGroups(sub string, groups ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[sub].Groups = groups
}

// revokeAll invalidates every refresh token, as disabling a user would.
func (f *fakeIDP) revokeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.refresh)
}

func (f *fakeIDP) set(fn func(f *fakeIDP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeIDP) refreshTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for tok := range f.refresh {
		out = append(out, tok)
	}
	return out
}

func (f *fakeIDP) tokenRequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokenRequests)
}

func (f *fakeIDP) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls
}

func (f *fakeIDP) discovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"issuer":                                f.issuer(),
		"authorization_endpoint":                f.srv.URL + "/authorize",
		"token_endpoint":                        f.srv.URL + "/token",
		"jwks_uri":                              f.srv.URL + "/jwks",
		"userinfo_endpoint":                     f.srv.URL + "/userinfo",
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
	}
	f.mu.Lock()
	doc["id_token_signing_alg_values_supported"] = append([]string{"RS256"}, f.advertiseAlgs...)
	if !f.noEndSession {
		doc["end_session_endpoint"] = f.srv.URL + "/end-session"
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, doc)
}

func (f *fakeIDP) jwks(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	status, kid := f.jwksStatus, f.kid
	if status != 0 && f.writeFailBody(w, status) {
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()
	if status != 0 {
		http.Error(w, "unavailable", status)
		return
	}
	writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &f.key.PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig",
	}}})
}

func (f *fakeIDP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("client_id") != f.clientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		!slices.Contains(strings.Fields(q.Get("scope")), "openid") {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	back := redirect.Query()
	back.Set("state", q.Get("state"))
	f.mu.Lock()
	if f.authorizeError != "" {
		back.Set("error", f.authorizeError)
		back.Set("error_description", "the user said no")
	} else {
		code := randHex(16)
		f.codes[code] = authCode{sub: f.current, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
			redirectURI: q.Get("redirect_uri"), scope: q.Get("scope")}
		back.Set("code", code)
	}
	f.mu.Unlock()
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (f *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenRequests = append(f.tokenRequests, r.PostForm)
	if f.dropConnections {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			f.t.Errorf("hijacking: %v", err)
			return
		}
		conn.Close()
		return
	}
	if f.tokenEcho != 0 {
		f.writeEcho(w, r, secret)
		return
	}
	if f.tokenStatus != 0 {
		if !f.writeFailBody(w, f.tokenStatus) {
			writeJSON(w, f.tokenStatus, map[string]string{"error": "server_error"})
		}
		return
	}
	if id != f.clientID || secret != f.secret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		code, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code")) // single use
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || code.redirectURI != r.PostForm.Get("redirect_uri") ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != code.challenge {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		offline := slices.Contains(strings.Fields(code.scope), "offline_access")
		f.issue(w, code.sub, code.nonce, offline, true)
	case "refresh_token":
		old := r.PostForm.Get("refresh_token")
		sub, ok := f.refresh[old]
		if ok && f.onRefresh != nil && f.onRefresh(r) {
			ok = false
		}
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "refresh token is not valid"})
			return
		}
		delete(f.refresh, old) // rotation: each refresh token works once
		f.refreshCalls++
		f.issue(w, sub, "", true, !f.noIDTokenRefresh)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

// issue writes a token response. The caller holds f.mu.
func (f *fakeIDP) issue(w http.ResponseWriter, sub, nonce string, offline, withIDToken bool) {
	u := f.users[sub]
	access := "at-" + randHex(16)
	f.access[access] = sub
	resp := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300}
	if withIDToken {
		now := f.clock.Now()
		atHash := sha256.Sum256([]byte(access))
		claims := map[string]any{
			"iss": f.issuer(), "sub": sub, "aud": f.clientID,
			"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
			"name": u.Name, "email": u.Email, "preferred_username": strings.TrimPrefix(u.Sub, "sub-"),
			"at_hash": base64.RawURLEncoding.EncodeToString(atHash[:16]),
		}
		if nonce != "" {
			claims["nonce"] = nonce
		}
		if f.groupsInIDToken {
			claims["groups"] = slices.Clone(u.Groups)
		}
		if f.mutateIDToken != nil {
			f.mutateIDToken(claims)
		}
		resp["id_token"] = f.sign(claims)
	}
	if offline && !f.noRefreshToken {
		rt := "rt-" + randHex(16)
		f.refresh[rt] = sub
		resp["refresh_token"] = rt
	}
	writeJSON(w, http.StatusOK, resp)
}

func (f *fakeIDP) sign(claims map[string]any) string {
	if f.signAlg != "" {
		return f.signUnsafe(claims)
	}
	key := f.key
	if f.signWithOther {
		key = f.otherKey
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: f.kid}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		f.t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func (f *fakeIDP) userinfo(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	sub, known := f.access[tok]
	if f.userinfoStatus != 0 {
		if !f.writeFailBody(w, f.userinfoStatus) {
			writeJSON(w, f.userinfoStatus, map[string]string{"error": "unavailable"})
		}
		return
	}
	if !ok || !known {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	u := f.users[sub]
	info := map[string]any{"sub": sub, "name": u.Name, "email": u.Email, "groups": u.Groups, "preferred_username": strings.TrimPrefix(sub, "sub-")}
	if f.userinfoSub != "" {
		info["sub"] = f.userinfoSub
	}
	writeJSON(w, http.StatusOK, info)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// signUnsafe makes the ID tokens an attacker would try: unsigned
// (alg none), or HS256 keyed with the client secret or with the provider's
// public key (algorithm confusion). The caller holds f.mu.
func (f *fakeIDP) signUnsafe(claims map[string]any) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	alg := "HS256"
	if f.signAlg == "none" {
		alg = "none"
	}
	header, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": f.kid})
	input := enc(header) + "." + enc(payload)
	var key []byte
	switch f.signAlg {
	case "none":
		return input + "."
	case "HS256-secret":
		key = []byte(f.secret)
	case "HS256-jwk":
		key, err = json.Marshal(jose.JSONWebKey{Key: &f.key.PublicKey, KeyID: f.kid, Algorithm: "RS256", Use: "sig"})
		if err != nil {
			f.t.Fatal(err)
		}
	default:
		f.t.Fatalf("unknown signAlg %q", f.signAlg)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(input))
	return input + "." + enc(m.Sum(nil))
}
