package apply

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// deleteVM destroys a VM and reports success only once the VM is confirmed
// gone and none of its disks are left on the storage. The destroy holds a
// concurrency.deletes slot for its whole task (see Limits).
//
// A destroy that failed after deleting part of the VM (proxmox.PartialDestroy)
// is not started again, and a VM left locked as destroyed fails with a
// halfDeletedError that says how to finish it; one found so before the delete
// gets no request. Each request is preceded by a read of the VM, so one sent
// again after its answer was lost sees the first one's effect: the VM gone,
// or locked as destroyed by that destroy, which gets up to destroyWait.
//
// A destroy task can also end OK with its disks still on the storage: it
// timed out on the storage lock while freeing them and carried on. So the
// VM's disks are read before the delete, and any of them still there
// afterwards are freed (see freeLeftovers).
func (e *Executor) deleteVM(ctx context.Context, node string, vmid int, name string) error {
	// A failed read is not fatal: the VM may already be gone. Not knowing
	// whether it is a template, the destroy takes the storage-ops slot as
	// one would: unneeded, that only makes it wait its turn.
	var disks map[string]string
	template := false
	switch cfg, gone, err := e.readVM(ctx, node, vmid); {
	case err != nil:
		template = true
	case gone:
	case cfg["lock"] == "destroyed":
		return &halfDeletedError{vmid: vmid, node: node}
	default:
		disks = pods.DiskVolumes(cfg)
		template = cfg["template"] == "1"
	}
	destroying := false // an earlier request's destroy is running
	destroy := func() error {
		err := e.task(ctx, func() (string, error) {
			cfg, err := e.api.VMConfig(ctx, node, vmid)
			switch {
			case err != nil:
				// "does not exist" goes to the check after the delete,
				// which tells a VM that is gone from one on another node.
				return "", err
			case cfg["lock"] == "destroyed":
				destroying = true
				return "", nil
			}
			return e.api.DeleteVM(ctx, node, vmid)
		})
		if destroying {
			e.waitGone(ctx, node, vmid, destroyWait)
		}
		return err
	}
	err := e.lim.deletes.do(ctx, func() error {
		if template {
			// Destroying a template frees its base disks under the storage
			// lock, like a conversion: one at a time (see Limits).
			return e.lim.storageOps.do(ctx, destroy)
		}
		return destroy()
	})
	if err != nil && ctx.Err() != nil {
		return err
	}
	cfg, gone, rerr := e.readVM(ctx, node, vmid)
	if gone && err != nil {
		// The delete failed, so "does not exist" on node may only mean the VM
		// is on another node now.
		if lerr := e.refresh(ctx); lerr != nil {
			return fmt.Errorf("the delete failed and VM %d is not on %s; could not list VMs to check where it is: %w (the delete: %s)",
				vmid, node, lerr, proxmox.Describe(err))
		}
		e.mu.Lock()
		vm, listed := e.present[vmid]
		e.mu.Unlock()
		if listed {
			return fmt.Errorf("VM %d is listed on node %s, not %s, so it was not deleted: %w", vmid, vm.Node, node, err)
		}
	}
	switch {
	case gone:
		return e.freeLeftovers(ctx, vmid, name, leftover{node: node, disks: disks, template: template})
	case rerr != nil && err != nil:
		return err
	case rerr != nil:
		return fmt.Errorf("the delete task finished, but could not confirm VM %d is gone: %w", vmid, rerr)
	case cfg["lock"] == "destroyed":
		return &halfDeletedError{vmid: vmid, node: node, cause: err}
	case err != nil:
		return err
	}
	return fmt.Errorf("the delete task for VM %d finished OK, but the VM still exists", vmid)
}

// destroyWait bounds how long a delete waits for a destroy that an earlier
// request of its own started.
const destroyWait = 5 * time.Minute

// waitGone polls the VM until it is gone, for up to limit.
func (e *Executor) waitGone(ctx context.Context, node string, vmid int, limit time.Duration) {
	wctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	e.pollVM(wctx, node, vmid, func(cfg map[string]string) bool { return cfg == nil })
}

// leftover is what a deleted VM left on the storage: its node and the disks
// it had, for a retry round to free what this one could not.
type leftover struct {
	node     string
	disks    map[string]string
	template bool // frees take the storage-ops slot
}

