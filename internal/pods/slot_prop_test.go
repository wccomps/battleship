package pods

import (
	"context"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/proxmox"
)

// A new environment takes the first slot with no VM in its VMID block,
// whether or not the user can see those VMs; if there's none, it says so
// and names the slots that only look free.
func TestPropEnvCreateSkipsSlotsWithHiddenVMs(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		f := envFake()
		cfg := envConfig() // slots 1-3
		f.Unseen = map[int]bool{}
		used := map[int]bool{}
		hiddenIn := map[int]bool{}
		for range rapid.IntRange(0, 4).Draw(t, "hidden count") {
			slot := rapid.IntRange(1, 3).Draw(t, "hidden slot")
			off := rapid.SampledFrom([]int{0, 1, 50, 99}).Draw(t, "hidden offset")
			f.Unseen[cfg.Environments.VMIDBase+slot*100+off] = true
			hiddenIn[slot] = true
		}
		if rapid.Bool().Draw(t, "slot 1 visibly used") {
			f.Add(proxmox.VM{VMID: 2100, Name: "gw.other.env", Node: "alder", Pool: "dev-01", Tags: "dev;other.env;router"},
				map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devext01"})
			used[1] = true
		}
		// Just outside every block: holds nothing.
		f.Unseen[cfg.Environments.VMIDBase+99] = true
		f.Unseen[cfg.Environments.VMIDBase+400] = true

		want := 0
		for n := 1; n <= 3; n++ {
			if !used[n] && !hiddenIn[n] {
				want = n
				break
			}
		}
		plan, err := NewPlanner(f, cfg).EnvCreate(context.Background(), EnvRequest{Domain: "kilo.alpha", Router: HostSpec{Host: "gw", Family: "gw"}})
		if want == 0 {
			if err == nil || !strings.Contains(err.Error(), "every dev slot is in use") {
				t.Fatalf("EnvCreate = %v, want every slot in use", err)
			}
			for n := 1; n <= 3; n++ {
				if name := "0" + string(rune('0'+n)); hiddenIn[n] && !used[n] && !strings.Contains(err.Error(), name) {
					t.Fatalf("error %q doesn't name slot %s", err, name)
				}
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := plan.Items[0].VMID; got != cfg.Environments.VMIDBase+want*100 {
			t.Fatalf("router VMID %d, want slot %d", got, want)
		}
	})
}

// A slot that can't be checked isn't taken as free, nor is one whose check
// was stopped.
func TestEnvCreateNeverGuessesASlotIsFree(t *testing.T) {
	f := envFake()
	f.FailOn("vmid-held:2150", &proxmox.APIError{Status: 500, Message: "pveproxy busy"})
	_, err := NewPlanner(f, envConfig()).EnvCreate(context.Background(), EnvRequest{Domain: "kilo.alpha", Router: HostSpec{Host: "gw", Family: "gw"}})
	if err == nil || !strings.Contains(err.Error(), "dev slot 01") {
		t.Errorf("EnvCreate with an unreadable VMID = %v, want an error naming slot 01", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewPlanner(envFake(), envConfig()).EnvCreate(ctx, EnvRequest{Domain: "kilo.alpha", Router: HostSpec{Host: "gw", Family: "gw"}}); err == nil {
		t.Error("EnvCreate with a stopped context planned anyway")
	}
}
