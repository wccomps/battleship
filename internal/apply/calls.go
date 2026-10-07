package apply

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/wccomps/battleship/internal/proxmox"
)

// retryable reports whether a call failing with err is retried: transient
// errors, a locked VM, "already exists" (re-probed), and "does not exist",
// which after a cross-node clone may just mean not visible yet.
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

// check is call for a "is the VM still there" read: "does not exist" is
// the answer, so it isn't retried.
func (e *Executor) check(ctx context.Context, fn func() error) error {
	r := e.retrier
	r.Retryable = func(err error) bool { return !proxmox.IsNotFound(err) && e.retryable(err) }
	return r.Do(ctx, func() error { return e.lim.Call(ctx, fn) })
}

// task starts a Proxmox task and waits for it. The POST is retried through
// call. A failed wait is never re-POSTed, since the task may have done part
// of its work; only a task error that restartable accepts is restarted, up
// to Retry.Attempts starts.
func (e *Executor) task(ctx context.Context, start func() (string, error)) error {
	_, err := e.taskUPID(ctx, 0, start, e.restartable)
	return err
}

// restartable reports whether a failed task may be restarted: only a
// transient TaskError or one naming another task's lock.
func (e *Executor) restartable(err error) bool {
	var te *proxmox.TaskError
	m := e.classifier.Classify(err)
	return errors.As(err, &te) && (m == proxmox.Transient || m == proxmox.Locked)
}

// taskOnce is task without restarts, for tasks that must not run again
// after failing, such as a timed-out shutdown.
func (e *Executor) taskOnce(ctx context.Context, start func() (string, error)) error {
	_, err := e.taskUPID(ctx, 0, start, nil)
	return err
}

// taskUPID is task that also returns the last UPID started ("" if none).
// restart decides whether a failed task restarts; nil never does. vmid is
// the task's VM when the UPID names another (a clone names its source but
// locks its target), else 0.
func (e *Executor) taskUPID(ctx context.Context, vmid int, start func() (string, error), restart func(error) bool) (string, error) {
	// Only failed tasks restart here; POST errors were already retried by call.
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
// status: another user's task without Sys.Audit). It never assumes the
// outcome: it waits, up to masterWaitBudget, for the VM to unlock, then
// returns an "outcome unknown" error so a retry round re-checks the VM.
// vmid, if given, is the locked VM (a clone locks its target).
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

// unfollowedError is a task unfollowable gave up following; unlike the 403
// itself, the outcome is unknown, so a retry re-checks it.
type unfollowedError struct{ msg string }

func (u *unfollowedError) Error() string { return u.msg }

// pollVM reads the VM's config every TaskPoll until done accepts it or ctx
// ends, returning the last config and error. A missing VM reads as nil;
// done never sees other errors. It takes no config-call slot.
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

// waitUnlocked polls the VM's config every TaskPoll until it is unlocked
// (or missing) or ctx ends, returning the last config read. Other read
// errors are polled again. If ctx ends first it returns the lock still
// held, or else the last read error.
func (e *Executor) waitUnlocked(ctx context.Context, node string, vmid int) (map[string]string, string, error) {
	cfg, err := e.pollVM(ctx, node, vmid, func(cfg map[string]string) bool { return cfg["lock"] == "" })
	if err != nil {
		return cfg, "", err
	}
	return cfg, cfg["lock"], nil
}
