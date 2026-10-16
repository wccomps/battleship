package pods

import (
	"context"

	"github.com/wccomps/battleship/internal/proxmox"
)

// vmidUse answers whether a VMID is in use: by a VM in the cluster listing,
// or by one the user can't see, which only VMIDHeld reveals (the listing
// shows what the user may audit).
type vmidUse struct {
	api    API
	listed map[int]bool
}

func newVMIDUse(api API, vms []proxmox.VM) vmidUse {
	listed := make(map[int]bool, len(vms))
	for _, vm := range vms {
		listed[vm.VMID] = true
	}
	return vmidUse{api, listed}
}

// inUse is whether vmid is in use, asking Proxmox only if it isn't listed.
func (u vmidUse) inUse(ctx context.Context, vmid int) (bool, error) {
	if u.listed[vmid] {
		return true, nil
	}
	return u.api.VMIDHeld(ctx, vmid)
}
