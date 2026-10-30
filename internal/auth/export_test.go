package auth

import (
	"github.com/wccomps/battleship/internal/proxmox"
)

// OpenRefreshToken opens a stored (sealed) refresh token of the session
// with store ID sessionID, for tests.
func OpenRefreshToken(s *Service, sessionID, stored string) (string, error) {
	return s.openRefreshToken(sessionID, stored)
}

func OpenTicketCookie(s *Service, sessionID, value string) (proxmox.Credential, bool) {
	return s.openTicketCookie(sessionID, value)
}
