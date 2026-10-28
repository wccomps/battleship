package apply

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// needsTemplate reports whether any item will clone from the template.
func needsTemplate(items []pods.Item, name string) bool {
	for _, it := range items {
		if it.Template == name && slices.Contains(it.Steps, pods.StepClone) {
			return true
		}
	}
	return false
}

type tplBuild struct {
	spec *pods.TemplateSpec // as planned
	node string             // where the template is
	// oldGone: a rebuild deleted the old template, so later rounds create or
	// resume the new one.
	oldGone     bool
	err         error
	done        bool
	ready       chan struct{} // closed when this round's build ends; nil if none
	freed       sync.WaitGroup
	mu          sync.Mutex
	teardownErr error
}

func (b *tplBuild) noteTeardownErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.teardownErr == nil {
		b.teardownErr = err
	}
}

func (e *Executor) startTemplates(ctx context.Context, specs []pods.TemplateSpec, builds map[string]*tplBuild, pending []pods.Item) (wait func()) {
	var wg sync.WaitGroup
	for _, spec := range specs {
		b := builds[spec.Name]
		b.ready = nil
		b.freed = sync.WaitGroup{}
		b.teardownErr = nil
		if spec.Blocked != "" || b.done || !needsTemplate(pending, spec.Name) {
			continue
		}
		if spec.Rebuild && !b.oldGone {
			for _, it := range pending {
				if it.Template == spec.Name && slices.Contains(it.Steps, pods.StepDelete) && !e.isTornDown(it.Name) {
					b.freed.Add(1)
				}
			}
		}
		b.ready = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(b.ready)
			b.err = e.ensureTemplate(ctx, b)
			b.done = b.err == nil
			if b.err != nil {
				e.emit(spec.Name, "", EventFailed, proxmox.Describe(b.err))
			}
		}()
	}
	return wg.Wait
}

// masterWaitBudget is how long a failed template build waits for the
// master's copy to finish before restarting the master.
const masterWaitBudget = 30 * time.Minute

// copyStopWait is how long a stopped run waits for a copy it stopped, and
// the master's stop task, to end before restarting the master;
// masterRestartBudget bounds the restart.
const (
	copyStopWait        = 2 * time.Minute
	masterRestartBudget = 2 * time.Minute
)

func (e *Executor) ensureTemplate(ctx context.Context, b *tplBuild) error {
	spec := b.spec
	name := spec.Name
	if spec.Rebuild && !b.oldGone {
		b.freed.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if b.teardownErr != nil {
			return b.teardownErr
		}
	}
	cur, found := e.lookup(spec.VMID, name)
	switch {
	case spec.Rebuild && !b.oldGone:
		// An earlier round deleted the old template but not all its disks.
		if err := e.retryLeftovers(ctx, spec.VMID, cmp.Or(spec.OldNode, spec.MasterNode), name); err != nil {
			return fmt.Errorf("deleting old template: %w", err)
		}
		if found {
			if vm, ok := e.teamVMUsing(spec.Host); ok {
				return fmt.Errorf("pod VMs such as %s still use this template; tear them down first", vm.Name)
			}
			node := cur.Node
			if node == "" {
				node = spec.OldNode
			}
			e.emit(name, pods.StepDelete, EventInfo, "deleting old template")
			if err := e.deleteVM(ctx, node, spec.VMID, name); err != nil {
				if e.classifier.Classify(err) == proxmox.InUse {
					return fmt.Errorf("deleting old template: linked-clone disks still use it; remove them in Proxmox and re-run: %w", err)
				}
				return fmt.Errorf("deleting old template: %w", err)
			}
			e.setPresent(proxmox.VM{VMID: spec.VMID}, false)
			e.emit(name, pods.StepDelete, EventDone, "old template deleted")
		}
		b.oldGone = true // later rounds must create or resume, never delete the new VM
	case found && cur.Template:
		// A half-converted template would fail every linked clone; fail once here.
		cfg, gone, err := e.readVM(ctx, cur.Node, spec.VMID)
		switch {
		case gone:
			return fmt.Errorf("template %s (%d) no longer exists", name, spec.VMID)
		case err != nil:
			return fmt.Errorf("checking template %s's disks: %w", name, err)
		}
		if key, vol := pods.UnconvertedDisk(cfg); key != "" {
			return &unconvertedError{name: name, vmid: spec.VMID, key: key, vol: vol, reused: true}
		}
		b.node = cur.Node
		e.emit(name, pods.StepClone, EventSkipped, "template exists")
		return nil
	case found:
		e.mu.Lock()
		o := e.owned[spec.VMID]
		e.mu.Unlock()
		if o == nil {
			return fmt.Errorf("%s exists but is not a template and wasn't created by this run; re-plan with rebuild", name)
		}
		if o.state == cloneTaskFailed {
			return fmt.Errorf("%s is the leftover of a failed copy; it will be removed, then re-run", name)
		}
		b.node = cur.Node
		return e.finishTemplate(ctx, spec, cur.Node)
	case !spec.Builds():
		return fmt.Errorf("template %s (%d) no longer exists", name, spec.VMID)
	}
	e.mu.Lock()
	other, taken := e.present[spec.VMID]
	e.mu.Unlock()
	if taken {
		return fmt.Errorf("VMID %d is now used by %s", spec.VMID, other.Name)
	}

	// builds bounds stopped masters; take both slots before stopping a running master.
	if err := e.lim.builds.do(ctx, func() error {
		return e.lim.clones.do(ctx, func() error { return e.copyMaster(ctx, spec) })
	}); err != nil {
		return err
	}
	e.setPresent(proxmox.VM{VMID: spec.VMID, Name: name, Node: spec.MasterNode}, true)
	b.node = spec.MasterNode
	return e.finishTemplate(ctx, spec, spec.MasterNode)
}

