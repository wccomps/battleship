package pods

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
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
		return proxmox.GuestNIC{Name: "x", MAC: mac, IPs: podstest.IPs(ips...)}
	}
	for _, c := range []struct {
		name string
		nics []proxmox.GuestNIC
		mac  string
		want string // address/prefix, the prefix -1 without a valid one
	}{
		{"by MAC, not name", []proxmox.GuestNIC{
			{Name: "wan", MAC: lanMAC, IPs: podstest.IPs("192.168.100.2/24")},
			{Name: "lan", MAC: wanMAC, IPs: podstest.IPs("10.50.12.133/24")},
		}, wanMAC, "10.50.12.133/24"},
		{"MAC in any case", []proxmox.GuestNIC{nic(lanMAC, "192.168.100.2/24")}, strings.ToUpper(lanMAC), "192.168.100.2/24"},
		{"IPv4 over an earlier IPv6", []proxmox.GuestNIC{nic(wanMAC, "2001:db8::5/64", "10.50.12.133/16")}, wanMAC, "10.50.12.133/16"},
		{"IPv6 when there is no IPv4", []proxmox.GuestNIC{nic(wanMAC, "fe80::1/64", "2001:DB8::5/64")}, wanMAC, "2001:db8::5/64"},
		{"no link-local, loopback or unspecified", []proxmox.GuestNIC{nic(wanMAC, "169.254.3.4/16", "127.0.0.1/8", "::1/128", "0.0.0.0/0", "fe80::1/64", "224.0.0.1/4", "255.255.255.255/32")}, wanMAC, ""},
		{"no prefix", []proxmox.GuestNIC{nic(wanMAC, "10.50.12.133")}, wanMAC, "10.50.12.133/-1"},
		{"no NIC with the MAC", []proxmox.GuestNIC{nic(lanMAC, "192.168.100.2/24")}, wanMAC, ""},
		{"no MAC to look for", []proxmox.GuestNIC{nic("", "192.168.100.2/24")}, "", ""},
		{"a later NIC with the MAC", []proxmox.GuestNIC{nic(wanMAC), nic(wanMAC, "10.50.12.133/24")}, wanMAC, "10.50.12.133/24"},
	} {
		got := guestAddr(c.nics, c.mac)
		s := ""
		if got.Addr().IsValid() {
			s = got.Addr().String() + "/" + strconv.Itoa(got.Bits())
		}
		if s != c.want {
			t.Errorf("%s: guestAddr = %q, want %q", c.name, s, c.want)
		}
	}
}

func TestExternalAddr(t *testing.T) {
	for _, c := range []struct {
		wan   string
		octet int
		want  string
	}{
		{"10.50.12.133/24", 10, "10.50.12.10"},
		{"10.50.12.133/24", 254, "10.50.12.254"},
		{"10.50.12.133/24", 1, "10.50.12.1"},
		{"10.50.12.133/24", 0, ""},
		{"10.50.12.133/24", 255, ""},
		{"10.50.12.133/24", -3, ""},
		{"10.50.12.133/16", 10, ""},
		{"10.50.12.133/25", 10, ""},
		{"10.50.12.133/32", 10, ""},
		{"2001:db8::5/24", 10, ""},
	} {
		if got := externalAddr(netip.MustParsePrefix(c.wan), c.octet); got != c.want {
			t.Errorf("externalAddr(%q, %d) = %q, want %q", c.wan, c.octet, got, c.want)
		}
	}
	if got := externalAddr(netip.PrefixFrom(netip.MustParseAddr("10.50.12.133"), -1), 10); got != "" {
		t.Errorf("externalAddr without a prefix = %q", got)
	}
	if got := externalAddr(netip.Prefix{}, 10); got != "" {
		t.Errorf("externalAddr without a WAN address = %q", got)
	}
}

// The router image's NAT puts host .N at the WAN /24's .N: for an IPv4 WAN
// address with a /24 prefix and a host octet, its first three octets and
// the octet; for any other prefix or octet, nothing.
func TestPropExternalAddr(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		b := [4]byte{}
		for i := range b {
			b[i] = rapid.Byte().Draw(t, "byte")
		}
		bits := rapid.IntRange(-1, 33).Draw(t, "bits")
		octet := rapid.IntRange(-300, 600).Draw(t, "octet")
		got := externalAddr(netip.PrefixFrom(netip.AddrFrom4(b), bits), octet)
		want := ""
		if bits == 24 && octet >= 1 && octet <= 254 {
			want = fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], octet)
		}
		if got != want {
			t.Fatalf("externalAddr(%v/%d, %d) = %q, want %q", b, bits, octet, got, want)
		}
		v6 := [16]byte{0x20, 0x01, 0x0d, 0xb8}
		if got := externalAddr(netip.PrefixFrom(netip.AddrFrom16(v6), bits), octet); got != "" {
			t.Fatalf("an IPv6 WAN address gave %q", got)
		}
	})
}

