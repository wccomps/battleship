package pods

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/proxmox"
)

// EnvRequest is an environment job's input.
type EnvRequest struct {
	Domain string
	// Slot is the dev slot a new environment goes in (Slot.N); 0 takes the
	// first free one.
	Slot   int
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
	h, ok := env.dnsHost(choice)
	if !ok {
		return "", fmt.Errorf("%s isn't a Windows or Linux host of %s with an address; choose router or one of those", choice, env.Domain)
	}
	return h.Notes.IP, nil
}

// EnvCreate plans a new environment in req.Slot, or else the lowest free
// slot. A chosen slot must be free, or hold req.Domain already: a create
// that stopped partway, which is resumed there.
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
	hosts := append([]HostSpec{req.Router}, req.Hosts...)
	if req.Slot != 0 {
		env, err := p.chosenSlot(ctx, envs, vms, req)
		if err != nil {
			return nil, err
		}
		return p.envPlan(ctx, KindEnvCreate, env, hosts, vms)
	}
	for _, e := range envs {
		if e.Domain == req.Domain {
			return nil, fmt.Errorf("%s is already in dev slot %s; add hosts to it instead", req.Domain, e.Slot.Name)
		}
	}
	open, queued, err := p.OpenSlots(ctx, envs)
	if err != nil {
		return nil, err
	}
	var slot *Slot
	var hidden []string // slots that look free but hold VMs this user can't see
	for _, s := range open {
		held, err := p.slotHeld(ctx, s, vms)
		if err != nil {
			return nil, err
		}
		if !held {
			slot = &s
			break
		}
		hidden = append(hidden, s.Name)
	}
	if slot == nil {
		msg := "every dev slot is in use; archive an environment to free one"
		if len(hidden) > 0 {
			msg += fmt.Sprintf(" (dev slot %s holds VMs you can't see)", strings.Join(hidden, ", "))
		}
		if len(queued) > 0 {
			var names []string
			for _, s := range queued {
				names = append(names, s.Name)
			}
			msg += fmt.Sprintf(" (a queued job will fill dev slot %s)", strings.Join(names, ", "))
		}
		return nil, errors.New(msg)
	}
	return p.envPlan(ctx, KindEnvCreate, Environment{Slot: *slot, Domain: req.Domain}, hosts, vms)
}

// chosenSlot is the environment req.Domain is created in when req names
// its slot: the slot, checked free as EnvCreate's own pick is, or the
// environment of that domain already there.
func (p Planner) chosenSlot(ctx context.Context, envs []Environment, vms []proxmox.VM, req EnvRequest) (Environment, error) {
	i := slices.IndexFunc(envs, func(e Environment) bool { return e.Slot.N == req.Slot })
	if i < 0 {
		return Environment{}, fmt.Errorf("there's no dev slot %s", SlotName(req.Slot))
	}
	env := envs[i]
	if e, ok := EnvByDomain(envs, req.Domain); ok && e.Slot.N != req.Slot {
		return Environment{}, fmt.Errorf("%s is already in dev slot %s; add hosts to it instead", req.Domain, e.Slot.Name)
	}
	if env.Domain == req.Domain {
		return env, nil
	}
	if !env.Free() {
		return Environment{}, fmt.Errorf("dev slot %s is in use; choose a free one", env.Slot.Name)
	}
	_, queued, err := p.OpenSlots(ctx, envs)
	if err != nil {
		return Environment{}, err
	}
	if slices.ContainsFunc(queued, func(s Slot) bool { return s.N == req.Slot }) {
		return Environment{}, fmt.Errorf("a queued job will fill dev slot %s; choose another", env.Slot.Name)
	}
	held, err := p.slotHeld(ctx, env.Slot, vms)
	if err != nil {
		return Environment{}, err
	}
	if held {
		return Environment{}, fmt.Errorf("dev slot %s holds VMs you can't see; choose another", env.Slot.Name)
	}
	return Environment{Slot: env.Slot, Domain: req.Domain}, nil
}

// slotHeld is whether any VMID in slot's block is in use, listed or not:
// what a user may see depends on their rights, but Proxmox reports any.
// VMs on the slot's bridges count through Environments' Users instead.
func (p Planner) slotHeld(ctx context.Context, slot Slot, vms []proxmox.VM) (bool, error) {
	use, err := p.vmidUse(ctx, vms)
	if err != nil {
		return false, err
	}
	// errSlotHeld cancels gctx, which stops the rest.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(envReads)
	for id := slot.FirstVMID; id <= slot.LastVMID(); id++ {
		g.Go(func() error {
			held, err := use.inUse(gctx, id)
			switch {
			case held:
				return errSlotHeld
			case err != nil && gctx.Err() == nil:
				return fmt.Errorf("checking whether dev slot %s is free (VMID %d): %w", slot.Name, id, err)
			}
			return nil
		})
	}
	switch err := g.Wait(); {
	case errors.Is(err, errSlotHeld):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, ctx.Err() // a stopped check isn't a free slot
}

var errSlotHeld = errors.New("slot holds a VM")

// OpenSlots splits envs' free slots, in order, into those a new environment
// may take and those a queued job will fill (Planner.Reserved). An open slot
// may still hold VMs the user can't see; only EnvCreate asks (slotHeld).
func (p Planner) OpenSlots(ctx context.Context, envs []Environment) (open, queued []Slot, err error) {
	reserved, err := p.reserved(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range envs {
		switch {
		case !e.Free():
		case reserved["slot:"+e.Slot.Name]:
			queued = append(queued, e.Slot)
		default:
			open = append(open, e.Slot)
		}
	}
	return open, queued, nil
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
	if e, ok := EnvByDomain(envs, domain); ok {
		return e, vms, nil
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
			id, err := p.freeVMID(ctx, vms, env.Slot.FirstVMID, env.Slot.LastVMID(), taken)
			if err != nil {
				ip.Blocked = "dev slot " + env.Slot.Name + " has " + err.Error()
				continue
			}
			ip.VMID, taken[id] = id, true
		}
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
		if h.servesDNS() {
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
