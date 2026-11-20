package pods

import (
	"context"
	"errors"
	"fmt"
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

	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at, err := p.HostToEdit(ctx, vms, 2101)
	if err != nil {
		t.Fatal(err)
	}
	if at.Kind() != ImageLinux || at.Octet() != 10 || at.Read.Details.OS != "Debian 12" || at.Read.Free != "Initial notes." {
		t.Fatalf("HostToEdit = %+v", at)
	}

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

	plan, err = p.EnvEdit(ctx, HostEditInput{VMID: 2101})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Items[0].Blocked, "already has these settings") {
		t.Fatalf("unchanged edit blocked = %q", plan.Items[0].Blocked)
	}

	if _, err := p.EnvEdit(ctx, HostEditInput{VMID: 2101, Octet: ClearOctet}); err == nil || !strings.Contains(err.Error(), "only other hosts can clear their address") {
		t.Fatalf("EnvEdit(ClearOctet on linux host) = %v, want error", err)
	}
	if _, err := p.EnvEdit(ctx, HostEditInput{VMID: 2101, Kind: ImageOther, Octet: ClearOctet}); err != nil {
		t.Fatalf("EnvEdit(ClearOctet on other host) = %v", err)
	}

	f.CPUModels = []string{"lab-baseline", "lab-baseline-nested"}
	plan, err = p.EnvEdit(ctx, HostEditInput{VMID: 2101, CPU: "lab-baseline"})
	if err != nil {
		t.Fatal(err)
	}
	it = plan.Items[0]
	if it.Blocked != "" || it.CPU != "custom-lab-baseline" || !slices.Equal(it.Steps, []Step{StepCPU}) {
		t.Fatalf("CPU edit item = %+v", it)
	}
	plan, err = p.EnvEdit(ctx, HostEditInput{VMID: 2101, CPU: "other-cpu"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Items[0].Blocked, "CPU model other-cpu is not one of lab-baseline or lab-baseline-nested") {
		t.Fatalf("unknown CPU model blocked = %q", plan.Items[0].Blocked)
	}
}

func TestEnvPowerAndDeleteHost(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=devint01"})
	f.Add(proxmox.VM{VMID: 2201, Name: "oak.other.env", Node: "alder", Pool: "dev-02", Tags: "dev;other.env", Status: "running"}, nil)
	f.Add(proxmox.VM{VMID: 500, Name: "scratch", Node: "alder", Status: "running"}, nil)
	f.Gate = func(_ context.Context, key string) (func(), error) {
		if key == "config:2201" || key == "config:500" {
			return nil, fmt.Errorf("unexpected read of %s outside dev-01", key)
		}
		return nil, nil
	}
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
	if _, err := p.EnvEdit(ctx, HostEditInput{VMID: 2101, Image: "debian-12-v2"}); err != nil {
		t.Fatal(err)
	}
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	delete(f.VMs, 2101)
	if _, err := p.HostToEdit(ctx, vms, 2101); !errors.Is(err, ErrNoEnvHost) || strings.Contains(err.Error(), "not in a dev slot pool") {
		t.Fatalf("HostToEdit on concurrently deleted VM = %v, want ErrNoEnvHost not-found error", err)
	}
}

// editFixture is dev slot 01 holding kilo.alpha: router gw, Windows dc
// (.20, its own DNS server), Linux teak (.10, DNS dc, CPU lab-baseline) and
// other host box (.30, no guest agent).
func editFixture() (*podstest.Fake, Planner) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router;keep", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devext01", "net1": "virtio=BC:24:11:00:00:02,bridge=devint01",
			"description": "battleship: base=gw-v1 dns=router"})
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;keep", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=devint01", "agent": "1", "cpu": "custom-lab-baseline",
			"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=dc"})
	f.Add(proxmox.VM{VMID: 2102, Name: "dc.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:04,bridge=devint01", "agent": "1",
			"description": "battleship: ip=192.168.100.20 os=windows base=debian-12-v1 dns=dc"})
	f.Add(proxmox.VM{VMID: 2103, Name: "box.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:05,bridge=devint01",
			"description": "battleship: ip=192.168.100.30 base=debian-12-v1"})
	return f, NewPlanner(f, envConfig())
}

