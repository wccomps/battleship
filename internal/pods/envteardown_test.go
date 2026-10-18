package pods

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// teardownFake has kilo.alpha in dev-01 (router gw, hosts teak and oak),
// oscar.lima in dev-02, a VM outside any pool on dev-01's LAN, and a team
// VM.
func teardownFake() *podstest.Fake {
	f := podstest.New("alder", "birch")
	add := func(vmid int, name, node, pool, tags, desc, bridge string) {
		f.Add(proxmox.VM{VMID: vmid, Name: name, Node: node, Pool: pool, Tags: tags, Status: "running"},
			map[string]string{"description": desc, "net0": "virtio=BC:24:11:00:00:01,bridge=" + bridge,
				"scsi0": fmt.Sprintf("competitions:vm-%d-disk-0,size=8G", vmid)})
	}
	add(2100, "gw.kilo.alpha", "alder", "dev-01", "dev;kilo.alpha;router", "battleship: base=gw-v1 dns=router", "devext01")
	add(2101, "teak.kilo.alpha", "alder", "dev-01", "dev;kilo.alpha", "battleship: ip=192.168.100.10 os=linux base=debian-12-v2 dns=router", "devint01")
	add(2102, "oak.kilo.alpha", "birch", "dev-01", "dev;kilo.alpha", "battleship: ip=192.168.100.11 os=windows base=win2022-v1 dns=router", "devint01")
	add(2200, "ad.oscar.lima", "birch", "dev-02", "dev;oscar.lima", "battleship: ip=192.168.100.10 os=windows base=win2022-v1", "devint02")
	add(150, "stray", "alder", "", "", "", "devint01")
	add(10101, "team01-dc", "alder", "team01", "", "", "team01int")
	return f
}

