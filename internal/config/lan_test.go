package config

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// octetRE is the rule for one of a LAN's octets: decimal, no leading zero.
var octetRE = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// A LAN is exactly three octets, each 0 to 255 in decimal without a
// leading zero. It reads back as written, and its hosts are those octets
// and the host's.
func TestPropParseLAN(t *testing.T) {
	octet := rapid.OneOf(
		rapid.StringMatching(`[0-9]{1,4}`),
		rapid.Map(rapid.IntRange(0, 255), strconv.Itoa),
		rapid.SampledFrom([]string{"", "+1", "-0", " 1", "1 ", "0x1", "1e2", "00", "01", "\u0661", "1\u200b", "::1", "%0"}),
	)
	rapid.Check(t, func(t *rapid.T) {
		parts := rapid.SliceOfN(octet, 1, 5).Draw(t, "octets")
		if rapid.IntRange(0, 3).Draw(t, "three") > 0 {
			for len(parts) < 3 {
				parts = append(parts, "0")
			}
			parts = parts[:3]
		}
		s := strings.Join(parts, ".")
		want := len(parts) == 3
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			want = want && octetRE.MatchString(p) && err == nil && n <= 255
		}
		lan, err := ParseLAN(s)
		if (err == nil) != want || lan.IsValid() != want {
			t.Fatalf("ParseLAN(%q) = %v, %v; want valid %v", s, lan, err, want)
		}
		if !want {
			return
		}
		if lan.String() != s {
			t.Fatalf("ParseLAN(%q) reads back as %q", s, lan.String())
		}
		o := rapid.IntRange(-300, 600).Draw(t, "host")
		h := lan.Host(o)
		if o < 0 || o > 255 {
			if h.IsValid() {
				t.Fatalf("%s.Host(%d) = %v", s, o, h)
			}
			return
		}
		if h.String() != s+"."+strconv.Itoa(o) {
			t.Fatalf("%s.Host(%d) = %v", s, o, h)
		}
		if got, ok := lan.Octet(h); !ok || got != o {
			t.Fatalf("%s.Octet(%v) = %d, %v", s, h, got, ok)
		}
		b := h.As4()
		b[rapid.IntRange(0, 2).Draw(t, "other")]++
		if _, ok := lan.Octet(netip.AddrFrom4(b)); ok {
			t.Fatalf("%s holds %v", s, netip.AddrFrom4(b))
		}
		if _, ok := lan.Octet(netip.AddrFrom16(h.As16())); ok {
			t.Fatalf("%s holds %v written as IPv6", s, h)
		}
	})
}

// Config refuses a subnet with a leading zero, which some tools read as
// octal and netip refuses.
func TestLoadRefusesLeadingZeroSubnet(t *testing.T) {
	const head = "[proxmox]\nurl = \"https://pve.example:8006\"\ninsecure_skip_verify = true\n[environments]\n"
	if _, err := Load(writeFile(t, head+"subnet = \"192.168.01\"\n")); err == nil || !strings.Contains(err.Error(), "environments.subnet") {
		t.Fatalf("192.168.01: %v", err)
	}
	cfg, err := Load(writeFile(t, head+"subnet = \"10.50.7\"\n"))
	if err != nil || cfg.Environments.Subnet.String() != "10.50.7" {
		t.Fatalf("10.50.7: %v, %v", cfg.Environments.Subnet, err)
	}
	if got := Default().Environments.Subnet.String(); got != "192.168.100" {
		t.Fatalf("default subnet %q", got)
	}
}

// The LAN of an address and prefix length is its /24, only for an IPv4
// address with length 24: its first three octets, 24 bits long, the same
// for every address on it.
func TestPropLANOf(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		b := [4]byte(rapid.SliceOfN(rapid.Byte(), 4, 4).Draw(t, "addr"))
		bits := rapid.IntRange(-1, 33).Draw(t, "bits")
		lan, ok := LANOf(netip.PrefixFrom(netip.AddrFrom4(b), bits))
		if ok != (bits == 24) || lan.IsValid() != ok {
			t.Fatalf("LANOf(%v/%d) = %v, %v", b, bits, lan, ok)
		}
		if want := strconv.Itoa(int(b[0])) + "." + strconv.Itoa(int(b[1])) + "." + strconv.Itoa(int(b[2])); ok && (lan.String() != want || lan.Bits() != 24) {
			t.Fatalf("LANOf(%v/24) = %q /%d, want %q /24", b, lan, lan.Bits(), want)
		}
		net := b
		net[3] = 0
		if want, _ := LANOf(netip.PrefixFrom(netip.AddrFrom4(net), 24)); ok && lan != want {
			t.Fatalf("LANOf(%v/24) = %v, not the LAN of its network %v", b, lan, want)
		}
		for _, bits := range []int{24, bits + 96} {
			if _, ok := LANOf(netip.PrefixFrom(netip.AddrFrom16(netip.AddrFrom4(b).As16()), bits)); ok {
				t.Fatalf("LANOf took %v/%d written as IPv6", b, bits)
			}
		}
	})
	if (LAN{}).Bits() != -1 {
		t.Fatal("the zero LAN has a length")
	}
}
