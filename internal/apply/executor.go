// Package apply carries out a pods.Plan on Proxmox: the Executor and the
// Limits that cap the load it puts on the cluster.
package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
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
	Step    pods.Step
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
	API     pods.API
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
	api pods.API

	naming     pods.Naming
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
	e.naming = pods.NewNaming(e.Cfg.Naming)
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

func (e *Executor) emit(item string, step pods.Step, status EventStatus, msg string) {
	if e.OnEvent != nil {
		e.OnEvent(Event{Time: time.Now(), Item: item, Step: step, Status: status, Message: msg})
	}
}

// Run executes the plan, then retries failed items for the configured number
// of rounds.
func (e *Executor) Run(ctx context.Context, plan *pods.Plan) Result {
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

// runItems runs items, up to concurrency.workers at once, and returns the
// ones to try again: those that failed, and those a stop kept from running
// (cut). An item that clones from a template being built waits for that
// build to end, and fails if the build did; other items start at once, so
// no item waits for a build it doesn't use.
func (e *Executor) runItems(ctx context.Context, kind pods.Kind, items []pods.Item, builds map[string]*tplBuild,
	failed map[string]error, succeeded map[string]bool) (retry []pods.Item, cut bool) {
	workers := make(sem, e.Cfg.Concurrency.Workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, it := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			clones := slices.Contains(it.Steps, pods.StepClone)
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

// StopBudget is the longest a Run takes to return once its context is done:
// copyStopWait and masterRestartBudget for a template build that stopped its
// master, then CleanupBudget. On shutdown a cancel's grace doesn't add to
// it: Halt ends it. A process that stops its jobs on shutdown (battleship
// serve) should wait a little longer than this for them.
const StopBudget = copyStopWait + masterRestartBudget + CleanupBudget
