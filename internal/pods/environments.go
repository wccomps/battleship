package pods

import (
	"cmp"
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

// wantNICs is the NIC-to-bridge map for a host in s.
func (s Slot) wantNICs(router bool) map[string]string {
	if router {
		return map[string]string{"net0": s.ExtBridge, "net1": s.IntBridge}
	}
	return map[string]string{"net0": s.IntBridge}
}

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
	Name   string
	Host   string
	VMID   int
	Node   string
	Status string
	Tags   string
	Router bool
	Notes  Notes
	// UserNotes are its other notes, such as what its base image's notes
	// carried over, less their details block, which Details holds; all of
	// them when Unreadable says the block is (see ParseDetails).
	UserNotes  string
	Details    ImageDetails
	Unreadable bool
	// Addr is the address its notes record within the subnet.
	Addr netip.Addr
	// LANMAC and WANMAC are its NICs' MACs: a host's LAN NIC is net0; a
	// router's WAN and LAN NICs are those on the slot's external and
	// internal bridges, or net0 and net1.
	LANMAC, WANMAC string
	Agent          bool              // its config enables the guest agent
	CDROM          bool              // a non-cloudinit CD-ROM drive has media attached
	CPUModels      []string          // custom CPU models on its node
	Config         map[string]string // its Proxmox config as read
	Warnings       []string          // discrepancies between its config or tags and its slot
	desc           string            // its notes, for the plans that write them
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

// EnvHostByID is the environment and host with vmid across envs.
func EnvHostByID(envs []Environment, vmid int) (Environment, EnvHost, bool) {
	for _, e := range envs {
		for _, h := range e.Hosts {
			if h.VMID == vmid {
				return e, h, true
			}
		}
	}
	return Environment{}, EnvHost{}, false
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
	cpuByNode := map[string][]string{}
	for i, vm := range read {
		cfg := cfgs[i]
		if cfg == nil {
			continue
		}
		if si, ok := byPool[vm.Pool]; ok {
			cpuModels, seen := cpuByNode[vm.Node]
			if !seen {
				cpuModels = p.customCPUModels(ctx, vm.Node)
				cpuByNode[vm.Node] = cpuModels
			}
			envs[si].Hosts = append(envs[si].Hosts, p.buildEnvHost(vm, cfg, envs[si].Slot, cpuModels))
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
		sort.Strings(e.Users)
		slices.SortFunc(e.HalfDeleted, func(a, b EnvHost) int { return a.VMID - b.VMID })
		setEnvDomain(e)
	}
	return envs, vms, nil
}

func (p Planner) buildEnvHost(vm proxmox.VM, cfg map[string]string, slot Slot, cpuModels []string) EnvHost {
	n, _ := ParseNotes(cfg["description"])
	n = n.Checked(p.Cfg.Environments.Subnet)
	h := EnvHost{Name: vm.Name, Host: Hostname(vm.Name), VMID: vm.VMID, Node: vm.Node, Status: vm.Status, Tags: vm.Tags,
		Router: slices.Contains(Tags(vm.Tags), ImageRouter), Notes: n, Addr: n.IP, desc: cfg["description"]}
	var derr error
	h.Details, h.UserNotes, derr = ParseDetails(cfg["description"])
	h.Unreadable = derr != nil
	h.LANMAC, _ = NICMAC(cfg["net0"])
	if h.Router {
		h.WANMAC, h.LANMAC = routerMACs(cfg, slot)
	}
	h.Agent = agentEnabled(cfg["agent"])
	h.CDROM = len(CDROMChanges(cfg)) > 0
	h.CPUModels = cpuModels
	h.Config = cfg
	h.Warnings = p.hostWarnings(vm, cfg, slot, h.Router, cpuModels)
	return h
}

func setEnvDomain(e *Environment) {
	sort.Slice(e.Hosts, func(a, b int) bool { return e.Hosts[a].Name < e.Hosts[b].Name })
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

func nicBridge(nic string) string {
	_, opts := splitOptions(nic)
	return opts["bridge"]
}

// routerMACs returns a router's WAN and LAN MAC addresses by matching its
// NICs' bridges to the slot's external and internal bridges, falling back to
// net0 and net1.
func routerMACs(cfg map[string]string, s Slot) (wan, lan string) {
	for i := range 32 {
		v := cfg[fmt.Sprintf("net%d", i)]
		mac, ok := NICMAC(v)
		if !ok {
			continue
		}
		switch nicBridge(v) {
		case s.ExtBridge:
			if s.ExtBridge != "" && wan == "" {
				wan = mac
			}
		case s.IntBridge:
			if s.IntBridge != "" && lan == "" {
				lan = mac
			}
		}
	}
	if wan == "" && (s.IntBridge == "" || nicBridge(cfg["net0"]) != s.IntBridge) {
		wan, _ = NICMAC(cfg["net0"])
	}
	if lan == "" && (s.ExtBridge == "" || nicBridge(cfg["net1"]) != s.ExtBridge) {
		lan, _ = NICMAC(cfg["net1"])
	}
	return wan, lan
}

// nicBridgesAll lists the bridge of every NIC (net0 to net31).
func nicBridgesAll(cfg map[string]string) []string {
	var out []string
	for i := range 32 {
		if b := nicBridge(cfg[fmt.Sprintf("net%d", i)]); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// CPUType is a VM config's CPU model ("cputype=…", or the head before ',').
func CPUType(raw string) string {
	head, opts := splitOptions(raw)
	if t := opts["cputype"]; t != "" {
		return t
	}
	return strings.TrimPrefix(head, "cputype=")
}

// CPUModel is the model in cpuTypes that raw ("cputype=…", "custom-<model>",
// or "<model>") matches, or "" when it matches none.
func CPUModel(raw string, cpuTypes []string) string {
	got := CPUType(raw)
	for _, want := range cpuTypes {
		if got == want || got == "custom-"+want {
			return want
		}
	}
	return ""
}

// cpuWarning returns a discrepancy when raw's CPU model isn't one of
// cpuTypes (or "custom-"+t), or "" when cpuTypes is empty or matches.
func cpuWarning(raw string, cpuTypes []string) string {
	if len(cpuTypes) == 0 || CPUModel(raw, cpuTypes) != "" {
		return ""
	}
	got := cmp.Or(CPUType(raw), "default (kvm64)")
	return fmt.Sprintf("CPU %s, expected %s", got, strings.Join(cpuTypes, " or "))
}

// vlanWarnings reports any NIC with a VLAN tag.
func vlanWarnings(cfg map[string]string) []string {
	var out []string
	for i := range 32 {
		k := fmt.Sprintf("net%d", i)
		if _, opts := splitOptions(cfg[k]); opts["tag"] != "" {
			out = append(out, fmt.Sprintf("%s has VLAN tag %s", k, opts["tag"]))
		}
	}
	return out
}

// slotNICWarnings checks a VM's NIC count, bridges (when s has bridges), and
// VLAN tags.
func slotNICWarnings(cfg map[string]string, s Slot, router bool) []string {
	var out []string
	n := CountInterfaces(cfg)
	hasHW := n > 0 || len(DiskVolumes(cfg)) > 0
	if router {
		if hasHW {
			if cfg["net0"] == "" || cfg["net1"] == "" {
				out = append(out, "missing router NIC (needs net0 and net1)")
			} else if s.ExtBridge != "" || s.IntBridge != "" {
				if b := nicBridge(cfg["net0"]); b != s.ExtBridge {
					out = append(out, fmt.Sprintf("net0 on %s, expected %s", cmp.Or(b, "no bridge"), s.ExtBridge))
				}
				if b := nicBridge(cfg["net1"]); b != s.IntBridge {
					out = append(out, fmt.Sprintf("net1 on %s, expected %s", cmp.Or(b, "no bridge"), s.IntBridge))
				}
			}
		}
		if n > 2 {
			out = append(out, fmt.Sprintf("%d NICs, expected 2", n))
		}
	} else {
		if hasHW {
			if cfg["net0"] == "" {
				out = append(out, "no net0 NIC")
			} else if s.IntBridge != "" {
				if b := nicBridge(cfg["net0"]); b != s.IntBridge {
					out = append(out, fmt.Sprintf("net0 on %s, expected %s", cmp.Or(b, "no bridge"), s.IntBridge))
				}
			}
		}
		if n > 1 {
			out = append(out, fmt.Sprintf("%d NICs, expected 1", n))
		}
	}
	return append(out, vlanWarnings(cfg)...)
}

// CPUModelReader is the optional part of an API that lists a node's custom
// QEMU CPU models.
type CPUModelReader interface {
	CustomCPUModels(ctx context.Context, node string) ([]string, error)
}

func (p Planner) customCPUModels(ctx context.Context, node string) []string {
	r, ok := p.API.(CPUModelReader)
	if !ok || node == "" {
		return nil
	}
	models, err := r.CustomCPUModels(ctx, node)
	if err != nil {
		return nil
	}
	return models
}

func vmHardwareWarnings(cfg map[string]string, cpuModels []string, warnNoAgent bool) []string {
	var out []string
	if w := cpuWarning(cfg["cpu"], cpuModels); w != "" {
		out = append(out, w)
	}
	if warnNoAgent && !agentEnabled(cfg["agent"]) {
		out = append(out, "guest agent off")
	}
	if len(CDROMChanges(cfg)) > 0 {
		out = append(out, "CD-ROM attached")
	}
	return out
}

// hostWarnings checks a slot VM's tags and config for discrepancies.
func (p Planner) hostWarnings(vm proxmox.VM, cfg map[string]string, s Slot, router bool, cpuModels []string) []string {
	var out []string
	tags := Tags(vm.Tags)
	var missing []string
	if p.Cfg.Deploy.MasterTag != "" && !slices.Contains(tags, p.Cfg.Deploy.MasterTag) {
		missing = append(missing, p.Cfg.Deploy.MasterTag)
	}
	if d := Domain(vm.Name); d != "" && CheckDomain(d) == nil && !slices.Contains(tags, d) {
		missing = append(missing, d)
	}
	if len(missing) == 1 {
		out = append(out, "missing tag "+missing[0])
	} else if len(missing) > 1 {
		out = append(out, "missing tags "+strings.Join(missing, ", "))
	}
	out = append(out, slotNICWarnings(cfg, s, router)...)
	return append(out, vmHardwareWarnings(cfg, cpuModels, cfg["agent"] != "" || len(DiskVolumes(cfg)) > 0)...)
}

// masterWarnings checks a deploy master VM's config for discrepancies.
func (p Planner) masterWarnings(vm proxmox.VM, cfg map[string]string, slotByPool map[string]Slot, cpuModels []string) []string {
	var out []string
	slot, inSlot := slotByPool[vm.Pool]
	if inSlot {
		router := slices.Contains(Tags(vm.Tags), ImageRouter)
		out = append(out, slotNICWarnings(cfg, slot, router)...)
	} else {
		out = append(out, vlanWarnings(cfg)...)
	}
	return append(out, vmHardwareWarnings(cfg, cpuModels, cfg["agent"] != "" || (inSlot && len(DiskVolumes(cfg)) > 0))...)
}

// registerWarnings checks a register source VM's config for discrepancies
// that don't block registration.
func (p Planner) registerWarnings(cfg map[string]string, kind string, cpuModels []string) []string {
	out := slotNICWarnings(cfg, Slot{}, kind == ImageRouter)
	warnNoAgent := (kind == ImageRouter || kind == ImageOther) && (cfg["agent"] != "" || len(DiskVolumes(cfg)) > 0)
	return append(out, vmHardwareWarnings(cfg, cpuModels, warnNoAgent)...)
}
