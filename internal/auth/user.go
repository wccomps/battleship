// Package auth decides who is signed in to battleship, and as whom it acts
// in Proxmox.
//
// Authentik decides who may open battleship; Proxmox decides what each user
// may do, with their own ticket. Battleship has no roles of its own.
package auth

// User is a signed-in person.
type User struct {
	Subject string // the identity provider's stable user ID
	Name    string
	Email   string
}
