package auth

import "errors"

// Refresh tokens are sealed at rest so a database leak doesn't hand out
// tokens that mint ID tokens. The key derives from oidc.client_secret, so
// replicas share it without extra config; rotating the secret makes
// sessions fail their next refresh. Each seal is bound to its session.
const refreshTokenPurpose = "refresh token v1"

var errSealedRefreshToken = errors.New("the stored refresh token can't be opened (was oidc.client_secret changed?)")

// sealRefreshToken seals a refresh token for session sessionID.
func (s *Service) sealRefreshToken(sessionID, token string) string {
	return s.refreshKey.Seal(sessionID, []byte(token))
}

// sealIDToken seals an ID token for session sessionID; "" stays "".
func (s *Service) sealIDToken(sessionID, token string) string {
	if token == "" {
		return ""
	}
	return s.refreshKey.Seal(idTokenLabel+sessionID, []byte(token))
}

// openIDToken reverses sealIDToken; "" if there is none or it won't open.
func (s *Service) openIDToken(sessionID, stored string) string {
	if stored == "" {
		return ""
	}
	plain, err := s.refreshKey.Open(idTokenLabel+sessionID, stored)
	if err != nil {
		return ""
	}
	return string(plain)
}

// idTokenLabel binds an ID token's seal apart from the refresh token's, so
// one can't be opened as the other.
const idTokenLabel = "id token "

// openRefreshToken reverses sealRefreshToken for the same session. An empty
// token never opens.
func (s *Service) openRefreshToken(sessionID, stored string) (string, error) {
	plain, err := s.refreshKey.Open(sessionID, stored)
	if err != nil || len(plain) == 0 {
		return "", errSealedRefreshToken
	}
	return string(plain), nil
}
