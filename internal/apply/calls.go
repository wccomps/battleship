package apply

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/wccomps/battleship/internal/proxmox"
)

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

// readVM reads a VM's config through check, reporting "does not exist" as
// gone.
func (e *Executor) readVM(ctx context.Context, node string, vmid int) (cfg map[string]string, gone bool, err error) {
	err = e.check(ctx, func() (err error) { cfg, err = e.api.VMConfig(ctx, node, vmid); return })
	if proxmox.IsNotFound(err) {
		return nil, true, nil
	}
	return cfg, false, err
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
