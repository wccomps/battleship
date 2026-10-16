package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func guestItem(os string) pods.Item {
	return pods.Item{Name: "oak.kilo.alpha", Host: "oak", VMID: 2101, Node: "alder",
		Guest: &pods.Guest{OS: os, Hostname: "oak", Domain: "kilo.alpha", IP: "192.168.100.20", Prefix: 24, Gateway: "192.168.100.2", DNS: "192.168.100.2"},
		Steps: []pods.Step{pods.StepWaitAgent, pods.StepGuest, pods.StepVerify}}
}

func guestFake(t *testing.T) *podstest.Fake {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2101, Name: "oak.kilo.alpha", Node: "alder", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:21:01,bridge=devint01"})
	f.SetGuest(2101, podstest.Guest{Up: true, Hostname: "WIN-123", NICs: []proxmox.GuestNIC{{Name: "Ethernet", MAC: "bc:24:11:00:21:01", IPs: []string{"169.254.1.1/16"}}}})
	return f
}

// applied makes the fake guest act on the script: the address and name take.
func applied(f *podstest.Fake, out string) func(int, []string, string) (int, string, string) {
	return func(vmid int, _ []string, _ string) (int, string, string) {
		g := f.Guests[vmid]
		g.Hostname = "oak"
		g.NICs[0].IPs = []string{"192.168.100.20/24"}
		return 0, out, ""
	}
}

func TestWindowsGuestRenamesAndReboots(t *testing.T) {
	f := guestFake(t)
	f.ExecHook = applied(f, pods.RebootMarker+"\n")
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageWindows)}})
	if len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if f.Called("shutdown:2101:10m0s:false") != 1 || f.Called("power:2101:start") != 1 || f.Called("power:2101:reboot") != 0 {
		t.Errorf("want one patient shutdown and one start after a rename; calls %v", f.Calls)
	}
	if len(f.Execs) != 1 || f.Execs[0].Command[0] != "powershell.exe" {
		t.Errorf("execs = %+v", f.Execs)
	}
}

func TestLinuxGuestScriptFailureFailsTheItem(t *testing.T) {
	f := guestFake(t)
	f.ExecHook = func(int, []string, string) (int, string, string) {
		return 3, "", "found no netplan, NetworkManager or /etc/network/interfaces"
	}
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageLinux)}})
	err := res.Failed["oak.kilo.alpha"]
	if err == nil || !strings.Contains(err.Error(), "netplan") || !strings.Contains(err.Error(), "exited 3") {
		t.Errorf("failed = %v", err)
	}
}

func TestVerifyFailsWhenTheAddressDoesntTake(t *testing.T) {
	f := guestFake(t)
	f.ExecHook = func(int, []string, string) (int, string, string) { return 0, "", "" } // reports success, changes nothing
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageLinux)}})
	if err := res.Failed["oak.kilo.alpha"]; err == nil || !strings.Contains(err.Error(), "192.168.100.20") {
		t.Errorf("failed = %v", err)
	}
}

func TestWaitAgentGivesUp(t *testing.T) {
	f := guestFake(t)
	f.Guests[2101].Up = false
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageLinux)}})
	if err := res.Failed["oak.kilo.alpha"]; err == nil || !strings.Contains(err.Error(), "guest agent") {
		t.Errorf("failed = %v", err)
	}
}

func TestGuestExecIsSentOnce(t *testing.T) {
	f := guestFake(t)
	f.FailOn("agent-exec:2101", &proxmox.APIError{Status: 500, Message: "connection reset"})
	f.ExecHook = applied(f, "")
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageLinux)}})
	if res.Failed["oak.kilo.alpha"] == nil {
		t.Fatalf("result = %+v", res)
	}
	if n := f.Called("agent-exec:2101"); n != 1 {
		t.Errorf("exec was sent %d times; calls %v", n, f.Calls)
	}
}

