package pods

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

type EventStatus string

const (
	EventDone    EventStatus = "done"
	EventSkipped EventStatus = "skipped" // already in the desired state
	EventFailed  EventStatus = "failed"
	EventInfo    EventStatus = "info"
	EventBlocked EventStatus = "blocked" // the plan blocked this item; Message says why
	// EventInterrupted: a stop (a cancel, a shutdown) cut the item off;
	// Message says when.
	EventInterrupted EventStatus = "interrupted"
)

// ErrCancelRequested is the cause of a run's context when someone cancelled
// the job. Unlike other stops, it gives a step that already sent Proxmox a
// change up to jobs.cancel_grace to see it through (see stepContext).
var ErrCancelRequested = errors.New("cancel requested")

// Left is how an interrupted item left its VM's config.
type Left int

const (
	// LeftChanged: a step that changes the config sent something, and not
	// every such step finished. Part of the config may have changed.
	LeftChanged Left = iota
	// LeftUntouched: no step that changes the config sent anything in
	// this run, so the VM is as the job before left it.
	LeftUntouched
	// LeftConverged: every step that changes the config finished; only
	// power steps were left.
	LeftConverged
)

func (l Left) String() string {
	switch l {
	case LeftUntouched:
		return "untouched"
	case LeftConverged:
		return "converged"
	}
	return "changed"
}

// stoppedError is the error of an item a stop cut off, and how it left
// the VM's config.
type stoppedError struct {
	msg  string
	left Left
}

func (s *stoppedError) Error() string { return s.msg }

// Is makes a stoppedError a context.Canceled, which is how Run tells
// interrupted items from failed ones.
func (s *stoppedError) Is(target error) bool { return target == context.Canceled }

// stopped is the error of an item ctx's stop cut off, saying why and when,
// e.g. "cancelled before delete", and how it left the VM's config.
func stopped(ctx context.Context, when string, left Left) error {
	why := "stopped"
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, ErrCancelRequested):
		why = "cancelled"
	case cause != nil && !errors.Is(cause, context.Canceled):
		why = "stopped (" + cause.Error() + ")"
	}
	return &stoppedError{msg: why + " " + when, left: left}
}

// LeftBy is how the item an interruption err ended left its VM's config;
// LeftChanged for an error that doesn't say.
func LeftBy(err error) Left {
	var se *stoppedError
	if errors.As(err, &se) {
		return se.left
	}
	return LeftChanged
}

// Event reports progress. Item is a VM or template name, or "" for the job.
type Event struct {
	Time    time.Time
	Item    string
	Step    Step
	Status  EventStatus
	Message string
}

// Result is the outcome of a run. Failed maps the items that failed to their
// last error; Interrupted maps those that a stop cut off, or that never
// started, to theirs.
type Result struct {
	Succeeded   []string
	Failed      map[string]error
	Interrupted map[string]error
	Blocked     []string
	// Completed lists templates whose finished copy was converted during
	// cleanup. Removed lists VMs this run created and then deleted because they
	// did not finish. CleanupFailed maps VMs that could not be removed to why;
	// they need removing in Proxmox by hand.
	Completed     []string
	Removed       []string
	CleanupFailed map[string]error
	AlreadyGone   []string
	// RoundsSkipped reports that the run stopped (its context ended) before
	// every round it had left was run: some items' errors may be from an
	// earlier round, not their last chance, so the run did not finish.
	RoundsSkipped bool
}

// Executor runs a plan. Its steps check the VM's current state first and
// skip work already done, except that a reset rolls back again, a reboot
// always acts and the network step regenerates cloud-init each time, so
// running the same plan again resumes or converges it.
//
// A single Executor must not Run concurrently. OnEvent is called from
// multiple goroutines, so it must be safe for concurrent use.
//
// Cleanup assumes one run per lock key (team or template) at a time: it
// decides that a VM is its own from what it saw and did during the run. With
// a database configured, the job system enforces this, including for direct
// CLI runs; without one, don't run overlapping operations.
type Executor struct {
	API     API
	Cfg     config.Config
	OnEvent func(Event)
	// Sleep waits out backoffs, polls and the pause before retry rounds;
	// tests replace it to run instantly.
	Sleep func(ctx context.Context, d time.Duration) error
	// Limits, if set, is shared with other Executors running at the same
	// time. Nil means a private one from Cfg.Concurrency.
	Limits *Limits
	// Halt, if set, ends a cancel's grace at once when it closes, e.g. on
	// shutdown, so a cancelled run still stops within StopBudget.
	Halt <-chan struct{}

	// api is API, guarded so a step makes no change after a stop (see
	// stepContext).
	api API

	naming     Naming
	classifier proxmox.Classifier
	retrier    proxmox.Retrier
	lim        *Limits
	mu         sync.Mutex
	present    map[int]proxmox.VM
	// owned are the VMs this run's clones created, by VMID: the only VMs
	// cleanup touches.
	owned map[int]*ownedVM
	// leftovers are the disks deleted VMs left on the storage that this run
	// could not free yet, by VMID.
	leftovers map[int]leftover
	// asked are the snapshots this run sent a request for, by VMID: the
	// only ones of their names it accepts as already taken.
	asked map[int][]string
	// mastersStopped are masters a template build left stopped, by name,
	// with what to do: Run reports them in CleanupFailed.
	mastersStopped map[string]string
	// left is how each item, by name, left its VM's config as of the last
	// change it sent from a step that changes the config, or the last time
	// it finished every such step, in any round; absent, untouched (see
	// leftAt).
	left map[string]Left
}

// cloneState is what a run knows about the outcome of a template clone.
type cloneState int

const (
	// cloneUnknown: the copy's task was never seen to end. Cleanup
	// completes the copy only if it is present, unlocked and still named
	// as expected.
	cloneUnknown cloneState = iota
	cloneTaskOK
	cloneTaskFailed // the task failed, or a stopped job cut it off; cleanup removes the copy
)

// ownedVM is a VM a clone of this run created.
type ownedVM struct {
	name, node string
	template   bool       // a template's copy of its master
	state      cloneState // templates only
}

func (e *Executor) init() {
	e.naming = NewNaming(e.Cfg.Naming)
	if e.Sleep == nil {
		e.Sleep = proxmox.SleepContext
	}
	e.classifier = proxmox.NewClassifier(e.Cfg.Retry.TransientPatterns)
	e.retrier = proxmox.Retrier{
		Retryable: e.retryable,
		Attempts:  e.Cfg.Retry.Attempts,
		Initial:   e.Cfg.Retry.InitialBackoff,
		Max:       e.Cfg.Retry.MaxBackoff,
		Sleep:     e.Sleep,
	}
	e.lim = e.Limits
	if e.lim == nil {
		e.lim = NewLimits(e.Cfg.Concurrency)
	}
	e.owned = map[int]*ownedVM{}
	e.api = guardedAPI{e.API}
	e.leftovers = map[int]leftover{}
	e.asked = map[int][]string{}
	e.mastersStopped = map[string]string{}
	e.left = map[string]Left{}
}

