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
