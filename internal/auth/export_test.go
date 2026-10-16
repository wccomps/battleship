package auth

import (
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// OpenRefreshToken opens a stored (sealed) refresh token of the session
// with store ID sessionID, for tests.
func OpenRefreshToken(s *Service, sessionID, stored string) (string, error) {
	return s.openRefreshToken(sessionID, stored)
}

// OpenSessionTicket opens a session's sealed Proxmox ticket, as the
// service does.
func OpenSessionTicket(s *Service, sess store.Session) (proxmox.Credential, bool) {
	return s.openTicket(sess)
}
