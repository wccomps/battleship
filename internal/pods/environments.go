package pods

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// Slot is a dev slot: the pool and bridges an environment is built in, and
// its block of VMIDs.
type Slot struct {
	N         int
	Name      string // two digits
	Pool      string
	IntBridge string
	ExtBridge string
	FirstVMID int
}

// LastVMID is the last VMID of the slot's block.
func (s Slot) LastVMID() int { return s.FirstVMID + 99 }

// ExpandSlot replaces {slot} with n as two digits.
func ExpandSlot(pattern string, n int) string {
	return strings.ReplaceAll(pattern, "{slot}", SlotName(n))
}

// SlotName is slot n's name: n as two digits.
func SlotName(n int) string { return fmt.Sprintf("%02d", n) }

// ParseSlotName is the slot number name stands for, if SlotName writes it
// so: "02" is 2, while "2" and "002" are no slot's name.
func ParseSlotName(name string) (int, bool) {
	n, err := strconv.Atoi(name)
	return n, err == nil && n >= 1 && SlotName(n) == name
}

// slotLock is the lock key of the slot named name.
func slotLock(name string) string { return "slot:" + name }

// DevSlot is the environment plan's slot name: its Slot, or the slot its
// lock names in a plan stored before Slot.
func (p Plan) DevSlot() string {
	if p.Slot != "" {
		return p.Slot
	}
	for _, l := range p.Locks {
		if name, ok := strings.CutPrefix(l, slotLock("")); ok {
			if _, ok := ParseSlotName(name); ok {
				return name
			}
		}
	}
	return ""
}

// Slots lists the configured slots; the config is validated, so the range
// parses.
func Slots(c config.Environments) []Slot {
	lo, hi, _ := config.ParseRange(c.Slots)
	var out []Slot
	for n := lo; n <= hi; n++ {
		out = append(out, Slot{N: n, Name: SlotName(n), Pool: ExpandSlot(c.Pool, n),
			IntBridge: ExpandSlot(c.IntBridge, n), ExtBridge: ExpandSlot(c.ExtBridge, n), FirstVMID: c.VMIDBase + n*100})
	}
	return out
}

// Domain is the part of a VM name after its host: "teak.kilo.alpha" ->
// "kilo.alpha".
func Domain(vmName string) string {
	_, d, _ := strings.Cut(vmName, ".")
	return d
}

// EnvHost is a VM in a slot's pool.
type EnvHost struct {
	Name     string
	Host     string
	VMID     int
	Node     string
	Status   string
	Tags     string
	Router   bool
	Notes    Notes
	HasNotes bool
	// UserNotes are its other notes, such as what its base image's notes
	// carried over, less their details block, which Details holds; all of
	// them when Unreadable says the block is (see ParseDetails).
	UserNotes  string
	Details    ImageDetails
	Unreadable bool
	// Addr is its address: the gateway for the router, else its notes'.
	Addr netip.Addr
	// LANMAC and WANMAC are its NICs' MACs: a host's LAN NIC is net0; the
	// router's WAN NIC is net0 and its LAN NIC net1.
	LANMAC, WANMAC string
	Agent          bool // its config enables the guest agent
}

// Environment is what a slot holds.
type Environment struct {
	Slot   Slot
	Domain string    // the domain its VMs share; "" when free or Mixed
	Hosts  []EnvHost // the VMs in its pool, by name
	// Users are VMs outside its pool with a NIC on one of its bridges.
	Users []string
	Mixed bool // its VMs don't all share one domain
	LAN   config.LAN
	// HalfDeleted are VMs, in its pool or else its VMIDs, that a failed
	// destroy left locked as destroyed, without their names. Only tearing it
	// down acts on them; they aren't Hosts.
	HalfDeleted []EnvHost
}

// EnvByDomain is the environment, of envs, whose hosts share domain (the
// lowest slot if two do). Environments leaves a mixed one's Domain empty.
func EnvByDomain(envs []Environment, domain string) (Environment, bool) {
	for _, e := range envs {
		if domain != "" && e.Domain == domain {
			return e, true
		}
	}
	return Environment{}, false
}

