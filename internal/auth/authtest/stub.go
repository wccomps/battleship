package authtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wccomps/battleship/internal/config"
)

// StubProvider starts an OpenID provider that answers only discovery, which
// is enough for auth.NewService to start, and points cfg's [oidc] section at
// it with a test client ID and secret. Nobody can log in through it: its
// other endpoints refuse everything. Tests log users in with Login. The
// provider stops when the test ends.
func StubProvider(t testing.TB, cfg *config.Config) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer := srv.URL + "/"
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                srv.URL + "/authorize",
			"token_endpoint":                        srv.URL + "/token",
			"userinfo_endpoint":                     srv.URL + "/userinfo",
			"jwks_uri":                              srv.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"authtest's stub provider logs nobody in"}`))
	})
	cfg.OIDC.Issuer = issuer
	cfg.OIDC.ClientID = "battleship-test"
	cfg.OIDC.ClientSecret = "authtest-client-secret"
}
