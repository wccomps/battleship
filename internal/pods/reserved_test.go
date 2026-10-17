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
