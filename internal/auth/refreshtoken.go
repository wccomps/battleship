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

// openRefreshToken reverses sealRefreshToken for the same session. An empty
// token never opens.
func (s *Service) openRefreshToken(sessionID, stored string) (string, error) {
	plain, err := s.refreshKey.Open(sessionID, stored)
	if err != nil || len(plain) == 0 {
		return "", errSealedRefreshToken
	}
	return string(plain), nil
}
