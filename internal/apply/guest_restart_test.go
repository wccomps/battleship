package apply

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
)

// earlyGuard wraps hook: it notes a script started before Windows finished
// first-boot setup.
func earlyGuard(f *podstest.Fake, early *bool, hook func(int, []string, string) (int, string, string)) func(int, []string, string) (int, string, string) {
	return func(vmid int, cmd []string, in string) (int, string, string) {
		if f.Guests[vmid].SetupPending > 0 {
			*early = true
		}
		return hook(vmid, cmd, in)
	}
}

func infoMessages(rec *recorder, item string) []string {
	var out []string
	for _, ev := range rec.find(item, EventInfo) {
		out = append(out, ev.Message)
	}
	return out
}

// A clone of a sysprepped image restarts during first-boot setup, so the
// script waits for setup to finish.
func TestWindowsScriptWaitsForFirstBootSetup(t *testing.T) {
	f := guestFake(t)
	f.Guests[2101].SetupPending = 3
	early := false
	f.ExecHook = earlyGuard(f, &early, applied(f, ""))
	rec := &recorder{}
	e := catalogExec(f)
	e.OnEvent = rec.add
	res := e.Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageWindows)}})
	if len(res.Succeeded) != 1 || len(f.Execs) != 1 {
		t.Fatalf("%d execs, result %+v", len(f.Execs), res)
	}
	if early {
		t.Error("the script started before first-boot setup finished")
	}
	if got := infoMessages(rec, "oak.kilo.alpha"); !slices.Equal(got, []string{"waiting for Windows to finish first-boot setup"}) {
		t.Errorf("info = %q", got)
	}
}

func TestFirstBootSetupThatNeverFinishesFailsTheStep(t *testing.T) {
	f := guestFake(t)
	f.Guests[2101].SetupPending = 1 << 20
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageWindows)}})
	err := res.Failed["oak.kilo.alpha"]
	if err == nil || !strings.Contains(err.Error(), "first-boot setup") || !strings.Contains(err.Error(), "IMAGE_STATE_SPECIALIZE_RESEAL_TO_OOBE") {
		t.Errorf("failed = %v", err)
	}
	if len(f.Execs) != 0 {
		t.Errorf("execs = %+v", f.Execs)
	}
}

// A Linux guest has no first-boot setup to wait for.
func TestLinuxGuestDoesntReadSetupState(t *testing.T) {
	f := guestFake(t)
	f.Guests[2101].SetupPending = 1 << 20
	f.ExecHook = applied(f, "")
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageLinux)}})
	if len(res.Succeeded) != 1 || f.Called("agent-exec:2101") != 1 {
		t.Errorf("result %+v, calls %v", res, f.Calls)
	}
}

// A restart under the script reruns it within the step, once the guest is
// ready again, with no retry round.
func TestRestartUnderTheScriptRerunsIt(t *testing.T) {
	f := guestFake(t)
	// PID 1 reads the setup state; 2 is the script.
	f.Restarts = map[int]podstest.Restart{2: {Down: 3, SetupPending: 2}}
	early := false
	f.ExecHook = earlyGuard(f, &early, applied(f, ""))
	rec := &recorder{}
	e := catalogExec(f)
	e.OnEvent = rec.add
	res := e.Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageWindows)}})
	if len(res.Succeeded) != 1 || len(f.Execs) != 2 {
		t.Fatalf("%d execs, result %+v; want the script run again and the item to succeed", len(f.Execs), res)
	}
	if early {
		t.Error("the rerun started before first-boot setup finished")
	}
	want := []string{"the guest restarted while the setup script ran; running it again once the guest is ready", "waiting for Windows to finish first-boot setup"}
	if got := infoMessages(rec, "oak.kilo.alpha"); !slices.Equal(got, want) {
		t.Errorf("info = %q, want %q", got, want)
	}
}