// copyMaster clones spec's master to spec.VMID, stopping a running master
// first and restarting it once the copy can't still be running. A stopped
// job stops a running copy too (see stopCopy).
func (e *Executor) copyMaster(ctx context.Context, spec *pods.TemplateSpec) error {
	name := spec.Name
	// Full clones on NFS are only reliable from a stopped source.
	var status, stopUPID, cloneUPID string
	var stopErr, cloneErr error
	// copyRunning: the copy task started and its end was not seen.
	copyRunning := func() bool {
		var te *proxmox.TaskError
		return cloneUPID != "" && cloneErr != nil && !errors.As(cloneErr, &te)
	}
	if err := e.call(ctx, func() (err error) {
		status, err = e.api.CurrentStatus(ctx, spec.MasterNode, spec.MasterVMID)
		return
	}); err != nil {
		return err
	}
	// Deferred before the stop is sent: a cancel or wait failure during the
	// stop may leave the master stopped.
	defer func() {
		wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), masterWaitBudget)
		defer wcancel()
		// A stop stops a copy it cut off and caps restartMaster's waits at copyStopWait.
		onStop := func() {
			go func() {
				if e.Sleep(wctx, copyStopWait) == nil {
					wcancel()
				}
			}()
			if copyRunning() {
				e.stopCopy(ctx, spec, cloneUPID)
			}
		}
		stoppedNow := ctx.Err() != nil
		if stoppedNow {
			onStop()
		}
		if status != "running" {
			return
		}
		if !stoppedNow {
			defer context.AfterFunc(ctx, onStop)()
		}
		e.restartMaster(ctx, wctx, spec, startedTask{"stop", stopUPID, stopErr}, startedTask{"clone", cloneUPID, cloneErr})
	}()
	if status == "running" {
		e.emit(name, pods.StepClone, EventInfo, "stopping master "+spec.MasterName)
		stopUPID, stopErr = e.taskUPID(ctx, 0, func() (string, error) {
			return e.api.Power(ctx, spec.MasterNode, spec.MasterVMID, "stop")
		}, nil)
		if stopErr != nil {
			return fmt.Errorf("stopping master: %w", stopErr)
		}
	}

	e.emit(name, pods.StepClone, EventInfo, "cloning master "+spec.MasterName)
	cloneUPID, cloneErr = e.taskUPID(ctx, spec.VMID, func() (string, error) {
		return e.startClone(ctx, true, proxmox.CloneRequest{
			SourceNode: spec.MasterNode, SourceVMID: spec.MasterVMID, NewVMID: spec.VMID, Name: name,
			TargetNode: spec.MasterNode, Full: true, Storage: e.Cfg.Deploy.Storage, Pool: e.Cfg.Images.Pool,
		})
	}, nil) // a failed copy task is never re-started: cleanup removes the leftover
	if cloneUPID != "" || cloneErr != nil {
		e.settleCopy(spec.VMID, cloneErr)
	}
	if cloneErr != nil {
		return fmt.Errorf("cloning master: %w", cloneErr)
	}
	return nil
}