func TestHostEditInputCheck(t *testing.T) {
	for _, tc := range []struct {
		in HostEditInput
		ok bool
	}{
		{HostEditInput{Host: "oak"}, true},
		{HostEditInput{Host: "Oak"}, false},
		{HostEditInput{Host: "oak.kilo.alpha"}, true},
		{HostEditInput{Host: "Oak.kilo.alpha"}, false},
		{HostEditInput{Host: "oak.alpha"}, false},
		{HostEditInput{Octet: ClearOctet}, true},
		{HostEditInput{Octet: 1}, false},
		{HostEditInput{Octet: 3}, false},
		{HostEditInput{Octet: FirstHostOctet}, true},
		{HostEditInput{Octet: LastHostOctet}, true},
		{HostEditInput{Octet: 255}, false},
		{HostEditInput{Image: NoBaseImage}, true},
		{HostEditInput{Image: "debian-12-v2"}, true},
		{HostEditInput{Image: "debian-12"}, false},
		{HostEditInput{CPU: "lab-baseline"}, true},
		{HostEditInput{CPU: "lab baseline"}, false},
	} {
		if err := tc.in.Check(); (err == nil) != tc.ok {
			t.Errorf("Check(%+v) = %v, want ok %v", tc.in, err, tc.ok)
		}
	}
}