// A script that outlasts the limit keeps running in the guest (the agent
// can't stop it), so a retry waits for it to exit before starting another:
// this run never has two copies changing the guest at once.
func TestGuestScriptRetryWaitsForTheRunningOne(t *testing.T) {
	limit := int(execLimit / execPoll)
	for _, c := range []struct {
		name     string
		runsFor  int // status reads the first copy stays running for
		execs    int
		succeeds bool
	}{
		{"exits during the retry's wait", limit + 10, 2, true},
		{"outlasts the retry's wait too", 2*limit + 10, 1, false},
		{"exits in time", 3, 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := guestFake(t)
			f.ExecRunsFor = func(pid int) int {
				if pid == 1 {
					return c.runsFor
				}
				return 0
			}
			apply := applied(f, "")
			f.ExecHook = func(vmid int, cmd []string, in string) (int, string, string) {
				for pid := 1; pid <= len(f.Execs)-1; pid++ {
					if f.ExecRunning(pid) {
						t.Errorf("exec %d started while exec %d still runs", len(f.Execs), pid)
					}
				}
				return apply(vmid, cmd, in)
			}
			e := catalogExec(f)
			e.Cfg.Retry.Rounds = 1
			res := e.Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageLinux)}})
			if len(f.Execs) != c.execs || (len(res.Succeeded) == 1) != c.succeeds {
				t.Fatalf("%d execs, result %+v; want %d execs, success %v", len(f.Execs), res, c.execs, c.succeeds)
			}
			if err := res.Failed["oak.kilo.alpha"]; !c.succeeds && (err == nil || !strings.Contains(err.Error(), "not starting a second copy")) {
				t.Errorf("failed = %v", err)
			}
		})
	}
}

// A previous copy whose PID can't be read any more (the guest restarted)
// isn't running: the retry runs the script again.
func TestGuestScriptRetryRunsAfterTheGuestRestarted(t *testing.T) {
	limit := int(execLimit / execPoll)
	f := guestFake(t)
	f.ExecRunsFor = func(pid int) int {
		if pid == 1 {
			return limit + 10 // outlasts the first round
		}
		return 0
	}
	f.ExecHook = applied(f, "")
	e := catalogExec(f)
	e.Cfg.Retry.Rounds = 1
	e.OnEvent = func(ev Event) {
		if ev.Status == EventFailed && strings.Contains(ev.Message, "still running") {
			f.ExecGone = func(pid int) bool { return pid == 1 } // then the guest restarts
		}
	}
	res := e.Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageLinux)}})
	if len(f.Execs) != 2 || len(res.Succeeded) != 1 {
		t.Fatalf("%d execs, result %+v; want the retry to run the script again and succeed", len(f.Execs), res)
	}
}

func TestVerifyStopsOnForbidden(t *testing.T) {
	f := guestFake(t)
	f.ExecHook = applied(f, "")
	f.FailOn("agent-nics:2101", &proxmox.APIError{Status: 403, Message: "Permission check failed (VM.GuestAgent.Audit)"})
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{guestItem(pods.ImageLinux)}})
	err := res.Failed["oak.kilo.alpha"]
	if err == nil || !strings.Contains(err.Error(), "VM.GuestAgent.Audit") {
		t.Fatalf("failed = %v", err)
	}
	if n := f.Called("agent-nics:2101"); n != 1 {
		t.Errorf("verify read the interfaces %d times; want 1", n)
	}
}

// A value a script can't take fails the step instead of panicking serve.
func TestGuestWithABadValueFailsTheItem(t *testing.T) {
	f := guestFake(t)
	it := guestItem(pods.ImageWindows)
	it.Guest.IP = "192.168.100.20'"
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvDNS, Items: []pods.Item{it}})
	if err := res.Failed["oak.kilo.alpha"]; err == nil || !strings.Contains(err.Error(), "IPv4") {
		t.Errorf("failed = %v", err)
	}
	if len(f.Execs) != 0 {
		t.Errorf("execs = %+v", f.Execs)
	}
}

func TestSetDNSRunsOnlyTheDNSScript(t *testing.T) {
	f := guestFake(t)
	f.ExecHook = applied(f, "")
	it := guestItem(pods.ImageLinux)
	it.Guest.DNSOnly = true
	it.Steps = []pods.Step{pods.StepGuest, pods.StepVerify}
	res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvDNS, Items: []pods.Item{it}})
	if len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(f.Execs) != 1 || strings.Contains(f.Execs[0].Input, "addresses: [$IP") || !strings.Contains(f.Execs[0].Input, "dns-nameservers") {
		t.Errorf("execs = %+v", f.Execs)
	}
}

// A script waiting for the guest lock gives up before the executor would
// stop waiting for it, so the failure names the other script.
func TestGuestLockWaitIsUnderTheExecLimit(t *testing.T) {
	if pods.GuestLockWait >= execLimit {
		t.Errorf("guest lock wait %v isn't under the exec limit %v", pods.GuestLockWait, execLimit)
	}
}