// startedTask is a Proxmox task a step started and how waiting for it ended.
type startedTask struct {
	kind, upid string
	err        error
}

// restartMaster starts a master the build stopped, even after a cancel, but
// only once its stop and copy can't still be running (starting it under a
// copy corrupts the copy). If it can't, the master is reported left stopped.
func (e *Executor) restartMaster(ctx, wctx context.Context, spec *pods.TemplateSpec, tasks ...startedTask) {
	// leftStopped reports the master left stopped, also in the run's result:
	// someone must start it.
	leftStopped := func(reason string) {
		msg := "master " + spec.MasterName + " left stopped: " + reason + "; start it in Proxmox once nothing is working on it"
		e.emit(spec.Name, pods.StepStart, EventFailed, msg)
		e.noteMasterStopped(spec.MasterName, msg)
	}
	lapsed := func(kind string) {
		leftStopped("the job's authorization lapsed, so its " + kind + " task can't be followed and the master can't be started")
	}
	var taskErr *proxmox.TaskError
	for _, t := range tasks {
		if t.upid == "" || t.err == nil || errors.As(t.err, &taskErr) {
			continue // never started, or seen to end
		}
		if proxmox.IsLapsed(t.err) { // every call would be refused
			lapsed(t.kind)
			return
		}
		err := e.api.WaitTask(wctx, t.upid, e.Cfg.Retry.TaskPoll)
		if t.kind == "clone" {
			e.settleCopy(spec.VMID, err)
		}
		switch {
		case proxmox.IsForbidden(err):
			// Can't be followed; the lock checks below say when it is over.
		case proxmox.IsLapsed(err):
			lapsed(t.kind)
			return
		case err != nil && !errors.As(err, &taskErr):
			leftStopped("its " + t.kind + " task may still be running: could not confirm it finished: " + err.Error())
			return
		}
	}
	// A clone this run has no UPID for shows only as a lock on its target.
	for _, vm := range []struct {
		what string
		vmid int
	}{{"clone target", spec.VMID}, {"master", spec.MasterVMID}} {
		_, lock, err := e.waitUnlocked(wctx, spec.MasterNode, vm.vmid)
		if lock != "" {
			leftStopped(fmt.Sprintf("%s still locked (lock: %s)", vm.what, lock))
			return
		}
		if err != nil {
			leftStopped("could not check that nothing is working on the " + vm.what + ": " + proxmox.Describe(err))
			return
		}
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), masterRestartBudget)
	defer cancel()
	// The stop may not have happened (or the master was started by hand).
	if st, err := e.api.CurrentStatus(rctx, spec.MasterNode, spec.MasterVMID); err == nil && st == "running" {
		return
	}
	e.emit(spec.Name, pods.StepClone, EventInfo, "restarting master "+spec.MasterName)
	if err := e.taskOnce(rctx, func() (string, error) { return e.api.Power(rctx, spec.MasterNode, spec.MasterVMID, "start") }); err != nil {
		leftStopped("restarting it failed: " + proxmox.Describe(err))
	}
}

// stopCopy stops a template copy a stopped job cut off and marks it failed,
// so cleanup removes it unless a later wait sees it finish OK (settleCopy).
func (e *Executor) stopCopy(ctx context.Context, spec *pods.TemplateSpec, upid string) {
	e.mu.Lock()
	if o := e.owned[spec.VMID]; o != nil && o.state == cloneUnknown {
		o.state = cloneTaskFailed
	}
	e.mu.Unlock()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := e.api.StopTask(sctx, upid); err != nil {
		e.emit(spec.Name, pods.StepClone, EventInfo, "stopping the copy of "+spec.MasterName+": "+proxmox.Describe(err))
		return
	}
	e.emit(spec.Name, pods.StepClone, EventInfo, "stopped the copy of "+spec.MasterName+" (the job was stopped)")
}

