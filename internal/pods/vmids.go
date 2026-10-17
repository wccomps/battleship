package pods

import (
	"context"
	"fmt"
	"strconv"

	"github.com/wccomps/battleship/internal/proxmox"
)

// vmidUse answers whether a VMID is in use: by a VM in the cluster listing,
// by another job that will create it (Planner.Reserved), or by a VM the
// user can't see, which only VMIDHeld reveals (the listing shows what the
// user may audit).
type vmidUse struct {
	api      API
	listed   map[int]bool
	reserved map[string]bool
}

func (p Planner) vmidUse(ctx context.Context, vms []proxmox.VM) (vmidUse, error) {
	listed := make(map[int]bool, len(vms))
	for _, vm := range vms {
		listed[vm.VMID] = true
	}
	reserved, err := p.reserved(ctx)
	return vmidUse{p.API, listed, reserved}, err
}

// reserved is p.Reserved's keys, or none without it.
func (p Planner) reserved(ctx context.Context) (map[string]bool, error) {
	if p.Reserved == nil {
		return nil, nil
	}
	keys, err := p.Reserved(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading what queued jobs will create: %w", err)
	}
	return keys, nil
}

// inUse is whether vmid is in use, asking Proxmox only if nothing else says.
func (u vmidUse) inUse(ctx context.Context, vmid int) (bool, error) {
	if u.listed[vmid] || u.reserved["vmid:"+strconv.Itoa(vmid)] {
		return true, nil
	}
	return u.api.VMIDHeld(ctx, vmid)
}