// reported is an address as GuestNIC.IPs might hold it and, from the rule,
// what guestAddr reads in it: whether it is a host's own, IPv4, with its
// prefix length (-1 if none valid).
type reported struct {
	ip   netip.Prefix
	host bool
	v4   bool
	want string // address/bits
}

var reports = rapid.OneOf(
	// IPv4, with any prefix or none.
	rapid.Custom(func(t *rapid.T) reported {
		var b [4]byte
		for i := range b {
			b[i] = rapid.Byte().Draw(t, "byte")
		}
		a := netip.AddrFrom4(b)
		host := !a.IsLoopback() && !a.IsLinkLocalUnicast() && !a.IsMulticast() && !a.IsUnspecified() && b != [4]byte{255, 255, 255, 255}
		p := rapid.IntRange(-1, 40).Draw(t, "prefix")
		bits := p
		if p > 32 {
			bits = -1
		}
		return reported{ip: netip.PrefixFrom(a, p), host: host, v4: true, want: a.String() + "/" + strconv.Itoa(bits)}
	}),
	// A global IPv6 address.
	rapid.Custom(func(t *rapid.T) reported {
		b := [16]byte{0x20, 0x01, 0x0d, 0xb8}
		for i := 4; i < 16; i++ {
			b[i] = rapid.Byte().Draw(t, "byte")
		}
		a := netip.AddrFrom16(b)
		p := rapid.IntRange(-1, 130).Draw(t, "prefix")
		bits := p
		if p > 128 {
			bits = -1
		}
		return reported{ip: netip.PrefixFrom(a, p), host: true, want: a.String() + "/" + strconv.Itoa(bits)}
	}),
	// Addresses that aren't a host's own.
	rapid.Map(rapid.SampledFrom([]string{"127.0.0.1/8", "169.254.1.1/16", "fe80::1/64", "::1/128", "0.0.0.0/0",
		"224.0.0.1/4", "ff02::1/64", "255.255.255.255/32", "::/0"}), func(s string) reported {
		return reported{ip: netip.MustParsePrefix(s)}
	}),
)

// From any agent answer, the address read is that of the first IPv4 host
// address the NIC with the MAC reports, else of its first IPv6 one, with its
// prefix length.
func TestPropGuestAddr(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		macs := rapid.SampledFrom([]string{wanMAC, lanMAC, strings.ToUpper(wanMAC), "", "<b>"})
		type nic struct {
			mac string
			ips []reported
		}
		nics := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) nic {
			return nic{mac: macs.Draw(t, "mac"), ips: rapid.SliceOfN(reports, 0, 5).Draw(t, "ips")}
		}), 0, 6).Draw(t, "nics")
		mac := macs.Draw(t, "want")
		var answer []proxmox.GuestNIC
		var v4, v6 []string
		for _, n := range nics {
			g := proxmox.GuestNIC{Name: rapid.String().Draw(t, "name"), MAC: n.mac}
			for _, r := range n.ips {
				g.IPs = append(g.IPs, r.ip)
				switch {
				case mac == "" || strings.ToLower(n.mac) != strings.ToLower(mac) || !r.host:
				case r.v4:
					v4 = append(v4, r.want)
				default:
					v6 = append(v6, r.want)
				}
			}
			answer = append(answer, g)
		}
		want := ""
		switch {
		case len(v4) > 0:
			want = v4[0]
		case len(v6) > 0:
			want = v6[0]
		}
		p := guestAddr(answer, mac)
		got := ""
		if p.Addr().IsValid() {
			got = p.Addr().String() + "/" + strconv.Itoa(p.Bits())
		}
		if got != want {
			t.Fatalf("guestAddr = %q, want %q", got, want)
		}
	})
}