func slotN(envs []Environment, n int) (Environment, error) {
	i := slices.IndexFunc(envs, func(e Environment) bool { return e.Slot.N == n })
	if i < 0 {
		return Environment{}, fmt.Errorf("there's no dev slot %s", SlotName(n))
	}
	return envs[i], nil
}

// Gateway is its router's address on its LAN.
func (e Environment) Gateway() netip.Addr { return e.LAN.Host(GatewayOctet) }

func (e Environment) Free() bool {
	return len(e.Hosts) == 0 && len(e.Users) == 0 && len(e.HalfDeleted) == 0
}

func (e Environment) HasRouter() bool { return e.RouterName() != "" }

// RouterName is its router's VM name, or "" if it has none.
func (e Environment) RouterName() string {
	for _, h := range e.Hosts {
		if h.Router {
			return h.Name
		}
	}
	return ""
}

// servesDNS is whether the host is one the guest agent configures (Windows
// or Linux) with an address: one its environment's DNS can point at.
func (h EnvHost) servesDNS() bool {
	return h.Notes.IP.IsValid() && (h.Notes.OS == ImageWindows || h.Notes.OS == ImageLinux)
}

// dnsHost is the host a DNS choice other than DNSRouter names, if it can
// serve DNS. DNSRouter always means the gateway, even beside a host so named.
func (e Environment) dnsHost(choice string) (EnvHost, bool) {
	if choice == DNSRouter || CheckHostname(choice) != nil {
		return EnvHost{}, false
	}
	h, ok := e.Host(choice + "." + e.Domain)
	return h, ok && h.servesDNS()
}

// DNSAddress is the address its hosts use for DNS choice: the gateway for
// DNSRouter (or ""), else the address of the host dnsHost accepts.
func (e Environment) DNSAddress(choice string) (netip.Addr, error) {
	if choice == "" || choice == DNSRouter {
		return e.Gateway(), nil
	}
	h, ok := e.dnsHost(choice)
	if !ok {
		return netip.Addr{}, fmt.Errorf("%s isn't a Windows or Linux host of %s with an address; choose router or one of those", choice, e.Domain)
	}
	return h.Addr, nil
}

