package pods

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func TestEnvEditUpdatesNotesDetailsRenameAndGuest(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devext01", "net1": "virtio=BC:24:11:00:00:02,bridge=devint01",
			"description": "battleship: base=gw-v1 dns=router"})
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=devint01", "agent": "1",
			"ide2":        "local:iso/debian.iso,media=cdrom",
			"description": "**Details**\n- OS: Debian 12\n\nInitial notes.\n\nbattleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=router"})
	p := NewPlanner(f, envConfig())
	ctx := context.Background()

	envs, vms, err := p.Environments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at, err := HostToEdit(envs, vms, 2101)
	if err != nil {
		t.Fatal(err)
	}
	if at.Kind() != ImageLinux || at.Octet() != 10 || at.Read.Details.OS != "Debian 12" || at.Read.Free != "Initial notes." {
		t.Fatalf("HostToEdit = %+v", at)
	}

	// Editing only details and base image does not touch the guest agent.
	det := at.Edit()
	det.OS = "Debian 12.8"
	free := "Patched kernel."
	det.Free = &free
	plan, err := p.EnvEdit(ctx, HostEditInput{VMID: 2101, Image: "debian-12-v2", Details: &det})
	if err != nil {
		t.Fatal(err)
	}
	it := plan.Items[0]
	if it.Blocked != "" || !slices.Equal(it.Steps, []Step{StepLabel, StepDetails}) || it.Guest != nil || it.Notes.Base != "debian-12-v2" {
		t.Fatalf("details+base edit item = %+v", it)
	}

	// Renaming and changing the octet schedules guest reconfiguration and ejects CD-ROM.
	plan, err = p.EnvEdit(ctx, HostEditInput{VMID: 2101, Host: "oak", Octet: 25, EjectCDROM: true})
	if err != nil {
		t.Fatal(err)
	}
	it = plan.Items[0]
	wantSteps := []Step{StepLabel, StepCDROM, StepWaitAgent, StepGuest, StepVerify}
	if it.Blocked != "" || it.Rename != "oak.kilo.alpha" || !slices.Equal(it.Steps, wantSteps) {
		t.Fatalf("rename+octet edit item = %+v", it)
	}
	if it.Guest == nil || it.Guest.Hostname != "oak" || it.Guest.IP.String() != "192.168.100.25" {
		t.Fatalf("guest = %+v", it.Guest)
	}

	// Unchanged settings are blocked.
	plan, err = p.EnvEdit(ctx, HostEditInput{VMID: 2101})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Items[0].Blocked, "already has these settings") {
		t.Fatalf("unchanged edit blocked = %q", plan.Items[0].Blocked)
	}
}

func TestEnvPowerAndDeleteHost(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=devint01"})
	p := NewPlanner(f, envConfig())
	ctx := context.Background()

	powerPlan, err := p.EnvPower(ctx, 2101, "reboot")
	if err != nil {
		t.Fatal(err)
	}
	if len(powerPlan.Items) != 1 || powerPlan.Items[0].Action != "reboot" || !slices.Equal(powerPlan.Items[0].Steps, []Step{StepPower}) {
		t.Fatalf("EnvPower = %+v", powerPlan)
	}

	delPlan, err := p.EnvDeleteHost(ctx, 2101, "teak.kilo.alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(delPlan.Items) != 1 || !slices.Equal(delPlan.Items[0].Steps, []Step{StepStop, StepDelete}) {
		t.Fatalf("EnvDeleteHost = %+v", delPlan)
	}
	if _, err := p.EnvDeleteHost(ctx, 2101, "wrong.kilo.alpha"); err == nil {
		t.Fatal("EnvDeleteHost accepted wrong VM name")
	}
}
