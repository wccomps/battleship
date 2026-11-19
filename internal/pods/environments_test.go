package pods

import (
	"context"
	"fmt"
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
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router", MaxMem: 16 << 30},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devext01", "net1": "virtio=BC:24:11:00:00:02,bridge=devint01",
			"cpu": "custom-baseline", "agent": "1", "scsi0": "competitions:2100/vm-2100-disk-0.qcow2"})
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", MaxMem: 8 << 30},
		map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=devint01",
			"cpu": "cputype=custom-baseline-nested", "agent": "1,fstrim_cloned_disks=1", "scsi0": "competitions:2101/vm-2101-disk-0.qcow2",
			"ide2": "competitions:vm-2101-cloudinit,media=cdrom", "ostype": "l26"})
	f.Add(proxmox.VM{VMID: 2103, Name: "win.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", MaxMem: 16 << 30},
		map[string]string{"net0": "virtio=BC:24:11:00:00:08,bridge=devint01",
			"cpu": "custom-baseline", "agent": "1", "scsi0": "competitions:2103/vm-2103-disk-0.qcow2",
			"ostype": "l26", "description": "battleship: ip=172.16.1.10"})
	// Drifted host: missing tags, wrong bridge, extra NIC, VLAN tag, wrong CPU, guest agent off, CD-ROM attached.
	f.Add(proxmox.VM{VMID: 2102, Name: "elm.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "", MaxMem: 8200 << 20},
		map[string]string{"net0": "virtio=BC:24:11:00:00:04,bridge=devint02,tag=10", "net1": "virtio=BC:24:11:00:00:05,bridge=devext01",
			"cpu": "x86-64-v2-AES", "agent": "0", "scsi0": "competitions:2102/vm-2102-disk-0.qcow2",
			"ide2": "local:iso/debian.iso,media=cdrom", "ostype": "l26"})
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
	win, _ := envs[0].Host("win.kilo.alpha")
	if len(gw.Warnings) != 0 || len(teak.Warnings) != 0 || len(win.Warnings) != 0 {
		t.Errorf("clean hosts have warnings: gw=%v teak=%v win=%v", gw.Warnings, teak.Warnings, win.Warnings)
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
		"8200 MiB of memory (Linux VMs are usually at most 8 GiB)",
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

// reversedListing lists the cluster's VMs highest VMID first: Proxmox lists
// them in no promised order.
type reversedListing struct{ *podstest.Fake }

func (r reversedListing) ClusterVMs(ctx context.Context) ([]proxmox.VM, error) {
	vms, err := r.Fake.ClusterVMs(ctx)
	slices.Reverse(vms)
	return vms, err
}

func hostNames(hs []EnvHost) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Name)
	}
	return out
}

func hostVMIDs(hs []EnvHost) []int {
	var out []int
	for _, h := range hs {
		out = append(out, h.VMID)
	}
	return out
}

// A slot's hosts are in name order and its half-deleted VMs in VMID order,
// however Proxmox lists them. A half-deleted VM outside every pool is the
// slot's whose VMID block holds it, its first and last VMIDs included.
func TestEnvironmentsOrderHostsAndHalfDeleted(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2110, Name: "oak.kilo.alpha", Node: "alder", Pool: "dev-01"}, nil)
	f.Add(proxmox.VM{VMID: 2111, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01"}, nil)
	f.Add(proxmox.VM{VMID: 2112, Name: "elm.kilo.alpha", Node: "alder", Pool: "dev-01"}, nil)
	for _, id := range []int{2100, 2150, 2199, 2200} {
		f.Add(proxmox.VM{VMID: id, Name: fmt.Sprintf("VM %d", id), Node: "alder"}, map[string]string{"lock": "destroyed"})
	}
	envs, _, err := NewPlanner(reversedListing{f}, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := hostNames(envs[0].Hosts); !slices.Equal(got, []string{"elm.kilo.alpha", "oak.kilo.alpha", "teak.kilo.alpha"}) {
		t.Errorf("slot 1 hosts = %q", got)
	}
	if got := hostVMIDs(envs[0].HalfDeleted); !slices.Equal(got, []int{2100, 2150, 2199}) {
		t.Errorf("slot 1 half-deleted = %v", got)
	}
	if got := hostVMIDs(envs[1].HalfDeleted); !slices.Equal(got, []int{2200}) {
		t.Errorf("slot 2 half-deleted = %v", got)
	}
}

// A VM gone between the listing and reading its config is left out; the
// VMs listed after it are still read.
func TestEnvironmentsSkipAVMGoneSinceTheListing(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 200, Name: "gone", Node: "alder"}, map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devint01"})
	f.FailOn("config:200", podstest.NotExist("alder", 200))
	f.Add(proxmox.VM{VMID: 300, Name: "stray", Node: "alder"}, map[string]string{"net0": "virtio=BC:24:11:00:00:02,bridge=devint02"})
	f.Add(proxmox.VM{VMID: 2100, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01"}, nil)
	envs, _, err := NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(envs[0].Users) != 0 || !slices.Equal(hostNames(envs[0].Hosts), []string{"teak.kilo.alpha"}) {
		t.Errorf("slot 1 = %+v", envs[0])
	}
	if !slices.Equal(envs[1].Users, []string{"stray"}) {
		t.Errorf("slot 2 users = %q", envs[1].Users)
	}
}

// A VM outside the pools holds a slot only through a NIC on that slot's
// bridges, and is named once however many it has there.
func TestEnvironmentsUsersAreOnTheSlotBridges(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 300, Name: "lab", Node: "alder"}, map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 301, Name: "bridged", Node: "alder"}, map[string]string{
		"net0": "virtio=BC:24:11:00:00:02,bridge=devext02", "net1": "virtio=BC:24:11:00:00:03,bridge=devint02"})
	envs, _, err := NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !envs[0].Free() || !envs[2].Free() {
		t.Errorf("slots 1 and 3 = %+v, %+v; a VM on vmbr0 holds neither", envs[0], envs[2])
	}
	if !slices.Equal(envs[1].Users, []string{"bridged"}) {
		t.Errorf("slot 2 users = %q", envs[1].Users)
	}
}

