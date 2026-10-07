package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

func (e *Executor) runItem(ctx context.Context, kind pods.Kind, it pods.Item, tpl *tplBuild) error {
	if len(it.Steps) == 0 {
		return e.endItem(ctx, it, "", -1, false, errors.New("no steps planned"))
	}
	vm, exists := e.lookup(it.VMID, it.Name)
	if exists {
		it.Node = vm.Node
	}
	if !exists && !slices.Contains(it.Steps, pods.StepClone) {
		if kind == pods.KindTeardown {
			// StepDelete, or StepFreeDisks for a VM already gone at planning.
			step := it.Steps[len(it.Steps)-1]
			// Gone by name isn't enough: a half-deleted VM is listed as "VM <vmid>".
			if err := e.checkVMIDFree(ctx, it.VMID); err != nil {
				return e.endItem(ctx, it, step, -1, false, err)
			}
			// Gone, but an earlier attempt may have left its disks.
			if err := e.retryLeftovers(ctx, it.VMID, it.Node, it.Name); err != nil {
				return e.endItem(ctx, it, step, -1, false, err)
			}
			if step == pods.StepFreeDisks {
				e.emit(it.Name, step, EventDone, "")
			} else {
				e.emit(it.Name, step, EventSkipped, "already deleted")
			}
			return nil
		}
		return e.endItem(ctx, it, "", -1, false, fmt.Errorf("VM %s (%d) no longer exists", it.Name, it.VMID))
	}
	for i, step := range it.Steps {
		if ctx.Err() != nil {
			return e.endItem(ctx, it, step, i, false, ctx.Err())
		}
		sctx, done, sent := e.stepContext(ctx)
		changed, err := e.runStep(sctx, kind, &it, step, tpl, exists)
		done()
		e.mu.Lock()
		if step.ChangesConfig() && sent() {
			e.left[it.Name] = LeftChanged
		}
		if err == nil && !slices.ContainsFunc(it.Steps[i+1:], pods.Step.ChangesConfig) {
			e.left[it.Name] = LeftConverged
		}
		e.mu.Unlock()
		switch {
		case err != nil:
			return e.endItem(ctx, it, step, i, sent(), err)
		case changed:
			e.emit(it.Name, step, EventDone, "")
		default:
			e.emit(it.Name, step, EventSkipped, "")
		}
		if step == pods.StepClone {
			exists = true
		}
	}
	return nil
}

// endItem reports how an unfinished item ended and returns its error; it is
// the one place that tells a stop from a failure. i is the step's index
// (-1: outside the steps); underWay means the step had sent a change. Only
// a step under way is logged as the item's step, so an item that never
// reached one reads as not run.
func (e *Executor) endItem(ctx context.Context, it pods.Item, step pods.Step, i int, underWay bool, err error) error {
	if ctx.Err() == nil || !errors.Is(err, context.Canceled) {
		e.emit(it.Name, step, EventFailed, proxmox.Describe(err))
		if step != "" {
			return fmt.Errorf("%s: %w", step, err)
		}
		return err
	}
	when, at := "before "+string(step), pods.Step("")
	switch {
	case underWay:
		when, at = "during "+string(step)+"; a Proxmox task it started may still be running", step
	case step == "":
		when = "before it started"
	}
	stop := stopped(ctx, when, e.leftAt(it, i))
	e.emit(it.Name, at, EventInterrupted, stop.Error())
	return stop
}

// leftAt is how an item stopped at step i (-1: outside its steps) left its
// VM's config, else untouched. An item with no config step is converged
// once it reaches its steps.
func (e *Executor) leftAt(it pods.Item, i int) Left {
	e.mu.Lock()
	l, ok := e.left[it.Name]
	e.mu.Unlock()
	switch {
	case ok:
		return l
	case i >= 0 && !slices.ContainsFunc(it.Steps, pods.Step.ChangesConfig):
		return LeftConverged
	}
	return LeftUntouched
}

