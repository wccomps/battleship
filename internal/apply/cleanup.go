package apply

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// CleanupBudget bounds Run's final cleanup. It runs detached from the job's
// context, so a cancelled job still cleans up.
const CleanupBudget = 10 * time.Minute

// vmRef identifies a VM this run created.
type vmRef struct {
	vmid int
	name string
	node string // where the run expected it; used when the listing is unusable
}

// cleanup leaves the cluster as Run found it, apart from finished work. It
// touches only VMs this run created, re-checking each name:
//   - unfinished team VMs are removed;
//   - a copy whose task failed or was cut off is removed, unless a later
//     wait saw it finish OK (see stopCopy);
//   - other unconverted copies are completed (if the task's end was never
//     seen, only when present, unlocked and correctly named), or removed if
//     that fails.
//
// A VM that stays locked, changed identity, or won't delete is reported,
// not forced.
func (e *Executor) cleanup(ctx context.Context, specs []pods.TemplateSpec, builds map[string]*tplBuild, succeeded map[string]bool, res *Result) {
	e.mu.Lock()
	var teams []vmRef
	for vmid, o := range e.owned {
		if !o.template && !succeeded[o.name] {
			teams = append(teams, vmRef{vmid, o.name, o.node})
		}
	}
	var tpls []*pods.TemplateSpec
	states := map[int]cloneState{}
	for i := range specs {
		if o := e.owned[specs[i].VMID]; o != nil && o.template && !builds[specs[i].Name].done {
			tpls = append(tpls, &specs[i])
			states[specs[i].VMID] = o.state
		}
	}
	e.mu.Unlock()
	if len(teams) == 0 && len(tpls) == 0 {
		return
	}
	sort.Slice(teams, func(i, j int) bool { return teams[i].vmid < teams[j].vmid })

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), CleanupBudget)
	defer cancel()
	after := "failure"
	if ctx.Err() != nil {
		after = "cancel"
	}
	listOK := true
	if err := e.refresh(cctx); err != nil {
		listOK = false
		e.emit("", "", EventFailed, "cleanup could not list VMs, checking each VM directly: "+proxmox.Describe(err))
	}
	fail := func(ref vmRef, err error) {
		res.CleanupFailed[ref.name] = err
		if errors.Is(err, errExplained) {
			e.emit(ref.name, pods.StepDelete, EventFailed, err.Error())
			return
		}
		e.emit(ref.name, pods.StepDelete, EventFailed, fmt.Sprintf("could not remove %s (%d): %s; remove it in Proxmox",
			ref.name, ref.vmid, proxmox.Describe(err)))
	}
	gone := func(ref vmRef) {
		res.AlreadyGone = append(res.AlreadyGone, ref.name)
		e.emit(ref.name, pods.StepDelete, EventInfo, fmt.Sprintf("%s (%d) is already gone", ref.name, ref.vmid))
	}
	exhausted := func(ref vmRef) bool {
		if cctx.Err() == nil {
			return false
		}
		fail(ref, fmt.Errorf("cleanup time budget ran out before %s could be checked: %w", ref.name, errUnconfirmed))
		return true
	}

	// Team VMs first: they are linked to the templates.
	for _, ref := range teams {
		if exhausted(ref) {
			continue
		}
		reason := "cancelled before it finished"
		if ferr := res.Failed[ref.name]; ferr != nil {
			reason = proxmox.Describe(ferr)
		}
		e.removeCreated(cctx, res, ref, listOK, reason, fail, gone)
	}
	for _, spec := range tpls {
		ref := vmRef{spec.VMID, spec.Name, spec.MasterNode}
		if exhausted(ref) {
			continue
		}
		// A copy cut off by a cancel may still be running; settle waits it out.
		node, cfg, isGone, err := e.settle(cctx, ref, listOK)
		switch {
		case err != nil:
			fail(ref, err)
			continue
		case isGone:
			gone(ref)
			continue
		}
		ref.node = node
		remove := func(reason string) { e.removeCreated(cctx, res, ref, false, reason, fail, gone) }
		switch key, vol := pods.UnconvertedDisk(cfg); {
		case cfg["template"] == "1" && key != "":
			// A failed conversion can set template: 1 without renaming the disks;
			// such a template can never be cloned.
			remove(fmt.Sprintf("it was left half-converted: disk %s (%s) was never renamed to a base- volume", key, vol))
		case cfg["template"] == "1":
			e.emit(spec.Name, "", EventInfo, "template "+spec.Name+" was already complete")
		case states[spec.VMID] == cloneTaskFailed:
			remove("its copy task failed")
		default:
			if err := e.finishTemplate(cctx, spec, node); err != nil {
				remove("could not complete it: " + proxmox.Describe(err))
				continue
			}
			res.Completed = append(res.Completed, spec.Name)
			e.emit(spec.Name, "", EventInfo, "completed template "+spec.Name+" after "+after)
		}
	}
	sort.Strings(res.Completed)
	sort.Strings(res.Removed)
	sort.Strings(res.AlreadyGone)
}

