package pods

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

const (
	wanMAC = "bc:24:11:00:21:00"
	lanMAC = "bc:24:11:00:21:01"
)

func TestGuestAddr(t *testing.T) {
	nic := func(mac string, ips ...string) proxmox.GuestNIC {
		return proxmox.GuestNIC{Name: "x", MAC: mac, IPs: ips}
	}
	for _, c := range []struct {
		name string
		nics []proxmox.GuestNIC
		mac  string
		want string
	}{
		{"by MAC, not name", []proxmox.GuestNIC{
			{Name: "wan", MAC: lanMAC, IPs: []string{"192.168.100.2/24"}},
			{Name: "lan", MAC: wanMAC, IPs: []string{"10.50.12.133/24"}},
		}, wanMAC, "10.50.12.133"},
		{"MAC in any case", []proxmox.GuestNIC{nic(lanMAC, "192.168.100.2/24")}, strings.ToUpper(lanMAC), "192.168.100.2"},
		{"IPv4 over an earlier IPv6", []proxmox.GuestNIC{nic(wanMAC, "2001:db8::5/64", "10.50.12.133/24")}, wanMAC, "10.50.12.133"},
		{"IPv6 when there is no IPv4", []proxmox.GuestNIC{nic(wanMAC, "fe80::1/64", "2001:DB8::5/64")}, wanMAC, "2001:db8::5"},
		{"no link-local, loopback or unspecified", []proxmox.GuestNIC{nic(wanMAC, "169.254.3.4/16", "127.0.0.1/8", "::1/128", "0.0.0.0/0", "fe80::1/64", "224.0.0.1/4", "255.255.255.255/32")}, wanMAC, ""},
		{"a zone is no host's address", []proxmox.GuestNIC{nic(wanMAC, "2001:db8::5%eth0/64")}, wanMAC, ""},
		{"IPv4 written as IPv6", []proxmox.GuestNIC{nic(wanMAC, "::ffff:10.50.12.133/120")}, wanMAC, "10.50.12.133"},
		{"not an address", []proxmox.GuestNIC{nic(wanMAC, "<b>10.50.12.133</b>", "10.50.12.133 ", "010.050.012.133/24", "")}, wanMAC, ""},
		{"no NIC with the MAC", []proxmox.GuestNIC{nic(lanMAC, "192.168.100.2/24")}, wanMAC, ""},
		{"no MAC to look for", []proxmox.GuestNIC{nic("", "192.168.100.2/24")}, "", ""},
		{"a later NIC with the MAC", []proxmox.GuestNIC{nic(wanMAC), nic(wanMAC, "10.50.12.133/24")}, wanMAC, "10.50.12.133"},
	} {
		if got := GuestAddr(c.nics, c.mac); got != c.want {
			t.Errorf("%s: GuestAddr = %q, want %q", c.name, got, c.want)
		}
	}
}

// Only so much of an agent's answer is looked at.
func TestGuestAddrBounded(t *testing.T) {
	nics := make([]proxmox.GuestNIC, maxGuestNICs+1)
	nics[maxGuestNICs] = proxmox.GuestNIC{MAC: wanMAC, IPs: []string{"10.50.12.133/24"}}
	if got := GuestAddr(nics, wanMAC); got != "" {
		t.Errorf("a NIC past the first %d gave %q", maxGuestNICs, got)
	}
	ips := make([]string, maxGuestIPs+1)
	ips[maxGuestIPs] = "10.50.12.133/24"
	if got := GuestAddr([]proxmox.GuestNIC{{MAC: wanMAC, IPs: ips}}, wanMAC); got != "" {
		t.Errorf("an address past the first %d gave %q", maxGuestIPs, got)
	}
}