// DNSChoices are what its DNS can be set to: DNSRouter, then each host
// dnsHost accepts, by name.
func (e Environment) DNSChoices() []string {
	out := []string{DNSRouter}
	for _, h := range e.Hosts {
		if name := strings.TrimSuffix(h.Name, "."+e.Domain); name != h.Name {
			if _, ok := e.dnsHost(name); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

func (e Environment) Host(name string) (EnvHost, bool) {
	for _, h := range e.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return EnvHost{}, false
}

// UsedOctets are the host octets its VMs' notes claim.
func (e Environment) UsedOctets() map[int]bool {
	used := map[int]bool{}
	for _, h := range e.Hosts {
		if o, ok := e.LAN.Octet(h.Notes.IP); ok {
			used[o] = true
		}
	}
	return used
}

// DNS is the environment's DNS choice from its notes: a host name, or
// DNSRouter. The router's own notes don't count.
func (e Environment) DNS() string {
	for _, h := range e.Hosts {
		if !h.Router && h.Notes.DNS != "" {
			return h.Notes.DNS
		}
	}
	return DNSRouter
}

// envReads is how many Proxmox reads environment planning makes at once.
const envReads = 8

// Environments reads every slot: the VMs in its pool with their notes, the
// other VMs on its bridges, and its half-deleted VMs. Templates never run,
// so they hold no slot; team VMs (see Classify) are on team bridges, so
// they aren't read, but a VM only named like one may be anywhere. It also
// returns the VM listing it read.
func (p Planner) Environments(ctx context.Context) ([]Environment, []proxmox.VM, error) {
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		return nil, nil, err
	}
	slots := Slots(p.Cfg.Environments)
	byPool := map[string]int{}
	bridgeSlot := map[string]int{}
	envs := make([]Environment, len(slots))
	for i, s := range slots {
		envs[i].Slot, envs[i].LAN = s, p.Cfg.Environments.Subnet
		byPool[s.Pool] = i
		bridgeSlot[s.IntBridge], bridgeSlot[s.ExtBridge] = i, i
	}
	var read []proxmox.VM
	for _, vm := range vms {
		si, inSlot := byPool[vm.Pool]
		if vm.Lock == "destroyed" {
			if !inSlot {
				si = slices.IndexFunc(slots, func(s Slot) bool { return vm.VMID >= s.FirstVMID && vm.VMID <= s.LastVMID() })
			}
			if si >= 0 { // its config is only the lock
				envs[si].HalfDeleted = append(envs[si].HalfDeleted, EnvHost{Name: vm.Name, Host: Hostname(vm.Name), VMID: vm.VMID, Node: vm.Node, Status: vm.Status})
			}
			continue
		}
		if kind, _, _ := p.Naming.Classify(vm); inSlot || (!vm.Template && kind != TeamVMKind) {
			read = append(read, vm)
		}
	}
	cfgs := make([]map[string]string, len(read))
	var g errgroup.Group
	g.SetLimit(envReads)
	for i, vm := range read {
		g.Go(func() error {
			cfg, err := p.API.VMConfig(ctx, vm.Node, vm.VMID)
			switch {
			case proxmox.IsNotFound(err):
				return nil // gone since the listing
			case err != nil:
				return fmt.Errorf("reading %s (%d): %w", vm.Name, vm.VMID, err)
			}
			cfgs[i] = cfg
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	for i, vm := range read {
		cfg := cfgs[i]
		if cfg == nil {
			continue
		}
		if si, ok := byPool[vm.Pool]; ok {
			n, has := ParseNotes(cfg["description"])
			n = n.Checked(p.Cfg.Environments.Subnet)
			h := EnvHost{Name: vm.Name, Host: Hostname(vm.Name), VMID: vm.VMID, Node: vm.Node, Status: vm.Status, Tags: vm.Tags,
				Router: slices.Contains(Tags(vm.Tags), ImageRouter), Notes: n, HasNotes: has, Addr: n.IP}
			var derr error
			h.Details, h.UserNotes, derr = ParseDetails(cfg["description"])
			h.Unreadable = derr != nil
			h.LANMAC, _ = NICMAC(cfg["net0"])
			if h.Router {
				h.Addr = envs[si].Gateway()
				h.WANMAC = h.LANMAC
				h.LANMAC, _ = NICMAC(cfg["net1"])
			}
			h.Agent = agentEnabled(cfg["agent"])
			envs[si].Hosts = append(envs[si].Hosts, h)
			continue
		}
		if vm.Template {
			continue
		}
		for _, b := range nicBridgesAll(cfg) {
			if si, ok := bridgeSlot[b]; ok && !slices.Contains(envs[si].Users, vm.Name) {
				envs[si].Users = append(envs[si].Users, vm.Name)
			}
		}
	}
	for i := range envs {
		e := &envs[i]
		sort.Slice(e.Hosts, func(a, b int) bool { return e.Hosts[a].Name < e.Hosts[b].Name })
		sort.Strings(e.Users)
		slices.SortFunc(e.HalfDeleted, func(a, b EnvHost) int { return a.VMID - b.VMID })
		for _, h := range e.Hosts {
			d := Domain(h.Name)
			switch {
			case d == "" || (e.Domain != "" && d != e.Domain):
				e.Mixed = true
			case e.Domain == "":
				e.Domain = d
			}
		}
		if e.Mixed {
			e.Domain = ""
		}
	}
	return envs, vms, nil
}

// nicBridgesAll lists the bridge of every NIC (net0 to net31).
func nicBridgesAll(cfg map[string]string) []string {
	var out []string
	for i := range 32 {
		if v, ok := cfg[fmt.Sprintf("net%d", i)]; ok {
			if _, opts := splitOptions(v); opts["bridge"] != "" {
				out = append(out, opts["bridge"])
			}
		}
	}
	return out
}
