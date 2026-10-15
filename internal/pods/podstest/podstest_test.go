package podstest_test

import (
	"context"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

var _ pods.API = (*podstest.Fake)(nil)

func TestFakeGuestAgent(t *testing.T) {
	ctx := context.Background()
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2501, Name: "teak.kilo.alpha", Node: "alder", Status: "running"}, nil)
	if err := f.AgentPing(ctx, "alder", 2501); err == nil {
		t.Fatal("ping answered with no guest set")
	}
	f.SetGuest(2501, podstest.Guest{Up: true, Hostname: "localhost"})
	if err := f.AgentPing(ctx, "alder", 2501); err != nil {
		t.Fatal(err)
	}
	f.ExecHook = func(vmid int, cmd []string, input string) (int, string, string) {
		f.Guests[vmid].Hostname = "teak" // hooks run with Mu held
		return 0, "ok", ""
	}
	pid, err := f.AgentExec(ctx, "alder", 2501, []string{"/bin/sh", "-s"}, "x")
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.AgentExecStatus(ctx, "alder", 2501, pid)
	if err != nil || !st.Exited || st.Out != "ok" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if h, _ := f.AgentHostname(ctx, "alder", 2501); h != "teak" {
		t.Errorf("hostname = %q", h)
	}
	if len(f.Execs) != 1 || f.Execs[0].Input != "x" {
		t.Errorf("execs = %+v", f.Execs)
	}
}
