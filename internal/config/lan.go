package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// LAN is every environment's LAN, a /24 network. Config writes it as its
// first three octets, in decimal without leading zeros: 192.168.100.
type LAN struct{ p netip.Prefix }

// ParseLAN reads a LAN's three octets.
func ParseLAN(s string) (LAN, error) {
	a, err := netip.ParseAddr(s + ".0")
	l, ok := LANOf(netip.PrefixFrom(a, 24))
	if err != nil || !ok {
		return LAN{}, fmt.Errorf("%q isn't three octets in decimal without leading zeros, e.g. 192.168.100", s)
	}
	return l, nil
}

// LANOf is the /24 network p is in, if p is an IPv4 address with prefix
// length 24.
func LANOf(p netip.Prefix) (LAN, bool) {
	if !p.Addr().Is4() || p.Bits() != 24 {
		return LAN{}, false
	}
	return LAN{p.Masked()}, true
}

func (l LAN) IsValid() bool { return l.p.IsValid() }

// Bits is l's prefix length, or -1 for the zero LAN.
func (l LAN) Bits() int { return l.p.Bits() }

// String is l's three octets, or "" for the zero LAN.
func (l LAN) String() string {
	if !l.IsValid() {
		return ""
	}
	s := l.p.Addr().String()
	return s[:strings.LastIndexByte(s, '.')]
}

// Host is l's address with last octet octet, or the zero Addr if octet
// isn't 0 to 255 or l is the zero LAN.
func (l LAN) Host(octet int) netip.Addr {
	if !l.IsValid() || octet < 0 || octet > 255 {
		return netip.Addr{}
	}
	b := l.p.Addr().As4()
	b[3] = byte(octet)
	return netip.AddrFrom4(b)
}

// Octet is a's last octet, if a is on l.
func (l LAN) Octet(a netip.Addr) (int, bool) {
	if !l.p.Contains(a) {
		return 0, false
	}
	return int(a.As4()[3]), true
}

func (l LAN) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

func (l *LAN) UnmarshalText(b []byte) error {
	v, err := ParseLAN(string(b))
	if err != nil {
		return err
	}
	*l = v
	return nil
}