// kiloAlpha is kilo.alpha in dev-01: a router whose WAN is net0 and LAN
// net1, and hosts on its LAN.
func kiloAlpha(hosts ...EnvHost) Environment {
	e := Environment{Slot: Slots(envConfig().Environments)[0], Domain: "kilo.alpha", LAN: kiloLAN}
	e.Hosts = append([]EnvHost{{Name: "gw.kilo.alpha", VMID: 2100, Router: true, WANMAC: wanMAC, LANMAC: lanMAC}}, hosts...)
	return e
}

// kiloLAN is kilo.alpha's LAN.
var kiloLAN = envConfig().Environments.Subnet

// readOf is what e's hosts' agents reported, as readAgents keeps it, from
// their answers.
func readOf(e Environment, nics map[int][]proxmox.GuestNIC) map[int]agentAddrs {
	out := map[int]agentAddrs{}
	for _, h := range e.Hosts {
		if n, ok := nics[h.VMID]; ok {
			out[h.VMID] = agentAddrs{lan: guestAddr(n, h.LANMAC), wan: guestAddr(n, h.WANMAC)}
		}
	}
	return out
}

func TestEnvironmentAddrs(t *testing.T) {
	host := func(vmid int, addr string) EnvHost {
		return EnvHost{Name: fmt.Sprint(vmid), VMID: vmid, Addr: parsedIP(addr), LANMAC: fmt.Sprintf("bc:24:11:00:00:%02d", vmid%100)}
	}
	answer := func(vmid int, ips ...string) []proxmox.GuestNIC {
		return []proxmox.GuestNIC{{Name: "lo", MAC: "00:00:00:00:00:00", IPs: podstest.IPs("127.0.0.1/8")},
			{Name: "eth0", MAC: fmt.Sprintf("BC:24:11:00:00:%02d", vmid%100), IPs: podstest.IPs(ips...)}}
	}
	router := []proxmox.GuestNIC{
		{Name: "vtnet1", MAC: lanMAC, IPs: podstest.IPs("fe80::2/64", "192.168.100.2/24")},
		{Name: "vtnet0", MAC: wanMAC, IPs: podstest.IPs("fe80::1/64", "10.50.12.133/24")},
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
	got := e.addrs(readOf(e, nics))
	for vmid, want := range map[int]LiveAddrs{
		2100: {Live: true, LAN: "192.168.100.2", WAN: "10.50.12.133"},
		2110: {Live: true, LAN: "192.168.100.10", External: "10.50.12.10"},
		2111: {Live: true, LAN: "192.168.100.21", Differs: true, External: "10.50.12.21"},
		2112: {External: "10.50.12.12"},
		2113: {Live: true, LAN: "10.50.99.4", Differs: true},
		2114: {Live: true, LAN: "192.168.100.30", External: "10.50.12.30"},
		2115: {Live: true, LAN: "2001:db8::9", Differs: true, External: "10.50.12.15"},
	} {
		if got[vmid] != want {
			t.Errorf("%d: %+v, want %+v", vmid, got[vmid], want)
		}
	}

	// Without the router's WAN address, no host has an external one.
	delete(nics, 2100)
	for vmid, a := range e.addrs(readOf(e, nics)) {
		if a.External != "" || a.WAN != "" {
			t.Errorf("%d: %+v without the router's answer", vmid, a)
		}
	}
	// A router reporting no WAN address is live, but gives none.
	nics[2100] = router[:1]
	if a := e.addrs(readOf(e, nics))[2100]; !a.Live || a.WAN != "" || a.LAN != "192.168.100.2" || a.Differs {
		t.Errorf("router without WAN: %+v", a)
	}
	// On a WAN network other than a /24, the router's WAN address shows,
	// but the NAT gives no host one address, so none has an external one.
	nics[2100] = []proxmox.GuestNIC{router[0], {MAC: wanMAC, IPs: podstest.IPs("10.50.12.133/16")}}
	for vmid, a := range e.addrs(readOf(e, nics)) {
		if a.External != "" || (vmid == 2100) != (a.WAN == "10.50.12.133") {
			t.Errorf("%d: %+v with a /16 WAN network", vmid, a)
		}
	}
	// A router whose LAN address isn't the gateway is flagged as differing,
	// even though its notes assign no address.
	nics[2100] = []proxmox.GuestNIC{{MAC: lanMAC, IPs: podstest.IPs("192.168.100.1/24")}, router[1]}
	if a := e.addrs(readOf(e, nics))[2100]; !a.Live || a.LAN != "192.168.100.1" || !a.Differs || a.WAN != "10.50.12.133" {
		t.Errorf("router at .1: %+v", a)
	}
}

// For any answers from a router and its hosts, a host's external address
// is the WAN /24's .N, N its live IPv4 address's last octet if it has one
// on the LAN, else (not live, or live without IPv4) its assigned one; and
// none without a /24 WAN address or with a live IPv4 one off the LAN. A
// live host's address differs only from an assigned one other than it.
func TestPropEnvironmentAddrs(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		e := kiloAlpha()
		nics := map[int][]proxmox.GuestNIC{}
		var wan [4]byte
		for i := range wan {
			wan[i] = rapid.Byte().Draw(t, "wan")
		}
		wan[3] = max(wan[3], 1) // 0.0.0.0 is no address
		bits := rapid.IntRange(0, 32).Draw(t, "bits")
		routerLive := rapid.Bool().Draw(t, "router live")
		if routerLive {
			nics[2100] = []proxmox.GuestNIC{{MAC: wanMAC, IPs: []netip.Prefix{netip.PrefixFrom(netip.AddrFrom4(wan), bits)}}}
		}
		// A host's own: not loopback, link-local, multicast or broadcast.
		wanHost := routerLive && wan[0] != 127 && !(wan[0] == 169 && wan[1] == 254) && !(wan[0] >= 224 && wan[0] <= 239) && wan != [4]byte{255, 255, 255, 255}
		want := map[int]string{}
		differs := map[int]bool{}
		for i := range rapid.IntRange(0, 6).Draw(t, "hosts") {
			vmid := 2101 + i
			mac := fmt.Sprintf("bc:24:11:00:00:%02d", i)
			assigned := rapid.IntRange(0, 255).Draw(t, "assigned")
			h := EnvHost{VMID: vmid, LANMAC: mac}
			if assigned != 0 {
				h.Addr = kiloLAN.Host(assigned)
			}
			e.Hosts = append(e.Hosts, h)
			octet := assigned
			switch rapid.IntRange(0, 3).Draw(t, "live") {
			case 0: // not live
			case 1: // live on the LAN
				n := rapid.SampledFrom([]int{assigned, rapid.IntRange(1, 254).Draw(t, "n")}).Draw(t, "lan")
				n = max(n, 1)
				differs[vmid] = assigned != 0 && n != assigned
				nics[vmid] = []proxmox.GuestNIC{{MAC: mac, IPs: podstest.IPs(fmt.Sprintf("192.168.100.%d/24", n))}}
				octet = n
			case 2: // live off the LAN
				nics[vmid] = []proxmox.GuestNIC{{MAC: mac, IPs: podstest.IPs(fmt.Sprintf("10.50.%d.%d/24", rapid.IntRange(0, 255).Draw(t, "c"), rapid.IntRange(1, 254).Draw(t, "d")))}}
				octet = 0
				differs[vmid] = assigned != 0
			case 3: // live without IPv4
				differs[vmid] = assigned != 0
				nics[vmid] = []proxmox.GuestNIC{{MAC: mac, IPs: podstest.IPs("2001:db8::7/64")}}
			}
			if wanHost && bits == 24 && octet >= 1 && octet <= 254 {
				want[vmid] = fmt.Sprintf("%d.%d.%d.%d", wan[0], wan[1], wan[2], octet)
			}
		}
		got := e.addrs(readOf(e, nics))
		if got[2100].External != "" {
			t.Fatalf("the router has external address %q", got[2100].External)
		}
		for _, h := range e.Hosts[1:] {
			if got[h.VMID].External != want[h.VMID] {
				t.Fatalf("%d: external %q, want %q", h.VMID, got[h.VMID].External, want[h.VMID])
			}
			if got[h.VMID].Differs != differs[h.VMID] {
				t.Fatalf("%d assigned %q: %+v, differs want %v", h.VMID, h.Addr, got[h.VMID], differs[h.VMID])
			}
		}
	})
}