// settleCopy records how a copy's task ended: nil is OK, a TaskError
// failed, anything else changes nothing.
func (e *Executor) settleCopy(vmid int, err error) {
	var taskErr *proxmox.TaskError
	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.owned[vmid]
	switch {
	case o == nil:
	case err == nil:
		o.state = cloneTaskOK
	case errors.As(err, &taskErr):
		o.state = cloneTaskFailed
	}
}

func (e *Executor) teamVMUsing(host string) (proxmox.VM, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var best proxmox.VM
	found := false
	for _, vm := range e.present {
		if _, h, ok := e.naming.TeamVM(vm); ok && h == host && (!found || vm.VMID < best.VMID) {
			best, found = vm, true
		}
	}
	return best, found
}

// finishTemplate ejects the CD-ROM and converts the cloned VM on node to a
// template. It is safe to repeat after a partial failure.
func (e *Executor) finishTemplate(ctx context.Context, spec *pods.TemplateSpec, node string) error {
	name := spec.Name
	tplItem := &pods.Item{Name: name, VMID: spec.VMID, Node: node}
	if _, err := e.applyConfig(ctx, tplItem, false, always(pods.CDROMChanges)); err != nil {
		return fmt.Errorf("ejecting CD-ROM: %w", err)
	}
	e.emit(name, pods.StepClone, EventInfo, "converting to template")
	if _, err := e.convert(ctx, node, spec.VMID, name); err != nil {
		var uerr *unconvertedError
		if errors.As(err, &uerr) {
			return err
		}
		if errors.Is(err, errVanished) {
			return fmt.Errorf("converting to template: VM %d disappeared", spec.VMID)
		}
		return fmt.Errorf("converting to template: %w", err)
	}
	e.setPresent(proxmox.VM{VMID: spec.VMID, Name: name, Node: node, Template: true}, true)
	e.emit(name, pods.StepClone, EventDone, "template created")
	return nil
}

// convertWait bounds how long a conversion whose outcome is unknown gets to
// rename its disks.
const convertWait = 2 * time.Minute

// convert converts the VM to a template and verifies its disks are base-*
// volumes. Proxmox sets template: 1 before renaming disks, so ConvertToTemplate
// is never re-sent once template: 1 is set.
func (e *Executor) convert(ctx context.Context, node string, vmid int, name string) (changed bool, err error) {
	convErr := e.lim.storageOps.do(ctx, true, func() error {
		upid, err := e.taskUPID(ctx, 0, func() (string, error) {
			cfg, err := e.api.VMConfig(ctx, node, vmid)
			switch {
			case proxmox.IsNotFound(err):
				return "", errVanished
			case err != nil:
				return "", err
			case cfg["template"] == "1":
				return "", nil
			}
			changed = true
			return e.api.ConvertToTemplate(ctx, node, vmid)
		}, nil)
		var taskErr *proxmox.TaskError
		if (err == nil && upid != "") || errors.As(err, &taskErr) || ctx.Err() != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(ctx, convertWait)
		defer cancel()
		cfg, _ := e.pollVM(wctx, node, vmid, func(cfg map[string]string) bool {
			if cfg["template"] != "1" {
				return true // not converting
			}
			key, _ := pods.UnconvertedDisk(cfg)
			return key == ""
		})
		if cfg["template"] == "1" {
			return nil
		}
		return err
	})
	if convErr != nil && ctx.Err() != nil {
		return changed, convErr
	}
	cfg, gone, rerr := e.readVM(ctx, node, vmid)
	switch {
	case (gone || rerr != nil) && convErr != nil:
		return changed, convErr
	case gone:
		return changed, errVanished
	case rerr != nil:
		return changed, fmt.Errorf("converting to template finished, but its disks could not be checked: %w", rerr)
	}
	isTemplate := cfg["template"] == "1"
	if key, vol := pods.UnconvertedDisk(cfg); key != "" && isTemplate {
		return changed, &unconvertedError{name: name, vmid: vmid, key: key, vol: vol, cause: convErr}
	}
	switch {
	case convErr != nil:
		return changed, convErr
	case !isTemplate:
		return changed, fmt.Errorf("the task finished OK, but VM %d is still not a template", vmid)
	}
	return changed, nil
}

// errVanished is a VM that disappeared mid-step. Unlike "does not exist"
// (maybe not visible on its node yet), it is not retried.
var errVanished = errors.New("the VM disappeared")
