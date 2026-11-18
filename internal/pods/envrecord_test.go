package pods

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func TestEnvMarkerKeepsTheRestOfTheComment(t *testing.T) {
	for _, c := range []struct{ was, domain, with, without string }{
		{"", "kilo.alpha", "[env kilo.alpha]", ""},
		{"lab slot", "kilo.alpha", "lab slot [env kilo.alpha]", "lab slot"},
		{"lab [env lima.alpha] slot", "kilo.alpha", "lab slot [env kilo.alpha]", "lab [env lima.alpha] slot"},
		{"[env kilo.alpha] lab", "kilo.alpha", "lab [env kilo.alpha]", "lab"},
	} {
		if got := WithEnvMarker(c.was, c.domain); got != c.with || EnvMarker(got) != c.domain {
			t.Errorf("WithEnvMarker(%q, %q) = %q, want %q", c.was, c.domain, got, c.with)
		}
		if got := WithoutEnvMarker(c.was, c.domain); got != c.without {
			t.Errorf("WithoutEnvMarker(%q, %q) = %q, want %q", c.was, c.domain, got, c.without)
		}
	}
	for _, c := range []string{"", "lab", "[env ]", "[env KILO]", "[env kilo]", "[env kilo.alpha"} {
		if d := EnvMarker(c); d != "" {
			t.Errorf("EnvMarker(%q) = %q, want none", c, d)
		}
	}
}

// emptyEnvFake is envFake with every slot pool, dev-02's recording
// kilo.alpha among an admin's notes.
func emptyEnvFake() *podstest.Fake {
	f := envFake()
	f.PoolComments = map[string]string{"dev-01": "", "dev-02": "spare [env kilo.alpha]", "dev-03": ""}
	return f
}