func TestExternalAddr(t *testing.T) {
	for _, c := range []struct {
		wan   string
		octet int
		want  string
	}{
		{"10.50.12.133", 10, "10.50.12.10"},
		{"10.50.12.133", 254, "10.50.12.254"},
		{"10.50.12.133", 1, "10.50.12.1"},
		{"10.50.12.133", 0, ""},
		{"10.50.12.133", 255, ""},
		{"10.50.12.133", -3, ""},
		{"2001:db8::5", 10, ""},
		{"", 10, ""},
		{"10.50.12", 10, ""},
	} {
		if got := ExternalAddr(c.wan, c.octet); got != c.want {
			t.Errorf("ExternalAddr(%q, %d) = %q, want %q", c.wan, c.octet, got, c.want)
		}
	}
}

// The external address is the WAN address's /24 with the octet, for any
// IPv4 WAN address and host octet, and nothing else.
func TestPropExternalAddr(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		b := [4]byte{}
		for i := range b {
			b[i] = rapid.Byte().Draw(t, "byte")
		}
		octet := rapid.IntRange(-300, 600).Draw(t, "octet")
		got := ExternalAddr(netip.AddrFrom4(b).String(), octet)
		if octet < 1 || octet > 254 {
			if got != "" {
				t.Fatalf("octet %d gave %q", octet, got)
			}
			return
		}
		want := netip.AddrFrom4([4]byte{b[0], b[1], b[2], byte(octet)}).String()
		if got != want {
			t.Fatalf("ExternalAddr(%v, %d) = %q, want %q", b, octet, got, want)
		}
		junk := rapid.String().Draw(t, "wan")
		if a, err := netip.ParseAddr(junk); err != nil || !a.Is4() {
			if got := ExternalAddr(junk, octet); got != "" {
				t.Fatalf("ExternalAddr(%q) = %q", junk, got)
			}
		}
	})
}

// guestIP draws what an agent might report as an address: real ones of
// every kind, and anything a guest could write.
var guestIP = rapid.OneOf(
	rapid.SampledFrom([]string{"10.50.12.133/24", "192.168.100.7/24", "127.0.0.1/8", "169.254.1.1/16", "fe80::1/64",
		"2001:db8::5/64", "::1/128", "0.0.0.0/0", "::ffff:10.50.12.9/120", "2001:db8::5%eth0/64", "224.0.0.1/4",
		"<script>alert(1)</script>", "10.50.12.133‮/24", "10.50.12.133​", ""}),
	rapid.Map(rapid.SliceOfN(rapid.Byte(), 4, 4), func(b []byte) string { return netip.AddrFrom4([4]byte(b)).String() + "/24" }),
	rapid.Map(rapid.SliceOfN(rapid.Byte(), 16, 16), func(b []byte) string { return netip.AddrFrom16([16]byte(b)).String() + "/64" }),
	rapid.String(),
)

// From any agent answer, the address picked is one the NIC with the MAC
// reports, within the bounds, in canonical form and a host's own: the
// first IPv4 one, else the first IPv6 one.
func TestPropGuestAddr(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		macs := rapid.SampledFrom([]string{wanMAC, lanMAC, strings.ToUpper(wanMAC), "", "<b>"})
		nics := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) proxmox.GuestNIC {
			return proxmox.GuestNIC{Name: rapid.String().Draw(t, "name"), MAC: macs.Draw(t, "mac"), IPs: rapid.SliceOfN(guestIP, 0, 5).Draw(t, "ips")}
		}), 0, 6).Draw(t, "nics")
		mac := macs.Draw(t, "want")
		got := GuestAddr(nics, mac)
		var v4, v6 []string
		for _, n := range nics {
			if mac == "" || !strings.EqualFold(n.MAC, mac) {
				continue
			}
			for _, ip := range n.IPs {
				if a, ok := hostAddr(ip); ok && a.Is4() {
					v4 = append(v4, a.String())
				} else if ok {
					v6 = append(v6, a.String())
				}
			}
		}
		want := ""
		switch {
		case len(v4) > 0:
			want = v4[0]
		case len(v6) > 0:
			want = v6[0]
		}
		if got != want {
			t.Fatalf("GuestAddr = %q, want %q", got, want)
		}
		if got == "" {
			return
		}
		a, err := netip.ParseAddr(got)
		if err != nil || a.String() != got || a.Zone() != "" || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
			t.Fatalf("GuestAddr = %q: not a host's address in canonical form", got)
		}
	})
}

