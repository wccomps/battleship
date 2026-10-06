package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
)

// interruptingAPI is a fakeAPI whose task waits end the run, as Ctrl-C
// does while a VM's step is under way.
type interruptingAPI struct {
	*fakeAPI
	cancel context.CancelFunc
}

func (a interruptingAPI) WaitTask(ctx context.Context, _ string, _ time.Duration) error {
	a.cancel()
	<-ctx.Done()
	return ctx.Err()
}

// A run whose only unfinished VMs were interrupted (nothing failed) still
// exits 1: the VMs weren't all done.
func TestInterruptedOnlyRunExitsOne(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := interruptingAPI{fakeAPI: e.api, cancel: cancel}
	e.d.newAPI = func(_ config.Proxmox) pods.API { return api }
	code := run(ctx, []string{"power", "-teams", "7", "-action", "start", "-yes", "-config", e.cfg}, e.d)
	out := e.stdout.String()
	if !strings.Contains(out, "\n1 VM interrupted") || strings.Contains(out, " failed:\n") {
		t.Fatalf("want an interrupted-only result:\n%s%s", out, e.stderr)
	}
	if code != 1 {
		t.Errorf("code = %d, want 1 for a run with interrupted VMs\n%s", code, out)
	}
}
