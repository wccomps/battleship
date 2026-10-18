package pods

import (
	"context"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/proxmox"
)

// Every plan that writes a VM's notes holds them to what Proxmox keeps,
// counted in characters: notes of 4100 two-byte characters (8200 bytes)
// are written; notes that come to more than 8192 characters once written
// block the item. Notes it leaves as they are, however long, don't.
func TestEveryNotesWriteIsBoundAtPlan(t *testing.T) {
	ctx := context.Background()
	fits := strings.Repeat("é", 4100)
	long := strings.Repeat("x", maxDescription-5)
	dnsHosts := func(desc string) *Planner {
		f := envFake()
		f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router", Status: "running"},
			map[string]string{"description": "battleship: base=gw-v1 dns=router"})
		f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
			map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:21:01,bridge=devint01",
				"description": desc + "\nbattleship: ip=192.168.100.77 os=linux base=debian-12-v1"})
		p := NewPlanner(f, envConfig())
		return &p
	}
	for name, plan := range map[string]func(desc string) (Item, error){
		"register": func(desc string) (Item, error) {
			f := imageFake()
			f.VMs[150].Config["description"] = desc
			plan, err := NewPlanner(f, envConfig()).ImageRegister(ctx, ImageRequest{VMID: 150, Family: "debian-12",
				Details: &ImageNotes{ImageDetails: ImageDetails{OS: "Debian 12"}}})
			if err != nil {
				return Item{}, err
			}
			return plan.Items[0], nil
		},
		"edit copy": func(desc string) (Item, error) {
			f := imageFake()
			f.VMs[8000].Config["description"] = desc
			plan, err := NewPlanner(f, envConfig()).ImageEdit(ctx, ImageRequest{Family: "debian-12"})
			if err != nil {
				return Item{}, err
			}
			return plan.Items[0], nil
		},
		"edit notes": func(desc string) (Item, error) {
			f := imageFake()
			f.VMs[8000].Config["description"] = desc
			plan, err := NewPlanner(f, envConfig()).ImageSetDetails(ctx, ImageRequest{Family: "debian-12",
				Details: &ImageNotes{ImageDetails: ImageDetails{OS: "Debian 13"}}})
			if err != nil {
				return Item{}, err
			}
			return plan.Items[0], nil
		},
		"environment host": func(desc string) (Item, error) {
			f := envFake()
			f.VMs[8001].Config["description"] = desc
			hs, _ := ParseHostSpec("teak=debian-12")
			plan, err := NewPlanner(f, envConfig()).EnvCreate(ctx, EnvRequest{Domain: "kilo.alpha", Router: HostSpec{Host: "gw", Family: "gw"},
				Hosts: []HostSpec{hs}})
			if err != nil {
				return Item{}, err
			}
			return plan.Items[1], nil
		},
		"resumed host": func(desc string) (Item, error) {
			hs, _ := ParseHostSpec("teak=debian-12")
			plan, err := dnsHosts(desc).EnvAdd(ctx, EnvRequest{Domain: "kilo.alpha", Hosts: []HostSpec{hs}})
			if err != nil {
				return Item{}, err
			}
			return plan.Items[0], nil
		},
		"set DNS": func(desc string) (Item, error) {
			plan, err := dnsHosts(desc).EnvDNS(ctx, EnvRequest{Domain: "kilo.alpha", DNS: "teak"})
			if err != nil {
				return Item{}, err
			}
			return plan.Items[1], nil
		},
	} {
		it, err := plan(fits)
		if err != nil || it.Blocked != "" {
			t.Errorf("%s: notes of %d characters: %v, blocked %q", name, len([]rune(fits)), err, it.Blocked)
		}
		it, err = plan(long)
		if err != nil || !strings.Contains(it.Blocked, "characters; Proxmox keeps at most 8192") {
			t.Errorf("%s: notes of %d characters: %v, blocked %q", name, len(long), err, it.Blocked)
		}
	}
	it := Item{Name: "teak.kilo.alpha", Notes: &Notes{IP: "192.168.100.77"}, Steps: []Step{StepLabel}}
	if err := it.blockLongNotes(long + "\nbattleship: ip=192.168.100.77"); err != nil || it.Blocked != "" {
		t.Errorf("notes left as they are: %v, blocked %q", err, it.Blocked)
	}
}
