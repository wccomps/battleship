package main

import (
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// asUser is api acting as cred's person: a client view that sends it and
// calls refused (if set) whenever Proxmox refuses it. Other APIs (test fakes)
// are returned as is.
func asUser(api pods.API, cred func() proxmox.Credential, refused func()) pods.API {
	if c, ok := api.(*proxmox.Client); ok {
		return c.AsSource(cred).WhenRefused(refused)
	}
	return api
}
