package pods

import (
	"context"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/proxmox"
)

// A slot's name reads back as its number, and nothing else reads as one:
// typed text such as "2", "002" or " 02" is no slot's name.
func TestPropParseSlotName(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 1000).Draw(t, "n")
		if got, ok := ParseSlotName(SlotName(n)); !ok || got != n {
			t.Fatalf("ParseSlotName(%q) = %d, %v", SlotName(n), got, ok)
		}
		s := rapid.OneOf(rapid.String(), rapid.StringMatching(`[ +-]?0*[0-9]{1,4} ?`)).Draw(t, "s")
		if got, ok := ParseSlotName(s); ok && (got < 1 || SlotName(got) != s) {
			t.Fatalf("ParseSlotName(%q) = %d, true", s, got)
		}
	})
}

// Every environment plan names its slot, and locks it.
func TestEnvPlansCarryTheirSlot(t *testing.T) {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2200, Name: "gw.kilo.bravo", Node: "alder", Pool: "dev-02", Tags: "dev;kilo.bravo;router", Status: "running"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	p := NewPlanner(f, envConfig())
	ctx := context.Background()
	plans := map[string]func() (*Plan, error){
		"create": func() (*Plan, error) {
			return p.EnvCreate(ctx, EnvRequest{Domain: "kilo.alpha", Slot: 3, Router: HostSpec{Host: "gw", Family: "gw"}})
		},
		"add": func() (*Plan, error) {
			return p.EnvAdd(ctx, EnvRequest{Domain: "kilo.bravo", Hosts: []HostSpec{{Host: "dc", Family: "debian-12"}}})
		},
		"dns":      func() (*Plan, error) { return p.EnvDNS(ctx, EnvRequest{Domain: "kilo.bravo"}) },
		"teardown": func() (*Plan, error) { return p.EnvTeardown(ctx, EnvRequest{Domain: "kilo.bravo"}) },
	}
	want := map[string]string{"create": "03", "add": "02", "dns": "02", "teardown": "02"}
	for name, plan := range plans {
		got, err := plan()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Slot != want[name] || !slices.Contains(got.Locks, "slot:"+want[name]) {
			t.Errorf("%s: slot %q, locks %q; want %s", name, got.Slot, got.Locks, want[name])
		}
		if got.Slot = ""; got.DevSlot() != want[name] {
			t.Errorf("%s: without Slot, DevSlot = %q", name, got.DevSlot())
		}
	}
}

// A plan stored before Plan.Slot names its slot only in its lock.
func TestPlanDevSlot(t *testing.T) {
	for _, c := range []struct {
		plan Plan
		want string
	}{
		{Plan{Slot: "03", Locks: []string{"slot:03"}}, "03"},
		{Plan{Locks: []string{"domain:kilo.alpha", "slot:07"}}, "07"},
		{Plan{Locks: []string{"slot:7", "family:debian-12"}}, ""},
		{Plan{}, ""},
	} {
		if got := c.plan.DevSlot(); got != c.want {
			t.Errorf("%+v.DevSlot() = %q, want %q", c.plan, got, c.want)
		}
	}
}