func TestEnvironmentsReadAMarkerOnlySlotAsItsEnvironment(t *testing.T) {
	envs, _, err := NewPlanner(emptyEnvFake(), envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := EnvByDomain(envs, "kilo.alpha")
	if !ok || e.Slot.N != 2 || e.Marker != "kilo.alpha" || e.Free() || !e.Empty() || e.Mixed {
		t.Fatalf("kilo.alpha = %+v (found %v), want the empty environment in dev-02", e, ok)
	}
	if !envs[0].Free() || !envs[2].Free() {
		t.Errorf("unmarked slots aren't free: %+v", envs)
	}
}

// A marker naming another domain than the slot's VMs makes the slot mixed,
// as VMs of two domains do: nothing acts on it by either.
func TestEnvironmentsMarkerAgainstVMsIsMixed(t *testing.T) {
	f := emptyEnvFake()
	f.Add(proxmox.VM{VMID: 2201, Name: "dc.lima.alpha", Node: "alder", Pool: "dev-02"}, map[string]string{"net0": "virtio=BC:24:11:00:22:01,bridge=devint02"})
	envs, _, err := NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if e := envs[1]; !e.Mixed || e.Domain != "" || e.Empty() {
		t.Errorf("dev-02 = %+v, want mixed", e)
	}
	if _, err := NewPlanner(f, envConfig()).EnvTeardown(context.Background(), EnvRequest{Domain: "kilo.alpha"}); err == nil || !strings.Contains(err.Error(), "records kilo.alpha") {
		t.Errorf("teardown of the marker's domain: %v, want refused as mixed", err)
	}
}

// A create of no machines plans one item, recording the domain on the
// first open slot's pool; the marked slot is taken, and its domain in use.
func TestEnvCreateWithoutMachinesRecordsTheDomain(t *testing.T) {
	ctx := context.Background()
	p := NewPlanner(emptyEnvFake(), envConfig())
	plan, err := p.EnvCreate(ctx, EnvRequest{Domain: "lima.alpha"})
	if err != nil {
		t.Fatal(err)
	}
	want := Item{Name: "dev-01", Pool: "dev-01", Record: "lima.alpha", Steps: []Step{StepRecord}}
	if len(plan.Items) != 1 || plan.Items[0].Name != want.Name || plan.Items[0].Pool != want.Pool || plan.Items[0].Record != want.Record ||
		!slices.Equal(plan.Items[0].Steps, want.Steps) || plan.Slot != "01" || !slices.Contains(plan.Locks, "domain:lima.alpha") {
		t.Fatalf("plan = %+v, want %+v", plan, want)
	}
	if _, err := p.EnvCreate(ctx, EnvRequest{Domain: "kilo.alpha"}); err == nil || !strings.Contains(err.Error(), "already in dev slot 02") {
		t.Errorf("create of the recorded domain: %v, want refused", err)
	}
	if _, err := p.EnvCreate(ctx, EnvRequest{Domain: "lima.alpha", Slot: 2}); err == nil || !strings.Contains(err.Error(), "dev slot 02 is in use") {
		t.Errorf("create in the marked slot: %v, want refused", err)
	}
	// Named again in its own slot, it is resumed.
	if plan, err := p.EnvCreate(ctx, EnvRequest{Domain: "kilo.alpha", Slot: 2}); err != nil || len(plan.Items) != 1 || plan.Items[0].Pool != "dev-02" {
		t.Errorf("resumed create = %+v, %v", plan, err)
	}
	if probs := CheckEnv(KindEnvCreate, mustEnvs(t, p), nil, nil, EnvRequest{Domain: "lima.alpha"}); len(probs) != 0 {
		t.Errorf("CheckEnv of a create without machines = %+v", probs)
	}
}

func mustEnvs(t *testing.T, p Planner) []Environment {
	t.Helper()
	envs, _, err := p.Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return envs
}

// Without a router row, the first router-kind host is the router.
func TestEnvCreateWithoutRouterRowTakesARouterHost(t *testing.T) {
	plan, err := NewPlanner(envFake(), envConfig()).EnvCreate(context.Background(), EnvRequest{Domain: "lima.alpha",
		Hosts: []HostSpec{{Host: "teak", Family: "debian-12"}, {Host: "fw", Family: "gw"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 || plan.Items[0].Blocked != "" || plan.Items[1].Blocked != "" || plan.Items[1].NICs["net1"] != "devint01" {
		t.Fatalf("items = %+v", plan.Items)
	}
}

// Hosts added to an empty environment take over from its marker, which the
// add clears last.
func TestEnvAddIntoAnEmptyEnvironment(t *testing.T) {
	plan, err := NewPlanner(emptyEnvFake(), envConfig()).EnvAdd(context.Background(), EnvRequest{Domain: "kilo.alpha",
		Router: HostSpec{Host: "gw", Family: "gw"}, Hosts: []HostSpec{{Host: "teak", Family: "debian-12"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(plan); !slices.Equal(got, []string{"gw.kilo.alpha", "teak.kilo.alpha", "dev-02"}) || plan.Items[0].VMID != 2200 ||
		plan.Items[0].Pool != "dev-02" || plan.Items[1].Blocked != "" || !plan.Items[2].Last || plan.Items[2].Steps[0] != StepClearRecord {
		t.Errorf("items = %+v", plan.Items)
	}
}

// Tearing down an empty environment only clears its record; one with VMs
// too clears it after them.
func TestEnvTeardownClearsTheRecord(t *testing.T) {
	f := emptyEnvFake()
	plan := envTeardown(t, f, EnvRequest{Domain: "kilo.alpha"})
	if len(plan.Items) != 1 || !slices.Equal(plan.Items[0].Steps, []Step{StepClearRecord}) || plan.Items[0].Pool != "dev-02" || plan.Items[0].Record != "kilo.alpha" {
		t.Fatalf("items = %+v", plan.Items)
	}
	f.Add(proxmox.VM{VMID: 2201, Name: "dc.kilo.alpha", Node: "alder", Pool: "dev-02"}, map[string]string{"net0": "virtio=BC:24:11:00:22:01,bridge=devint02"})
	plan = envTeardown(t, f, EnvRequest{Domain: "kilo.alpha"})
	if got := itemNames(plan); !slices.Equal(got, []string{"dc.kilo.alpha", "dev-02"}) || !plan.Items[1].IsRecord() {
		t.Errorf("items = %q", got)
	}
}

func TestRecordNeedsPoolAllocate(t *testing.T) {
	plan := &Plan{Kind: KindEnvCreate, Items: []Item{recordItem(Slot{Pool: "dev-01"}, "kilo.alpha", StepRecord)}}
	_ = BlockUnpermitted(context.Background(), plan, grants{"/pool/dev-01": {"Pool.Audit"}}, envConfig())
	if it := plan.Items[0]; it.Blocked != "you don't have Pool.Allocate on /pool/dev-01" || !it.Unpermitted {
		t.Errorf("item = %+v", it)
	}
}
