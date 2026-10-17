package pods

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/proxmox"
)

// An environment's VMs have the addresses battleship assigned them (EnvHost
// Addr), and the ones their guest agents report. The router's WAN NIC (net0)
// takes an address on its slot's external bridge by DHCP, and the router
// maps each LAN host <subnet>.N one to one to <WAN /24>.N: the host's
// external address.

const (
	// AddrReads is how many guest agent reads of addresses run at once.
	AddrReads = 4
	// AddrReadTimeout bounds one read, far below Proxmox's own agent timeout
	// (about 10 s): a VM whose agent doesn't answer at once goes without its
	// live address rather than holding up the page.
	AddrReadTimeout = 2 * time.Second
	// AddrReadsWithin bounds all of an environment page's reads.
	AddrReadsWithin = 5 * time.Second
)

// maxGuestNICs and maxGuestIPs bound how much of an agent's answer, which
// the guest writes, is looked at.
const (
	maxGuestNICs = 64
	maxGuestIPs  = 64
)

// GuestAddr is the address the agent reports on the NIC with MAC mac: its
// first IPv4 address, else its first IPv6 one, skipping loopback,
// link-local, multicast and unspecified addresses; "" if there is none. It
// is in canonical form, whatever the guest wrote.
func GuestAddr(nics []proxmox.GuestNIC, mac string) string {
	if mac == "" {
		return ""
	}
	var v6 netip.Addr
	for i, n := range nics {
		if i == maxGuestNICs {
			break
		}
		if !strings.EqualFold(n.MAC, mac) {
			continue
		}
		for j, ip := range n.IPs {
			if j == maxGuestIPs {
				break
			}
			a, ok := hostAddr(ip)
			switch {
			case !ok:
			case a.Is4():
				return a.String()
			case !v6.IsValid():
				v6 = a
			}
		}
	}
	if v6.IsValid() {
		return v6.String()
	}
	return ""
}

// hostAddr is the address of an agent's "address/prefix", if it can be a
// host's own address.
func hostAddr(s string) (netip.Addr, bool) {
	addr, _, _ := strings.Cut(s, "/")
	a, err := netip.ParseAddr(addr)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() ||
		a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return netip.Addr{}, false
	}
	return a, true
}

// ExternalAddr is the router's one-to-one NAT address of LAN host octet:
// wan's /24 with octet as its last octet. It is "" unless wan is an IPv4
// address and octet a host's (1 to 254).
func ExternalAddr(wan string, octet int) string {
	a, err := netip.ParseAddr(wan)
	if err != nil || !a.Is4() || octet < 1 || octet > 254 {
		return ""
	}
	b := a.As4()
	b[3] = byte(octet)
	return netip.AddrFrom4(b).String()
}

// LiveAddrs are a VM's addresses as its guest agent reports them.
type LiveAddrs struct {
	// Live says its agent answered; the rest are empty unless it did, but
	// External.
	Live bool
	LAN  string // on its LAN NIC (the router's net1, a host's net0)
	// Differs says LAN isn't its assigned address (EnvHost.Addr).
	Differs bool
	WAN     string // the router's, on its WAN NIC (net0)
	// External is a host's address from outside, through the router's NAT
	// (ExternalAddr): by its live LAN octet, else its assigned one. It is
	// "" without the router's WAN address, and for a live LAN address
	// outside the environment's subnet, which the NAT doesn't map.
	External string
}

// Addrs are its hosts' addresses, by VMID, from the interfaces their
// agents reported (GuestNICs); a host missing from nics isn't live.
func (e Environment) Addrs(nics map[int][]proxmox.GuestNIC) map[int]LiveAddrs {
	out := map[int]LiveAddrs{}
	wan := ""
	for _, h := range e.Hosts {
		got, live := nics[h.VMID]
		a := LiveAddrs{Live: live}
		if live {
			a.LAN = GuestAddr(got, h.LANMAC)
			a.Differs = a.LAN != h.Addr
			if h.Router {
				a.WAN = GuestAddr(got, h.WANMAC)
				if wan == "" {
					wan = a.WAN
				}
			}
		}
		out[h.VMID] = a
	}
	subnet := strings.TrimSuffix(e.Gateway, "."+strconv.Itoa(GatewayOctet)) + "."
	for _, h := range e.Hosts {
		a := out[h.VMID]
		if h.Router || wan == "" {
			continue
		}
		octet := LastOctet(h.Addr)
		if lan, err := netip.ParseAddr(a.LAN); err == nil && lan.Is4() {
			octet = 0
			if rest, ok := strings.CutPrefix(a.LAN, subnet); ok {
				octet, _ = strconv.Atoi(rest)
			}
		}
		a.External = ExternalAddr(wan, octet)
		out[h.VMID] = a
	}
	return out
}

// asksAgent is whether h's agent may report its addresses: it is running,
// with its agent enabled and a LAN NIC to look on.
func (h EnvHost) asksAgent() bool {
	return h.Status == "running" && h.Agent && (h.LANMAC != "" || h.WANMAC != "")
}

// GuestNICs reads the interfaces of envs' hosts through their guest agents,
// AddrReads at a time, each read bounded by timeout and all of them by
// within. A VM that isn't running, has no agent enabled, or whose read
// fails (a user without VM.GuestAgent.Audit, an agent that doesn't answer
// in time) is left out.
func (p Planner) GuestNICs(ctx context.Context, envs []Environment, timeout, within time.Duration) map[int][]proxmox.GuestNIC {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	var mu sync.Mutex
	out := map[int][]proxmox.GuestNIC{}
	var g errgroup.Group
	g.SetLimit(AddrReads)
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
				out[h.VMID] = nics
				mu.Unlock()
				return nil
			})
		}
	}
	_ = g.Wait() // no read fails the group
	return out
}
