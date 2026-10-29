package podstest

import (
	"context"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
)

// AllPrivileges are every privilege battleship's code names.
var AllPrivileges = []string{"VM.Audit", "VM.PowerMgmt", "VM.Snapshot", "VM.Snapshot.Rollback", "VM.Clone", "VM.Allocate",
	"VM.Config.Network", "VM.Config.Disk", "VM.Config.Cloudinit", "VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Options", "Datastore.AllocateSpace",
	"Datastore.Allocate", "Datastore.Audit", "Pool.Audit", "Pool.Allocate", "Sys.Audit", "Sys.Modify", "SDN.Use", "Mapping.Use",
	"VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted"}

func allPrivileges() map[string]bool {
	m := make(map[string]bool, len(AllPrivileges))
	for _, p := range AllPrivileges {
		m[p] = true
	}
	return m
}

// Permissions grants every privilege on /, propagating.
func (f *Fake) Permissions(context.Context) (proxmox.Permissions, error) {
	return proxmox.Permissions{"/": allPrivileges()}, nil
}

// PermissionsAt grants every privilege at any path.
func (f *Fake) PermissionsAt(context.Context, string) (map[string]bool, error) {
	return allPrivileges(), nil
}

// TagAccess is unrestricted.
func (f *Fake) TagAccess(context.Context) (proxmox.TagAccess, error) {
	return proxmox.TagAccess{}, nil
}

// NodeTasks lists no tasks.
func (f *Fake) NodeTasks(context.Context, string, time.Time) ([]proxmox.Task, error) {
	return nil, nil
}