func TestEnvironmentsReadsNICs(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:21:00,bridge=devext01", "net1": "virtio=BC:24:11:00:21:01,bridge=devint01", "agent": "1"})
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha"},
		map[string]string{"net0": "e1000=BC:24:11:00:21:10,bridge=devint01", "agent": "enabled=0"})
	f.Add(proxmox.VM{VMID: 2200, Name: "gw.kilo.bravo", Node: "alder", Pool: "dev-02", Tags: "dev;kilo.bravo;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:22:01,bridge=devint02", "net1": "virtio=BC:24:11:00:22:00,bridge=devext02", "agent": "1"})
	envs, _, err := NewPlanner(f, envConfig()).Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gw, _ := envs[0].Host("gw.kilo.alpha")
	teak, _ := envs[0].Host("teak.kilo.alpha")
	swapped, _ := envs[1].Host("gw.kilo.bravo")
	if gw.WANMAC != wanMAC || gw.LANMAC != lanMAC || !gw.Agent {
		t.Errorf("router: WAN %q LAN %q agent %v", gw.WANMAC, gw.LANMAC, gw.Agent)
	}
	if swapped.WANMAC != "bc:24:11:00:22:00" || swapped.LANMAC != "bc:24:11:00:22:01" {
		t.Errorf("router with net0 on devint02: WAN %q LAN %q", swapped.WANMAC, swapped.LANMAC)
	}
	if teak.WANMAC != "" || teak.LANMAC != "bc:24:11:00:21:10" || teak.Agent {
		t.Errorf("host: WAN %q LAN %q agent %v", teak.WANMAC, teak.LANMAC, teak.Agent)
	}
}

