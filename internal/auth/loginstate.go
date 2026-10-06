package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wccomps/battleship/internal/seal"
)

// loginState is a login in progress, kept in the browser between
// /auth/login and /auth/callback so any replica can finish it.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"` // PKCE code verifier
	Next     string `json:"x"` // where to go after login; already safeNext
	Issued   int64  `json:"t"` // Unix seconds
}

// clockSkew is how far in the future a login cookie may claim to be from,
// for replicas whose clocks differ slightly.
const clockSkew = time.Minute

var errBadLoginCookie = errors.New("login cookie is invalid")

// loginStatePurpose is the seal purpose of the login cookie; the seal is
// bound to the cookie's base name, loginCookie.
const loginStatePurpose = "battleship oidc login cookie v1"

// sealLoginState encrypts and authenticates st, so the browser can neither
// read the verifier and nonce nor change anything.
func sealLoginState(key seal.Key, st loginState) (string, error) {
	plain, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	return key.Seal(loginCookie, plain), nil
}

// openLoginState reverses sealLoginState and rejects states issued more
// than loginTTL before now (or from the future, beyond clock skew).
func openLoginState(key seal.Key, value string, now time.Time) (loginState, error) {
	plain, err := key.Open(loginCookie, value)
	if err != nil {
		return loginState{}, errBadLoginCookie
	}
	var st loginState
	if err := json.Unmarshal(plain, &st); err != nil || st.State == "" || st.Nonce == "" || st.Verifier == "" {
		return loginState{}, errBadLoginCookie
	}
	issued := time.Unix(st.Issued, 0)
	if now.Before(issued.Add(-clockSkew)) || !now.Before(issued.Add(loginTTL)) {
		return loginState{}, fmt.Errorf("login cookie issued at %s has expired", issued.UTC().Format(time.RFC3339))
	}
	return st, nil
}
