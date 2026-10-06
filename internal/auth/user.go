// Package auth decides who is signed in to battleship, and as whom it acts
// in Proxmox.
//
// Who may open battleship at all is decided in Authentik (the application's
// policy bindings); inside, every signed-in person is a user, and Proxmox
// decides what each may do, with their own ticket (see proxmoxlogin.go).
// Battleship has no roles of its own.
//
// Service logs users in with OpenID Connect (authorization code with PKCE),
// signs them in to Proxmox through its OpenID realm, keeps sessions in the
// store keyed by the hash of the cookie token, re-checks the user with the
// identity provider every web.session_refresh with the refresh token, and
// guards handlers: RequireUser and the CSRF check.
package auth

// User is a signed-in person.
type User struct {
	Subject string // the identity provider's stable user ID
	Name    string
	Email   string
}
