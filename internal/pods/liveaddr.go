package pods

import (
	"context"
	"maps"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// An environment's VMs have the addresses battleship assigned them (EnvHost
// Addr), and the ones their guest agents report (LiveAddrs).

// guestAddr is the address the agent reports on the NIC with MAC mac, with
// its prefix length as GuestNIC.IPs has it: its first IPv4 address, else
// its first IPv6 one, skipping loopback, link-local, multicast and
// unspecified addresses. Its Addr is invalid if there is none.
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
			switch a := ip.Addr(); {
			case !isHostAddr(a):
			case a.Is4():
				return ip
			case !v6.Addr().IsValid():
				v6 = ip
			}
		}
	}
	return v6
}

// isHostAddr is whether a can be a host's own address.
func isHostAddr(a netip.Addr) bool {
	return !a.IsLoopback() && !a.IsLinkLocalUnicast() && !a.IsLinkLocalMulticast() && !a.IsMulticast() && !a.IsUnspecified() &&
		a != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

// externalAddr is the address of LAN host octet from outside, given the
// router's WAN address and prefix. It assumes the router image's one-to-one
// NAT of LAN .N to WAN /24 .N, which battleship neither configures nor
// checks, so there is none unless wan is an IPv4 /24 and octet 1 to 254.
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
	// Differs says LAN isn't its assigned address (EnvHost.Addr), if it
	// has one.
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

// addrs are e's hosts' addresses, by VMID, from what their agents reported
// (readAgents); a host missing from read isn't live. lan is the
// environments' LAN.
func (e Environment) addrs(read map[int]agentAddrs, lan config.LAN) map[int]LiveAddrs {
	out := map[int]LiveAddrs{}
	var wan netip.Prefix
	for _, h := range e.Hosts {
		got, live := read[h.VMID]
		a := LiveAddrs{Live: live}
		if live {
			if got.lan.Addr().IsValid() {
				a.LAN = got.lan.Addr().String()
			}
			a.Differs = h.Addr != "" && a.LAN != h.Addr
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
			octet, _ = lan.Octet(lanAddr)
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
// agents, reads at a time (at least one), each read bounded by timeout and
// all by ctx. A VM whose agent isn't asked (asksAgent), or whose read fails
// or isn't made before ctx ends, isn't live.
func (p Planner) LiveAddrs(ctx context.Context, envs []Environment, reads int, timeout time.Duration) map[int]LiveAddrs {
	read := p.readAgents(ctx, envs, reads, timeout)
	out := map[int]LiveAddrs{}
	for _, e := range envs {
		maps.Copy(out, e.addrs(read, p.Cfg.Environments.Subnet))
	}
	return out
}

// readAgents reads the addresses of envs' hosts whose agents may report
// them (asksAgent), as LiveAddrs says. Each answer is cut to its addresses
// as it comes, so no more than reads answers are held at once.
func (p Planner) readAgents(ctx context.Context, envs []Environment, reads int, timeout time.Duration) map[int]agentAddrs {
	var mu sync.Mutex
	out := map[int]agentAddrs{}
	var g errgroup.Group
	g.SetLimit(max(reads, 1))
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
				out[h.VMID] = agentAddrs{lan: guestAddr(nics, h.LANMAC), wan: guestAddr(nics, h.WANMAC)}
				mu.Unlock()
				return nil
			})
		}
	}
	_ = g.Wait() // no read fails the group
	return out
}
