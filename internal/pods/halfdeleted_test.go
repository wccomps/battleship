package pods

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/proxmox"
)

// halfDeleted adds what a failed destroy leaves: the VM listed as "VM
// <vmid>" with its tags, its config only the lock.
func halfDeleted(vmid int, pool, tags string) (proxmox.VM, map[string]string) {
	return proxmox.VM{VMID: vmid, Name: fmt.Sprintf("VM %d", vmid), Node: "birch", Pool: pool, Tags: tags, Status: "stopped"},
		map[string]string{"lock": "destroyed"}
}

func envOf(t *testing.T, p Planner, slot int) Environment {
	t.Helper()
	envs, _, err := p.Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(envs, func(e Environment) bool { return e.Slot.N == slot })
	if i < 0 {
		t.Fatalf("no slot %d", slot)
	}
	return envs[i]
}

// A half-deleted VM isn't one of the environment's hosts: Set DNS doesn't
// plan it, and it isn't a DNS choice. It still keeps the slot from being
// free.
func TestSetDNSLeavesAHalfDeletedVMAlone(t *testing.T) {
	f := teardownFake()
	f.Add(halfDeleted(2103, "dev-01", "dev;kilo.alpha"))
	p := NewPlanner(f, envConfig())
	env := envOf(t, p, 1)
	if env.Domain != "kilo.alpha" || env.Free() || len(env.HalfDeleted) != 1 || env.HalfDeleted[0].VMID != 2103 {
		t.Fatalf("env = %+v", env)
	}
	if _, ok := env.Host("VM 2103"); ok || slices.Contains(env.DNSChoices(), "VM 2103") {
		t.Errorf("the half-deleted VM is a host: %+v", env)
	}
	plan, err := p.EnvDNS(context.Background(), EnvRequest{Domain: "kilo.alpha", DNS: "teak"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(plan.Items, func(it Item) bool { return it.VMID == 2103 }) {
		t.Errorf("Set DNS plans the half-deleted VM: %q", itemNames(plan))
	}
}

// A half-deleted router doesn't count as the environment's router: Add
// hosts may add one.
func TestHalfDeletedRouterIsNoRouter(t *testing.T) {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2300, Name: "dc.kilo.bravo", Node: "alder", Pool: "dev-03", Tags: "dev;kilo.bravo", Status: "running"},
		map[string]string{"description": "battleship: ip=192.168.100.10 os=windows base=win2022-v1 dns=router"})
	f.Add(halfDeleted(2301, "dev-03", "dev;kilo.bravo;router"))
	p := NewPlanner(f, envConfig())
	if env := envOf(t, p, 3); env.HasRouter() || env.RouterName() != "" {
		t.Errorf("env = %+v", env)
	}
	plan, err := p.EnvAdd(context.Background(), EnvRequest{Domain: "kilo.bravo", Router: HostSpec{Host: "gw", Family: "gw"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Blocked != "" || plan.Items[0].NICs["net1"] == "" {
		t.Errorf("items = %+v", plan.Items)
	}
}

// Whatever VMs a slot holds, half-deleted ones are never hosts: no Set DNS
// or Add hosts plan touches them, while a teardown lists each one, blocked,
// whether in the pool or only in the slot's VMIDs.
func TestPropHalfDeletedOnlyForTeardown(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		f := envFake()
		f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router", Status: "running"},
			map[string]string{"description": "battleship: base=gw-v1 dns=router"})
		var gone []int
		for i := range rapid.IntRange(1, 5).Draw(t, "half-deleted") {
			vmid := 2101 + i
			pool := rapid.SampledFrom([]string{"dev-01", ""}).Draw(t, "pool")
			if pool != "" && rapid.Bool().Draw(t, "outside VMIDs") {
				vmid = 9100 + i
			}
			tags := rapid.SampledFrom([]string{"", "dev;kilo.alpha", "dev;kilo.alpha;router", "base;linux"}).Draw(t, "tags")
			vm, cfg := halfDeleted(vmid, pool, tags)
			vm.Name = rapid.SampledFrom([]string{vm.Name, "dc.kilo.alpha", "x"}).Draw(t, "name") // or a destroy that kept the name
			f.Add(vm, cfg)
			gone = append(gone, vmid)
		}
		p := NewPlanner(f, envConfig())
		ctx := context.Background()
		touches := func(plan *Plan) bool {
			return slices.ContainsFunc(plan.Items, func(it Item) bool { return slices.Contains(gone, it.VMID) })
		}
		dns, err := p.EnvDNS(ctx, EnvRequest{Domain: "kilo.alpha"})
		if err != nil {
			t.Fatal(err)
		}
		if touches(dns) {
			t.Fatalf("Set DNS plans a half-deleted VM: %+v", dns.Items)
		}
		add, err := p.EnvAdd(ctx, EnvRequest{Domain: "kilo.alpha", Hosts: []HostSpec{{Host: "dc", Family: "debian-12"}}})
		if err != nil {
			t.Fatal(err)
		}
		if touches(add) {
			t.Fatalf("Add hosts = %+v", add.Items)
		}
		down, err := p.EnvTeardown(ctx, EnvRequest{Domain: "kilo.alpha"})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range gone {
			i := slices.IndexFunc(down.Items, func(it Item) bool { return it.VMID == id })
			if i < 0 || !strings.Contains(down.Items[i].Blocked, "half-deleted") {
				t.Fatalf("teardown items = %q, missing half-deleted %d", itemNames(down), id)
			}
		}
	})
}

// A volume at a slot VMID that isn't a VM disk (a container's subvol-) has
// no owner VMID, so it is never a free-disks item.
func TestEnvTeardownIgnoresVolumesOfNoVM(t *testing.T) {
	f := teardownFake()
	f.Vols["competitions:subvol-2105-disk-0"] = 2105
	plan := envTeardown(t, f, EnvRequest{Domain: "kilo.alpha"})
	for _, it := range plan.Items {
		if slices.Contains(it.Steps, StepFreeDisks) {
			t.Errorf("free-disks item %+v", it)
		}
	}
}

// A slot holding only a half-deleted VM, in its pool or at one of its
// VMIDs, isn't free: Create doesn't offer it.
func TestHalfDeletedVMHoldsItsSlot(t *testing.T) {
	f := envFake()
	f.Add(halfDeleted(2200, "dev-02", ""))
	f.Add(halfDeleted(2350, "", ""))
	p := NewPlanner(f, envConfig())
	envs, _, err := p.Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	open, _, err := p.OpenSlots(context.Background(), envs)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(open, func(s Slot) bool { return s.N == 2 || s.N == 3 }) || !slices.ContainsFunc(open, func(s Slot) bool { return s.N == 1 }) {
		t.Errorf("open slots %+v", open)
	}
}
