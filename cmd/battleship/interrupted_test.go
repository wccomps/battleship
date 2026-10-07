package main

import (
	"context"
	"strings"
	"testing"
)

// A run whose only unfinished VMs were interrupted (nothing failed) still
// exits 1: the VMs weren't all done.
func TestInterruptedOnlyRunExitsOne(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Task waits end the run, as Ctrl-C does while a VM's step is under way.
	e.api.WaitGate = func(ctx context.Context, _ string) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	code := run(ctx, []string{"power", "-teams", "7", "-action", "start", "-yes", "-config", e.cfg}, e.d)
	out := e.stdout.String()
	if !strings.Contains(out, "\n1 VM interrupted") || strings.Contains(out, " failed:\n") {
		t.Fatalf("want an interrupted-only result:\n%s%s", out, e.stderr)
	}
	if code != 1 {
		t.Errorf("code = %d, want 1 for a run with interrupted VMs\n%s", code, out)
	}
}
