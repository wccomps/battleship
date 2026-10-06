package auth

import "errors"

// Refresh tokens are sealed at rest (internal/seal), so a database leak (a
// backup, a replica, a stray dump) doesn't hand out tokens that mint new ID
// tokens. The key is derived from oidc.client_secret with purpose
// refreshTokenPurpose, so every replica shares it without extra config. The
// seal is bound to the session's store ID (the token hash), so a sealed
// value copied into another session's row doesn't open.
//
// Rotating the client secret changes the key: stored tokens no longer open,
// and those sessions are rejected at their next refresh (the user logs in
// again).
const refreshTokenPurpose = "refresh token v1"

var errSealedRefreshToken = errors.New("the stored refresh token can't be opened (was oidc.client_secret changed?)")

// sealRefreshToken seals a refresh token for the session with store ID
// sessionID.
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