// noteMasterStopped records a master a template build left stopped.
func (e *Executor) noteMasterStopped(master, msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mastersStopped[master] = msg
}

func (e *Executor) emit(item string, step Step, status EventStatus, msg string) {
	if e.OnEvent != nil {
		e.OnEvent(Event{Time: time.Now(), Item: item, Step: step, Status: status, Message: msg})
	}
}

// Run executes the plan, then retries failed items for the configured number
// of rounds.
func (e *Executor) Run(ctx context.Context, plan *Plan) Result {
	e.init()
	res := Result{Failed: map[string]error{}, Interrupted: map[string]error{}, CleanupFailed: map[string]error{}}
	failed := map[string]error{} // by item, its last error
	for _, it := range plan.Items {
		if it.Blocked != "" {
			res.Blocked = append(res.Blocked, it.Name)
			e.emit(it.Name, "", EventBlocked, it.Blocked)
		}
	}

	builds := map[string]*tplBuild{}
	for i := range plan.Templates {
		spec := &plan.Templates[i]
		builds[spec.Name] = &tplBuild{spec: spec, node: spec.Node}
	}
	succeeded := map[string]bool{}
	pending := plan.Runnable()

	round, cut := 0, false // cut: a stop kept some of a round's items from running
	for ; round <= e.Cfg.Retry.Rounds && len(pending) > 0; round++ {
		if ctx.Err() != nil {
			break // remaining items are reported as not run
		}
		if round > 0 {
			e.emit("", "", EventInfo, fmt.Sprintf("retry round %d: %d VMs", round, len(pending)))
			if err := e.Sleep(ctx, e.Cfg.Retry.RoundPause); err != nil {
				break
			}
		}
		if err := e.refresh(ctx); err != nil {
			if ctx.Err() != nil {
				break
			}
			for _, it := range pending {
				if failed[it.Name] == nil { // keep the more useful earlier error
					failed[it.Name] = fmt.Errorf("listing VMs: %w", err)
				}
			}
			e.emit("", "", EventFailed, "listing VMs: "+err.Error())
			continue
		}
		// Templates build concurrently, up to concurrency.template_builds, and
		// each template's items start as soon as it is built.
		built := e.startTemplates(ctx, plan.Templates, builds, pending)
		pending, cut = e.runItems(ctx, plan.Kind, pending, builds, failed, succeeded)
		built()
	}
	// Only a stop cuts a round short or breaks out of the loop with work and
	// rounds left.
	res.RoundsSkipped = cut || ctx.Err() != nil && len(pending) > 0 && round <= e.Cfg.Retry.Rounds

	for _, it := range plan.Runnable() {
		err := failed[it.Name]
		switch {
		case succeeded[it.Name]:
			res.Succeeded = append(res.Succeeded, it.Name)
		case err == nil:
			// Never ran, e.g. the job was cancelled.
			res.Interrupted[it.Name] = stopped(ctx, "before it started", LeftUntouched)
		case errors.Is(err, context.Canceled):
			res.Interrupted[it.Name] = err
		default:
			res.Failed[it.Name] = err
		}
	}
	e.cleanup(ctx, plan.Templates, builds, succeeded, &res)
	e.mu.Lock()
	for master, msg := range e.mastersStopped {
		res.CleanupFailed[master] = explained(msg) // shown as is: start it, never remove it
	}
	e.mu.Unlock()
	msg := fmt.Sprintf("finished: %d succeeded, %d failed, ", len(res.Succeeded), len(res.Failed))
	if n := len(res.Interrupted); n > 0 {
		msg += fmt.Sprintf("%d interrupted, ", n)
	}
	msg += fmt.Sprintf("%d blocked", len(res.Blocked))
	if n := len(res.Completed) + len(res.Removed) + len(res.CleanupFailed); n > 0 {
		// Only when there was something to clean up: a power job's log
		// shouldn't end with a line of zeros.
		msg += fmt.Sprintf("; cleanup: %d completed, %d removed, %d failed", len(res.Completed), len(res.Removed), len(res.CleanupFailed))
	}
	e.emit("", "", EventInfo, msg)
	return res
}

// CleanupBudget bounds the cleanup at the end of Run. It runs on a context
// detached from the job's, so a cancelled job still cleans up; only killing
// the process skips it.
const CleanupBudget = 10 * time.Minute

// vmRef identifies a VM this run created.
type vmRef struct {
	vmid int
	name string
	node string // where the run expected it; used when the listing is unusable
}

// cleanup leaves the cluster as Run found it, apart from finished work. It
// only ever touches VMs this run created, and re-checks each one's name:
//   - team VMs whose item did not finish are removed;
//   - a template copy whose task failed, or that a stopped job cut off, is
//     removed, unless a later wait saw its task finish OK (see stopCopy);
//   - any other unconverted copy is completed, and removed if that fails.
//     When the copy's task was never seen to end, the copy is completed
//     only if it is present, unlocked and still carries the expected name.
//     A converted template is kept.
//
// A VM that stays locked, changed identity, or fails to delete is not forced;
// it is reported.
func (e *Executor) cleanup(ctx context.Context, specs []TemplateSpec, builds map[string]*tplBuild, succeeded map[string]bool, res *Result) {
	e.mu.Lock()
	var teams []vmRef
	for vmid, o := range e.owned {
		if !o.template && !succeeded[o.name] {
			teams = append(teams, vmRef{vmid, o.name, o.node})
		}
	}
	var tpls []*TemplateSpec
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
			e.emit(ref.name, StepDelete, EventFailed, err.Error())
			return
		}
		e.emit(ref.name, StepDelete, EventFailed, fmt.Sprintf("could not remove %s (%d): %s; remove it in Proxmox",
			ref.name, ref.vmid, proxmox.Describe(err)))
	}
	gone := func(ref vmRef) {
		res.AlreadyGone = append(res.AlreadyGone, ref.name)
		e.emit(ref.name, StepDelete, EventInfo, fmt.Sprintf("%s (%d) is already gone", ref.name, ref.vmid))
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
		// A copy cut off by a cancel may still be running on the node:
		// settle waits it out.
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
		switch key, vol := UnconvertedDisk(cfg); {
		case cfg["template"] == "1" && key != "":
			// A conversion that failed partway sets template: 1 without
			// renaming the disks; such a template can never be cloned.
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

// settle finds out what is at a recorded VMID right before cleanup acts on
// it: the node the VM is on (from the listing if listOK, else ref.node) and
// its config once no task holds it locked, or that it is gone. Anything
// that is not the recorded VM, stays locked or cannot be read is an error,
// and is never acted on.
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

// removeCreated deletes a VM this run created, once settle says nothing is
// working on it and it is still the VM we created. It never forces: if the
// VM stays locked, changed identity, or the delete fails, it is reported
// for manual removal instead.
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
		e.emit(ref.name, StepDelete, EventInfo, fmt.Sprintf("removed %s (created this run; %s)", ref.name, reason))
	}
}

