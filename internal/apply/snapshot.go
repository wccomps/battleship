package apply

import (
	"context"
	"fmt"
	"slices"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// takeSnapshot takes the item's snapshot, never replacing one: an existing
// snapshot of the name fails unless this run asked for it (see
// createSnapshot). The result is checked to be present and finished.
func (e *Executor) takeSnapshot(ctx context.Context, it *pods.Item) (bool, error) {
	find := func() (snap proxmox.Snapshot, found bool, err error) {
		var snaps []proxmox.Snapshot
		if err := e.call(ctx, func() (err error) { snaps, err = e.api.Snapshots(ctx, it.Node, it.VMID); return }); err != nil {
			return proxmox.Snapshot{}, false, err
		}
		i := slices.IndexFunc(snaps, func(s proxmox.Snapshot) bool { return s.Name == it.Snapshot })
		if i < 0 {
			return proxmox.Snapshot{}, false, nil
		}
		return snaps[i], true, nil
	}
	snap, found, err := find()
	if err != nil {
		return false, err
	}
	if found && !e.askedFor(it.VMID, it.Snapshot) {
		return false, fmt.Errorf("already has a snapshot named %q, taken since the preview; battleship never replaces a snapshot", it.Snapshot)
	}
	if !found {
		err := e.createSnapshot(ctx, it.Node, it.VMID, proxmox.SnapshotRequest{
			Name: it.Snapshot, Description: it.Description, VMState: it.VMState,
		})
		if err != nil {
			return false, err
		}
		if snap, found, err = find(); err != nil {
			return false, err
		}
	}
	switch {
	case !found:
		return false, fmt.Errorf("the snapshot task ended, but %s has no snapshot %q", it.Name, it.Snapshot)
	case snap.State != "":
		return false, fmt.Errorf("snapshot %q of %s was left unfinished (snapstate %s); delete it in Proxmox, then retry", it.Snapshot, it.Name, snap.State)
	}
	return true, nil
}

// createSnapshot takes a snapshot through task. After this run's first
// request for the name, each one first checks whether an earlier request
// took it, so a lost answer doesn't cause an "already used" resend.
func (e *Executor) createSnapshot(ctx context.Context, node string, vmid int, req proxmox.SnapshotRequest) error {
	return e.task(ctx, func() (string, error) {
		if e.askedFor(vmid, req.Name) {
			snaps, err := e.api.Snapshots(ctx, node, vmid)
			if err != nil {
				return "", err
			}
			if slices.Contains(pods.SnapshotNames(snaps), req.Name) {
				return "", nil // an earlier request took it
			}
		}
		e.askFor(vmid, req.Name)
		return e.api.CreateSnapshot(ctx, node, vmid, req)
	})
}

// askFor records that this run sent a request for a VM's snapshot.
func (e *Executor) askFor(vmid int, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.asked[vmid] = append(e.asked[vmid], name)
}

// askedFor reports whether this run sent a request for a VM's snapshot.
func (e *Executor) askedFor(vmid int, name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Contains(e.asked[vmid], name)
}
