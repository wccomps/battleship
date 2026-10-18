package pods

import (
	"testing"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// Each host's address is what its notes record within the subnet; a DNS
// choice resolves to the gateway or the chosen host's address.
func TestEnvHostAddresses(t *testing.T) {
	f := podstest.New("alder")
	add := func(vmid int, name, tags, desc string) {
		f.Add(proxmox.VM{VMID: vmid, Name: name, Node: "alder", Pool: "dev-01", Tags: tags, Status: "running"},
			map[string]string{"description": desc})
	}
	add(2100, "gw.kilo.alpha", "dev;kilo.alpha;router", "battleship: base=gw-v1 dns=router")
	add(2101, "oak.kilo.alpha", "dev;kilo.alpha", "battleship: ip=192.168.100.20 os=windows base=win2022-v1 dns=router")
	add(2102, "elm.kilo.alpha", "dev;kilo.alpha", "battleship: ip=10.50.0.9 os=linux base=debian-12-v1 dns=router")
	env := envOf(t, NewPlanner(f, envConfig()), 1)
	want := map[string]string{"gw.kilo.alpha": "", "oak.kilo.alpha": "192.168.100.20", "elm.kilo.alpha": ""}
	for _, h := range env.Hosts {
		if AddrText(h.Addr) != want[h.Name] {
			t.Errorf("%s: addr %q, want %q", h.Name, h.Addr, want[h.Name])
		}
	}
	for choice, addr := range map[string]string{"": "192.168.100.2", DNSRouter: "192.168.100.2", "oak": "192.168.100.20"} {
		if got, err := env.DNSAddress(choice); err != nil || AddrText(got) != addr {
			t.Errorf("DNSAddress(%q) = %q, %v; want %q", choice, got, err, addr)
		}
	}
	for _, choice := range []string{"elm", "gw", "nobody", "OAK"} {
		if got, err := env.DNSAddress(choice); err == nil {
			t.Errorf("DNSAddress(%q) = %q, want an error", choice, got)
		}
	}
}
