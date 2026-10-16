package pods

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// noteValues are what someone editing a VM's notes in Proxmox might write
// for one field: right, nearly right, or an attempt at a script.
var noteValues = []string{
	"192.168.100.10", "192.168.100.5;id", "192.168.100.05", "192.168.100.+5", "192.168.100.3", "192.168.100.255",
	"192.168.100.10'", "192.168.100.1\u0661", "10.0.0.5", "windows", "linux", "windows'", "Windows", "router", "DC",
	"dc", "teak", "oak$(id)", "\u202eoak", "debian-12-v1", "", "=", "'",
}

// noteIP draws an address at or near the edge of subnet's host range,
// sometimes spoilt the way a hand edit might.
func noteIP(t *rapid.T, subnet string) string {
	o := rapid.OneOf(rapid.IntRange(-1, 300), rapid.SampledFrom([]int{-1, 0, 3, FirstHostOctet, LastHostOctet, 255, 256})).Draw(t, "octet")
	octet := strconv.Itoa(o)
	return subnet + "." + rapid.SampledFrom([]string{octet, octet, octet, "0" + octet, "+" + octet, octet + " ", octet + "'", octet + ".1", "\u0661" + octet}).Draw(t, "spoilt octet")
}

// drawNotes draws a VM description holding battleship lines with hostile
// values among user text.
func drawNotes(t *rapid.T, subnet, label string) string {
	value := rapid.OneOf(rapid.SampledFrom(noteValues), rapid.String())
	line := rapid.Custom(func(t *rapid.T) string {
		var b strings.Builder
		b.WriteString(rapid.SampledFrom([]string{"", " ", "\u200b", "\ufeff"}).Draw(t, "indent") + notesPrefix)
		for _, k := range rapid.SliceOfN(rapid.SampledFrom([]string{"ip", "os", "base", "dns", "x"}), 0, 5).Draw(t, "keys") {
			v := value
			if k == "ip" {
				v = rapid.OneOf(value, rapid.Custom(func(t *rapid.T) string { return noteIP(t, subnet) }))
			}
			b.WriteString(" " + k + "=" + v.Draw(t, k))
		}
		return b.String()
	})
	lines := rapid.SliceOfN(rapid.OneOf(line, rapid.SampledFrom([]string{"Login: root", "", "\r"}), rapid.String()), 0, 4).Draw(t, label)
	return strings.Join(lines, "\n")
}

// inSubnet reports whether ip is a host address of subnet that scripts may
// set: canonical, octet 4-254.
func inSubnet(ip, subnet string) bool {
	rest, ok := strings.CutPrefix(ip, subnet+".")
	o, err := strconv.Atoi(rest)
	return ok && err == nil && safeIPv4(ip) && strconv.Itoa(o) == rest && o >= FirstHostOctet && o <= LastHostOctet
}

// Checked keeps an address of the subnet exactly when its last octet is a
// host octet.
func TestCheckedNotesOctets(t *testing.T) {
	for o := -1; o <= 300; o++ {
		ip := "192.168.100." + strconv.Itoa(o)
		if got, want := (Notes{IP: ip}).Checked("192.168.100").IP != "", o >= 4 && o <= 254; got != want {
			t.Errorf("Checked kept %s: %v, want %v", ip, got, want)
		}
	}
}

// Checked keeps from notes only an address of the subnet, a known OS and a
// DNS choice that is router or a host name, and checking again changes
// nothing.
func TestPropCheckedNotes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		subnet := rapid.SampledFrom([]string{"192.168.100", "10.0.0", "172.16.5"}).Draw(t, "subnet")
		n, _ := ParseNotes(drawNotes(t, subnet, "notes"))
		if rapid.Bool().Draw(t, "near ip") {
			n.IP = noteIP(t, subnet)
		}
		c := n.Checked(subnet)
		if c.IP != "" && !inSubnet(c.IP, subnet) {
			t.Fatalf("kept address %q outside %s", c.IP, subnet)
		}
		if c.OS != "" && c.OS != ImageWindows && c.OS != ImageLinux {
			t.Fatalf("kept OS %q", c.OS)
		}
		if c.DNS != "" && c.DNS != DNSRouter && CheckHostname(c.DNS) != nil {
			t.Fatalf("kept DNS %q", c.DNS)
		}
		if c.Checked(subnet) != c {
			t.Fatalf("checking again changed %+v to %+v", c, c.Checked(subnet))
		}
	})
}

