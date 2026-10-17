package pods

import (
	"context"
	"strings"
	"testing"
)

// A VMID another queued job will create isn't free, though no VM has it yet.
func TestRegisterSkipsAReservedVMID(t *testing.T) {
	p := NewPlanner(registerFake("teak", nil, nil), envConfig())
	plan, err := p.ImageRegister(context.Background(), ImageRequest{VMID: 150, Family: "teak", Kind: ImageLinux})
	if err != nil || plan.Items[0].VMID != 8000 {
		t.Fatalf("unreserved: %+v, %v; want 8000", plan, err)
	}
	p.Reserved = func(context.Context) (map[string]bool, error) {
		return map[string]bool{"vmid:8000": true, "read:vmid:8001": true}, nil
	}
	plan, err = p.ImageRegister(context.Background(), ImageRequest{VMID: 150, Family: "teak", Kind: ImageLinux})
	if err != nil || plan.Items[0].VMID != 8001 {
		t.Fatalf("with 8000 reserved: %+v, %v; want 8001", plan, err)
	}
}

// A slot a queued create will fill isn't free; with none left, the error
// says why.
func TestEnvCreateSkipsAReservedSlot(t *testing.T) {
	f := envFake()
	p := NewPlanner(f, envConfig())
	p.Reserved = func(context.Context) (map[string]bool, error) { return map[string]bool{"slot:01": true}, nil }
	plan, err := p.EnvCreate(context.Background(), EnvRequest{Domain: "kilo.bravo", Router: HostSpec{Host: "gw", Family: "gw"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Locks, " "), "slot:02") {
		t.Errorf("locks = %v, want slot 02 (01 is reserved)", plan.Locks)
	}
	p.Reserved = func(context.Context) (map[string]bool, error) {
		return map[string]bool{"slot:01": true, "slot:02": true, "slot:03": true}, nil
	}
	if _, err := p.EnvCreate(context.Background(), EnvRequest{Domain: "kilo.bravo", Router: HostSpec{Host: "gw", Family: "gw"}}); err == nil ||
		!strings.Contains(err.Error(), "a queued job will fill dev slot 01, 02, 03") {
		t.Errorf("err = %v, want every slot taken, two by queued jobs", err)
	}
}

// OpenSlots lists the free slots in order, less those a queued job will
// fill, which it lists apart; a slot with VMs, or with VMs on its bridges,
// is in neither.
func TestOpenSlots(t *testing.T) {
	p := NewPlanner(envFake(), envConfig())
	p.Reserved = func(context.Context) (map[string]bool, error) {
		return map[string]bool{"slot:02": true, "vmid:2300": true}, nil
	}
	envs := []Environment{
		{Slot: Slot{Name: "01"}, Hosts: []EnvHost{{Name: "gw.kilo.alpha"}}},
		{Slot: Slot{Name: "02"}},
		{Slot: Slot{Name: "03"}},
		{Slot: Slot{Name: "04"}, Users: []string{"stray"}},
		{Slot: Slot{Name: "05"}},
	}
	open, queued, err := p.OpenSlots(context.Background(), envs)
	names := func(ss []Slot) (out []string) {
		for _, s := range ss {
			out = append(out, s.Name)
		}
		return out
	}
	if err != nil || strings.Join(names(open), ",") != "03,05" || strings.Join(names(queued), ",") != "02" {
		t.Errorf("open %v, queued %v, %v; want 03,05 and 02", names(open), names(queued), err)
	}
}