// kiloAlpha is kilo.alpha in dev-01: a router whose WAN is net0 and LAN
// net1, and hosts on its LAN.
func kiloAlpha(hosts ...EnvHost) Environment {
	e := Environment{Slot: Slots(envConfig().Environments)[0], Domain: "kilo.alpha", Gateway: "192.168.100.2"}
	e.Hosts = append([]EnvHost{{Name: "gw.kilo.alpha", VMID: 2100, Router: true, Addr: "192.168.100.2", WANMAC: wanMAC, LANMAC: lanMAC}}, hosts...)
	return e
}

func TestEnvironmentAddrs(t *testing.T) {
	host := func(vmid int, addr string) EnvHost {
		return EnvHost{Name: fmt.Sprint(vmid), VMID: vmid, Addr: addr, LANMAC: fmt.Sprintf("bc:24:11:00:00:%02d", vmid%100)}
	}
	answer := func(vmid int, ips ...string) []proxmox.GuestNIC {
		return []proxmox.GuestNIC{{Name: "lo", MAC: "00:00:00:00:00:00", IPs: []string{"127.0.0.1/8"}},
			{Name: "eth0", MAC: fmt.Sprintf("BC:24:11:00:00:%02d", vmid%100), IPs: ips}}
	}
	router := []proxmox.GuestNIC{
		{Name: "vtnet1", MAC: lanMAC, IPs: []string{"fe80::2/64", "192.168.100.2/24"}},
		{Name: "vtnet0", MAC: wanMAC, IPs: []string{"fe80::1/64", "10.50.12.133/24"}},
	}
	e := kiloAlpha(host(2110, "192.168.100.10"), host(2111, "192.168.100.11"), host(2112, "192.168.100.12"),
		host(2113, "192.168.100.13"), host(2114, ""), host(2115, "192.168.100.15"))
	nics := map[int][]proxmox.GuestNIC{
		2100: router,
		2110: answer(2110, "192.168.100.10/24"),            // as assigned
		2111: answer(2111, "192.168.100.21/24"),            // moved
		2113: answer(2113, "10.50.99.4/24"),                // off the LAN
		2114: answer(2114, "192.168.100.30/24"),            // none assigned
		2115: answer(2115, "fe80::9/64", "2001:db8::9/64"), // no IPv4
	}
	got := e.Addrs(nics)
	for vmid, want := range map[int]LiveAddrs{
		2100: {Live: true, LAN: "192.168.100.2", WAN: "10.50.12.133"},
		2110: {Live: true, LAN: "192.168.100.10", External: "10.50.12.10"},
		2111: {Live: true, LAN: "192.168.100.21", Differs: true, External: "10.50.12.21"},
		2112: {External: "10.50.12.12"},
		2113: {Live: true, LAN: "10.50.99.4", Differs: true},
		2114: {Live: true, LAN: "192.168.100.30", Differs: true, External: "10.50.12.30"},
		2115: {Live: true, LAN: "2001:db8::9", Differs: true, External: "10.50.12.15"},
	} {
		if got[vmid] != want {
			t.Errorf("%d: %+v, want %+v", vmid, got[vmid], want)
		}
	}

	// Without the router's WAN address, no host has an external one.
	delete(nics, 2100)
	for vmid, a := range e.Addrs(nics) {
		if a.External != "" || a.WAN != "" {
			t.Errorf("%d: %+v without the router's answer", vmid, a)
		}
	}
	// A router reporting no WAN address is live, but gives none.
	nics[2100] = router[:1]
	if a := e.Addrs(nics)[2100]; !a.Live || a.WAN != "" || a.LAN != "192.168.100.2" || a.Differs {
		t.Errorf("router without WAN: %+v", a)
	}
}