// checkEnvPlan checks that every guest plan would configure passes Check,
// makes a script, keeps a Windows rename to 15 characters and has an address
// in subnet. It returns how many it checked.
func checkEnvPlan(t interface{ Fatalf(string, ...any) }, plan *Plan, subnet string) int {
	n := 0
	for _, it := range plan.Items {
		if it.Blocked != "" || it.Guest == nil {
			continue
		}
		n++
		g := *it.Guest
		if err := g.Check(); err != nil {
			t.Fatalf("%s: guest %+v: %v", it.Name, g, err)
		}
		if _, err := GuestScript(g, "BC:24:11:00:21:01"); err != nil {
			t.Fatalf("%s: %v", it.Name, err)
		}
		if g.OS == ImageWindows && !g.DNSOnly && len(g.Hostname) > MaxWindowsHostname {
			t.Fatalf("%s: renames Windows to %q", it.Name, g.Hostname)
		}
		if !inSubnet(g.IP, subnet) {
			t.Fatalf("%s: address %q outside %s", it.Name, g.IP, subnet)
		}
	}
	return n
}

// slotFake is envFake with kilo.alpha in slot 1: its router and the given
// host, running, with notes.
func slotFake(name, notes string) *podstest.Fake {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:21:00,bridge=devext01", "net1": "virtio=BC:24:11:00:21:01,bridge=devint01",
			"description": "battleship: base=gw-v1 dns=router"})
	f.Add(proxmox.VM{VMID: 2101, Name: name, Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:21:02,bridge=devint01", "description": notes})
	return f
}

// Hostile notes or names on a well-formed host of a slot give an env-dns
// guest that is blocked or passes every check; valid ones give a guest.
func TestEnvDNSHostileNotes(t *testing.T) {
	const subnet = "192.168.100"
	valid := map[string]string{"ip": "192.168.100.10", "os": ImageLinux, "dns": "router"}
	for _, key := range []string{"ip", "os", "dns"} {
		for _, v := range append(slices.Clone(noteValues), "192.168.100.255", "192.168.100.3", " 192.168.100.10", "192.168.100.10 os=windows") {
			fields := maps.Clone(valid)
			fields[key] = v
			notes := fmt.Sprintf("battleship: ip=%s os=%s base=debian-12-v1 dns=%s", fields["ip"], fields["os"], fields["dns"])
			plan, err := NewPlanner(slotFake("teak.kilo.alpha", notes), envConfig()).EnvDNS(context.Background(), EnvRequest{Domain: "kilo.alpha", DNS: DNSRouter})
			if err != nil {
				t.Fatalf("%s=%q: %v", key, v, err)
			}
			checkEnvPlan(t, plan, subnet)
		}
	}
	for _, host := range []string{"teak", "Oak", "a'b", "x$(id)", "te\u0430k", "averyveryverylongname"} {
		plan, err := NewPlanner(slotFake(host+".kilo.alpha", "battleship: ip=192.168.100.10 os=windows base=win2022-v1 dns=router"), envConfig()).
			EnvDNS(context.Background(), EnvRequest{Domain: "kilo.alpha", DNS: DNSRouter})
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if n := checkEnvPlan(t, plan, subnet); host == "teak" && n != 1 {
			t.Errorf("teak with valid notes: %d guests, want 1", n)
		}
	}
}

// However slot VMs are named, tagged and noted, every guest an environment
// plan would configure passes Check, makes a script, keeps a Windows rename
// to 15 characters and has an address in the subnet.
func TestPropEnvPlansConfigureOnlyCheckedGuests(t *testing.T) {
	const subnet = "192.168.100"
	host := rapid.OneOf(rapid.SampledFrom([]string{"teak", "oak", "dc", "averyveryverylongname"}),
		rapid.SampledFrom([]string{"Oak", "a'b", "x$(id)", "te\u0430k", "", "-x"}),
		rapid.Map(rapid.String(), func(s string) string { return strings.ReplaceAll(s, ".", "") }))
	rapid.Check(t, func(t *rapid.T) {
		// kilo.alpha always holds slot 1, with its router.
		f := slotFake("teak.kilo.alpha", "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=router")
		// Sometimes the last VM is of another domain, so no slot holds kilo.alpha.
		// (A middle value: rapid favours the ends of a range.)
		mixed := rapid.IntRange(0, 9).Draw(t, "mixed") == 5
		vms := rapid.IntRange(0, 5).Draw(t, "vms")
		for i := range vms {
			domain := "kilo.alpha"
			if mixed && i == vms-1 {
				domain = rapid.SampledFrom([]string{"Kilo.alpha", "other.env"}).Draw(t, "domain")
			}
			// Names, tags and notes vary apart: mostly as battleship left them, the rest anything.
			tags := rapid.SampledFrom([]string{"dev;kilo.alpha", "dev;kilo.alpha", "dev;kilo.alpha", "dev;kilo.alpha;router", "dev", "router;x"}).Draw(t, "tags")
			notes := fmt.Sprintf("battleship: ip=%s os=%s base=debian-12-v1 dns=router", noteIP(t, subnet),
				rapid.SampledFrom([]string{ImageWindows, ImageLinux}).Draw(t, "os"))
			if rapid.Bool().Draw(t, "hostile notes") {
				notes = drawNotes(t, subnet, "notes")
			}
			status := rapid.SampledFrom([]string{"running", "running", "stopped"}).Draw(t, "status")
			f.Add(proxmox.VM{VMID: 2102 + i, Name: host.Draw(t, "host") + "." + domain, Node: "alder", Pool: "dev-01", Tags: tags, Status: status},
				map[string]string{"net0": fmt.Sprintf("virtio=BC:24:11:00:21:%02x,bridge=devint01", i+3), "description": notes})
		}
		p := NewPlanner(f, envConfig())
		var specs []HostSpec
		for range rapid.IntRange(0, 3).Draw(t, "hosts") {
			h := HostSpec{Host: rapid.SampledFrom([]string{"teak", "oak", "dc", "gw", "averyveryverylongname"}).Draw(t, "spec host"),
				Family: rapid.SampledFrom([]string{"debian-12", "win2022", "gw"}).Draw(t, "family")}
			if rapid.Bool().Draw(t, "octet") {
				h.Octet = rapid.IntRange(FirstHostOctet, LastHostOctet).Draw(t, "octet value")
			}
			specs = append(specs, h)
		}
		dns := rapid.SampledFrom([]string{"", DNSRouter, "teak", "oak", "dc"}).Draw(t, "dns")
		var plan *Plan
		var err error
		switch op := rapid.SampledFrom([]string{"create", "add", "dns"}).Draw(t, "op"); op {
		case "create":
			plan, err = p.EnvCreate(context.Background(), EnvRequest{Domain: "new.env", Router: HostSpec{Host: "gw", Family: "gw"}, Hosts: specs})
		case "add":
			plan, err = p.EnvAdd(context.Background(), EnvRequest{Domain: "kilo.alpha", Hosts: specs})
		case "dns":
			plan, err = p.EnvDNS(context.Background(), EnvRequest{Domain: "kilo.alpha", DNS: dns})
		}
		if err != nil {
			return
		}
		checkEnvPlan(t, plan, subnet)
	})
}