// runStep performs one step and reports whether it changed anything.
func (e *Executor) runStep(ctx context.Context, kind pods.Kind, it *pods.Item, step pods.Step, tpl *tplBuild, exists bool) (bool, error) {
	switch step {
	case pods.StepClone:
		if exists {
			return false, nil
		}
		err := e.lim.cloneSlot(tpl.node).do(ctx, func() error {
			_, err := e.taskUPID(ctx, it.VMID, func() (string, error) {
				return e.startClone(ctx, false, proxmox.CloneRequest{
					SourceNode: tpl.node, SourceVMID: tpl.spec.VMID, NewVMID: it.VMID, Name: it.Name,
					TargetNode: it.Node, Pool: e.naming.Pool(it.Team),
					Full: !e.Cfg.Deploy.Linked, Storage: e.Cfg.Deploy.Storage,
				})
			}, e.restartable)
			return err
		})
		if err == nil {
			e.setPresent(proxmox.VM{VMID: it.VMID, Name: it.Name, Node: it.Node}, true)
		}
		return err == nil, err

	case pods.StepNetwork:
		return e.applyConfig(ctx, it, true, func(cur map[string]string) map[string]string {
			return pods.NetworkChanges(e.Cfg.Network, cur, it.Team, tpl.spec.Interfaces)
		})
	case pods.StepDiskLimits:
		return e.applyConfig(ctx, it, false, func(cur map[string]string) map[string]string {
			return pods.DiskLimitChanges(cur, e.Cfg.Deploy.DiskMBpsRead, e.Cfg.Deploy.DiskMBpsWrite)
		})
	case pods.StepCDROM:
		return e.applyConfig(ctx, it, false, pods.CDROMChanges)

	case pods.StepSnapshot:
		if kind == pods.KindSnapshot {
			return e.takeSnapshot(ctx, it)
		}
		var snaps []proxmox.Snapshot
		if err := e.call(ctx, func() (err error) { snaps, err = e.api.Snapshots(ctx, it.Node, it.VMID); return }); err != nil {
			return false, err
		}
		// Any baseline will do: one taken now would capture whatever the VM has
		// been through.
		if pods.HasBaseline(e.Cfg.Deploy, pods.SnapshotNames(snaps)) {
			return false, nil
		}
		status, err := e.status(ctx, it)
		if err != nil {
			return false, err
		}
		if status == "running" {
			return false, fmt.Errorf("VM is running and has %s; stop it first so the baseline is clean", pods.NoBaseline(e.Cfg.Deploy))
		}
		return true, e.createSnapshot(ctx, it.Node, it.VMID, proxmox.SnapshotRequest{Name: e.Cfg.Deploy.SnapshotName, Description: "baseline from battleship deploy"})

	case pods.StepStart:
		return e.power(ctx, it, "start")
	case pods.StepStop:
		if kind == pods.KindTeardown {
			return e.shutdownForDelete(ctx, it)
		}
		return e.power(ctx, it, "stop") // reset: the rollback discards the state anyway
	case pods.StepPower:
		return e.power(ctx, it, it.Action)

	case pods.StepRollback:
		return true, e.task(ctx, func() (string, error) { return e.api.Rollback(ctx, it.Node, it.VMID, it.Snapshot) })

	case pods.StepDelete:
		err := e.deleteVM(ctx, it.Node, it.VMID, it.Name)
		if err == nil {
			e.setPresent(proxmox.VM{VMID: it.VMID}, false)
		}
		return err == nil, err
	}
	return false, fmt.Errorf("unknown step %q", step)
}

// checkVMIDFree, for a delete whose VM is no longer listed by name, fails
// if the VMID holds a VM a failed destroy left locked as destroyed. Any
// other VM there is left alone.
func (e *Executor) checkVMIDFree(ctx context.Context, vmid int) error {
	e.mu.Lock()
	vm, ok := e.present[vmid]
	e.mu.Unlock()
	if !ok {
		return nil
	}
	cfg, gone, err := e.readVM(ctx, vm.Node, vmid)
	switch {
	case gone:
		return nil
	case err != nil:
		return fmt.Errorf("VMID %d is listed as %q; could not check whether it is a half-deleted VM: %w", vmid, vm.Name, err)
	case cfg["lock"] == "destroyed":
		return &halfDeletedError{vmid: vmid, node: vm.Node}
	}
	return nil
}

// shutdownForDelete shuts a running VM down cleanly before deletion, so
// routers release their DHCP lease (udhcpc -R) for the next deploy.
// Proxmox hard-stops it after teardown.shutdown_timeout (forceStop); if the
// task fails and it is still running, it is hard-stopped here.
func (e *Executor) shutdownForDelete(ctx context.Context, it *pods.Item) (bool, error) {
	status, err := e.status(ctx, it)
	if err != nil || status == "stopped" {
		return false, err
	}
	timeout := e.Cfg.Teardown.ShutdownTimeout
	err = e.taskOnce(ctx, func() (string, error) { return e.api.Shutdown(ctx, it.Node, it.VMID, timeout, true) })
	if err == nil || ctx.Err() != nil {
		return true, err
	}
	if st, serr := e.status(ctx, it); serr == nil && st == "stopped" {
		return true, nil
	}
	e.emit(it.Name, pods.StepStop, EventInfo, "clean shutdown failed, stopping it hard: "+proxmox.Describe(err))
	if serr := e.taskOnce(ctx, func() (string, error) { return e.api.Power(ctx, it.Node, it.VMID, "stop") }); serr != nil {
		return true, fmt.Errorf("hard stop after a failed shutdown (%s): %w", proxmox.Describe(err), serr)
	}
	return true, nil
}

