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
	st, err := f.AgentExecStatus(ctx, "alder", 2501, pid, 1<<20)
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

// The fake answers pods' setup state read, and restarts a guest under a
// command: the PID is forgotten, then the agent is down for a while.
func TestFakeGuestRestarts(t *testing.T) {
	ctx := context.Background()
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2501, Name: "teak.kilo.alpha", Node: "alder", Status: "running"}, nil)
	f.SetGuest(2501, podstest.Guest{Up: true, SetupPending: 1})
	read := func() string {
		t.Helper()
		pid, err := f.AgentExec(ctx, "alder", 2501, pods.SetupStateCommand, "")
		if err != nil {
			t.Fatal(err)
		}
		st, err := f.AgentExecStatus(ctx, "alder", 2501, pid, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return st.Out
	}
	if out := read(); pods.SetupComplete(out) {
		t.Errorf("first read %q says setup finished", out)
	}
	if out := read(); !pods.SetupComplete(out) {
		t.Errorf("second read %q says setup isn't finished", out)
	}
	if len(f.Execs) != 0 {
		t.Errorf("setup reads listed in execs: %+v", f.Execs)
	}
	f.Restarts = map[int]podstest.Restart{3: {Down: 2, SetupPending: 1}}
	pid, err := f.AgentExec(ctx, "alder", 2501, []string{"/bin/sh", "-s"}, "x")
	if err != nil || pid != 3 {
		t.Fatalf("pid %d, %v", pid, err)
	}
	if _, err := f.AgentExecStatus(ctx, "alder", 2501, pid, 1<<20); !proxmox.IsExecGone(err) {
		t.Errorf("status after the restart: %v", err)
	}
	for i := range 2 {
		if err := f.AgentPing(ctx, "alder", 2501); err == nil {
			t.Errorf("ping %d answered while the guest restarts", i)
		}
	}
	if err := f.AgentPing(ctx, "alder", 2501); err != nil {
		t.Error(err)
	}
	if out := read(); pods.SetupComplete(out) {
		t.Errorf("read %q after the restart says setup finished", out)
	}
}
