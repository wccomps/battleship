package apply

import (
	"context"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func checkLastLines(t interface{ Fatalf(string, ...any) }, s string, n int) {
	// A line ends at \n or \r: a terminal returns the cursor at either.
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if n <= 0 {
		lines = nil
	} else if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	got := lastLines(s, n)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("lastLines(%q, %d) = %q has a line break", s, n, got)
	}
	var segs []string
	if got != "" {
		segs = strings.Split(got, " / ")
	}
	if !slices.Equal(segs, lines) {
		t.Fatalf("lastLines(%q, %d) = %q, want the last %d of non-blank lines %q", s, n, got, n, lines)
	}
}

// The tail of a script's output is its last n non-blank lines, trimmed, on
// one line; none for n <= 0.
func TestPropLastLines(t *testing.T) {
	for _, s := range []string{"", "\n", " \n \n", "a", "a\n", "\na\n\nb\n\n", "a\r\nb\r\n", " a \n b ", "a\n\n\n\nb\nc\nd\ne\nf\ng", "a\rb", "a\r\rb\r"} {
		for n := -2; n < 8; n++ {
			checkLastLines(t, s, n)
		}
	}
	pieces := rapid.SampledFrom([]string{"a", "b c", "  ", "\t", "x/y", "\r", "line one", "-"})
	rapid.Check(t, func(t *rapid.T) {
		parts := rapid.SliceOfN(pieces, 0, 14).Draw(t, "parts")
		seps := rapid.SampledFrom([]string{"\n", "\n", "\r\n", "\n\n", "\n \n", ""})
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p)
			b.WriteString(seps.Draw(t, "sep"))
		}
		checkLastLines(t, b.String(), rapid.IntRange(-2, 7).Draw(t, "n"))
	})
}

// The guest agent's answer for verification: its interfaces and host name.
type verifyNIC struct {
	mac string
	ips []string
}

// verifyModel is whether verification should pass: some interface with
// net0's MAC, ignoring case, has the address and prefix, and the host
// name's first label is the host, ignoring case.
func verifyModel(nics []verifyNIC, hostname, mac, ip, host string) bool {
	onNet0 := slices.ContainsFunc(nics, func(n verifyNIC) bool {
		return strings.ToLower(n.mac) == strings.ToLower(mac) && slices.Contains(n.ips, ip)
	})
	label := hostname
	if i := strings.IndexByte(label, '.'); i >= 0 {
		label = label[:i]
	}
	return onNet0 && strings.EqualFold(label, host)
}

// Verification passes exactly when the guest reports the host name and the
// address on an interface with net0's MAC.
func TestPropVerifyGuest(t *testing.T) {
	const mac = "bc:24:11:00:21:01"
	hostnames := []string{"oak", "OAK", "Oak.kilo.alpha", "oak.", "oak.x.y", "oakx", "oa", "", ".oak", "WIN-123", "xoak", "oak-1.kilo", "oak kilo", "oak\n"}
	macs := []string{mac, mac, mac, "bc:24:11:00:21:02", "bc:24:11:00:21:00", "bc-24-11-00-21-01", "", "BC:24:11:00:21:01"}
	ips := []string{"192.168.100.20/24", "192.168.100.20/24", "192.168.100.20/25", "192.168.100.20/240", "192.168.100.200/24", "192.168.100.2/24",
		"192.168.100.21/24", "192.168.100.20", "192.168.100.20/", "92.168.100.20/24", "fe80::1/64", "169.254.1.1/16", " 192.168.100.20/24"}
	check := func(t interface{ Fatalf(string, ...any) }, nics []verifyNIC, hostname string) {
		want := verifyModel(nics, hostname, mac, "192.168.100.20/24", "oak")

		f := podstest.New("alder")
		f.Add(proxmox.VM{VMID: 2101, Name: "oak.kilo.alpha", Node: "alder", Status: "running"},
			map[string]string{"net0": "virtio=BC:24:11:00:21:01,bridge=devint01"})
		var gn []proxmox.GuestNIC
		for i, n := range nics {
			gn = append(gn, proxmox.GuestNIC{Name: []string{"eth0", "eth0.10", "eth1"}[i], MAC: n.mac, IPs: n.ips})
		}
		f.SetGuest(2101, podstest.Guest{Up: true, Hostname: hostname, NICs: gn})
		it := guestItem(pods.ImageLinux)
		it.Steps = []pods.Step{pods.StepVerify}
		res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{it}})
		if got := len(res.Succeeded) == 1; got != want {
			t.Fatalf("verify of hostname %q with %+v: passed = %v, want %v (result %+v)", hostname, nics, got, want, res)
		}
	}
	ok, other := "192.168.100.20/24", "192.168.100.21/24"
	upper := strings.ToUpper(mac)
	for _, tc := range []struct {
		name string
		nics []verifyNIC
		want bool
	}{
		{"VLAN subinterface after eth0", []verifyNIC{{mac, []string{ok}}, {mac, []string{"10.0.10.5/24"}}}, true},
		{"address only on the VLAN subinterface", []verifyNIC{{mac, []string{other}}, {mac, []string{ok}}}, true},
		{"MAC in upper case", []verifyNIC{{upper, []string{ok}}}, true},
		{"MAC case differs between eth0 and its VLAN", []verifyNIC{{upper, nil}, {mac, []string{ok}}}, true},
		{"MAC absent", []verifyNIC{{"bc:24:11:00:21:02", []string{ok}}}, false},
	} {
		if got := verifyModel(tc.nics, "oak", mac, ok, "oak"); got != tc.want {
			t.Fatalf("%s: model says %v", tc.name, got)
		}
		check(t, tc.nics, "oak")
	}
	// Two interfaces with net0's MAC, and other interfaces' addresses.
	for _, nics := range [][]verifyNIC{
		{{mac, []string{ok}}, {mac, nil}}, {{mac, nil}, {mac, []string{ok}}}, {{mac, []string{other}}, {mac, []string{ok}}}, {{mac, []string{ok}}, {mac, []string{other}}},
		{{"bc:24:11:00:21:02", []string{ok}}, {mac, []string{other}}}, {{mac, []string{other}}, {"bc:24:11:00:21:02", []string{ok}}}, {{mac, []string{ok}}},
	} {
		for _, h := range []string{"oak", "OAK.kilo", "oakx"} {
			check(t, nics, h)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		var nics []verifyNIC
		for range rapid.IntRange(1, 3).Draw(t, "nics") {
			nics = append(nics, verifyNIC{rapid.SampledFrom(macs).Draw(t, "mac"), rapid.SliceOfN(rapid.SampledFrom(ips), 0, 3).Draw(t, "ips")})
		}
		check(t, nics, rapid.SampledFrom(hostnames).Draw(t, "hostname"))
	})
}