// freeLeftovers frees those of a deleted VM's disks that are still on the
// storage. Proxmox itself refuses to free a base volume that linked clones
// still use, so this never breaks a clone; that refusal is reported as is.
// What can't be freed fails with a leftoverDisksError, and is remembered so
// a retry round that finds the VM gone tries again rather than calling it
// already deleted. A stop before they are freed is an interruption.
func (e *Executor) freeLeftovers(ctx context.Context, vmid int, name string, l leftover) error {
	node, disks := l.node, l.disks
	if len(disks) == 0 {
		e.forgetLeftover(vmid)
		return nil
	}
	left, err := e.leftDisks(ctx, node, vmid, disks)
	if err != nil {
		e.rememberLeftover(vmid, l)
		return fmt.Errorf("VM %d is deleted, but checking the storage for disks it left behind failed: %w", vmid, err)
	}
	if len(left) == 0 {
		e.forgetLeftover(vmid)
		return nil
	}
	// A VM created at vmid since gets disks with the same names.
	if err := e.refresh(ctx); err != nil {
		e.rememberLeftover(vmid, l)
		return fmt.Errorf("VM %d is deleted, but could not list VMs to check that VMID %d is still free before freeing the disks it left: %w", vmid, vmid, err)
	}
	e.mu.Lock()
	holder, taken := e.present[vmid]
	e.mu.Unlock()
	if !taken { // a VM the user can't see doesn't show in the list
		var held bool
		if err := e.call(ctx, func() (err error) { held, err = e.api.VMIDHeld(ctx, vmid); return }); err != nil {
			e.rememberLeftover(vmid, l)
			return fmt.Errorf("VM %d is deleted, but could not check that VMID %d is still free before freeing the disks it left: %w", vmid, vmid, err)
		}
		taken, holder.Name = held, "a VM you can't see"
	}
	if taken {
		e.rememberLeftover(vmid, l)
		return &leftoverDisksError{vmid: vmid, node: node, vols: left, holder: holder.Name}
	}
	var causes []string
	for _, vol := range left {
		storage, _, _ := strings.Cut(vol, ":")
		free := func() error {
			return e.taskOnce(ctx, func() (string, error) { return e.api.DeleteVolume(ctx, node, storage, vol) })
		}
		var err error
		if l.template {
			err = e.lim.storageOps.do(ctx, free)
		} else {
			err = free()
		}
		if errors.Is(err, context.Canceled) { // a stop: nothing more is sent
			e.rememberLeftover(vmid, l)
			e.emit(name, pods.StepDelete, EventInfo, fmt.Sprintf("VM %d is deleted; the job was stopped before it freed the disks the delete left on the storage: %s", vmid, strings.Join(left, ", ")))
			return fmt.Errorf("VM %d is deleted; freeing the disks it left: %w", vmid, err)
		}
		if err != nil {
			causes = append(causes, vol+": "+proxmox.Describe(err))
		}
	}
	// Check again rather than trust each free: a timed-out request may still
	// have freed its volume.
	still, err := e.leftDisks(ctx, node, vmid, disks)
	if err != nil {
		still = left
		causes = append(causes, "checking what is left: "+proxmox.Describe(err))
	}
	if freed := minus(left, still); len(freed) > 0 {
		e.emit(name, pods.StepDelete, EventInfo, fmt.Sprintf("freed disks that deleting VM %d left on the storage: %s", vmid, strings.Join(freed, ", ")))
	}
	if len(still) > 0 {
		e.rememberLeftover(vmid, l)
		return &leftoverDisksError{vmid: vmid, node: node, vols: still, causes: causes}
	}
	e.forgetLeftover(vmid)
	return nil
}

// leftDisks lists which of disks are still on their storage for vmid, by
// the volids the storage reports. Volumes are matched on their file name,
// which is unique per VMID and storage.
func (e *Executor) leftDisks(ctx context.Context, node string, vmid int, disks map[string]string) ([]string, error) {
	files := map[string]map[string]bool{} // by storage
	for _, vol := range disks {
		storage, _, _ := strings.Cut(vol, ":")
		if files[storage] == nil {
			files[storage] = map[string]bool{}
		}
		files[storage][pods.VolumeFile(vol)] = true
	}
	var left []string
	for _, storage := range slices.Sorted(maps.Keys(files)) {
		var vols []string
		if err := e.call(ctx, func() (err error) { vols, err = e.api.StorageContent(ctx, node, storage, vmid); return }); err != nil {
			return nil, fmt.Errorf("listing %s: %w", storage, err)
		}
		for _, v := range vols {
			if files[storage][pods.VolumeFile(v)] {
				left = append(left, v)
			}
		}
	}
	sort.Strings(left)
	return left, nil
}

// minus returns the elements of a that are not in b.
func minus(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func (e *Executor) rememberLeftover(vmid int, l leftover) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.leftovers[vmid] = l
}

func (e *Executor) forgetLeftover(vmid int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.leftovers, vmid)
}

// retryLeftovers frees what an earlier delete of vmid left on the storage,
// if anything: this run's, as remembered, or else whatever the deploy
// storage, asked on node, holds of vmid's while no VM holds the VMID.
func (e *Executor) retryLeftovers(ctx context.Context, vmid int, node, name string) error {
	e.mu.Lock()
	l, ok := e.leftovers[vmid]
	_, held := e.present[vmid]
	e.mu.Unlock()
	if !ok {
		if held {
			return nil // the disks at vmid are its holder's
		}
		storage := e.Cfg.Deploy.Storage
		var vols []string
		if err := e.call(ctx, func() (err error) { vols, err = e.api.StorageContent(ctx, node, storage, vmid); return }); err != nil {
			return fmt.Errorf("checking %s for disks a deleted VM %d left: %w", storage, vmid, err)
		}
		l = leftover{node: node, disks: map[string]string{}}
		for _, v := range vols {
			if owner, base := pods.VolumeOwner(v); owner == vmid {
				l.disks[v] = v
				l.template = l.template || base
			}
		}
	}
	return e.freeLeftovers(ctx, vmid, name, l)
}
