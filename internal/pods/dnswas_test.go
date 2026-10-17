package pods

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// Each host's address is the gateway for the router, else what its notes
// record within the subnet; a DNS choice resolves to the router's or the
// chosen host's address.
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
	want := map[string]string{"gw.kilo.alpha": "192.168.100.2", "oak.kilo.alpha": "192.168.100.20", "elm.kilo.alpha": ""}
	for _, h := range env.Hosts {
		if h.Addr != want[h.Name] {
			t.Errorf("%s: addr %q, want %q", h.Name, h.Addr, want[h.Name])
		}
	}
	for choice, addr := range map[string]string{"": "192.168.100.2", DNSRouter: "192.168.100.2", "oak": "192.168.100.20"} {
		if got, err := env.DNSAddress(choice); err != nil || got != addr {
			t.Errorf("DNSAddress(%q) = %q, %v; want %q", choice, got, err, addr)
		}
	}
	for _, choice := range []string{"elm", "gw", "nobody", "OAK"} {
		if got, err := env.DNSAddress(choice); err == nil {
			t.Errorf("DNSAddress(%q) = %q, want an error", choice, got)
		}
	}
}

// Set DNS keeps each host's DNS before it on its item: the choice its notes
// recorded, resolved to an address when it names a host that can serve DNS.
// Notes are anyone's to write.
func TestPropEnvDNSKeepsTheDNSBefore(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		f := podstest.New("alder")
		f.Add(proxmox.VM{VMID: 2100, Name: "oak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
			map[string]string{"description": "battleship: ip=192.168.100.20 os=windows base=win2022-v1 dns=router"})
		dns := rapid.SampledFrom([]string{"", "router", "oak", "teak", "gone", "Oak"}).Draw(t, "teak's dns")
		desc := "battleship: ip=192.168.100.10 os=linux base=debian-12-v1"
		if dns != "" {
			desc += " dns=" + dns
		}
		f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
			map[string]string{"description": desc})
		p := NewPlanner(f, envConfig())
		plan, err := p.EnvDNS(context.Background(), EnvRequest{Domain: "kilo.alpha", DNS: "oak"})
		if err != nil {
			t.Fatal(err)
		}
		env := envOf(t, p, 1)
		teak, _ := env.Host("teak.kilo.alpha")
		for _, it := range plan.Items {
			if it.Name != "teak.kilo.alpha" {
				continue
			}
			addr, _ := env.DNSAddress(teak.Notes.DNS)
			if it.DNSWas == nil || it.DNSWas.Choice != teak.Notes.DNS || it.DNSWas.Addr != addr || it.Notes.DNS != "oak" {
				t.Fatalf("teak (dns %q) was %+v, notes %+v", dns, it.DNSWas, it.Notes)
			}
			want := map[string]string{"": "192.168.100.2", "router": "192.168.100.2", "oak": "192.168.100.20", "teak": "192.168.100.10"}[teak.Notes.DNS]
			if it.DNSWas.Addr != want {
				t.Fatalf("teak (dns %q) was at %q, want %q", dns, it.DNSWas.Addr, want)
			}
			return
		}
		t.Fatal("no item for teak")
	})
}
