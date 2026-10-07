package main

import (
	"errors"
	"os"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// errNoUserToken explains how the CLI authenticates.
var errNoUserToken = errors.New("Battleship acts as you: set BATTLESHIP_PROXMOX_TOKEN_ID (e.g. jdoe@auth.example.org!cli) and " +
	"BATTLESHIP_PROXMOX_TOKEN_SECRET to your own Proxmox API token, ideally privilege-separated with only the privileges you need")

// userToken is the CLI user's own Proxmox API token, from the environment.
// The CLI never acts as anyone else.
func userToken() (proxmox.Credential, error) {
	id, secret := os.Getenv("BATTLESHIP_PROXMOX_TOKEN_ID"), os.Getenv("BATTLESHIP_PROXMOX_TOKEN_SECRET")
	if id == "" || secret == "" {
		return proxmox.Credential{}, errNoUserToken
	}
	return proxmox.TokenCredential(id, secret), nil
}

// asUser is api acting as cred's person: a client view that sends it and
// calls refused (if set) whenever Proxmox refuses it. Other APIs (test fakes)
// are returned as is.
func asUser(api pods.API, cred func() proxmox.Credential, refused func()) pods.API {
	if c, ok := api.(*proxmox.Client); ok {
		return c.AsSource(cred).WhenRefused(refused)
	}
	return api
}

// bindAs is a jobs.Worker's Bind over api.
func bindAs(api pods.API) func(cred func() proxmox.Credential, refused func()) pods.API {
	return func(cred func() proxmox.Credential, refused func()) pods.API { return asUser(api, cred, refused) }
}

// asToken is asUser with a fixed credential.
func asToken(api pods.API, cred proxmox.Credential) pods.API {
	return asUser(api, func() proxmox.Credential { return cred }, nil)
}
