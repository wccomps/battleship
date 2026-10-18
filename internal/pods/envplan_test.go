package pods

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func envFake() *podstest.Fake {
	f := podstest.New("alder", "birch")
	f.Add(proxmox.VM{VMID: 8000, Name: "gw-v1", Node: "alder", Template: true, Tags: "base;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:80:00,bridge=vmbr0", "net1": "virtio=BC:24:11:00:80:01,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v1", Node: "alder", Template: true, Tags: "base;linux"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:80:02,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 8002, Name: "win2022-v1", Node: "birch", Template: true, Tags: "base;windows"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:80:03,bridge=vmbr0"})
	return f
}

func create(t *testing.T, p Planner, hosts ...string) *Plan {
	t.Helper()
	req := EnvRequest{Domain: "kilo.alpha", Router: HostSpec{Host: "gw", Family: "gw"}}
	for _, h := range hosts {
		hs, err := ParseHostSpec(h)
		if err != nil {
			t.Fatal(err)
		}
		req.Hosts = append(req.Hosts, hs)
	}
	plan, err := p.EnvCreate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestEnvCreatePlansSlotOne(t *testing.T) {
	plan := create(t, NewPlanner(envFake(), envConfig()), "teak=debian-12:10", "oak=win2022")
	if len(plan.Items) != 3 {
		t.Fatalf("items = %+v", plan.Items)
	}
	gw, teak, oak := plan.Items[0], plan.Items[1], plan.Items[2]
	if gw.Name != "gw.kilo.alpha" || gw.VMID != 2100 || gw.Pool != "dev-01" || gw.Tags != "dev;kilo.alpha;router" ||
		gw.NICs["net0"] != "devext01" || gw.NICs["net1"] != "devint01" || gw.Guest != nil ||
		!slices.Equal(gw.Steps, []Step{StepClone, StepLabel, StepNetwork, StepStart}) {
		t.Errorf("router = %+v", gw)
	}
	if teak.Guest == nil || AddrText(teak.Guest.IP) != "192.168.100.10" || AddrText(teak.Guest.Gateway) != "192.168.100.2" || AddrText(teak.Guest.DNS) != "192.168.100.2" ||
		AddrText(teak.Notes.IP) != "192.168.100.10" || teak.Notes.OS != "linux" || teak.Notes.Base != "debian-12-v1" || teak.Notes.DNS != "router" ||
		teak.NICs["net0"] != "devint01" || !slices.Equal(teak.Steps, []Step{StepClone, StepLabel, StepNetwork, StepStart, StepWaitAgent, StepGuest, StepVerify}) {
		t.Errorf("teak = %+v guest %+v notes %+v", teak, teak.Guest, teak.Notes)
	}
	if oak.VMID%100 == teak.VMID%100 || oak.Node != "birch" || oak.Guest.OS != ImageWindows || oak.Guest.IP.As4()[3] == 10 {
		t.Errorf("oak = %+v", oak)
	}
	for _, k := range []string{"slot:01", "domain:kilo.alpha"} {
		if !slices.Contains(plan.Locks, k) {
			t.Errorf("locks %v lack %s", plan.Locks, k)
		}
	}
}

func TestEnvCreateTakesNextSlotWhenOneIsHeld(t *testing.T) {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2100, Name: "x.other.env", Node: "alder", Pool: "dev-01"}, nil)
	plan := create(t, NewPlanner(f, envConfig()), "teak=debian-12")
	if plan.Items[0].Pool != "dev-02" || plan.Items[0].VMID != 2200 || !slices.Contains(plan.Locks, "slot:02") {
		t.Errorf("router = %+v; slot 1 is held", plan.Items[0])
	}
}

func TestEnvCreateBlocks(t *testing.T) {
	plan := create(t, NewPlanner(envFake(), envConfig()), "averyveryverylongname=win2022", "teak=nope", "oak=debian-12:9", "elm=debian-12:9")
	reasons := map[string]string{}
	for _, it := range plan.Items {
		reasons[it.Host] = it.Blocked
	}
	if !strings.Contains(reasons["averyveryverylongname"], "15") {
		t.Errorf("long Windows name: %q", reasons["averyveryverylongname"])
	}
	if !strings.Contains(reasons["teak"], "nope") {
		t.Errorf("unknown image: %q", reasons["teak"])
	}
	if reasons["oak"] != "" || !strings.Contains(reasons["elm"], ".9") {
		t.Errorf("duplicate octet: oak %q, elm %q", reasons["oak"], reasons["elm"])
	}
}

func TestEnvCreateRefusesDomainInUse(t *testing.T) {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01"}, nil)
	_, err := NewPlanner(f, envConfig()).EnvCreate(context.Background(),
		EnvRequest{Domain: "kilo.alpha", Router: HostSpec{Host: "gw", Family: "gw"}})
	if err == nil || !strings.Contains(err.Error(), "add hosts") {
		t.Errorf("err = %v", err)
	}
}

func TestEnvAddResumesExistingHost(t *testing.T) {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha"},
		map[string]string{"net0": "virtio=BC:24:11:00:21:01,bridge=vmbr0", "description": "battleship: ip=192.168.100.77 os=linux base=debian-12-v1 dns=router"})
	hs, _ := ParseHostSpec("teak=debian-12")
	plan, err := NewPlanner(f, envConfig()).EnvAdd(context.Background(), EnvRequest{Domain: "kilo.alpha", Hosts: []HostSpec{hs}})
	if err != nil {
		t.Fatal(err)
	}
	it := plan.Items[0]
	if it.VMID != 2101 || slices.Contains(it.Steps, StepClone) || AddrText(it.Guest.IP) != "192.168.100.77" || it.Blocked != "" {
		t.Errorf("resumed = %+v", it)
	}
	if _, err := NewPlanner(f, envConfig()).EnvAdd(context.Background(), EnvRequest{Domain: "kilo.alpha", Router: HostSpec{Host: "gw2", Family: "gw"}}); err == nil {
		t.Error("a second router was accepted")
	}
}

