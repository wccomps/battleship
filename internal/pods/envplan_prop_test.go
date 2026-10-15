package pods

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/proxmox"
)

// noteValues are what someone editing a VM's notes in Proxmox might write
// for one field: right, nearly right, or an attempt at a script.
var noteValues = []string{
	"192.168.100.10", "192.168.100.5;id", "192.168.100.05", "192.168.100.+5", "192.168.100.3", "192.168.100.255",
	"192.168.100.10'", "192.168.100.1\u0661", "10.0.0.5", "windows", "linux", "windows'", "Windows", "router", "DC",
	"dc", "teak", "oak$(id)", "\u202eoak", "debian-12-v1", "", "=", "'",
}

// drawNotes draws a VM description holding battleship lines with hostile
// values among user text.
func drawNotes(t *rapid.T, label string) string {
	value := rapid.OneOf(rapid.SampledFrom(noteValues), rapid.String())
	line := rapid.Custom(func(t *rapid.T) string {
		var b strings.Builder
		b.WriteString(rapid.SampledFrom([]string{"", " ", "\u200b", "\ufeff"}).Draw(t, "indent") + notesPrefix)
		for _, k := range rapid.SliceOfN(rapid.SampledFrom([]string{"ip", "os", "base", "dns", "x"}), 0, 5).Draw(t, "keys") {
			b.WriteString(" " + k + "=" + value.Draw(t, k))
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
	return ok && err == nil && isIPv4(ip) && strconv.Itoa(o) == rest && o >= FirstHostOctet && o <= LastHostOctet
}

// Checked keeps from notes only an address of the subnet, a known OS and a
// DNS choice that is router or a host name, and checking again changes
// nothing.
func TestPropCheckedNotes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		subnet := rapid.SampledFrom([]string{"192.168.100", "10.0.0", "172.16.5"}).Draw(t, "subnet")
		n, _ := ParseNotes(drawNotes(t, "notes"))
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

// However slot VMs are named, tagged and noted, every guest an environment
// plan would configure passes Check, makes a script, keeps a Windows rename
// to 15 characters and has an address in the subnet.
func TestPropEnvPlansConfigureOnlyCheckedGuests(t *testing.T) {
	const subnet = "192.168.100"
	host := rapid.OneOf(rapid.SampledFrom([]string{"teak", "oak", "dc", "gw", "Oak", "averyveryverylongname", "a'b", "x$(id)", ""}), rapid.String())
	rapid.Check(t, func(t *rapid.T) {
		f := envFake()
		for i := range rapid.IntRange(0, 5).Draw(t, "vms") {
			// Some hosts are as battleship left them, the rest anything.
			name, tags := "", "dev;kilo.alpha"
			notes := fmt.Sprintf("battleship: ip=%s.%d os=%s base=debian-12-v1 dns=router", subnet, rapid.IntRange(0, 300).Draw(t, "octet"),
				rapid.SampledFrom([]string{ImageWindows, ImageLinux}).Draw(t, "os"))
			if rapid.Bool().Draw(t, "proper") {
				name = rapid.SampledFrom([]string{"teak", "oak", "dc", "averyveryverylongname"}).Draw(t, "host") + ".kilo.alpha"
			} else {
				name = host.Draw(t, "host") + "." + rapid.SampledFrom([]string{"kilo.alpha", "kilo.alpha", "kilo.alpha", "kilo.alpha", "Kilo.alpha", "other.env"}).Draw(t, "domain")
				tags = rapid.SampledFrom([]string{"dev;kilo.alpha", "dev;kilo.alpha;router", "dev", "router;x"}).Draw(t, "tags")
				notes = drawNotes(t, "notes")
			}
			status := rapid.SampledFrom([]string{"running", "stopped"}).Draw(t, "status")
			f.Add(proxmox.VM{VMID: 2101 + i, Name: name, Node: "alder", Pool: "dev-01", Tags: tags, Status: status},
				map[string]string{"net0": fmt.Sprintf("virtio=BC:24:11:00:21:%02x,bridge=devint01", i), "description": notes})
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
		for _, it := range plan.Items {
			if it.Blocked != "" || it.Guest == nil {
				continue
			}
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
	})
}
