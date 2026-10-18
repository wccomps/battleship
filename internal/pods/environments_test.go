package pods

import (
	"context"
	"slices"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func envConfig() config.Config {
	c := config.Default()
	c.Environments.Slots = "1-3"
	return c
}

func TestSlots(t *testing.T) {
	s := Slots(envConfig().Environments)
	if len(s) != 3 || s[1] != (Slot{N: 2, Name: "02", Pool: "dev-02", IntBridge: "devint02", ExtBridge: "devext02", FirstVMID: 2200}) {
		t.Errorf("Slots = %+v", s)
	}
}

func TestEnvironmentsReadsSlots(t *testing.T) {
	f := podstest.New("alder")
	// Slot 1: kilo.alpha with a router and a host.
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devext01", "net1": "virtio=BC:24:11:00:00:02,bridge=devint01",
			"description": "battleship: base=gw-v1 dns=router"})
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=devint01",
			"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=teak"})
	// Slot 2: nothing in its pool, but a VM elsewhere sits on devint02.
	f.Add(proxmox.VM{VMID: 300, Name: "stray", Node: "alder"}, map[string]string{"net0": "virtio=BC:24:11:00:00:04,bridge=devint02"})
	// A template on devint03 doesn't hold slot 3.
	f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v1", Node: "alder", Template: true, Tags: "base;linux"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:05,bridge=devint03"})

	envs, _, err := NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 3 {
		t.Fatalf("envs = %+v", envs)
	}
	e1 := envs[0]
	if e1.Domain != "kilo.alpha" || len(e1.Hosts) != 2 || !e1.HasRouter() || e1.DNS() != "teak" || !e1.UsedOctets()[10] || e1.Free() {
		t.Errorf("slot 1 = %+v", e1)
	}
	if envs[1].Free() || len(envs[1].Users) != 1 || envs[1].Users[0] != "stray" {
		t.Errorf("slot 2 = %+v; a VM on its bridge holds it", envs[1])
	}
	if !envs[2].Free() {
		t.Errorf("slot 3 = %+v; templates don't hold a slot", envs[2])
	}

	// A VM named like a team VM but outside every team's VMIDs is someone's
	// own: on a slot's bridge, it holds the slot. A team VM is never read.
	f.Add(proxmox.VM{VMID: 8100, Name: "team05-test", Node: "alder"}, map[string]string{"net0": "virtio=BC:24:11:00:00:06,bridge=devint03"})
	f.Add(proxmox.VM{VMID: 10521, Name: "team05-web", Node: "alder"}, map[string]string{"net0": "virtio=BC:24:11:00:00:07,bridge=devint03"})
	envs, _, err = NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if envs[2].Free() || !slices.Equal(envs[2].Users, []string{"team05-test"}) {
		t.Errorf("slot 3 = %+v; the stray team-named VM on its bridge holds it, the team VM isn't read", envs[2])
	}
	// Nor does a name ending like a template's make a running VM one.
	f.Add(proxmox.VM{VMID: 8101, Name: "team05-x.tpl", Node: "alder"}, map[string]string{"net0": "virtio=BC:24:11:00:00:08,bridge=devint02"})
	envs, _, err = NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(envs[1].Users, "team05-x.tpl") {
		t.Errorf("slot 2 users = %v; a VM only named like a template holds it", envs[1].Users)
	}
}

func TestEnvironmentMixedDomains(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2100, Name: "a.kilo.alpha", Node: "alder", Pool: "dev-01"}, nil)
	f.Add(proxmox.VM{VMID: 2101, Name: "ghosts", Node: "alder", Pool: "dev-01"}, nil)
	envs, _, err := NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !envs[0].Mixed || envs[0].Domain != "" {
		t.Errorf("slot 1 = %+v; a VM without the domain makes it mixed", envs[0])
	}
}

func TestNICMAC(t *testing.T) {
	if m, ok := NICMAC("virtio=BC:24:11:AA:00:01,bridge=devint01,firewall=1"); !ok || m != "bc:24:11:aa:00:01" {
		t.Errorf("NICMAC = %q, %v", m, ok)
	}
	if _, ok := NICMAC("virtio,bridge=x"); ok {
		t.Error("NICMAC found a MAC in a NIC without one")
	}
}

func TestEnvironmentHostWarnings(t *testing.T) {
	f := podstest.New("alder")
	f.CPUModels = []string{"baseline", "baseline-nested"}
	cfg := envConfig()

	// Clean router and host: no warnings.
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devext01", "net1": "virtio=BC:24:11:00:00:02,bridge=devint01",
			"cpu": "custom-baseline", "agent": "1", "scsi0": "competitions:2100/vm-2100-disk-0.qcow2"})
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=devint01",
			"cpu": "cputype=custom-baseline-nested", "agent": "1,fstrim_cloned_disks=1", "scsi0": "competitions:2101/vm-2101-disk-0.qcow2",
			"ide2": "competitions:vm-2101-cloudinit,media=cdrom"})
	// Drifted host: missing tags, wrong bridge, extra NIC, VLAN tag, wrong CPU, guest agent off, CD-ROM attached.
	f.Add(proxmox.VM{VMID: 2102, Name: "elm.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: ""},
		map[string]string{"net0": "virtio=BC:24:11:00:00:04,bridge=devint02,tag=10", "net1": "virtio=BC:24:11:00:00:05,bridge=devext01",
			"cpu": "x86-64-v2-AES", "agent": "0", "scsi0": "competitions:2102/vm-2102-disk-0.qcow2",
			"ide2": "local:iso/debian.iso,media=cdrom"})
	// Drifted router: swapped bridges and default CPU.
	f.Add(proxmox.VM{VMID: 2200, Name: "gw.oscar.lima", Node: "alder", Pool: "dev-02", Tags: "dev;oscar.lima;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:06,bridge=devint02", "net1": "virtio=BC:24:11:00:00:07,bridge=devext02",
			"agent": "1", "scsi0": "competitions:2200/vm-2200-disk-0.qcow2"})

	envs, _, err := NewPlanner(f, cfg).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gw, _ := envs[0].Host("gw.kilo.alpha")
	teak, _ := envs[0].Host("teak.kilo.alpha")
	if len(gw.Warnings) != 0 || len(teak.Warnings) != 0 {
		t.Errorf("clean hosts have warnings: gw=%v teak=%v", gw.Warnings, teak.Warnings)
	}
	elm, _ := envs[0].Host("elm.kilo.alpha")
	wantElm := []string{
		"missing tags dev, kilo.alpha",
		"net0 on devint02, expected devint01",
		"2 NICs, expected 1",
		"net0 has VLAN tag 10",
		"CPU x86-64-v2-AES, expected baseline or baseline-nested",
		"guest agent off",
		"CD-ROM attached",
	}
	if !slices.Equal(elm.Warnings, wantElm) {
		t.Errorf("elm.Warnings = %v, want %v", elm.Warnings, wantElm)
	}
	rgw, _ := envs[1].Host("gw.oscar.lima")
	wantRouter := []string{
		"net0 on devint02, expected devext02",
		"net1 on devext02, expected devint02",
		"CPU default (kvm64), expected baseline or baseline-nested",
	}
	if !slices.Equal(rgw.Warnings, wantRouter) {
		t.Errorf("rgw.Warnings = %v, want %v", rgw.Warnings, wantRouter)
	}
}
