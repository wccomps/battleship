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

// A chosen slot is where the environment goes, though a lower one is free.
func TestEnvCreateInAChosenSlot(t *testing.T) {
	plan, err := NewPlanner(envFake(), envConfig()).EnvCreate(context.Background(),
		EnvRequest{Domain: "kilo.alpha", Slot: 3, Router: HostSpec{Host: "gw", Family: "gw"}, Hosts: []HostSpec{{Host: "teak", Family: "debian-12"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plan.Items {
		if it.Pool != "dev-03" || it.VMID/100 != 23 || it.Blocked != "" {
			t.Errorf("item %+v, want it in dev-03", it)
		}
	}
	if !slices.Contains(plan.Locks, "slot:03") {
		t.Errorf("locks = %v, want slot:03", plan.Locks)
	}
}

// A chosen slot that isn't free is refused, saying why.
func TestEnvCreateRefusesAChosenSlotThatIsNotFree(t *testing.T) {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.bravo", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.bravo;router"}, nil)
	f.Unseen = map[int]bool{2350: true}
	p := NewPlanner(f, envConfig())
	p.Reserved = func(context.Context) (map[string]bool, error) { return map[string]bool{"slot:02": true}, nil }
	for slot, want := range map[int]string{
		1: "dev slot 01 is in use", 2: "a queued job will fill dev slot 02", 3: "dev slot 03 holds VMs you can't see", 4: "there's no dev slot 04",
	} {
		_, err := p.EnvCreate(context.Background(), EnvRequest{Domain: "kilo.alpha", Slot: slot, Router: HostSpec{Host: "gw", Family: "gw"}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("slot %d: err = %v, want %q", slot, err, want)
		}
	}
}

// A create that stopped partway is resumed in its own slot: what exists is
// picked up, the rest is cloned. Another slot, or none, is refused.
func TestEnvCreateResumesInItsOwnSlot(t *testing.T) {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2200, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-02", Tags: "dev;kilo.alpha;router"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	p := NewPlanner(f, envConfig())
	req := EnvRequest{Domain: "kilo.alpha", Slot: 2, Router: HostSpec{Host: "gw", Family: "gw"}, Hosts: []HostSpec{{Host: "teak", Family: "debian-12"}}}
	plan, err := p.EnvCreate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	gw, teak := plan.Items[0], plan.Items[1]
	if gw.VMID != 2200 || gw.Blocked != "" || slices.Contains(gw.Steps, StepClone) {
		t.Errorf("router = %+v, want it resumed", gw)
	}
	if teak.Pool != "dev-02" || teak.VMID == 2200 || teak.VMID/100 != 22 || !slices.Contains(teak.Steps, StepClone) {
		t.Errorf("teak = %+v, want it cloned into dev-02", teak)
	}
	for _, slot := range []int{0, 1} {
		req.Slot = slot
		if _, err := p.EnvCreate(context.Background(), req); err == nil || !strings.Contains(err.Error(), "already in dev slot 02") {
			t.Errorf("slot %d: err = %v, want kilo.alpha already in 02", slot, err)
		}
	}
}

// Whatever each slot holds, a chosen slot is taken just when it is free
// (no VMs seen or unseen, no queued job) or holds the domain already, and
// is refused naming it otherwise.
func TestPropEnvCreateChosenSlot(t *testing.T) {
	states := []string{"free", "other", "queued", "hidden", "ours"}
	rapid.Check(t, func(t *rapid.T) {
		f := envFake()
		f.Unseen = map[int]bool{}
		reserved := map[string]bool{}
		state := map[int]string{}
		ours := false
		for n := 1; n <= 3; n++ {
			s := rapid.SampledFrom(states).Draw(t, fmt.Sprintf("slot %d", n))
			if s == "ours" && ours {
				s = "free" // a domain is in one slot
			}
			state[n] = s
			base := 2000 + n*100
			switch s {
			case "other":
				f.Add(proxmox.VM{VMID: base, Name: "gw.kilo.bravo", Node: "alder", Pool: fmt.Sprintf("dev-%02d", n), Tags: "dev;kilo.bravo;router"}, nil)
			case "queued":
				reserved[fmt.Sprintf("slot:%02d", n)] = true
			case "hidden":
				f.Unseen[base+rapid.IntRange(0, 99).Draw(t, "unseen")] = true
			case "ours":
				ours = true
				f.Add(proxmox.VM{VMID: base, Name: "gw.kilo.alpha", Node: "alder", Pool: fmt.Sprintf("dev-%02d", n), Tags: "dev;kilo.alpha;router"},
					map[string]string{"description": "battleship: base=gw-v1 dns=router"})
			}
		}
		p := NewPlanner(f, envConfig())
		p.Reserved = func(context.Context) (map[string]bool, error) { return reserved, nil }
		chosen := rapid.IntRange(1, 4).Draw(t, "chosen")
		plan, err := p.EnvCreate(context.Background(), EnvRequest{Domain: "kilo.alpha", Slot: chosen,
			Router: HostSpec{Host: "gw", Family: "gw"}, Hosts: []HostSpec{{Host: "teak", Family: "debian-12"}}})
		name := fmt.Sprintf("%02d", chosen)
		want := state[chosen] == "free" && !ours || state[chosen] == "ours"
		if !want {
			if err == nil {
				t.Fatalf("slot %s (%s) was taken; states %v", name, state[chosen], state)
			}
			if !strings.Contains(err.Error(), name) && !strings.Contains(err.Error(), "already in dev slot") {
				t.Fatalf("refusal %q doesn't name slot %s", err, name)
			}
			return
		}
		if err != nil {
			t.Fatalf("slot %s (%s): %v; states %v", name, state[chosen], err, state)
		}
		for _, it := range plan.Items {
			if it.Pool != "dev-"+name || it.VMID/100 != 20+chosen {
				t.Fatalf("item %+v, want it in dev-%s", it, name)
			}
		}
		if !slices.Contains(plan.Locks, "slot:"+name) {
			t.Fatalf("locks %v lack slot:%s", plan.Locks, name)
		}
	})
}
