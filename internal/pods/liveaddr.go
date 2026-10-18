package pods

import (
	"context"
	"maps"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/proxmox"
)

// An environment's VMs have the addresses battleship assigned them (EnvHost
// Addr), and the ones their guest agents report (LiveAddrs).

// guestAddr is the address the agent reports on the NIC with MAC mac, with
// the prefix length it reports (Bits is -1 without a valid one): its first
// IPv4 address, else its first IPv6 one, skipping loopback, link-local,
// multicast and unspecified addresses. Its Addr is in canonical form,
// whatever the guest wrote, and invalid if there is none.
func guestAddr(nics []proxmox.GuestNIC, mac string) netip.Prefix {
	if mac == "" {
		return netip.Prefix{}
	}
	var v6 netip.Prefix
	for _, n := range nics {
		if !strings.EqualFold(n.MAC, mac) {
			continue
		}
		for _, ip := range n.IPs {
			p, ok := hostAddr(ip)
			switch {
			case !ok:
			case p.Addr().Is4():
				return p
			case !v6.Addr().IsValid():
				v6 = p
			}
		}
	}
	return v6
}

// hostAddr is an agent's "address/prefix", if its address can be a host's
// own; an IPv4 address written as IPv6 is read as IPv4, prefix and all.
func hostAddr(s string) (netip.Prefix, bool) {
	addr, prefix, _ := strings.Cut(s, "/")
	a, err := netip.ParseAddr(addr)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, false
	}
	bits, err := strconv.Atoi(prefix)
	if err != nil {
		bits = -1
	}
	if a.Is4In6() {
		a, bits = a.Unmap(), bits-96
	}
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() ||
		a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, bits), true
}

// externalAddr is the address of LAN host octet from outside its
// environment, given the router's WAN address and prefix. It assumes what
// the router image does, which battleship neither configures nor checks: a
// one-to-one NAT of each LAN host <subnet>.N to the WAN network's .N. Only a
// /24 WAN network has one such address, so there is none for any other
// prefix, nor unless wan is IPv4 and octet a host's (1 to 254).
func externalAddr(wan netip.Prefix, octet int) string {
	if !wan.Addr().Is4() || wan.Bits() != 24 || octet < 1 || octet > 254 {
		return ""
	}
	b := wan.Addr().As4()
	b[3] = byte(octet)
	return netip.AddrFrom4(b).String()
}

// LiveAddrs are a VM's addresses as its guest agent reports them.
type LiveAddrs struct {
	// Live: its agent answered; LAN, Differs and WAN are set only then.
	Live bool
	LAN  string // on its LAN NIC (the router's net1, a host's net0)
	// Differs says LAN isn't its assigned address (EnvHost.Addr).
	Differs bool
	WAN     string // the router's, on its WAN NIC (net0)
	// External is a host's address from outside, through the router's NAT
	// (externalAddr): by its live LAN octet, else its assigned one. It is
	// "" without the router's WAN address, and for a live LAN address
	// outside the environment's subnet, which the NAT doesn't map.
	External string
}

// agentAddrs are what a host's agent reported on its LAN and WAN NICs.
type agentAddrs struct{ lan, wan netip.Prefix }

// agentAddrsOf are h's addresses in its agent's answer nics.
func agentAddrsOf(h EnvHost, nics []proxmox.GuestNIC) agentAddrs {
	return agentAddrs{lan: guestAddr(nics, h.LANMAC), wan: guestAddr(nics, h.WANMAC)}
}

// addrs are e's hosts' addresses, by VMID, from what their agents reported
// (readAgents); a host missing from read isn't live. lan is the
// environments' LAN network.
func (e Environment) addrs(read map[int]agentAddrs, lan netip.Prefix) map[int]LiveAddrs {
	out := map[int]LiveAddrs{}
	var wan netip.Prefix
	for _, h := range e.Hosts {
		got, live := read[h.VMID]
		a := LiveAddrs{Live: live}
		if live {
			if got.lan.Addr().IsValid() {
				a.LAN = got.lan.Addr().String()
			}
			a.Differs = a.LAN != h.Addr
			if h.Router && got.wan.Addr().IsValid() {
				a.WAN = got.wan.Addr().String()
				if !wan.Addr().IsValid() {
					wan = got.wan
				}
			}
		}
		out[h.VMID] = a
	}
	for _, h := range e.Hosts {
		a := out[h.VMID]
		if h.Router || !wan.Addr().IsValid() {
			continue
		}
		octet := LastOctet(h.Addr)
		if lanAddr := read[h.VMID].lan.Addr(); lanAddr.Is4() {
			octet = 0
			if lan.Contains(lanAddr) {
				octet = int(lanAddr.As4()[3])
			}
		}
		a.External = externalAddr(wan, octet)
		out[h.VMID] = a
	}
	return out
}

// asksAgent is whether h's agent may report its addresses: it is running,
// with its agent enabled and a NIC to look on.
func (h EnvHost) asksAgent() bool {
	return h.Status == "running" && h.Agent && (h.LANMAC != "" || h.WANMAC != "")
}

// LiveAddrs are envs' hosts' addresses by VMID, read through their guest
// agents reads at a time, each read bounded by timeout and all of them by
// within. A VM that isn't running, has no agent enabled, or whose read
// fails (a user without LiveAddrsPrivilege, an agent that doesn't answer in
// time) isn't live.
func (p Planner) LiveAddrs(ctx context.Context, envs []Environment, reads int, timeout, within time.Duration) map[int]LiveAddrs {
	read := p.readAgents(ctx, envs, reads, timeout, within)
	lan, _ := netip.ParsePrefix(p.Cfg.Environments.Subnet + ".0/" + strconv.Itoa(HostPrefix))
	out := map[int]LiveAddrs{}
	for _, e := range envs {
		maps.Copy(out, e.addrs(read, lan))
	}
	return out
}

// readAgents reads the addresses of envs' hosts whose agents may report
// them (asksAgent), as LiveAddrs says. Each answer is cut to its addresses
// as it comes, so no more than reads answers are held at once.
func (p Planner) readAgents(ctx context.Context, envs []Environment, reads int, timeout, within time.Duration) map[int]agentAddrs {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	var mu sync.Mutex
	out := map[int]agentAddrs{}
	var g errgroup.Group
	g.SetLimit(reads)
	for _, e := range envs {
		for _, h := range e.Hosts {
			if !h.asksAgent() || ctx.Err() != nil {
				continue
			}
			g.Go(func() error {
				cctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				nics, err := p.API.AgentInterfaces(cctx, h.Node, h.VMID)
				if err != nil {
					return nil
				}
				mu.Lock()
				out[h.VMID] = agentAddrsOf(h, nics)
				mu.Unlock()
				return nil
			})
		}
	}
	_ = g.Wait() // no read fails the group
	return out
}