func envTeardown(t *testing.T, f *podstest.Fake, req EnvRequest) *Plan {
	t.Helper()
	plan, err := NewPlanner(f, envConfig()).EnvTeardown(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func itemNames(plan *Plan) []string {
	var out []string
	for _, it := range plan.Items {
		out = append(out, it.Name)
	}
	return out
}

// A teardown deletes the VMs in its slot's pool, router included, each
// shut down and then deleted, with their notes for the review. A VM on its
// networks from outside the pool is named, never planned.
func TestEnvTeardownDeletesOnlyItsOwnVMs(t *testing.T) {
	plan := envTeardown(t, teardownFake(), EnvRequest{Domain: "kilo.alpha"})
	if got := itemNames(plan); !slices.Equal(got, []string{"gw.kilo.alpha", "oak.kilo.alpha", "teak.kilo.alpha"}) {
		t.Fatalf("items = %q", got)
	}
	for _, it := range plan.Items {
		if !slices.Equal(it.Steps, []Step{StepStop, StepDelete}) || it.Blocked != "" || it.Notes == nil || it.Source != nil {
			t.Errorf("%s = %+v", it.Name, it)
		}
	}
	if teak := plan.Items[2]; teak.VMID != 2101 || teak.Node != "alder" || teak.Notes.IP != "192.168.100.10" || teak.Notes.Base != "debian-12-v2" {
		t.Errorf("teak = %+v notes %+v", teak, teak.Notes)
	}
	if want := []KeptVM{{Name: "stray", Why: "outside dev-01 but has a NIC on its networks"}}; !slices.Equal(plan.Kept, want) || len(plan.Notices) != 0 {
		t.Errorf("kept = %q, notices = %q", plan.Kept, plan.Notices)
	}
	if got := KeptLines(plan.Kept); !slices.Equal(got, []string{"stray is outside dev-01 but has a NIC on its networks."}) {
		t.Errorf("kept lines: %q", got)
	}
	if plan.Slot != "01" || slices.ContainsFunc(plan.Items, func(it Item) bool { return it.Pool != "dev-01" }) {
		t.Errorf("slot %q, items %+v", plan.Slot, plan.Items)
	}
	if plan.Kind != KindEnvTeardown || plan.Config != ConfigHash(KindEnvTeardown, envConfig()) {
		t.Errorf("plan = %+v", plan)
	}
}

// Its locks are its slot and domain, and a write on each VM it deletes;
// never on a VM it leaves alone.
func TestEnvTeardownLocks(t *testing.T) {
	plan := envTeardown(t, teardownFake(), EnvRequest{Domain: "kilo.alpha"})
	writes, reads := plan.LockKeys()
	want := []string{"domain:kilo.alpha", "slot:01", "vmid:2100", "vmid:2101", "vmid:2102"}
	if !slices.Equal(writes, want) || len(reads) != 0 {
		t.Errorf("locks = %q, %q; want writes %q", writes, reads, want)
	}
}

// A slot whose VMs don't share one domain can't be torn down by one, named
// by domain or by slot; nor can a slot holding another domain, or a domain
// no slot holds.
func TestEnvTeardownRefuses(t *testing.T) {
	f := teardownFake()
	f.Add(proxmox.VM{VMID: 2300, Name: "a.kilo.bravo", Node: "alder", Pool: "dev-03"}, nil)
	f.Add(proxmox.VM{VMID: 2301, Name: "b.other.zone", Node: "alder", Pool: "dev-03"}, nil)
	p := NewPlanner(f, envConfig())
	for _, tc := range []struct {
		req  EnvRequest
		want string
	}{
		{EnvRequest{Domain: "kilo.bravo"}, "dev slot 03 holds VMs of several domains"},
		{EnvRequest{Domain: "kilo.bravo", Slot: 3}, "dev slot 03 holds VMs of several domains"},
		{EnvRequest{Domain: "kilo.alpha", Slot: 2}, "dev slot 02 holds oscar.lima, not kilo.alpha"},
		{EnvRequest{Domain: "kilo.alpha", Slot: 9}, "there's no dev slot 09"},
		{EnvRequest{Domain: "nowhere.zone"}, "no dev slot holds nowhere.zone"},
		{EnvRequest{Domain: "Bad Domain"}, "domain"},
	} {
		if _, err := p.EnvTeardown(context.Background(), tc.req); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v, want %q", tc.req, err, tc.want)
		}
	}
}

// What a failed teardown leaves is planned again in its slot: a VM left
// half-deleted (in its pool, or out of it) is listed blocked with how to
// finish it, and disks a gone VM left in the slot's VMIDs are freed. Disks
// at a VMID outside the slot are not its.
func TestEnvTeardownPlansWhatAFailedOneLeft(t *testing.T) {
	f := teardownFake()
	for _, id := range []int{2100, 2101, 2102} {
		delete(f.VMs, id)
		delete(f.Vols, fmt.Sprintf("competitions:vm-%d-disk-0", id))
	}
	f.Add(proxmox.VM{VMID: 2103, Name: "VM 2103", Node: "birch", Pool: "dev-01"}, map[string]string{"lock": "destroyed"})
	f.Add(proxmox.VM{VMID: 2104, Name: "VM 2104", Node: "alder"}, map[string]string{"lock": "destroyed"})
	f.Vols["competitions:vm-2105-disk-0"] = 2105
	f.Vols["competitions:vm-2405-disk-0"] = 2405
	plan := envTeardown(t, f, EnvRequest{Domain: "kilo.alpha", Slot: 1})
	if got := itemNames(plan); !slices.Equal(got, []string{"disks-2105", "half-deleted-2103", "half-deleted-2104"}) {
		t.Fatalf("items = %q", got)
	}
	if it := plan.Items[0]; it.Blocked != "" || !slices.Equal(it.Steps, []Step{StepFreeDisks}) || it.VMID != 2105 {
		t.Errorf("disks = %+v", it)
	}
	for _, it := range plan.Items[1:] {
		if !strings.Contains(it.Blocked, "half-deleted") || !strings.Contains(it.Blocked, "qm destroy") {
			t.Errorf("%s = %+v", it.Name, it)
		}
	}
	if slices.ContainsFunc(plan.Items, func(it Item) bool { return it.Pool != "dev-01" }) {
		t.Errorf("items %+v, want each in dev-01", plan.Items)
	}
	// By domain alone there is nothing of kilo.alpha left to find.
	if _, err := NewPlanner(f, envConfig()).EnvTeardown(context.Background(), EnvRequest{Domain: "kilo.alpha"}); err == nil {
		t.Error("a domain no slot holds any more was planned")
	}
}

// A half-deleted VM in the pool doesn't make the slot mixed: the rest of
// the environment is still torn down by its domain, and it is listed, even
// at a VMID outside the slot's.
func TestEnvTeardownBesideAHalfDeletedVM(t *testing.T) {
	f := teardownFake()
	f.Add(proxmox.VM{VMID: 9103, Name: "VM 9103", Node: "birch", Pool: "dev-01"}, map[string]string{"lock": "destroyed"})
	plan := envTeardown(t, f, EnvRequest{Domain: "kilo.alpha"})
	if got := itemNames(plan); !slices.Equal(got, []string{"gw.kilo.alpha", "half-deleted-9103", "oak.kilo.alpha", "teak.kilo.alpha"}) {
		t.Errorf("items = %q", got)
	}
}

// A template in the pool is not deleted with the environment.
func TestEnvTeardownBlocksATemplate(t *testing.T) {
	f := teardownFake()
	f.Add(proxmox.VM{VMID: 2150, Name: "base.kilo.alpha", Node: "alder", Pool: "dev-01", Template: true}, nil)
	plan := envTeardown(t, f, EnvRequest{Domain: "kilo.alpha"})
	i := slices.IndexFunc(plan.Items, func(it Item) bool { return it.VMID == 2150 })
	if i < 0 || !strings.Contains(plan.Items[i].Blocked, "is a template") {
		t.Errorf("items = %+v", plan.Items)
	}
}

// Whatever the slots hold, a teardown plans to delete just the VMs in its
// slot's pool named in its domain, each of them, and every other VM is
// left alone; a slot of mixed domains is refused.
func TestPropEnvTeardownDeletesOnlyItsOwnVMs(t *testing.T) {
	domains := []string{"kilo.alpha", "kilo.bravo", "oscar.lima"}
	rapid.Check(t, func(t *rapid.T) {
		f := podstest.New("alder", "birch")
		n := rapid.IntRange(0, 12).Draw(t, "vms")
		used := map[int]bool{}
		for i := range n {
			slot := rapid.IntRange(0, 3).Draw(t, "slot") // 0: outside every pool
			host := rapid.SampledFrom([]string{"gw", "dc", "web", "x"}).Draw(t, "host")
			name := host + "." + rapid.SampledFrom(domains).Draw(t, "domain")
			if rapid.IntRange(0, 5).Draw(t, "odd name") == 0 {
				name = rapid.SampledFrom([]string{"stray", "team01-dc", "VM 2150", "dc."}).Draw(t, "name")
			}
			vmid := 2000 + slot*100 + i
			if slot == 0 {
				vmid = 150 + i
			}
			if rapid.Bool().Draw(t, "foreign vmid") {
				vmid = 9000 + i // a pool member outside the slot's VMIDs
			}
			if used[vmid] {
				continue
			}
			used[vmid] = true
			vm := proxmox.VM{VMID: vmid, Name: name, Node: "alder", Status: "running", Template: rapid.IntRange(0, 6).Draw(t, "template") == 0}
			if slot > 0 {
				vm.Pool = "dev-" + SlotName(slot)
			}
			bridge := fmt.Sprintf("devint%02d", rapid.IntRange(1, 3).Draw(t, "bridge"))
			f.Add(vm, map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=" + bridge})
		}
		domain := rapid.SampledFrom(domains).Draw(t, "teardown")
		envs, _, err := NewPlanner(f, envConfig()).Environments(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		plan, err := NewPlanner(f, envConfig()).EnvTeardown(context.Background(), EnvRequest{Domain: domain})
		env, found := EnvByDomain(envs, domain)
		if err != nil {
			if found {
				t.Fatalf("%s is in dev slot %s, yet: %v", domain, env.Slot.Name, err)
			}
			return
		}
		if !found {
			t.Fatalf("planned %s, which no slot holds alone", domain)
		}
		want := map[int]bool{}
		for _, vm := range f.VMs {
			if vm.Pool == env.Slot.Pool && Domain(vm.Name) == domain {
				want[vm.VMID] = true
			}
		}
		got := map[int]bool{}
		for _, it := range plan.Items {
			vm := f.VMs[it.VMID]
			if vm == nil || vm.Pool != env.Slot.Pool || Domain(vm.Name) != domain || vm.Name != it.Name {
				t.Fatalf("plans %+v, not a VM of %s in %s (%+v)", it, domain, env.Slot.Pool, vm)
			}
			if it.Blocked == "" && vm.Template {
				t.Fatalf("deletes template %s", it.Name)
			}
			got[it.VMID] = true
		}
		if len(got) != len(want) {
			t.Fatalf("planned %v, want %v", got, want)
		}
	})
}