func (e *Executor) status(ctx context.Context, it *pods.Item) (string, error) {
	var status string
	err := e.call(ctx, func() (err error) { status, err = e.api.CurrentStatus(ctx, it.Node, it.VMID); return })
	return status, err
}

// startClone starts a clone into req.NewVMID on req.TargetNode and returns
// its UPID. A timed-out POST may still have created the VM, so the target is
// probed first; a VM with the expected name there means "" is returned (for
// a template copy it must be this run's). Only a VM seen absent becomes this
// run's, and stays so from the POST on even if the request times out.
func (e *Executor) startClone(ctx context.Context, template bool, req proxmox.CloneRequest) (string, error) {
	p, err := e.probeClone(ctx, req.TargetNode, req.NewVMID, req.Name)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	_, ours := e.owned[req.NewVMID]
	claimed := false
	if p == probeAbsent && !ours {
		e.owned[req.NewVMID] = &ownedVM{name: req.Name, node: req.TargetNode, template: template}
		claimed = true
	}
	e.mu.Unlock()
	if p == probeSame {
		if template && !ours {
			return "", fmt.Errorf("VMID %d appeared as %s while cloning; not converting a VM this run did not create", req.NewVMID, req.Name)
		}
		return "", nil
	}
	upid, err := e.api.Clone(ctx, req)
	if err != nil && claimed && e.classifier.Classify(err) == proxmox.Exists {
		e.mu.Lock() // somebody else got there first
		delete(e.owned, req.NewVMID)
		e.mu.Unlock()
	}
	return upid, err
}

// probe is what a clone pre-check found at the target VMID.
type probe int

const (
	probeAbsent probe = iota // definitely no VM there: safe to clone, and the VM will be ours
	probeSame                // a VM with the expected name: an earlier POST took effect, or it is not ours
	probeOther               // some other VM: the POST would fail with "already exists"
)

// probeClone looks at vmid before a clone. Only "does not exist" counts as
// absent; other read errors are returned rather than claim a VM it may not
// have created. A locked VM is reported as proxmox.ErrLocked so the caller
// backs off.
func (e *Executor) probeClone(ctx context.Context, node string, vmid int, name string) (probe, error) {
	cfg, err := e.api.VMConfig(ctx, node, vmid)
	if err != nil {
		if proxmox.IsNotFound(err) {
			return probeAbsent, nil
		}
		return probeOther, err
	}
	lock := cfg["lock"]
	if cfg["name"] != name {
		if cfg["name"] == "" && lock != "" {
			return probeOther, fmt.Errorf("VM %d is locked (%s) and has no name yet: %w", vmid, lock, proxmox.ErrLocked)
		}
		return probeOther, fmt.Errorf("VMID %d is used by %q; not cloning over it", vmid, cfg["name"])
	}
	if lock != "" {
		err := fmt.Errorf("VM %d is locked (%s): %w", vmid, lock, proxmox.ErrLocked)
		e.emit(name, pods.StepClone, EventInfo, "waiting: "+err.Error())
		return probeSame, err
	}
	return probeSame, nil
}

// applyConfig applies the VM's config changes and reports whether any were
// made. With cloudInit, a cloud-init drive is regenerated every time.
func (e *Executor) applyConfig(ctx context.Context, it *pods.Item, cloudInit bool,
	changes func(map[string]string) map[string]string) (bool, error) {
	var cur map[string]string
	if err := e.call(ctx, func() (err error) { cur, err = e.api.VMConfig(ctx, it.Node, it.VMID); return }); err != nil {
		return false, err
	}
	ch := changes(cur)
	changed := len(ch) > 0
	if changed {
		if err := e.call(ctx, func() error { return e.api.SetVMConfig(ctx, it.Node, it.VMID, ch) }); err != nil {
			return false, err
		}
	}
	// Regenerate even without changes, to repair an earlier failed regeneration.
	if cloudInit && pods.HasCloudInit(cur) {
		if err := e.call(ctx, func() error { return e.api.RegenerateCloudInit(ctx, it.Node, it.VMID) }); err != nil {
			return changed, fmt.Errorf("regenerating cloud-init: %w", err)
		}
	}
	return changed, nil
}

// power applies action, skipping a VM already in the status it leaves (see
// PowerAction.Leaves). Rebooting a stopped VM starts it.
func (e *Executor) power(ctx context.Context, it *pods.Item, action string) (bool, error) {
	status, err := e.status(ctx, it)
	if err != nil {
		return false, err
	}
	if a, _ := pods.PowerActionOf(action); a.Leaves != "" && status == a.Leaves {
		return false, nil
	}
	if action == "reboot" && status != "running" {
		action = "start"
	}
	return true, e.taskOnce(ctx, func() (string, error) { return e.api.Power(ctx, it.Node, it.VMID, action) })
}
