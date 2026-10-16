package pods

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/proxmox"
)

// EnvRequest is an environment job's input.
type EnvRequest struct {
	Domain string
	Router HostSpec   // create (required); add (only if the environment has none)
	Hosts  []HostSpec // create, add
	DNS    string     // set DNS: DNSRouter or one of the environment's host names
}

func (p Planner) gateway() string {
	return p.Cfg.Environments.Subnet + "." + strconv.Itoa(GatewayOctet)
}

// dnsAddress is the address hosts use for choice.
func (p Planner) dnsAddress(env Environment, choice string) (string, error) {
	if choice == "" || choice == DNSRouter {
		return p.gateway(), nil
	}
	h, ok := env.Host(choice + "." + env.Domain)
	if !ok || h.Notes.IP == "" || (h.Notes.OS != ImageWindows && h.Notes.OS != ImageLinux) {
		return "", fmt.Errorf("%s isn't a Windows or Linux host of %s with an address; choose router or one of those", choice, env.Domain)
	}
	return h.Notes.IP, nil
}

// EnvCreate plans a new environment in the lowest free slot.
func (p Planner) EnvCreate(ctx context.Context, req EnvRequest) (*Plan, error) {
	if err := CheckDomain(req.Domain); err != nil {
		return nil, err
	}
	if req.Router.Host == "" {
		return nil, errors.New("a new environment needs a router")
	}
	envs, vms, err := p.Environments(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range envs {
		if e.Domain == req.Domain {
			return nil, fmt.Errorf("%s is already in dev slot %s; add hosts to it instead", req.Domain, e.Slot.Name)
		}
	}
	var slot *Environment
	var hidden []string // slots that look free but hold VMs this user can't see
	for i := range envs {
		if !envs[i].Free() {
			continue
		}
		held, err := p.slotHeld(ctx, envs[i].Slot, vms)
		if err != nil {
			return nil, err
		}
		if !held {
			slot = &envs[i]
			break
		}
		hidden = append(hidden, envs[i].Slot.Name)
	}
	if slot == nil {
		msg := "every dev slot is in use; archive an environment to free one"
		if len(hidden) > 0 {
			msg += fmt.Sprintf(" (dev slot %s holds VMs you can't see)", strings.Join(hidden, ", "))
		}
		return nil, errors.New(msg)
	}
	env := *slot
	env.Domain = req.Domain
	return p.envPlan(ctx, KindEnvCreate, env, append([]HostSpec{req.Router}, req.Hosts...), vms)
}

// slotHeld is whether any VMID in slot's block exists, listed or not: what a
// user may see of a slot depends on their rights, but Proxmox reports any
// VMID in use, so an environment never lands in a slot someone else uses.
func (p Planner) slotHeld(ctx context.Context, slot Slot, vms []proxmox.VM) (bool, error) {
	last := slot.FirstVMID + 99
	if slices.ContainsFunc(vms, func(vm proxmox.VM) bool { return vm.VMID >= slot.FirstVMID && vm.VMID <= last }) {
		return true, nil
	}
	for id := slot.FirstVMID; id <= last; id++ {
		held, err := p.API.VMIDHeld(ctx, id)
		if err != nil {
			return false, fmt.Errorf("checking whether dev slot %s is free (VMID %d): %w", slot.Name, id, err)
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

// EnvAdd plans adding hosts to the environment with req.Domain. Hosts that
// already exist there are resumed.
func (p Planner) EnvAdd(ctx context.Context, req EnvRequest) (*Plan, error) {
	env, vms, err := p.findEnv(ctx, req.Domain)
	if err != nil {
		return nil, err
	}
	hosts := req.Hosts
	if req.Router.Host != "" {
		// The existing router may be named again, to resume a failed create.
		if _, exists := env.Host(req.Router.Host + "." + env.Domain); env.HasRouter() && !exists {
			return nil, fmt.Errorf("%s already has a router", req.Domain)
		}
		hosts = append([]HostSpec{req.Router}, hosts...)
	}
	if len(hosts) == 0 {
		return nil, errors.New("name at least one host to add")
	}
	return p.envPlan(ctx, KindEnvAdd, env, hosts, vms)
}

func (p Planner) findEnv(ctx context.Context, domain string) (Environment, []proxmox.VM, error) {
	if err := CheckDomain(domain); err != nil {
		return Environment{}, nil, err
	}
	envs, vms, err := p.Environments(ctx)
	if err != nil {
		return Environment{}, nil, err
	}
	for _, e := range envs {
		if e.Domain == domain {
			return e, vms, nil
		}
	}
	return Environment{}, nil, fmt.Errorf("no dev slot holds %s; create it first", domain)
}

// envPlan plans hosts into env. The first router-kind host gets the slot's
// WAN and LAN bridges; every other host the LAN.
func (p Planner) envPlan(ctx context.Context, kind Kind, env Environment, hosts []HostSpec, vms []proxmox.VM) (*Plan, error) {
	images := BaseImages(vms)
	dnsChoice := env.DNS()
	dnsAddr, err := p.dnsAddress(env, dnsChoice)
	if err != nil {
		dnsChoice, dnsAddr = DNSRouter, p.gateway() // the chosen host is gone: fall back for new hosts
	}
	plan := &Plan{Kind: kind, Config: ConfigHash(kind, p.Cfg),
		Locks: []string{"slot:" + env.Slot.Name, "domain:" + env.Domain}}
	used := env.UsedOctets()
	claimed := map[int]string{} // octet -> host, from this request
	seen := map[string]bool{}
	taken := map[int]bool{}
	for _, h := range env.Hosts {
		taken[h.VMID] = true
	}
	imageCfg := map[int]map[string]string{}
	readImage := func(im BaseImage) (map[string]string, error) {
		if c, ok := imageCfg[im.VMID]; ok {
			return c, nil
		}
		c, err := p.API.VMConfig(ctx, im.Node, im.VMID)
		if err != nil {
			return nil, fmt.Errorf("reading base image %s: %w", im.Name, err)
		}
		imageCfg[im.VMID] = c
		return c, nil
	}
	// Explicit addresses first, so picked ones avoid them.
	for _, h := range hosts {
		if existing, ok := env.Host(h.Host + "." + env.Domain); ok && existing.Notes.IP != "" {
			continue // a resumed host keeps its address
		}
		if h.Octet != 0 && !used[h.Octet] && claimed[h.Octet] == "" {
			claimed[h.Octet] = h.Host
		}
	}
	for o := range claimed {
		used[o] = true
	}
	for _, h := range hosts {
		name := h.Host + "." + env.Domain
		it := Item{Name: name, Host: h.Host, Pool: env.Slot.Pool}
		plan.Items = append(plan.Items, it)
		ip := &plan.Items[len(plan.Items)-1]
		if seen[h.Host] {
			ip.Blocked = "named twice"
			continue
		}
		seen[h.Host] = true
		im, ok := Newest(images, h.Family)
		if !ok {
			ip.Blocked = fmt.Sprintf("no usable base image of family %q", h.Family)
			continue
		}
		ip.Source, ip.Node = &Source{VMID: im.VMID, Node: im.Node, Name: im.Name}, im.Node
		existing, resumed := env.Host(name)
		switch {
		case resumed:
			ip.VMID, ip.Node = existing.VMID, existing.Node
			if why := resumeMismatch(existing, im, h.Family); why != "" {
				ip.Blocked = name + " already exists " + why
				continue
			}
		case len(vmsNamed(vms, name)) > 0:
			ip.Blocked = "a VM named " + name + " already exists outside " + env.Slot.Pool
			continue
		default:
			id, err := p.freeVMID(ctx, vms, env.Slot.FirstVMID, env.Slot.FirstVMID+99, taken)
			if err != nil {
				ip.Blocked = "dev slot " + env.Slot.Name + " has " + err.Error()
				continue
			}
			ip.VMID, taken[id] = id, true
		}
		plan.Locks = append(plan.Locks, "vmid:"+strconv.Itoa(ip.VMID))
		cfg, err := readImage(im)
		if err != nil {
			return nil, err
		}
		ip.Source.Bridges = sourceBridges(cfg)
		tags := p.Cfg.Deploy.MasterTag + ";" + env.Domain
		notes := Notes{Base: im.Name, DNS: dnsChoice}
		if resumed {
			notes.Base = orString(existing.Notes.Base, im.Name)
		}
		switch im.Kind {
		case ImageRouter:
			if env.HasRouter() && !resumed || slices.ContainsFunc(plan.Items[:len(plan.Items)-1], func(o Item) bool { return o.NICs["net1"] != "" }) {
				ip.Blocked = "an environment has one router"
				continue
			}
			if cfg["net1"] == "" {
				ip.Blocked = "base image " + im.Name + " has one NIC; a router needs two (WAN on net0, LAN on net1)"
				continue
			}
			tags += ";" + ImageRouter
			ip.NICs = map[string]string{"net0": env.Slot.ExtBridge, "net1": env.Slot.IntBridge}
		default:
			if cfg["net0"] == "" {
				ip.Blocked = "base image " + im.Name + " has no net0"
				continue
			}
			ip.NICs = map[string]string{"net0": env.Slot.IntBridge}
		}
		ip.Tags = tags
		ip.Steps = []Step{StepClone, StepLabel, StepNetwork, StepStart}
		if resumed {
			ip.Steps = ip.Steps[1:]
		}
		if im.Kind == ImageWindows || im.Kind == ImageLinux {
			if im.Kind == ImageWindows && len(h.Host) > MaxWindowsHostname {
				ip.Blocked = fmt.Sprintf("Windows host names are at most %d characters", MaxWindowsHostname)
				continue
			}
			octet := LastOctet(existing.Notes.IP)
			switch {
			case resumed && octet != 0:
			case h.Octet != 0 && claimed[h.Octet] == h.Host:
				octet = h.Octet
			case h.Octet != 0:
				ip.Blocked = fmt.Sprintf("address .%d is already taken in %s", h.Octet, env.Domain)
				continue
			default:
				o, ok := PickOctet(env.Domain, h.Host, used)
				if !ok {
					ip.Blocked = "no free address left in " + env.Domain
					continue
				}
				octet, used[o] = o, true
			}
			addr := p.Cfg.Environments.Subnet + "." + strconv.Itoa(octet)
			notes.IP, notes.OS = addr, im.Kind
			ip.Guest = &Guest{OS: im.Kind, Hostname: h.Host, Domain: env.Domain, IP: addr, Prefix: HostPrefix, Gateway: p.gateway(), DNS: dnsAddr}
			if err := ip.Guest.Check(); err != nil {
				ip.Blocked = err.Error()
				continue
			}
			ip.Steps = append(ip.Steps, StepWaitAgent, StepGuest, StepVerify)
		}
		ip.Notes = &notes
	}
	return plan, nil
}

// resumeMismatch says how an existing host differs from the requested image,
// or "" when it can be resumed.
func resumeMismatch(existing EnvHost, im BaseImage, family string) string {
	kind := existing.Notes.OS
	if existing.Router {
		kind = ImageRouter
	}
	if kind != "" && kind != im.Kind || kind == "" && im.Kind != ImageOther {
		return "as a different kind of host than " + family + " (" + im.Kind + ")"
	}
	if f, _, ok := ParseImageName(existing.Notes.Base); ok && f != family {
		return "from image family " + f + ", not " + family
	}
	return ""
}

func orString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// EnvDNS plans pointing every Windows and Linux host of an environment at
// req.DNS, and recording the choice on every host.
func (p Planner) EnvDNS(ctx context.Context, req EnvRequest) (*Plan, error) {
	env, _, err := p.findEnv(ctx, req.Domain)
	if err != nil {
		return nil, err
	}
	choice := strings.TrimSpace(req.DNS)
	if choice == "" {
		choice = DNSRouter
	}
	addr, err := p.dnsAddress(env, choice)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Kind: KindEnvDNS, Config: ConfigHash(KindEnvDNS, p.Cfg),
		Locks: []string{"slot:" + env.Slot.Name, "domain:" + env.Domain}}
	for _, h := range env.Hosts {
		notes := h.Notes
		notes.DNS = choice
		it := Item{Name: h.Name, Host: h.Host, VMID: h.VMID, Node: h.Node, Tags: h.Tags, Notes: &notes, Steps: []Step{StepLabel}}
		if h.Notes.IP != "" && (h.Notes.OS == ImageWindows || h.Notes.OS == ImageLinux) {
			it.Guest = &Guest{OS: h.Notes.OS, Hostname: h.Host, Domain: env.Domain, IP: h.Notes.IP, Prefix: HostPrefix,
				Gateway: p.gateway(), DNS: addr, DNSOnly: true}
			if err := it.Guest.Check(); err != nil {
				it.Blocked = err.Error()
			}
			it.Steps = []Step{StepGuest, StepVerify, StepLabel}
			if it.Blocked == "" && h.Status != "running" {
				it.Blocked = h.Name + " is " + h.Status + "; start it first so its DNS can be set"
			}
		}
		plan.Items = append(plan.Items, it)
	}
	return plan, nil
}
