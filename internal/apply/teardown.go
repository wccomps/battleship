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

// deleteVM destroys a VM and succeeds only once the VM is gone and none of
// its disks remain. It holds a concurrency.deletes slot for the whole task.
//
// A partially failed destroy (proxmox.PartialDestroy) is not restarted; a
// VM left locked as destroyed fails with a halfDeletedError saying how to
// finish it. Each request is preceded by a read, so a resend after a lost
// answer sees the first one's effect (allowed up to destroyWait).
//
// A destroy can end OK yet leave disks (it timed out on the storage lock
// while freeing them), so disks read beforehand and still present are freed
// (see freeLeftovers).
func (e *Executor) deleteVM(ctx context.Context, node string, vmid int, name string) error {
	// A failed read isn't fatal: the VM may be gone. Not knowing if it is a
	// template, the destroy takes the storage-ops slot anyway.
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
				// "does not exist" goes to the post-delete check, which tells gone from
				// moved to another node.
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
			// Destroying a template frees base disks under the storage lock, like a
			// conversion: one at a time (see Limits).
			return e.lim.storageOps.do(ctx, destroy)
		}
		return destroy()
	})
	if err != nil && ctx.Err() != nil {
		return err
	}
	cfg, gone, rerr := e.readVM(ctx, node, vmid)
	if gone && err != nil {
		// The delete failed, so "does not exist" may mean the VM moved nodes.
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

// destroyWait bounds how long a delete waits for a destroy its own earlier
// request started.
const destroyWait = 5 * time.Minute

// waitGone polls the VM until it is gone, for up to limit.
func (e *Executor) waitGone(ctx context.Context, node string, vmid int, limit time.Duration) {
	wctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	e.pollVM(wctx, node, vmid, func(cfg map[string]string) bool { return cfg == nil })
}

// leftover is a deleted VM's node and disks, for a retry round to free what
// this one couldn't.
type leftover struct {
	node     string
	disks    map[string]string
	template bool // frees take the storage-ops slot
}

// freeLeftovers frees a deleted VM's disks still on the storage. Proxmox
// refuses to free a base volume linked clones use, so this never breaks a
// clone; that refusal is reported as is. Unfreeable disks fail with a
// leftoverDisksError and are remembered for the next round. A stop before
// they are freed is an interruption.
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
	// Re-check rather than trust each free: a timed-out request may still have
	// freed its volume.
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

// leftDisks lists which of disks are still on storage for vmid, matched by
// file name (unique per VMID and storage).
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

// retryLeftovers frees what an earlier delete of vmid left: this run's
// remembered leftovers, or else what the deploy storage on node holds for
// vmid while no VM holds it.
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