// settle finds what is at a recorded VMID right before cleanup acts: its
// node (from the listing if listOK, else ref.node) and its config once
// unlocked, or that it is gone. Anything else is an error, never acted on.
func (e *Executor) settle(ctx context.Context, ref vmRef, listOK bool) (node string, cfg map[string]string, gone bool, err error) {
	node = ref.node
	if listOK {
		e.mu.Lock()
		vm, ok := e.present[ref.vmid]
		e.mu.Unlock()
		if !ok {
			return "", nil, true, nil
		}
		node = vm.Node
	}
	cfg, gone, err = e.readVM(ctx, node, ref.vmid)
	switch {
	case gone:
		return "", nil, true, nil
	case err != nil:
		return "", nil, false, unconfirmed(ref, err)
	}
	if cfg["lock"] != "" {
		var lock string
		cfg, lock, err = e.waitUnlocked(ctx, node, ref.vmid)
		switch {
		case lock != "":
			return "", nil, false, fmt.Errorf("still locked (lock: %s)", lock)
		case err != nil:
			return "", nil, false, unconfirmed(ref, err)
		case cfg == nil:
			return "", nil, true, nil
		}
	}
	if cfg["name"] != ref.name {
		return "", nil, false, &notOursError{ref.vmid, cfg["name"], ref.name}
	}
	return node, cfg, false, nil
}

func unconfirmed(ref vmRef, err error) error {
	return fmt.Errorf("%w %s (%d) is gone; check Proxmox: %s: %w", errUnconfirmed, ref.name, ref.vmid, proxmox.Describe(err), err)
}

// removeCreated deletes a VM this run created once settle confirms it is
// idle and unchanged. It never forces; failures are reported for manual
// removal.
func (e *Executor) removeCreated(ctx context.Context, res *Result, ref vmRef, listOK bool, reason string,
	fail func(vmRef, error), gone func(vmRef)) {
	node, _, isGone, err := e.settle(ctx, ref, listOK)
	if err == nil && !isGone {
		var status string
		if err = e.check(ctx, func() (err error) { status, err = e.api.CurrentStatus(ctx, node, ref.vmid); return }); proxmox.IsNotFound(err) {
			isGone, err = true, nil
		} else if err == nil {
			if status == "running" {
				if err = e.taskOnce(ctx, func() (string, error) { return e.api.Power(ctx, node, ref.vmid, "stop") }); err != nil {
					err = fmt.Errorf("stopping it: %w", err)
				}
			}
			if err == nil {
				err = e.deleteVM(ctx, node, ref.vmid, ref.name)
			}
		}
	}
	switch {
	case err != nil:
		fail(ref, err)
	case isGone:
		e.setPresent(proxmox.VM{VMID: ref.vmid}, false)
		gone(ref)
	default:
		e.setPresent(proxmox.VM{VMID: ref.vmid}, false)
		res.Removed = append(res.Removed, ref.name)
		e.emit(ref.name, pods.StepDelete, EventInfo, fmt.Sprintf("removed %s (created this run; %s)", ref.name, reason))
	}
}