// LiveAddrs asks only running VMs with an agent enabled and a NIC to look
// on, reads at a time; one whose agent doesn't answer within the read
// timeout isn't live, and the rest are.
func TestLiveAddrs(t *testing.T) {
	f := podstest.New("alder")
	var hosts []EnvHost
	for i := range 12 {
		vmid := 2110 + i
		mac := fmt.Sprintf("bc:24:11:00:00:%02d", i)
		f.Add(proxmox.VM{VMID: vmid, Name: fmt.Sprint(vmid), Node: "alder", Status: "running"}, nil)
		f.SetGuest(vmid, podstest.Guest{Up: true, NICs: []proxmox.GuestNIC{{MAC: mac, IPs: podstest.IPs(fmt.Sprintf("192.168.100.%d/24", 10+i))}}})
		hosts = append(hosts, EnvHost{VMID: vmid, Node: "alder", Status: "running", Agent: true, LANMAC: mac})
	}
	hosts[0].Status = "stopped"
	hosts[1].Agent = false
	hosts[2].LANMAC = ""
	const hung, reads = 2113, 3
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
	got := NewPlanner(f, envConfig()).LiveAddrs(context.Background(), envs, reads, 100*time.Millisecond)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v with one agent hung", took)
	}
	for _, h := range hosts {
		if want := h.VMID > 2112 && h.VMID != hung; got[h.VMID].Live != want {
			t.Errorf("%d live %v, want %v", h.VMID, got[h.VMID].Live, want)
		}
	}
	if n := f.Called("agent-nics:2110") + f.Called("agent-nics:2111") + f.Called("agent-nics:2112"); n != 0 {
		t.Errorf("%d reads of VMs whose agent can't answer", n)
	}
	if most > reads || most < 2 {
		t.Errorf("%d reads at once, want 2 to %d", most, reads)
	}
	if got[2120].LAN != "192.168.100.20" {
		t.Errorf("2120: %+v", got[2120])
	}

	// All the reads are bounded together by ctx.
	start = time.Now()
	f.Gate = func(ctx context.Context, key string) (func(), error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	for vmid, a := range NewPlanner(f, envConfig()).LiveAddrs(ctx, envs, reads, time.Minute) {
		if a.Live {
			t.Errorf("%d live from a hung agent", vmid)
		}
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v with every agent hung", took)
	}
}

// Fewer than one read at a time is one.
func TestLiveAddrsReadsAtLeastOne(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2110, Name: "2110", Node: "alder", Status: "running"}, nil)
	f.SetGuest(2110, podstest.Guest{Up: true, NICs: []proxmox.GuestNIC{{MAC: lanMAC, IPs: podstest.IPs("192.168.100.10/24")}}})
	envs := []Environment{{Hosts: []EnvHost{{VMID: 2110, Node: "alder", Status: "running", Agent: true, LANMAC: lanMAC}}}}
	for _, reads := range []int{0, -1} {
		done := make(chan map[int]LiveAddrs, 1)
		go func() { done <- NewPlanner(f, envConfig()).LiveAddrs(context.Background(), envs, reads, time.Second) }()
		select {
		case got := <-done:
			if got[2110].LAN != "192.168.100.10" {
				t.Errorf("%d reads at a time: %+v", reads, got[2110])
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%d reads at a time never finished", reads)
		}
	}
}
