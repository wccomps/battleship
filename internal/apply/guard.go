package apply

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// stepContext is a step's context. A stop reaches it at once, except a
// cancel (ErrCancelRequested) after the step sent a change: then only after
// jobs.cancel_grace or Halt, so the step sees its task end. After the stop
// the step sends nothing new (see guardedAPI).
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

// stepState is a step's run and whether it sent a change.
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
// run has stopped, else records that a change was sent. Calls outside a
// step (template builds, cleanup) aren't held back.
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

// guardedAPI is an API whose changes go through held, if set, then
// mayChange. Reads, task waits and StopTask pass through.
type guardedAPI struct {
	pods.API
	held func() (time.Time, error)
}

// may refuses a change if the run may no longer hold its locks, and else
// bounds its request by Held's deadline (see Executor.Held); then as
// mayChange. Call done when the change's call returns.
func (g guardedAPI) may(ctx context.Context) (_ context.Context, done context.CancelFunc, _ error) {
	done = func() {}
	if g.held != nil {
		deadline, err := g.held()
		if err != nil {
			return ctx, done, err
		}
		ctx, done = context.WithDeadline(ctx, deadline)
	}
	if err := mayChange(ctx); err != nil {
		done()
		return ctx, func() {}, err
	}
	return ctx, done, nil
}

// guard sends change through g's checks (see may), with may's context.
func guard[T any](g guardedAPI, ctx context.Context, change func(context.Context) (T, error)) (T, error) {
	ctx, done, err := g.may(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	defer done()
	return change(ctx)
}

func (g guardedAPI) SetVMConfig(ctx context.Context, node string, vmid int, changes map[string]string) error {
	_, err := guard(g, ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, g.API.SetVMConfig(ctx, node, vmid, changes)
	})
	return err
}

func (g guardedAPI) AgentExec(ctx context.Context, node string, vmid int, command []string, input string) (int, error) {
	return guard(g, ctx, func(ctx context.Context) (int, error) { return g.API.AgentExec(ctx, node, vmid, command, input) })
}

func (g guardedAPI) Clone(ctx context.Context, r proxmox.CloneRequest) (string, error) {
	return guard(g, ctx, func(ctx context.Context) (string, error) { return g.API.Clone(ctx, r) })
}

func (g guardedAPI) ConvertToTemplate(ctx context.Context, node string, vmid int) (string, error) {
	return guard(g, ctx, func(ctx context.Context) (string, error) { return g.API.ConvertToTemplate(ctx, node, vmid) })
}

func (g guardedAPI) RegenerateCloudInit(ctx context.Context, node string, vmid int) error {
	_, err := guard(g, ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, g.API.RegenerateCloudInit(ctx, node, vmid)
	})
	return err
}

func (g guardedAPI) CreateSnapshot(ctx context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error) {
	return guard(g, ctx, func(ctx context.Context) (string, error) { return g.API.CreateSnapshot(ctx, node, vmid, r) })
}

func (g guardedAPI) Rollback(ctx context.Context, node string, vmid int, snapshot string) (string, error) {
	return guard(g, ctx, func(ctx context.Context) (string, error) { return g.API.Rollback(ctx, node, vmid, snapshot) })
}

func (g guardedAPI) Power(ctx context.Context, node string, vmid int, action string) (string, error) {
	return guard(g, ctx, func(ctx context.Context) (string, error) { return g.API.Power(ctx, node, vmid, action) })
}

func (g guardedAPI) Shutdown(ctx context.Context, node string, vmid int, timeout time.Duration, forceStop bool) (string, error) {
	return guard(g, ctx, func(ctx context.Context) (string, error) { return g.API.Shutdown(ctx, node, vmid, timeout, forceStop) })
}

func (g guardedAPI) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	return guard(g, ctx, func(ctx context.Context) (string, error) { return g.API.DeleteVM(ctx, node, vmid) })
}

func (g guardedAPI) DeleteVolume(ctx context.Context, node, storage, volid string) (string, error) {
	return guard(g, ctx, func(ctx context.Context) (string, error) { return g.API.DeleteVolume(ctx, node, storage, volid) })
}