func TestEnvAddBlocksResumeOfAnotherKind(t *testing.T) {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha"},
		map[string]string{"net0": "virtio=BC:24:11:00:21:01,bridge=vmbr0", "description": "battleship: ip=192.168.100.77 os=linux base=debian-12-v1 dns=router"})
	for _, spec := range []string{"teak=win2022", "teak=gw"} {
		hs, _ := ParseHostSpec(spec)
		plan, err := NewPlanner(f, envConfig()).EnvAdd(context.Background(), EnvRequest{Domain: "kilo.alpha", Hosts: []HostSpec{hs}})
		if err != nil {
			t.Fatal(err)
		}
		if it := plan.Items[0]; it.Blocked == "" || it.Guest != nil && it.Guest.OS == ImageWindows {
			t.Errorf("%s: resumed as another kind: %+v", spec, it)
		}
	}
}

func TestEnvCreatePlanIsDeterministic(t *testing.T) {
	f := envFake()
	a := create(t, NewPlanner(f, envConfig()), "teak=debian-12:10", "oak=win2022")
	b := create(t, NewPlanner(f, envConfig()), "teak=debian-12:10", "oak=win2022")
	if !reflect.DeepEqual(a, b) {
		t.Errorf("plans differ:\n%+v\n%+v", a, b)
	}
}

// Re-running a partly failed create through Add hosts names the existing
// router again: it is resumed, not refused.
func TestEnvAddResumesTheExistingRouter(t *testing.T) {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	hs, _ := ParseHostSpec("oak=win2022")
	plan, err := NewPlanner(f, envConfig()).EnvAdd(context.Background(),
		EnvRequest{Domain: "kilo.alpha", Router: HostSpec{Host: "gw", Family: "gw"}, Hosts: []HostSpec{hs}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("items = %+v", plan.Items)
	}
	gw, oak := plan.Items[0], plan.Items[1]
	if gw.VMID != 2100 || gw.Blocked != "" || slices.Contains(gw.Steps, StepClone) {
		t.Errorf("router = %+v", gw)
	}
	if oak.Blocked != "" || oak.VMID == 2100 || !slices.Contains(oak.Steps, StepClone) || oak.Guest == nil {
		t.Errorf("oak = %+v", oak)
	}
}

// A config built in Go may leave the LAN zero, which Load never does: then
// no host has an address, so each guest is blocked rather than configured.
func TestEnvCreateWithoutALANBlocksGuests(t *testing.T) {
	cfg := envConfig()
	cfg.Environments.Subnet = config.LAN{}
	plan := create(t, NewPlanner(envFake(), cfg), "teak=debian-12:10", "oak=win2022")
	for _, it := range plan.Items[1:] {
		if it.Blocked == "" || slices.Contains(it.Steps, StepGuest) {
			t.Errorf("%s = %+v", it.Name, it)
		}
	}
}