// A host's details block is read as unreadable only when it is, and a
// CD-ROM only when an image is in a drive (a cloud-init drive isn't one).
func TestEnvironmentsReadHostNotesAndCDROM(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2100, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01"}, map[string]string{
		"ide2": "competitions:vm-2100-cloudinit,media=cdrom", "description": "**Details**\n- Colour: blue\nbattleship: base=debian-12-v1"})
	f.Add(proxmox.VM{VMID: 2101, Name: "oak.kilo.alpha", Node: "alder", Pool: "dev-01"}, map[string]string{
		"ide2": "local:iso/debian.iso,media=cdrom", "description": "Login: root / pw\nbattleship: base=debian-12-v1"})
	envs, _, err := NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	teak, _ := envs[0].Host("teak.kilo.alpha")
	oak, _ := envs[0].Host("oak.kilo.alpha")
	if !teak.Unreadable || teak.CDROM {
		t.Errorf("teak: Unreadable %v, CDROM %v", teak.Unreadable, teak.CDROM)
	}
	if oak.Unreadable || !oak.CDROM || oak.UserNotes != "Login: root / pw" {
		t.Errorf("oak: Unreadable %v, CDROM %v, notes %q", oak.Unreadable, oak.CDROM, oak.UserNotes)
	}
}

// DNS can point at a Windows or Linux host with an address, named by its
// host name as CheckHostname takes it; at no other host.
func TestEnvironmentDNSAddress(t *testing.T) {
	lan := envConfig().Environments.Subnet
	host := func(name, os string, octet int) EnvHost {
		n := Notes{IP: lan.Host(octet), OS: os}
		return EnvHost{Name: name, Notes: n, Addr: n.IP}
	}
	e := Environment{Domain: "kilo.alpha", LAN: lan, Hosts: []EnvHost{
		host("teak.kilo.alpha", ImageLinux, 10), host("oak.kilo.alpha", ImageWindows, 11),
		host("box.kilo.alpha", "", 12), host("Elm.kilo.alpha", ImageLinux, 13)}}
	for choice, want := range map[string]string{"": "192.168.100.2", DNSRouter: "192.168.100.2", "teak": "192.168.100.10", "oak": "192.168.100.11"} {
		if got, err := e.DNSAddress(choice); err != nil || AddrText(got) != want {
			t.Errorf("DNSAddress(%q) = %v, %v; want %s", choice, got, err, want)
		}
	}
	for _, choice := range []string{"box", "Elm", "nope"} {
		if got, err := e.DNSAddress(choice); err == nil {
			t.Errorf("DNSAddress(%q) = %v", choice, got)
		}
	}
}

// A router's WAN and LAN MACs are those of its first NICs on the slot's
// external and internal bridges, else net0's and net1's unless that NIC is
// the other one's.
func TestRouterMACs(t *testing.T) {
	s := Slots(envConfig().Environments)[0]
	nic := func(n int, bridge string) string {
		return fmt.Sprintf("virtio=BC:24:11:00:00:0%d,bridge=%s", n, bridge)
	}
	mac := func(n int) string { return fmt.Sprintf("bc:24:11:00:00:0%d", n) }
	for _, tc := range []struct {
		name     string
		bridges  []string // by NIC: net0, net1, ...
		wan, lan string
	}{
		{"in order", []string{"devext01", "devint01"}, mac(0), mac(1)},
		{"swapped", []string{"devint01", "devext01"}, mac(1), mac(0)},
		{"more NICs on the slot's bridges", []string{"devext01", "devint01", "devext01", "devint01"}, mac(0), mac(1)},
		{"on no slot bridge", []string{"vmbr0", "vmbr1"}, mac(0), mac(1)},
		{"LAN on net0, net1 elsewhere", []string{"devint01", "vmbr1"}, "", mac(0)},
		{"WAN on net1, net0 elsewhere", []string{"vmbr0", "devext01"}, mac(1), ""},
		{"WAN past net1", []string{"vmbr0", "devint01", "devext01"}, mac(2), mac(1)},
		{"LAN past net1", []string{"devext01", "vmbr1", "devint01"}, mac(0), mac(2)},
	} {
		cfg := map[string]string{}
		for i, b := range tc.bridges {
			cfg[fmt.Sprintf("net%d", i)] = nic(i, b)
		}
		if wan, lan := routerMACs(cfg, s); wan != tc.wan || lan != tc.lan {
			t.Errorf("%s: WAN %q, LAN %q; want %q, %q", tc.name, wan, lan, tc.wan, tc.lan)
		}
	}
}

