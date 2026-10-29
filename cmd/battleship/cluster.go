package main

import (
	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// cluster is the credential-less Proxmox connection; every API call goes
// through Bind.
type cluster interface {
	auth.ProxmoxLogin
	// Bind acts as cred's person; refused, if set, runs on each 401.
	Bind(cred func() proxmox.Credential, refused func()) pods.API
}

type proxmoxCluster struct {
	auth.ProxmoxLogin
	client *proxmox.Client
}

func (c proxmoxCluster) Bind(cred func() proxmox.Credential, refused func()) pods.API {
	return c.client.AsSource(cred).WhenRefused(refused)
}