func TestRestartUnderALinuxScriptRerunsIt(t *testing.T) {
	f := guestFake(t)
	f.Restarts = map[int]podstest.Restart{1: {Down: 2, Msg: "Agent error: PID 1 does not exist"}}
	f.ExecHook = applied(f, "")
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageLinux)}})
	if len(res.Succeeded) != 1 || len(f.Execs) != 2 {
		t.Fatalf("%d execs, result %+v", len(f.Execs), res)
	}
}

// A guest that restarts under every run fails the step after guestReruns.
func TestRestartsUnderTheScriptAreBounded(t *testing.T) {
	f := guestFake(t)
	f.Restarts = map[int]podstest.Restart{}
	for pid := 1; pid <= 10; pid++ {
		f.Restarts[pid] = podstest.Restart{Msg: "Agent error: Invalid parameter 'pid'"}
	}
	f.ExecHook = applied(f, "")
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageLinux)}})
	if err := res.Failed["oak.kilo.alpha"]; err == nil || !strings.Contains(err.Error(), "restarted") {
		t.Errorf("failed = %v", err)
	}
	if len(f.Execs) != 1+guestReruns {
		t.Errorf("%d execs, want %d", len(f.Execs), 1+guestReruns)
	}
}

// A restart that cuts the setup state read short counts as setup not
// finished.
func TestRestartUnderTheSetupReadKeepsWaiting(t *testing.T) {
	f := guestFake(t)
	f.Restarts = map[int]podstest.Restart{1: {Down: 2, SetupPending: 1}}
	early := false
	f.ExecHook = earlyGuard(f, &early, applied(f, ""))
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageWindows)}})
	if len(res.Succeeded) != 1 || len(f.Execs) != 1 || early {
		t.Fatalf("%d execs (early %v), result %+v", len(f.Execs), early, res)
	}
}

// stopInWait runs it with a Sleep that cancels the run at its first wait,
// and reports whether the wait then ended within seconds, though the
// cancel grace is an hour.
func stopInWait(t *testing.T, f *podstest.Fake, it pods.Item) (Result, bool) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var ended atomic.Bool
	e := catalogExec(f)
	e.Cfg.Jobs.CancelGrace = time.Hour
	e.Sleep = func(sctx context.Context, _ time.Duration) error {
		cancel(ErrCancelRequested)
		select {
		case <-sctx.Done():
			ended.Store(true)
			return sctx.Err()
		case <-time.After(5 * time.Second):
			return errors.New("the wait outlived the stop")
		}
	}
	res := e.Run(ctx, &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{it}})
	return res, ended.Load()
}

// A cancel during the wait for first-boot setup stops it at once: reading
// the setup state is no change for the cancel grace to wait on.
func TestCancelDuringFirstBootWaitStops(t *testing.T) {
	f := guestFake(t)
	f.Guests[2101].SetupPending = 1 << 20
	res, ended := stopInWait(t, f, guestItem(pods.ImageWindows))
	if !ended {
		t.Error("the wait didn't end with the stop")
	}
	if err := res.Interrupted["oak.kilo.alpha"]; err == nil || err.Error() != "cancelled before configure-guest" {
		t.Errorf("result = %+v", res)
	}
	if len(f.Execs) != 0 {
		t.Errorf("execs = %+v", f.Execs)
	}
}

// A cancel while waiting for a guest that restarted under the script stops
// at once: the script is no longer running for the grace to see end.
func TestCancelDuringRerunWaitStops(t *testing.T) {
	f := guestFake(t)
	f.Restarts = map[int]podstest.Restart{1: {Down: 1 << 20, Msg: "Agent error: PID 1 does not exist"}}
	f.ExecHook = applied(f, "")
	res, ended := stopInWait(t, f, guestItem(pods.ImageLinux))
	if !ended {
		t.Error("the wait didn't end with the stop")
	}
	want := "cancelled during configure-guest; a Proxmox task it started may still be running"
	if err := res.Interrupted["oak.kilo.alpha"]; err == nil || err.Error() != want {
		t.Errorf("result = %+v, want %q", res, want)
	}
	if len(f.Execs) != 1 {
		t.Errorf("execs = %+v", f.Execs)
	}
}
