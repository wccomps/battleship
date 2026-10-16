package pods

import (
	"context"
	"fmt"
	"slices"
	"sort"
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
	FirstVMID int // the block is FirstVMID to FirstVMID+99
}

// ExpandSlot replaces {slot} with n as two digits.
func ExpandSlot(pattern string, n int) string {
	return strings.ReplaceAll(pattern, "{slot}", fmt.Sprintf("%02d", n))
}

// Slots lists the configured slots; the config is validated, so the range
// parses.
func Slots(c config.Environments) []Slot {
	lo, hi, _ := config.ParseRange(c.Slots)
	var out []Slot
	for n := lo; n <= hi; n++ {
		out = append(out, Slot{N: n, Name: fmt.Sprintf("%02d", n), Pool: ExpandSlot(c.Pool, n),
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
	// UserNotes are its other notes, such as the logins its base image's
	// notes carried over.
	UserNotes string
}

// Environment is what a slot holds.
type Environment struct {
	Slot   Slot
	Domain string    // the domain its VMs share; "" when free or Mixed
	Hosts  []EnvHost // the VMs in its pool, by name
	// Users are VMs outside its pool with a NIC on one of its bridges.
	Users []string
	Mixed bool // its VMs don't all share one domain
}

func (e Environment) Free() bool { return len(e.Hosts) == 0 && len(e.Users) == 0 }

func (e Environment) HasRouter() bool {
	return slices.ContainsFunc(e.Hosts, func(h EnvHost) bool { return h.Router })
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
		if o := LastOctet(h.Notes.IP); o != 0 {
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

// envReads is how many VM configs Environments reads at once.
const envReads = 8

// Environments reads every slot: the VMs in its pool with their notes, and
// the other VMs on its bridges. Templates never run, so they hold no slot;
// team VMs are on team bridges, so they aren't read. It also returns the
// VM listing it read.
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
		envs[i].Slot = s
		byPool[s.Pool] = i
		bridgeSlot[s.IntBridge], bridgeSlot[s.ExtBridge] = i, i
	}
	var read []proxmox.VM
	for _, vm := range vms {
		_, inSlot := byPool[vm.Pool]
		if kind, _, _ := p.Naming.Classify(vm); inSlot || (!vm.Template && kind == NotTeamNamed) {
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
			envs[si].Hosts = append(envs[si].Hosts, EnvHost{Name: vm.Name, Host: Hostname(vm.Name), VMID: vm.VMID, Node: vm.Node,
				Status: vm.Status, Tags: vm.Tags, Router: slices.Contains(Tags(vm.Tags), ImageRouter), Notes: n, HasNotes: has, UserNotes: UserNotes(cfg["description"])})
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