// A slot VM without NICs or disks (a config being built) isn't missing a
// NIC, and wants a guest agent only once it has a disk or an agent option.
// Memory over the usual is given in GiB when whole.
func TestEnvironmentHostWarningsByHardware(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router"}, nil)
	f.Add(proxmox.VM{VMID: 2101, Name: "bare.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha"}, nil)
	f.Add(proxmox.VM{VMID: 2102, Name: "disk.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha"},
		map[string]string{"scsi0": "competitions:2102/vm-2102-disk-0.qcow2"})
	f.Add(proxmox.VM{VMID: 2103, Name: "win.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", MaxMem: 13 << 30},
		map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=devint01", "agent": "1", "description": "battleship: os=windows"})
	envs, _, err := NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"gw.kilo.alpha":   nil,
		"bare.kilo.alpha": nil,
		"disk.kilo.alpha": {"no net0 NIC", "guest agent off"},
		"win.kilo.alpha":  {"13 GiB of memory (Windows VMs are usually at most 12 GiB)"},
	} {
		if h, _ := envs[0].Host(name); !slices.Equal(h.Warnings, want) {
			t.Errorf("%s warnings = %q, want %q", name, h.Warnings, want)
		}
	}
}

// cpuBlind is an API that can't list a node's custom CPU models.
type cpuBlind struct{ Reads }

// Without a way to list custom CPU models, no CPU model is wanted.
func TestEnvironmentsWithoutCPUModels(t *testing.T) {
	f := podstest.New("alder")
	f.CPUModels = []string{"baseline"}
	f.Add(proxmox.VM{VMID: 2100, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devint01", "cpu": "x86-64-v2-AES"})
	envs, _, err := NewPlanner(cpuBlind{f}, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := envs[0].Host("teak.kilo.alpha"); len(h.Warnings) != 0 || len(h.CPUModels) != 0 {
		t.Errorf("teak warnings %q, CPU models %q", h.Warnings, h.CPUModels)
	}
}

// Registering an image without a guest agent is noted for a router or an
// "other" image once its VM has a disk or an agent option; a router's NICs
// may be on any bridge.
func TestImageRegisterAgentAndBridgeWarnings(t *testing.T) {
	for _, tc := range []struct {
		kind string
		cfg  map[string]string
		want []string
	}{
		{ImageOther, map[string]string{"agent": "0"}, []string{"box: guest agent off."}},
		{ImageOther, map[string]string{}, nil},
		{ImageOther, map[string]string{"scsi0": "competitions:160/vm-160-disk-0.qcow2"}, []string{"box: guest agent off."}},
		{ImageRouter, map[string]string{"net1": "virtio=BC:24:11:00:01:61,bridge=vmbr1", "agent": "1"}, nil},
	} {
		f := imageFake()
		tc.cfg["net0"] = "virtio=BC:24:11:00:01:60,bridge=vmbr0"
		f.Add(proxmox.VM{VMID: 160, Name: "box", Node: "birch"}, tc.cfg)
		plan, err := NewPlanner(f, envConfig()).ImageRegister(context.Background(), ImageRequest{VMID: 160, Family: "box", ImageKind: tc.kind})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(plan.Notices, tc.want) {
			t.Errorf("%s %v: notices %q, want %q", tc.kind, tc.cfg, plan.Notices, tc.want)
		}
	}
}

// A deploy master in a slot wants a guest agent once it has a disk or an
// agent option; one outside the slots only with an agent option.
func TestDeployMasterAgentWarnings(t *testing.T) {
	f := podstest.New("cedar")
	disk := func(id int) string { return fmt.Sprintf("competitions:%d/vm-%d-disk-0.qcow2", id, id) }
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "cedar", Pool: "dev-01", Tags: "dev;kilo.alpha"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devint01"})
	f.Add(proxmox.VM{VMID: 2102, Name: "oak.kilo.alpha", Node: "cedar", Pool: "dev-01", Tags: "dev;kilo.alpha"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:02,bridge=devint01", "scsi0": disk(2102)})
	f.Add(proxmox.VM{VMID: 500, Name: "elm.kilo.alpha", Node: "cedar", Tags: "dev;kilo.alpha"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=vmbr0", "scsi0": disk(500)})
	plan, err := NewPlanner(f, config.Default()).Deploy(context.Background(), DeployRequest{Pattern: "*.kilo.alpha", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"oak.kilo.alpha: guest agent off."}; !slices.Equal(plan.Notices, want) {
		t.Errorf("Notices = %q, want %q", plan.Notices, want)
	}
}