func TestEnvEditItem(t *testing.T) {
	name16 := "abcdefghijklmnop"
	for _, tc := range []struct {
		name    string
		prep    func(f *podstest.Fake, p *Planner)
		req     HostEditInput
		blocked string // substring of Blocked; "" wants it unblocked
		check   func(t *testing.T, plan *Plan, it Item)
	}{
		{name: "rename keeps DNS on another host", req: HostEditInput{VMID: 2101, Host: "oak"},
			check: func(t *testing.T, _ *Plan, it Item) {
				want := []Step{StepLabel, StepWaitAgent, StepGuest, StepVerify}
				if it.Rename != "oak.kilo.alpha" || !slices.Equal(it.Steps, want) || it.Notes.DNS != "dc" ||
					it.Guest == nil || it.Guest.DNS.String() != "192.168.100.20" {
					t.Errorf("item = %+v, guest %+v", it, it.Guest)
				}
			}},
		{name: "renamed DNS server keeps serving itself", req: HostEditInput{VMID: 2102, Host: "ad"},
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.Notes.DNS != "ad" || it.Guest == nil || it.Guest.DNS.String() != "192.168.100.20" {
					t.Errorf("notes = %+v, guest %+v", it.Notes, it.Guest)
				}
			}},
		{name: "other host made Linux takes the environment's DNS", req: HostEditInput{VMID: 2103, Kind: ImageLinux},
			blocked: "box.kilo.alpha has no QEMU guest agent enabled",
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.Notes.OS != ImageLinux || it.Notes.DNS != "dc" {
					t.Errorf("notes = %+v", it.Notes)
				}
			}},
		{name: "Windows name at the limit", req: HostEditInput{VMID: 2102, Host: name16[:MaxWindowsHostname]}},
		{name: "Windows name over the limit", req: HostEditInput{VMID: 2102, Host: name16},
			blocked: "Windows host names are at most 15 characters"},
		{name: "Linux name over the Windows limit", req: HostEditInput{VMID: 2101, Host: name16}},
		{name: "first host octet", req: HostEditInput{VMID: 2101, Octet: FirstHostOctet},
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.Notes.IP.String() != "192.168.100.4" {
					t.Errorf("notes = %+v", it.Notes)
				}
			}},
		{name: "octet of a host listed after it", req: HostEditInput{VMID: 2102, Octet: 10},
			blocked: "address .10 is already taken by teak.kilo.alpha"},
		{name: "kept address another host claims",
			prep: func(f *podstest.Fake, _ *Planner) {
				f.Add(proxmox.VM{VMID: 2104, Name: "twin.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
					map[string]string{"net0": "virtio=BC:24:11:00:00:06,bridge=devint01", "agent": "1",
						"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=dc"})
			},
			req: HostEditInput{VMID: 2104, Image: "debian-12-v2"}, blocked: "address .10 is already taken by teak.kilo.alpha"},
		{name: "other host clears its address", req: HostEditInput{VMID: 2103, Octet: ClearOctet},
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.Notes.IP.IsValid() {
					t.Errorf("notes = %+v", it.Notes)
				}
			}},
		{name: "other host keeps its address", req: HostEditInput{VMID: 2103, Image: "debian-12-v2"},
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.Notes.IP.String() != "192.168.100.30" {
					t.Errorf("notes = %+v", it.Notes)
				}
			}},
		{name: "other host takes the first host octet", req: HostEditInput{VMID: 2103, Octet: FirstHostOctet},
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.Notes.IP.String() != "192.168.100.4" {
					t.Errorf("notes = %+v", it.Notes)
				}
			}},
		{name: "other host takes a used octet", req: HostEditInput{VMID: 2103, Octet: 10},
			blocked: "address .10 is already taken by teak.kilo.alpha"},
		{name: "second router", req: HostEditInput{VMID: 2101, Kind: ImageRouter},
			blocked: "an environment has one router (gw.kilo.alpha)"},
		{name: "router edit keeps router and other tags", req: HostEditInput{VMID: 2100, Image: "gw-v2"},
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.Tags != "dev;kilo.alpha;router;keep" || it.Notes.Base != "gw-v2" {
					t.Errorf("item = %+v", it)
				}
			}},
		{name: "router with one NIC",
			prep: func(f *podstest.Fake, _ *Planner) { delete(f.VMs, 2100) },
			req:  HostEditInput{VMID: 2101, Kind: ImageRouter}, blocked: "teak.kilo.alpha has one NIC"},
		{name: "name in use", req: HostEditInput{VMID: 2101, Host: "dc"},
			blocked: "a VM named dc.kilo.alpha already exists"},
		{name: "eject without a CD-ROM", req: HostEditInput{VMID: 2101, EjectCDROM: true},
			blocked: "teak.kilo.alpha has no CD-ROM attached"},
		{name: "eject without a CD-ROM beside another change", req: HostEditInput{VMID: 2101, EjectCDROM: true, Image: "debian-12-v2"},
			check: func(t *testing.T, _ *Plan, it Item) {
				if !slices.Equal(it.Steps, []Step{StepLabel}) {
					t.Errorf("steps = %v", it.Steps)
				}
			}},
		{name: "CPU on a node listing no custom models", req: HostEditInput{VMID: 2101, CPU: "lab-baseline-nested"},
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.CPU != "custom-lab-baseline-nested" || !slices.Equal(it.Steps, []Step{StepCPU}) {
					t.Errorf("item = %+v", it)
				}
			}},
		{name: "CPU already set", req: HostEditInput{VMID: 2101, CPU: "lab-baseline"},
			blocked: "teak.kilo.alpha already has these settings",
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.CPU != "" {
					t.Errorf("CPU = %q", it.CPU)
				}
			}},
		{name: "full name in the slot's domain", req: HostEditInput{VMID: 2101, Host: "oak.kilo.alpha"},
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.Rename != "oak.kilo.alpha" {
					t.Errorf("rename = %q", it.Rename)
				}
			}},
		{name: "sole host moves to another domain",
			prep: func(f *podstest.Fake, _ *Planner) {
				f.Add(proxmox.VM{VMID: 2201, Name: "solo.kilo.beta", Node: "alder", Pool: "dev-02", Tags: "dev;kilo.beta;keep", Status: "running"},
					map[string]string{"net0": "virtio=BC:24:11:00:00:07,bridge=devint02", "agent": "1",
						"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=router"})
			},
			req: HostEditInput{VMID: 2201, Host: "solo.new.domain"},
			check: func(t *testing.T, plan *Plan, it Item) {
				if it.Rename != "solo.new.domain" || it.Tags != "dev;new.domain;keep" {
					t.Errorf("item = %+v", it)
				}
				if want := []string{"slot:02", "domain:kilo.beta", "domain:new.domain"}; !slices.Equal(plan.Locks, want) {
					t.Errorf("locks = %v, want %v", plan.Locks, want)
				}
			}},
		{name: "bad ops CA key",
			prep: func(_ *podstest.Fake, p *Planner) { p.Cfg.Environments.OpsCAKey = "not-a-key" },
			req:  HostEditInput{VMID: 2101, Host: "oak"}, blocked: "ops CA key: "},
		{name: "bad ops CA key after a missing agent",
			prep: func(_ *podstest.Fake, p *Planner) { p.Cfg.Environments.OpsCAKey = "not-a-key" },
			req:  HostEditInput{VMID: 2103, Kind: ImageLinux}, blocked: "box.kilo.alpha has no QEMU guest agent enabled"},
		{name: "guest host without a domain",
			prep: func(f *podstest.Fake, _ *Planner) {
				f.Add(proxmox.VM{VMID: 2301, Name: "lonely", Node: "alder", Pool: "dev-03", Tags: "dev", Status: "running"},
					map[string]string{"net0": "virtio=BC:24:11:00:00:08,bridge=devint03", "agent": "1",
						"description": "battleship: ip=192.168.100.40 base=debian-12-v1"})
			},
			req: HostEditInput{VMID: 2301, Kind: ImageLinux}, blocked: "lonely has no valid domain",
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.Guest != nil || !slices.Equal(it.Steps, []Step{StepLabel}) {
					t.Errorf("item = %+v, guest %+v", it, it.Guest)
				}
			}},
		{name: "no free address",
			prep: func(f *podstest.Fake, _ *Planner) {
				for o := FirstHostOctet; o <= LastHostOctet; o++ {
					if o == 10 || o == 20 || o == 30 {
						continue
					}
					f.Add(proxmox.VM{VMID: 3000 + o, Name: fmt.Sprintf("h%d.kilo.alpha", o), Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha"},
						map[string]string{"description": fmt.Sprintf("battleship: ip=192.168.100.%d", o)})
				}
				f.Add(proxmox.VM{VMID: 2104, Name: "spare.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
					map[string]string{"net0": "virtio=BC:24:11:00:00:06,bridge=devint01", "agent": "1"})
			},
			req: HostEditInput{VMID: 2104, Kind: ImageLinux}, blocked: "no free address left in kilo.alpha",
			check: func(t *testing.T, _ *Plan, it Item) {
				if it.Guest != nil {
					t.Errorf("guest = %+v", it.Guest)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := editFixture()
			if tc.prep != nil {
				tc.prep(f, &p)
			}
			plan, err := p.EnvEdit(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			it := plan.Items[0]
			if tc.blocked == "" && it.Blocked != "" || !strings.Contains(it.Blocked, tc.blocked) {
				t.Errorf("blocked = %q, want %q", it.Blocked, tc.blocked)
			}
			if tc.check != nil {
				tc.check(t, plan, it)
			}
		})
	}
}

func TestEnvEditNameInOtherDomain(t *testing.T) {
	_, p := editFixture()
	_, err := p.EnvEdit(context.Background(), HostEditInput{VMID: 2101, Host: "oak.other.env"})
	if err == nil || !strings.Contains(err.Error(), "dev slot 01 holds kilo.alpha") {
		t.Fatalf("EnvEdit(oak.other.env in kilo.alpha's slot) = %v, want error", err)
	}
}

func TestEnvEditStaleDetails(t *testing.T) {
	f, p := editFixture()
	ctx := context.Background()
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at, err := p.HostToEdit(ctx, vms, 2101)
	if err != nil {
		t.Fatal(err)
	}
	det := at.Edit()
	det.OS = "Debian 13"
	f.VMs[2101].Config["description"] = "Changed elsewhere.\n\n" + f.VMs[2101].Config["description"]

	plan, err := p.EnvEdit(ctx, HostEditInput{VMID: 2101, Details: &det})
	if err != nil {
		t.Fatal(err)
	}
	if b := plan.Items[0].Blocked; !strings.Contains(b, "the notes of teak.kilo.alpha changed since you opened this form") {
		t.Errorf("stale details blocked = %q", b)
	}
	// An earlier block reason stands.
	plan, err = p.EnvEdit(ctx, HostEditInput{VMID: 2101, Host: "dc", Details: &det})
	if err != nil {
		t.Fatal(err)
	}
	if b := plan.Items[0].Blocked; b != "a VM named dc.kilo.alpha already exists" {
		t.Errorf("stale details after a name clash blocked = %q", b)
	}
}

func TestEnvEditRenameIntoOtherDomainDropsApproval(t *testing.T) {
	for _, tc := range []struct {
		name, host, want string
	}{
		{"other domain", "oak.other.env", "dev;other.env;Keep"},
		{"same domain", "oak", "dev;kilo.alpha;Keep;approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := podstest.New("alder")
			f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;Keep;approved", Status: "running"},
				map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=devint01", "agent": "1",
					"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1"})
			cfg := envConfig()
			cfg.Deploy.ApprovedTag = "approved"
			p := NewPlanner(f, cfg)
			plan, err := p.EnvEdit(context.Background(), HostEditInput{VMID: 2101, Host: tc.host})
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Items[0].Tags; got != tc.want {
				t.Errorf("tags = %q, want %q", got, tc.want)
			}
		})
	}
}