// needsTemplate reports whether any item will clone from the template.
func needsTemplate(items []Item, name string) bool {
	for _, it := range items {
		if it.Template == name && slices.Contains(it.Steps, StepClone) {
			return true
		}
	}
	return false
}

// tplBuild is what the run knows of a template it may build. A build
// writes it before closing ready; items read it after.
type tplBuild struct {
	spec *TemplateSpec // as planned
	node string        // where the template is
	// oldGone: a rebuild deleted the old template, so later rounds create
	// or resume the new one.
	oldGone bool
	err     error         // the last build's error
	done    bool          // built (or found) in this run
	ready   chan struct{} // closed when this round's build ends; nil if none
}

// startTemplates starts building, each in its own goroutine, every template
// that pending items still need, setting its ready channel, which closes
// when its build ends. wait blocks until every build has ended.
//
// Builds take a template_builds slot from the shared Limits for their whole
// run, so the masters stopped at once stay bounded; the copy itself also
// takes one of the master node's clone slots (see ensureTemplate).
func (e *Executor) startTemplates(ctx context.Context, specs []TemplateSpec, builds map[string]*tplBuild, pending []Item) (wait func()) {
	var wg sync.WaitGroup
	for _, spec := range specs {
		b := builds[spec.Name]
		b.ready = nil
		if spec.Blocked != "" || b.done || !needsTemplate(pending, spec.Name) {
			continue
		}
		b.ready = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(b.ready)
			b.err = e.lim.builds.do(ctx, func() error { return e.ensureTemplate(ctx, b) })
			b.done = b.err == nil
			if b.err != nil {
				e.emit(spec.Name, "", EventFailed, proxmox.Describe(b.err))
			}
		}()
	}
	return wg.Wait
}

// runItems runs items, up to concurrency.workers at once, and returns the
// ones to try again: those that failed, and those a stop kept from running
// (cut). An item that clones from a template being built waits for that
// build to end, and fails if the build did; other items start at once, so
// no item waits for a build it doesn't use.
func (e *Executor) runItems(ctx context.Context, kind Kind, items []Item, builds map[string]*tplBuild,
	failed map[string]error, succeeded map[string]bool) (retry []Item, cut bool) {
	workers := make(sem, e.Cfg.Concurrency.Workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, it := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			clones := slices.Contains(it.Steps, StepClone)
			b := builds[it.Template]
			run := func() error {
				var err error
				if clones && b.err != nil {
					err = e.endItem(ctx, it, "", -1, false, fmt.Errorf("template %s: %w", it.Template, b.err))
				} else {
					err = e.runItem(ctx, kind, it, b)
				}
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					delete(failed, it.Name)
					succeeded[it.Name] = true
				} else if failed[it.Name] = err; !proxmox.IsForbidden(err) { // a missing privilege is never tried again
					retry = append(retry, it)
				}
				return nil
			}
			var err error
			if clones && b.ready != nil {
				select {
				case <-b.ready:
				case <-ctx.Done():
					err = ctx.Err()
				}
			}
			if err == nil {
				err = workers.do(ctx, run)
			}
			if err == nil {
				return
			}
			// A stop kept it from running this round.
			mu.Lock()
			defer mu.Unlock()
			cut = true
			retry = append(retry, it)
			if failed[it.Name] != nil { // its earlier round's error is no longer its last word
				failed[it.Name] = e.endItem(ctx, it, "", -1, false, err)
			}
		}()
	}
	wg.Wait()
	return retry, cut
}

func (e *Executor) refresh(ctx context.Context) error {
	var vms []proxmox.VM
	if err := e.call(ctx, func() (err error) { vms, err = e.api.ClusterVMs(ctx); return }); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.present = map[int]proxmox.VM{}
	for _, vm := range vms {
		e.present[vm.VMID] = vm
	}
	return nil
}

// lookup returns the VM currently at vmid, if its name matches.
func (e *Executor) lookup(vmid int, name string) (proxmox.VM, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	vm, ok := e.present[vmid]
	return vm, ok && vm.Name == name
}

func (e *Executor) setPresent(vm proxmox.VM, exists bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if exists {
		e.present[vm.VMID] = vm
	} else {
		delete(e.present, vm.VMID)
	}
}

// errExplained marks an error that says itself what to check or do, so
// it is shown as is, with no advice to remove the VM: the VM may not be this
// run's, or removing it may not be the fix.
var errExplained = errors.New("explained")

// explained is an error text that matches errExplained.
type explained string

func (x explained) Error() string        { return string(x) }
func (x explained) Is(target error) bool { return target == errExplained }

// errUnconfirmed marks a VM whose state cleanup could not determine.
var errUnconfirmed error = explained("could not confirm")

type notOursError struct {
	vmid         int
	holder, name string
}

func (n *notOursError) Error() string {
	return fmt.Sprintf("VMID %d now holds %s, which this run did not create; left alone. Check whether %s needs removing.",
		n.vmid, n.holder, n.name)
}

func (n *notOursError) Is(target error) bool { return target == errExplained }

// CleanupAdvice is the line to show for a Result.CleanupFailed entry. It
// advises removing the VM by hand only when the VM is known to be this run's;
// for a VM of unknown identity or ownership the error already says what to
// check, and suggesting deletion could destroy someone else's VM.
func CleanupAdvice(name string, err error) string {
	if errors.Is(err, errExplained) {
		return err.Error()
	}
	return fmt.Sprintf("Could not remove %s: %s; remove it in Proxmox.", name, proxmox.Describe(err))
}

// halfDeletedError is a VM a failed destroy task left half-deleted: some of
// it (usually its disks) gone and its config holding only lock=destroyed,
// so every later destroy is refused. The error says how to finish it.
type halfDeletedError struct {
	vmid  int
	node  string
	cause error // the failed delete, if this run saw it
}

func (h *halfDeletedError) Error() string {
	msg := fmt.Sprintf("Proxmox left VM %d half-deleted (locked as destroyed); an admin must finish it: `qm destroy %d --skiplock --purge` on %s",
		h.vmid, h.vmid, h.node)
	if h.cause != nil {
		msg += "\nthe delete failed with: " + proxmox.Describe(h.cause)
	}
	return msg
}

// Is matches errExplained. There is no Unwrap: errors.Is and errors.As don't
// reach the cause.
func (h *halfDeletedError) Is(target error) bool { return target == errExplained }

// unconvertedError is a template whose config says template: 1 but one of
// whose disks was never renamed to a base- volume: Proxmox's convert failed
// partway, typically on a storage lock timeout. Linked clones from it fail
// ("Linked clone feature is not supported"), so only a rebuild helps. There
// is no Unwrap: errors.Is and errors.As don't reach the cause.
type unconvertedError struct {
	name     string
	vmid     int
	key, vol string
	cause    error // the convert task's error, if it failed
	reused   bool  // found on an existing template rather than after converting
}