func TestEnvironmentsReadsNICs(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:21:00,bridge=devext01", "net1": "virtio=BC:24:11:00:21:01,bridge=devint01", "agent": "1"})
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha"},
		map[string]string{"net0": "e1000=BC:24:11:00:21:10,bridge=devint01", "agent": "enabled=0"})
	envs, _, err := NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gw, _ := envs[0].Host("gw.kilo.alpha")
	teak, _ := envs[0].Host("teak.kilo.alpha")
	if gw.WANMAC != wanMAC || gw.LANMAC != lanMAC || !gw.Agent {
		t.Errorf("router: WAN %q LAN %q agent %v", gw.WANMAC, gw.LANMAC, gw.Agent)
	}
	if teak.WANMAC != "" || teak.LANMAC != "bc:24:11:00:21:10" || teak.Agent {
		t.Errorf("host: WAN %q LAN %q agent %v", teak.WANMAC, teak.LANMAC, teak.Agent)
	}
}

// GuestNICs asks only running VMs with an agent enabled, a few at a time,
// and leaves out one that doesn't answer in time without waiting for
// Proxmox's own timeout.
func TestGuestNICs(t *testing.T) {
	f := podstest.New("alder")
	var hosts []EnvHost
	for i := range 12 {
		vmid := 2110 + i
		mac := fmt.Sprintf("bc:24:11:00:00:%02d", i)
		f.Add(proxmox.VM{VMID: vmid, Name: fmt.Sprint(vmid), Node: "alder", Status: "running"}, nil)
		f.SetGuest(vmid, podstest.Guest{Up: true, NICs: []proxmox.GuestNIC{{MAC: mac, IPs: []string{fmt.Sprintf("192.168.100.%d/24", 10+i)}}}})
		hosts = append(hosts, EnvHost{VMID: vmid, Node: "alder", Status: "running", Agent: true, LANMAC: mac})
	}
	hosts[0].Status = "stopped"
	hosts[1].Agent = false
	hosts[2].LANMAC = ""
	const hung = 2113
	var mu sync.Mutex
	inFlight, most := 0, 0
	f.Gate = func(ctx context.Context, key string) (func(), error) {
		if !strings.HasPrefix(key, "agent-nics:") {
			return nil, nil
		}
		mu.Lock()
		inFlight++
		most = max(most, inFlight)
		mu.Unlock()
		done := func() { mu.Lock(); inFlight--; mu.Unlock() }
		if key == fmt.Sprintf("agent-nics:%d", hung) {
			<-ctx.Done()
			return done, ctx.Err()
		}
		time.Sleep(5 * time.Millisecond) // long enough for reads to overlap
		return done, nil
	}
	envs := []Environment{{Hosts: hosts[:6]}, {Hosts: hosts[6:]}}
	start := time.Now()
	got := NewPlanner(f, envConfig()).GuestNICs(context.Background(), envs, 100*time.Millisecond, time.Minute)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v with one agent hung", took)
	}
	for _, h := range hosts {
		_, read := got[h.VMID]
		if want := h.VMID > 2112 && h.VMID != hung; read != want {
			t.Errorf("%d read %v, want %v", h.VMID, read, want)
		}
	}
	if n := f.Called("agent-nics:2110") + f.Called("agent-nics:2111") + f.Called("agent-nics:2112"); n != 0 {
		t.Errorf("%d reads of VMs whose agent can't answer", n)
	}
	if most > AddrReads || most < 2 {
		t.Errorf("%d reads at once, want 2 to %d", most, AddrReads)
	}
	if got[2120][0].IPs[0] != "192.168.100.20/24" {
		t.Errorf("2120's NICs: %+v", got[2120])
	}

	// All the reads are bounded together too.
	start = time.Now()
	f.Gate = func(ctx context.Context, key string) (func(), error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if got := NewPlanner(f, envConfig()).GuestNICs(context.Background(), envs, time.Minute, 100*time.Millisecond); len(got) != 0 {
		t.Errorf("read %v from hung agents", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v with every agent hung", took)
	}
}
