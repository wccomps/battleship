package pods

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

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
	if teak.Guest == nil || teak.Guest.DNSOnly || teak.Guest.IP != "192.168.100.10" || teak.Guest.Gateway != "192.168.100.2" || teak.Guest.DNS != "192.168.100.2" ||
		teak.Notes.IP != "192.168.100.10" || teak.Notes.OS != "linux" || teak.Notes.Base != "debian-12-v1" || teak.Notes.DNS != "router" ||
		teak.NICs["net0"] != "devint01" || !slices.Equal(teak.Steps, []Step{StepClone, StepLabel, StepNetwork, StepStart, StepWaitAgent, StepGuest, StepVerify}) {
		t.Errorf("teak = %+v guest %+v notes %+v", teak, teak.Guest, teak.Notes)
	}
	if oak.VMID%100 == teak.VMID%100 || oak.Node != "birch" || oak.Guest.OS != ImageWindows || LastOctet(oak.Guest.IP) == 10 {
		t.Errorf("oak = %+v", oak)
	}
	for _, k := range []string{"slot:01", "domain:kilo.alpha", "vmid:2100"} {
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
	if it.VMID != 2101 || slices.Contains(it.Steps, StepClone) || it.Guest.IP != "192.168.100.77" || it.Blocked != "" {
		t.Errorf("resumed = %+v", it)
	}
	if _, err := NewPlanner(f, envConfig()).EnvAdd(context.Background(), EnvRequest{Domain: "kilo.alpha", Router: HostSpec{Host: "gw2", Family: "gw"}}); err == nil {
		t.Error("a second router was accepted")
	}
}

func TestEnvDNSWorksWithoutImages(t *testing.T) {
	f := podstest.New("alder") // no base images left on the cluster
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router", Status: "running"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	f.Add(proxmox.VM{VMID: 2101, Name: "oak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.20 os=windows base=win2022-v1 dns=router"})
	f.Add(proxmox.VM{VMID: 2102, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "stopped"},
		map[string]string{"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=router"})
	plan, err := NewPlanner(f, envConfig()).EnvDNS(context.Background(), EnvRequest{Domain: "kilo.alpha", DNS: "oak"})
	if err != nil {
		t.Fatal(err)
	}
	byHost := map[string]Item{}
	for _, it := range plan.Items {
		byHost[it.Host] = it
	}
	if gw := byHost["gw"]; !slices.Equal(gw.Steps, []Step{StepLabel}) || gw.Notes.DNS != "oak" || gw.Tags != "dev;kilo.alpha;router" {
		t.Errorf("gw = %+v", gw)
	}
	if oak := byHost["oak"]; oak.Guest.DNS != "192.168.100.20" || oak.Guest.OS != ImageWindows || !oak.Guest.DNSOnly ||
		!slices.Equal(oak.Steps, []Step{StepGuest, StepVerify, StepLabel}) {
		t.Errorf("oak = %+v", oak)
	}
	if !strings.Contains(byHost["teak"].Blocked, "stopped") {
		t.Errorf("teak = %+v", byHost["teak"])
	}
	if _, err := NewPlanner(f, envConfig()).EnvDNS(context.Background(), EnvRequest{Domain: "kilo.alpha", DNS: "gw"}); err == nil {
		t.Error("the router host was accepted as a DNS server (use router)")
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

// Notes are user-editable: an address that isn't a host of the subnet,
// quoted or not, leaves the host without one rather than reaching a script.
func TestEnvDNSIgnoresBadNoteAddresses(t *testing.T) {
	for _, ip := range []string{"192.168.100.20'", "192.168.100.20\u2019;x", "10.50.1.20", "192.168.100.3", "192.168.100.020"} {
		f := podstest.New("alder")
		f.Add(proxmox.VM{VMID: 2101, Name: "oak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
			map[string]string{"description": "battleship: ip=" + ip + " os=windows base=win2022-v1 dns=router"})
		p := NewPlanner(f, envConfig())
		if _, err := p.EnvDNS(context.Background(), EnvRequest{Domain: "kilo.alpha", DNS: "oak"}); err == nil {
			t.Errorf("%q: oak was accepted as a DNS server", ip)
		}
		plan, err := p.EnvDNS(context.Background(), EnvRequest{Domain: "kilo.alpha", DNS: "router"})
		if err != nil {
			t.Fatal(err)
		}
		if it := plan.Items[0]; it.Guest != nil || !slices.Equal(it.Steps, []Step{StepLabel}) {
			t.Errorf("%q: oak = %+v guest %+v", ip, it, it.Guest)
		}
	}
}

func TestEnvDNSBlocksAHostNameScriptsCantTake(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2101, Name: "Oak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.20 os=windows base=win2022-v1 dns=router"})
	plan, err := NewPlanner(f, envConfig()).EnvDNS(context.Background(), EnvRequest{Domain: "kilo.alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if b := plan.Items[0].Blocked; !strings.Contains(b, "Oak") {
		t.Errorf("blocked = %q", b)
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