func (u *unconvertedError) Error() string {
	if u.reused {
		return fmt.Sprintf("template %s (%d) has disk %s (%s) unconverted: an earlier conversion failed partway, so linked clones from it would fail; deploy with rebuild to recreate it",
			u.name, u.vmid, u.key, u.vol)
	}
	msg := fmt.Sprintf("converting to template left disk %s (%s) unconverted (Proxmox may have timed out on a storage lock); rebuild the template", u.key, u.vol)
	if u.cause != nil {
		msg += "\nthe convert task failed with: " + proxmox.Describe(u.cause)
	}
	return msg
}

// leftoverDisksError is a VM that is deleted but whose disks could not all
// be freed. The error says how to free them by hand.
type leftoverDisksError struct {
	vmid   int
	node   string
	vols   []string
	causes []string // why freeing failed, per volume
	// holder names the VM that has the VMID now; its disks may be the
	// ones listed, so nothing was freed.
	holder string
}

func (l *leftoverDisksError) Error() string {
	if l.holder != "" {
		return fmt.Sprintf("VM %d is deleted, but VMID %d is now held by %s, so the disks the deleted VM left (%s) were not freed: they may be that VM's own now. Check in Proxmox which VM they belong to.",
			l.vmid, l.vmid, l.holder, strings.Join(l.vols, ", "))
	}
	var cmds []string
	for _, v := range l.vols {
		cmds = append(cmds, "`pvesm free "+v+"`")
	}
	msg := fmt.Sprintf("VM %d is deleted, but its disks %s are still on the storage and could not be freed. "+
		"First check that VMID %d is still free (`qm config %d` fails on every node): a VM created there since gets disks with the same names. Then free them by hand: %s on %s",
		l.vmid, strings.Join(l.vols, ", "), l.vmid, l.vmid, strings.Join(cmds, ", "), l.node)
	if len(l.causes) > 0 {
		msg += "\nfreeing failed with: " + strings.Join(l.causes, "; ")
	}
	return msg
}

func (l *leftoverDisksError) Is(target error) bool { return target == errExplained }

// masterWaitBudget is how long a failed template build waits for the
// master's copy to finish before restarting the master.
const masterWaitBudget = 30 * time.Minute

// copyStopWait is how long a stopped run waits for a template copy it
// stopped, and for its master's stop task, to end before restarting the
// master; masterRestartBudget bounds the restart.
const (
	copyStopWait        = 2 * time.Minute
	masterRestartBudget = 2 * time.Minute
)

// StopBudget is the longest a Run takes to return once its context is done:
// copyStopWait and masterRestartBudget for a template build that stopped its
// master, then CleanupBudget. On shutdown a cancel's grace doesn't add to
// it: Halt ends it. A process that stops its jobs on shutdown (battleship
// serve) should wait a little longer than this for them.
const StopBudget = copyStopWait + masterRestartBudget + CleanupBudget

// retryable reports whether a call that failed with err is tried again.
// Besides transient errors, that is a VM locked by another task, a VMID that
// "already exists" (the retry probes it again), and a VM that "does not
// exist", which right after a clone to another node may only not be visible
// there yet.
func (e *Executor) retryable(err error) bool {
	switch e.classifier.Classify(err) {
	case proxmox.Transient, proxmox.Locked, proxmox.Exists, proxmox.NotFound:
		return true
	}
	return false
}

// call limits concurrent API calls and retries them (see retryable) with
// exponential backoff.
func (e *Executor) call(ctx context.Context, fn func() error) error {
	return e.retrier.Do(ctx, func() error { return e.lim.Call(ctx, fn) })
}

// check is call for a read that asks whether a VM is still there: "does not
// exist" is its answer, returned at once rather than retried.
func (e *Executor) check(ctx context.Context, fn func() error) error {
	r := e.retrier
	r.Retryable = func(err error) bool { return !proxmox.IsNotFound(err) && e.retryable(err) }
	return r.Do(ctx, func() error { return e.lim.Call(ctx, fn) })
}

// task starts a Proxmox task and waits for it. The POST that starts it is
// retried through call. A failed wait is not retried by re-POSTing, because
// the task may have done part of its work; only a task that ended with an
// error restartable accepts is started again, up to Retry.Attempts starts in
// total with exponential backoff. Any other wait error is returned as is.
func (e *Executor) task(ctx context.Context, start func() (string, error)) error {
	_, err := e.taskUPID(ctx, 0, start, e.restartable)
	return err
}

// restartable reports whether a task that ended with err may be started
// again: only a TaskError that is transient or names a lock another task
// held.
func (e *Executor) restartable(err error) bool {
	var te *proxmox.TaskError
	m := e.classifier.Classify(err)
	return errors.As(err, &te) && (m == proxmox.Transient || m == proxmox.Locked)
}

// taskOnce is task without the task-level restart, for tasks that must not
// be started again after failing, such as a shutdown that timed out.
func (e *Executor) taskOnce(ctx context.Context, start func() (string, error)) error {
	_, err := e.taskUPID(ctx, 0, start, nil)
	return err
}

// taskUPID is task, but also returns the UPID of the last task it started,
// which is empty if none was. restart decides whether a failed task is
// started again; nil never restarts. vmid is the task's VM if its UPID
// names another (a clone's names its source, but the clone locks its
// target), else 0.
func (e *Executor) taskUPID(ctx context.Context, vmid int, start func() (string, error), restart func(error) bool) (string, error) {
	// Only a failed task is re-started here; errors from the POST were already
	// retried by call and pass through as permanent.
	tr := e.retrier
	tr.Retryable = func(err error) bool {
		var te *proxmox.TaskError
		return restart != nil && errors.As(err, &te) && restart(err)
	}
	var upid string
	err := tr.Do(ctx, func() error {
		upid = ""
		if err := e.call(ctx, func() (err error) { upid, err = start(); return }); err != nil {
			return err
		}
		err := e.api.WaitTask(ctx, upid, e.Cfg.Retry.TaskPoll)
		if proxmox.IsForbidden(err) {
			return e.unfollowable(ctx, upid, vmid, err)
		}
		return err
	})
	return upid, err
}

// unfollowable handles a task Proxmox won't let the job follow (403 on its
// status: a task of another user's, without Sys.Audit on its node). Its
// outcome is never assumed: unfollowable waits, up to masterWaitBudget,
// until the task's VM is no longer locked, so the task has ended and nothing
// races it, and returns an error saying the outcome is unknown. The step
// fails; a retry round, whose steps check the VM's state first, finds out
// what the task did. The VM is vmid if given (a clone locks its target, not
// the source its UPID names), else the one in the UPID.
func (e *Executor) unfollowable(ctx context.Context, upid string, vmid int, err error) error {
	u, _ := proxmox.ParseUPID(upid)
	if vmid == 0 && u.Type != "qmclone" {
		vmid, _ = strconv.Atoi(u.ID)
	}
	node := u.Node
	state := "its VM couldn't be checked"
	if vmid > 0 && node != "" {
		wctx, cancel := context.WithTimeout(ctx, masterWaitBudget)
		defer cancel()
		switch _, lock, lerr := e.waitUnlocked(wctx, node, vmid); {
		case lerr != nil:
			state = "VM " + strconv.Itoa(vmid) + " couldn't be read: " + proxmox.Describe(lerr)
		case lock != "":
			state = "VM " + strconv.Itoa(vmid) + " is still locked (" + lock + ")"
		default:
			state = "VM " + strconv.Itoa(vmid) + " is no longer locked, so it has ended"
		}
	}
	return &unfollowedError{msg: fmt.Sprintf("couldn't follow task %s (%s); %s, but whether it worked is unknown", upid, proxmox.Describe(err), state)}
}

// unfollowedError is a task unfollowable gave up following. It is not the
// 403 itself: the step's outcome is unknown, so a retry round re-checks it.
type unfollowedError struct{ msg string }

func (u *unfollowedError) Error() string { return u.msg }

func (e *Executor) runItem(ctx context.Context, kind Kind, it Item, tpl *tplBuild) error {
	if len(it.Steps) == 0 {
		return e.endItem(ctx, it, "", -1, false, errors.New("no steps planned"))
	}
	vm, exists := e.lookup(it.VMID, it.Name)
	if exists {
		it.Node = vm.Node
	}
	if !exists && !slices.Contains(it.Steps, StepClone) {
		if kind == KindTeardown {
			// StepDelete, or StepFreeDisks for a VM already gone at planning.
			step := it.Steps[len(it.Steps)-1]
			// Gone by name is not enough: a VM half-deleted by a failed
			// destroy is listed under a placeholder name ("VM <vmid>").
			if err := e.checkVMIDFree(ctx, it.VMID); err != nil {
				return e.endItem(ctx, it, step, -1, false, err)
			}
			// Gone, but an earlier attempt may have left its disks.
			if err := e.retryLeftovers(ctx, it.VMID, it.Node, it.Name); err != nil {
				return e.endItem(ctx, it, step, -1, false, err)
			}
			if step == StepFreeDisks {
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
		if err == nil && !slices.ContainsFunc(it.Steps[i+1:], Step.ChangesConfig) {
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
		if step == StepClone {
			exists = true
		}
	}
	return nil
}

// endItem reports how an item that didn't finish ended, and returns its
// error: the one place that tells a stop (ctx done, err a cancellation)
// from a failure. step is where it ended, i its index in the item's steps
// (-1: outside them), and underWay that the step had sent Proxmox a change.
// Only a step under way is logged as the item's step, so the step column
// keeps the last step the item reported, and one that never reached a
// step reads as not run.
func (e *Executor) endItem(ctx context.Context, it Item, step Step, i int, underWay bool, err error) error {
	if ctx.Err() == nil || !errors.Is(err, context.Canceled) {
		e.emit(it.Name, step, EventFailed, proxmox.Describe(err))
		if step != "" {
			return fmt.Errorf("%s: %w", step, err)
		}
		return err
	}
	when, at := "before "+string(step), Step("")
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

// leftAt is how an item stopped at its step i (-1: outside its steps) left
// its VM's config: as its last change sent or config steps finished left
// it, else untouched. An item with no step that changes the config is
// converged once it reaches its steps, though a later round's stop before
// them still finds it untouched.
func (e *Executor) leftAt(it Item, i int) Left {
	e.mu.Lock()
	l, ok := e.left[it.Name]
	e.mu.Unlock()
	switch {
	case ok:
		return l
	case i >= 0 && !slices.ContainsFunc(it.Steps, Step.ChangesConfig):
		return LeftConverged
	}
	return LeftUntouched
}

// stepContext is the context a step runs with. A stop reaches it at once,
// unless it is a cancel (ErrCancelRequested) and the step already sent
// Proxmox a change: then only after jobs.cancel_grace, or when Halt closes,
// so the step sees how its task ended. From the stop on, the step sends no
// further change (see guardedAPI).
func (e *Executor) stepContext(ctx context.Context) (_ context.Context, done func(), sent func() bool) {
	st := &stepState{run: ctx}
	sctx, cancel := context.WithCancelCause(context.WithValue(context.WithoutCancel(ctx), stepKey{}, st))
	go func() {
		select {
		case <-sctx.Done():
			return
		case <-ctx.Done():
		}
		cause := context.Cause(ctx)
		if errors.Is(cause, ErrCancelRequested) && st.hasSent() {
			t := time.NewTimer(e.Cfg.Jobs.CancelGrace)
			defer t.Stop()
			select {
			case <-sctx.Done():
				return
			case <-t.C:
			case <-e.Halt:
			}
		}
		cancel(cause)
	}()
	return sctx, func() { cancel(nil) }, st.hasSent
}

type stepKey struct{}

// stepState is what a step's context knows of it: the run it belongs to,
// and whether it sent Proxmox a change.
type stepState struct {
	run  context.Context
	mu   sync.Mutex
	sent bool
}

func (s *stepState) hasSent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

// errHeldBack is a change a step didn't send because its run was stopped.
var errHeldBack = fmt.Errorf("not sent to Proxmox: the job was stopped: %w", context.Canceled)

// mayChange is called before each change a step sends: it refuses once the
// step's run has stopped, and otherwise records that the step sent one.
// Calls outside a step (template builds, cleanup) aren't held back.
func mayChange(ctx context.Context) error {
	st, ok := ctx.Value(stepKey{}).(*stepState)
	if !ok {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.run.Err() != nil {
		return errHeldBack
	}
	st.sent = true
	return nil
}

// guardedAPI is an API whose changes go through mayChange. Reads, task
// waits and StopTask pass through.
type guardedAPI struct{ API }

func (g guardedAPI) SetVMConfig(ctx context.Context, node string, vmid int, changes map[string]string) error {
	if err := mayChange(ctx); err != nil {
		return err
	}
	return g.API.SetVMConfig(ctx, node, vmid, changes)
}

func (g guardedAPI) Clone(ctx context.Context, r proxmox.CloneRequest) (string, error) {
	if err := mayChange(ctx); err != nil {
		return "", err
	}
	return g.API.Clone(ctx, r)
}

func (g guardedAPI) ConvertToTemplate(ctx context.Context, node string, vmid int) (string, error) {
	if err := mayChange(ctx); err != nil {
		return "", err
	}
	return g.API.ConvertToTemplate(ctx, node, vmid)
}

func (g guardedAPI) RegenerateCloudInit(ctx context.Context, node string, vmid int) error {
	if err := mayChange(ctx); err != nil {
		return err
	}
	return g.API.RegenerateCloudInit(ctx, node, vmid)
}

func (g guardedAPI) CreateSnapshot(ctx context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error) {
	if err := mayChange(ctx); err != nil {
		return "", err
	}
	return g.API.CreateSnapshot(ctx, node, vmid, r)
}

func (g guardedAPI) Rollback(ctx context.Context, node string, vmid int, snapshot string) (string, error) {
	if err := mayChange(ctx); err != nil {
		return "", err
	}
	return g.API.Rollback(ctx, node, vmid, snapshot)
}

func (g guardedAPI) Power(ctx context.Context, node string, vmid int, action string) (string, error) {
	if err := mayChange(ctx); err != nil {
		return "", err
	}
	return g.API.Power(ctx, node, vmid, action)
}

func (g guardedAPI) Shutdown(ctx context.Context, node string, vmid int, timeout time.Duration, forceStop bool) (string, error) {
	if err := mayChange(ctx); err != nil {
		return "", err
	}
	return g.API.Shutdown(ctx, node, vmid, timeout, forceStop)
}

func (g guardedAPI) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	if err := mayChange(ctx); err != nil {
		return "", err
	}
	return g.API.DeleteVM(ctx, node, vmid)
}

func (g guardedAPI) DeleteVolume(ctx context.Context, node, storage, volid string) (string, error) {
	if err := mayChange(ctx); err != nil {
		return "", err
	}
	return g.API.DeleteVolume(ctx, node, storage, volid)
}

// runStep performs one step and reports whether it changed anything.
func (e *Executor) runStep(ctx context.Context, kind Kind, it *Item, step Step, tpl *tplBuild, exists bool) (bool, error) {
	switch step {
	case StepClone:
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

	case StepNetwork:
		return e.applyConfig(ctx, it, true, func(cur map[string]string) map[string]string {
			return NetworkChanges(e.Cfg.Network, cur, it.Team, tpl.spec.Interfaces)
		})
	case StepDiskLimits:
		return e.applyConfig(ctx, it, false, func(cur map[string]string) map[string]string {
			return DiskLimitChanges(cur, e.Cfg.Deploy.DiskMBpsRead, e.Cfg.Deploy.DiskMBpsWrite)
		})
	case StepCDROM:
		return e.applyConfig(ctx, it, false, CDROMChanges)

	case StepSnapshot:
		if kind == KindSnapshot {
			return e.takeSnapshot(ctx, it)
		}
		var snaps []proxmox.Snapshot
		if err := e.call(ctx, func() (err error) { snaps, err = e.api.Snapshots(ctx, it.Node, it.VMID); return }); err != nil {
			return false, err
		}
		// Any baseline will do, such as the old deploy tool's: a new one
		// taken now would capture whatever the VM has been through.
		if HasBaseline(e.Cfg.Deploy, SnapshotNames(snaps)) {
			return false, nil
		}
		status, err := e.status(ctx, it)
		if err != nil {
			return false, err
		}
		if status == "running" {
			return false, fmt.Errorf("VM is running and has %s; stop it first so the baseline is clean", NoBaseline(e.Cfg.Deploy))
		}
		return true, e.createSnapshot(ctx, it.Node, it.VMID, proxmox.SnapshotRequest{Name: e.Cfg.Deploy.SnapshotName, Description: "baseline from battleship deploy"})

	case StepStart:
		return e.power(ctx, it, "start")
	case StepStop:
		if kind == KindTeardown {
			return e.shutdownForDelete(ctx, it)
		}
		return e.power(ctx, it, "stop") // reset: the rollback discards the state anyway
	case StepPower:
		return e.power(ctx, it, it.Action)

	case StepRollback:
		return true, e.task(ctx, func() (string, error) { return e.api.Rollback(ctx, it.Node, it.VMID, it.Snapshot) })

	case StepDelete:
		err := e.deleteVM(ctx, it.Node, it.VMID, it.Name)
		if err == nil {
			e.setPresent(proxmox.VM{VMID: it.VMID}, false)
		}
		return err == nil, err
	}
	return false, fmt.Errorf("unknown step %q", step)
}

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
		disks = DiskVolumes(cfg)
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

// pollVM reads the VM's config every TaskPoll until done accepts it or ctx
// ends, and returns the last config read and its error. A VM that does not
// exist is read as a nil config; done never sees other read errors. Like
// task polling, it takes no config-call slot.
func (e *Executor) pollVM(ctx context.Context, node string, vmid int, done func(cfg map[string]string) bool) (map[string]string, error) {
	for {
		cfg, err := e.api.VMConfig(ctx, node, vmid)
		if proxmox.IsNotFound(err) {
			cfg, err = nil, nil
		}
		if err == nil && done(cfg) {
			return cfg, nil
		}
		if proxmox.IsForbidden(err) || proxmox.IsLapsed(err) {
			return cfg, err // asking again can't help
		}
		if e.Sleep(ctx, e.Cfg.Retry.TaskPoll) != nil {
			return cfg, err
		}
	}
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
			e.emit(name, StepDelete, EventInfo, fmt.Sprintf("VM %d is deleted; the job was stopped before it freed the disks the delete left on the storage: %s", vmid, strings.Join(left, ", ")))
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
		e.emit(name, StepDelete, EventInfo, fmt.Sprintf("freed disks that deleting VM %d left on the storage: %s", vmid, strings.Join(freed, ", ")))
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
		files[storage][volumeFile(vol)] = true
	}
	var left []string
	for _, storage := range slices.Sorted(maps.Keys(files)) {
		var vols []string
		if err := e.call(ctx, func() (err error) { vols, err = e.api.StorageContent(ctx, node, storage, vmid); return }); err != nil {
			return nil, fmt.Errorf("listing %s: %w", storage, err)
		}
		for _, v := range vols {
			if files[storage][volumeFile(v)] {
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
			if owner, base := volumeOwner(v); owner == vmid {
				l.disks[v] = v
				l.template = l.template || base
			}
		}
	}
	return e.freeLeftovers(ctx, vmid, name, l)
}

// volumeOwner is the VMID a volume's file name says owns it
// (vm-<vmid>-… or base-<vmid>-…), and whether it is a base volume; 0 for
// any other file, such as an ISO or an import.
func volumeOwner(vol string) (vmid int, base bool) {
	file := volumeFile(vol)
	rest, base := strings.CutPrefix(file, "base-")
	if !base {
		var ok bool
		if rest, ok = strings.CutPrefix(file, "vm-"); !ok {
			return 0, false
		}
	}
	digits, _, ok := strings.Cut(rest, "-")
	n, err := strconv.Atoi(digits)
	if !ok || err != nil {
		return 0, false
	}
	return n, base
}

// readVM reads a VM's config through check, reporting "does not exist" as
// gone.
func (e *Executor) readVM(ctx context.Context, node string, vmid int) (cfg map[string]string, gone bool, err error) {
	err = e.check(ctx, func() (err error) { cfg, err = e.api.VMConfig(ctx, node, vmid); return })
	if proxmox.IsNotFound(err) {
		return nil, true, nil
	}
	return cfg, false, err
}

// checkVMIDFree is for a delete whose VM is no longer listed under its name:
// it fails if the VMID now holds a VM that a failed destroy left locked as
// destroyed. Any other VM there is not this item's and is left alone.
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

// shutdownForDelete takes a running VM down before teardown deletes it. It
// asks the guest to shut down cleanly, which lets routers
// release their DHCP lease (udhcpc -R) so the next deploy gets the address
// at once. Proxmox hard-stops the VM if it is still running after
// teardown.shutdown_timeout (forceStop); if the shutdown task fails anyway
// and the VM is still running, it is hard-stopped here.
func (e *Executor) shutdownForDelete(ctx context.Context, it *Item) (bool, error) {
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
	e.emit(it.Name, StepStop, EventInfo, "clean shutdown failed, stopping it hard: "+proxmox.Describe(err))
	if serr := e.taskOnce(ctx, func() (string, error) { return e.api.Power(ctx, it.Node, it.VMID, "stop") }); serr != nil {
		return true, fmt.Errorf("hard stop after a failed shutdown (%s): %w", proxmox.Describe(err), serr)
	}
	return true, nil
}

func (e *Executor) status(ctx context.Context, it *Item) (string, error) {
	var status string
	err := e.call(ctx, func() (err error) { status, err = e.api.CurrentStatus(ctx, it.Node, it.VMID); return })
	return status, err
}

// startClone starts a clone into req.NewVMID, on req.TargetNode, and
// returns its UPID. A POST that timed out may still have created the VM, so
// the target is probed first: a VM there with the expected name means an
// earlier POST took effect, and "" is returned. For a template's copy that
// VM must be this run's, since only a VM this run created is converted.
// Only a VM that was definitely absent becomes this run's, and from the POST
// on it stays so even if the request times out after Proxmox accepted it.
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

// probeClone looks at vmid before a clone. Only a "does not exist" answer
// counts as absent; any other read error is returned so the step retries or
// fails rather than claiming a VM it may not have created. A VM that is
// still locked is reported as proxmox.ErrLocked so the caller backs off until the
// clone finishes.
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
		e.emit(name, StepClone, EventInfo, "waiting: "+err.Error())
		return probeSame, err
	}
	return probeSame, nil
}

// applyConfig reads the VM config, computes changes, and applies them. When
// cloudInit is set and the VM has a cloud-init drive, the drive is
// regenerated every time. It reports whether config changes were applied.
func (e *Executor) applyConfig(ctx context.Context, it *Item, cloudInit bool,
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
	// Regenerate even without changes, so a failed regeneration on an earlier
	// attempt is repaired by the next one.
	if cloudInit && HasCloudInit(cur) {
		if err := e.call(ctx, func() error { return e.api.RegenerateCloudInit(ctx, it.Node, it.VMID) }); err != nil {
			return changed, fmt.Errorf("regenerating cloud-init: %w", err)
		}
	}
	return changed, nil
}

// power applies action, skipping a VM already in the status the action
// leaves (see PowerAction.Leaves). Rebooting a VM that is not running starts
// it.
func (e *Executor) power(ctx context.Context, it *Item, action string) (bool, error) {
	status, err := e.status(ctx, it)
	if err != nil {
		return false, err
	}
	if a, _ := PowerActionOf(action); a.Leaves != "" && status == a.Leaves {
		return false, nil
	}
	if action == "reboot" && status != "running" {
		action = "start"
	}
	return true, e.taskOnce(ctx, func() (string, error) { return e.api.Power(ctx, it.Node, it.VMID, action) })
}

// ensureTemplate creates b's template from its master, first deleting the
// old one when rebuilding, and records where it is. A VM with the
// template's name that was left unconverted by an earlier attempt is
// resumed rather than cloned again. Ported from create_template_from_master.
func (e *Executor) ensureTemplate(ctx context.Context, b *tplBuild) error {
	spec := b.spec
	name := spec.Name
	cur, found := e.lookup(spec.VMID, name)
	switch {
	case spec.Rebuild && !b.oldGone:
		// An earlier round deleted the old template but not all its disks.
		if err := e.retryLeftovers(ctx, spec.VMID, cmp.Or(spec.OldNode, spec.MasterNode), name); err != nil {
			return fmt.Errorf("deleting old template: %w", err)
		}
		if found {
			if vm, ok := e.teamVMUsing(spec.Host); ok {
				return fmt.Errorf("team VMs such as %s still use this template; tear them down first", vm.Name)
			}
			node := cur.Node
			if node == "" {
				node = spec.OldNode
			}
			if err := e.deleteVM(ctx, node, spec.VMID, name); err != nil {
				if e.classifier.Classify(err) == proxmox.InUse {
					return fmt.Errorf("deleting old template: linked-clone disks still use it; remove them in Proxmox and re-run: %w", err)
				}
				return fmt.Errorf("deleting old template: %w", err)
			}
			e.setPresent(proxmox.VM{VMID: spec.VMID}, false)
			e.emit(name, StepDelete, EventDone, "old template deleted")
		}
		b.oldGone = true // later rounds must create or resume, never delete the new VM
	case found && cur.Template:
		// A template an earlier run left half-converted would fail every
		// linked clone; fail once here instead.
		cfg, gone, err := e.readVM(ctx, cur.Node, spec.VMID)
		switch {
		case gone:
			return fmt.Errorf("template %s (%d) no longer exists", name, spec.VMID)
		case err != nil:
			return fmt.Errorf("checking template %s's disks: %w", name, err)
		}
		if key, vol := UnconvertedDisk(cfg); key != "" {
			return &unconvertedError{name: name, vmid: spec.VMID, key: key, vol: vol, reused: true}
		}
		b.node = cur.Node
		e.emit(name, StepClone, EventSkipped, "template exists")
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
	case spec.Exists && !spec.Rebuild:
		return fmt.Errorf("template %s (%d) no longer exists", name, spec.VMID)
	}
	e.mu.Lock()
	other, taken := e.present[spec.VMID]
	e.mu.Unlock()
	if taken {
		return fmt.Errorf("VMID %d is now used by %s", spec.VMID, other.Name)
	}

	// The copy counts against the master node's clone slots, like team
	// clones from that node. The slot is taken before the master is stopped,
	// so the master is not left down while the build waits for one.
	if err := e.lim.cloneSlot(spec.MasterNode).do(ctx, func() error { return e.copyMaster(ctx, spec) }); err != nil {
		return err
	}
	e.setPresent(proxmox.VM{VMID: spec.VMID, Name: name, Node: spec.MasterNode}, true)
	b.node = spec.MasterNode
	return e.finishTemplate(ctx, spec, spec.MasterNode)
}

// copyMaster clones spec's master to spec.VMID, stopping the master first if
// it is running and starting it again once the copy can no longer be
// running. A copy still running when the job is stopped is stopped too (see
// stopCopy).
func (e *Executor) copyMaster(ctx context.Context, spec *TemplateSpec) error {
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
	// Registered before the master's stop is sent: a cancel or wait failure
	// during the stop leaves it possibly stopped, to restart or report.
	defer func() {
		wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), masterWaitBudget)
		defer wcancel()
		// A stop stops a copy it cut off and cuts restartMaster's waits to copyStopWait.
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
		stopUPID, stopErr = e.taskUPID(ctx, 0, func() (string, error) {
			return e.api.Power(ctx, spec.MasterNode, spec.MasterVMID, "stop")
		}, nil)
		if stopErr != nil {
			return fmt.Errorf("stopping master: %w", stopErr)
		}
	}

	cloneUPID, cloneErr = e.taskUPID(ctx, spec.VMID, func() (string, error) {
		return e.startClone(ctx, true, proxmox.CloneRequest{
			SourceNode: spec.MasterNode, SourceVMID: spec.MasterVMID, NewVMID: spec.VMID, Name: name,
			TargetNode: spec.MasterNode, Full: true, Storage: e.Cfg.Deploy.Storage,
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

// startedTask is a Proxmox task a step started: its kind, UPID and how waiting
// for it ended.
type startedTask struct {
	kind, upid string
	err        error
}

// restartMaster starts the master a template build stopped, even after a
// cancel, but only once its stop and its copy can no longer be running,
// waiting for them within wctx: starting the master under a copy corrupts
// it. A master it can't restart is reported left stopped.
func (e *Executor) restartMaster(ctx, wctx context.Context, spec *TemplateSpec, tasks ...startedTask) {
	// leftStopped reports the master left stopped, in the run's result
	// too: someone must start it.
	leftStopped := func(reason string) {
		msg := "master " + spec.MasterName + " left stopped: " + reason + "; start it in Proxmox once nothing is working on it"
		e.emit(spec.Name, StepStart, EventFailed, msg)
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
			// It can't be followed; the lock checks below say when it is
			// over.
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
	if err := e.taskOnce(rctx, func() (string, error) { return e.api.Power(rctx, spec.MasterNode, spec.MasterVMID, "start") }); err != nil {
		leftStopped("restarting it failed: " + proxmox.Describe(err))
	}
}

// stopCopy asks Proxmox to stop a template copy that a stopped job cut off,
// and marks the copy failed: cleanup removes it, unless a later wait sees its
// task finish OK (see settleCopy).
func (e *Executor) stopCopy(ctx context.Context, spec *TemplateSpec, upid string) {
	e.mu.Lock()
	if o := e.owned[spec.VMID]; o != nil && o.state == cloneUnknown {
		o.state = cloneTaskFailed
	}
	e.mu.Unlock()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := e.api.StopTask(sctx, upid); err != nil {
		e.emit(spec.Name, StepClone, EventInfo, "stopping the copy of "+spec.MasterName+": "+proxmox.Describe(err))
		return
	}
	e.emit(spec.Name, StepClone, EventInfo, "stopped the copy of "+spec.MasterName+" (the job was stopped)")
}

// settleCopy records how a template copy's task ended, once a wait for it
// says: nil is OK, a TaskError failed; anything else leaves it as it was.
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

// teamVMUsing returns a team VM for host that currently exists. Rebuilding
// deletes the template, which would break its linked clones.
func (e *Executor) teamVMUsing(host string) (proxmox.VM, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var best proxmox.VM
	found := false
	for _, vm := range e.present {
		if _, h, ok := e.naming.ParseVMName(vm.Name); ok && h == host && !vm.Template && (!found || vm.VMID < best.VMID) {
			best, found = vm, true
		}
	}
	return best, found
}

// waitUnlocked polls the VM's config every TaskPoll until it has no lock or
// ctx is done, and returns the last config it read, nil if the VM does not
// exist. A missing VM counts as unlocked. Any other read error counts as
// unknown and is polled again. If ctx ends first it returns the lock still
// held, or else the last read error.
func (e *Executor) waitUnlocked(ctx context.Context, node string, vmid int) (map[string]string, string, error) {
	cfg, err := e.pollVM(ctx, node, vmid, func(cfg map[string]string) bool { return cfg["lock"] == "" })
	if err != nil {
		return cfg, "", err
	}
	return cfg, cfg["lock"], nil
}

// finishTemplate ejects the CD-ROM and converts the cloned VM on node to a
// template. It is safe to repeat after a partial failure.
func (e *Executor) finishTemplate(ctx context.Context, spec *TemplateSpec, node string) error {
	name := spec.Name
	tplItem := &Item{Name: name, VMID: spec.VMID, Node: node}
	if _, err := e.applyConfig(ctx, tplItem, false, CDROMChanges); err != nil {
		return fmt.Errorf("ejecting CD-ROM: %w", err)
	}
	// Converting renames disks to base-*, which linked clones require. Its
	// result is checked below rather than trusted. Conversions take the
	// storage lock, so they run one at a time (see Limits).
	convErr := e.lim.storageOps.do(ctx, func() error { return e.convert(ctx, node, spec.VMID) })
	if convErr != nil && ctx.Err() != nil {
		return fmt.Errorf("converting to template: %w", convErr)
	}
	cfg, gone, rerr := e.readVM(ctx, node, spec.VMID)
	switch {
	case (gone || rerr != nil) && convErr != nil:
		return fmt.Errorf("converting to template: %w", convErr)
	case gone:
		return fmt.Errorf("converting to template: VM %d disappeared", spec.VMID)
	case rerr != nil:
		return fmt.Errorf("converting to template finished, but its disks could not be checked: %w", rerr)
	}
	isTemplate := cfg["template"] == "1"
	if key, vol := UnconvertedDisk(cfg); key != "" && isTemplate {
		return &unconvertedError{name: name, vmid: spec.VMID, key: key, vol: vol, cause: convErr}
	}
	switch {
	case convErr != nil:
		return fmt.Errorf("converting to template: %w", convErr)
	case !isTemplate:
		return fmt.Errorf("converting to template: the task finished OK, but VM %d is still not a template", spec.VMID)
	}
	e.setPresent(proxmox.VM{VMID: spec.VMID, Name: name, Node: node, Template: true}, true)
	e.emit(name, StepClone, EventDone, "template created")
	return nil
}

// convertWait bounds how long a conversion whose outcome is unknown gets to
// rename its disks.
const convertWait = 2 * time.Minute

// convert converts the VM to a template and waits for the task. Proxmox sets
// template: 1 before its task renames the disks, so each request is preceded
// by a read and none is sent once the VM says template: 1 (it would fail, and
// a restart can't rename what a failed task left), and a failed task is never
// restarted. When an earlier request converted the VM or this one's outcome
// is unknown, convert waits up to convertWait for the disks to be renamed and
// returns nil if the VM says template: 1; the caller checks the disks.
func (e *Executor) convert(ctx context.Context, node string, vmid int) error {
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
		key, _ := UnconvertedDisk(cfg)
		return key == ""
	})
	if cfg["template"] == "1" {
		return nil
	}
	return err
}

// errVanished is a VM that disappeared during a step. Unlike "does not
// exist", which may mean the VM is not visible on its node yet, it is not
// retried.
var errVanished = errors.New("the VM disappeared")
