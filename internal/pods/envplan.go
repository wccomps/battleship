package pods

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/proxmox"
)

// EnvRequest is an environment job's input.
type EnvRequest struct {
	Domain string
	// Slot is the dev slot (Slot.N) a new environment goes in, or a teardown
	// acts on; 0 takes the first free one, or the slot holding Domain.
	Slot   int
	Router HostSpec   // create (required); add (only if the environment has none)
	Hosts  []HostSpec // create, add
	DNS    string     // set DNS: DNSRouter or one of the environment's host names
}

// envPlanIn is an empty plan of kind for domain in slot, locking both.
func (p Planner) envPlanIn(kind Kind, slot Slot, domain string) *Plan {
	return &Plan{Kind: kind, Config: ConfigHash(kind, p.Cfg), Slot: slot.Name, Locks: []string{slotLock(slot.Name), "domain:" + domain}}
}

// Gateway is the router's address on subnet, every environment's LAN.
func Gateway(subnet string) string { return subnet + "." + strconv.Itoa(GatewayOctet) }

// EnvCreate plans a new environment in req.Slot, or else the lowest free
// slot. A chosen slot must be free, or hold req.Domain already: a create
// that stopped partway, which is resumed there.
func (p Planner) EnvCreate(ctx context.Context, req EnvRequest) (*Plan, error) {
	if err := CheckDomain(req.Domain); err != nil {
		return nil, err
	}
	if req.Router.Host == "" {
		return nil, errNoRouter
	}
	envs, vms, err := p.Environments(ctx)
	if err != nil {
		return nil, err
	}
	hosts := append([]HostSpec{req.Router}, req.Hosts...)
	var env Environment
	if req.Slot != 0 {
		env, err = p.chosenSlot(ctx, envs, vms, req)
	} else {
		env, err = p.firstOpenSlot(ctx, envs, vms, req.Domain)
	}
	if err != nil {
		return nil, err
	}
	return p.envPlan(ctx, KindEnvCreate, env, hosts, true, vms)
}

// firstOpenSlot is the lowest slot of envs a new environment for domain may
// take (slotState), or why none may.
func (p Planner) firstOpenSlot(ctx context.Context, envs []Environment, vms []proxmox.VM, domain string) (Environment, error) {
	if e, ok := EnvByDomain(envs, domain); ok {
		return Environment{}, domainElsewhere(domain, e)
	}
	reserved, err := p.reserved(ctx)
	if err != nil {
		return Environment{}, err
	}
	var hidden, queued []string
	for _, e := range envs {
		st, err := p.slotState(ctx, e, reserved, vms)
		if err != nil {
			return Environment{}, err
		}
		switch st {
		case slotOpen:
			return Environment{Slot: e.Slot, Domain: domain, Gateway: e.Gateway}, nil
		case slotQueued:
			queued = append(queued, e.Slot.Name)
		case slotHidden:
			hidden = append(hidden, e.Slot.Name)
		}
	}
	msg := allSlotsInUse
	if len(hidden) > 0 {
		msg += fmt.Sprintf(" (dev slot %s holds VMs you can't see)", strings.Join(hidden, ", "))
	}
	if len(queued) > 0 {
		msg += fmt.Sprintf(" (a queued job will fill dev slot %s)", strings.Join(queued, ", "))
	}
	return Environment{}, EnvProblem{Field: FieldSlot, Why: msg}
}

// chosenSlot is the environment req.Domain is created in when req names
// its slot: the slot, if a new environment may take it (slotState), or the
// environment of that domain already there.
func (p Planner) chosenSlot(ctx context.Context, envs []Environment, vms []proxmox.VM, req EnvRequest) (Environment, error) {
	reserved, err := p.reserved(ctx)
	if err != nil {
		return Environment{}, err
	}
	env, resumed, err := chosenEnv(envs, reserved, req)
	if err != nil || resumed {
		return env, err
	}
	switch held, err := p.slotHeld(ctx, env.Slot, vms); {
	case err != nil:
		return Environment{}, err
	case held:
		return Environment{}, slotRefused(slotHidden, env.Slot)
	}
	return env, nil
}

// slotState says whether a new environment may take a slot: one free
// (Environment.Free), not one a queued job will fill (reserved lock keys),
// and without VMs the user can't see.
type slotState int

const (
	slotOpen slotState = iota
	slotInUse
	slotQueued
	slotHidden
)

// freeState is env's slotState as far as envs and reserved tell: never
// slotHidden.
func freeState(env Environment, reserved map[string]bool) slotState {
	switch {
	case !env.Free():
		return slotInUse
	case reserved[slotLock(env.Slot.Name)]:
		return slotQueued
	}
	return slotOpen
}

// slotState is env's slotState, asking Proxmox about VMs the user can't see
// (slotHeld) only of a slot open otherwise.
func (p Planner) slotState(ctx context.Context, env Environment, reserved map[string]bool, vms []proxmox.VM) (slotState, error) {
	if st := freeState(env, reserved); st != slotOpen {
		return st, nil
	}
	held, err := p.slotHeld(ctx, env.Slot, vms)
	if held {
		return slotHidden, nil
	}
	return slotOpen, err
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
		switch freeState(e, reserved) {
		case slotQueued:
			queued = append(queued, e.Slot)
		case slotOpen:
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
	if err := oneRouter(env, req); err != nil {
		return nil, err
	}
	hosts := req.Hosts
	if req.Router.Host != "" {
		hosts = append([]HostSpec{req.Router}, hosts...)
	}
	if len(hosts) == 0 {
		return nil, errors.New("name at least one host to add")
	}
	return p.envPlan(ctx, KindEnvAdd, env, hosts, req.Router.Host != "", vms)
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
	return Environment{}, nil, noEnv(domain)
}

// envPlan plans hosts into env; with routerFirst, hosts[0] is the request's
// router. The first router-kind host gets the slot's WAN and LAN bridges,
// other hosts the LAN; checkHosts blocks a second router. What checkHosts
// finds blocks a host; so does what only the cluster tells.
func (p Planner) envPlan(ctx context.Context, kind Kind, env Environment, hosts []HostSpec, routerFirst bool, vms []proxmox.VM) (*Plan, error) {
	images := BaseImages(vms)
	dnsChoice := env.DNS()
	dnsAddr, err := env.DNSAddress(dnsChoice)
	if err != nil {
		dnsChoice, dnsAddr = DNSRouter, env.Gateway // the chosen host is gone: fall back for new hosts
	}
	plan := p.envPlanIn(kind, env.Slot, env.Domain)
	problems := checkHosts(env, images, hosts, routerFirst)
	// Explicit addresses are claimed first, so picked ones avoid them.
	used, _ := claimOctets(env, hosts)
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
	for i, h := range hosts {
		name := h.Host + "." + env.Domain
		it := Item{Name: name, Host: h.Host, Pool: env.Slot.Pool}
		plan.Items = append(plan.Items, it)
		ip := &plan.Items[len(plan.Items)-1]
		im, found := Newest(images, h.Family)
		if found {
			ip.Source, ip.Node = &Source{VMID: im.VMID, Node: im.Node, Name: im.Name}, im.Node
		}
		existing, resumed := env.Host(name)
		if resumed {
			ip.VMID, ip.Node = existing.VMID, existing.Node
		}
		if why := problems[i].Why; why != "" {
			ip.Blocked = why
			continue
		}
		switch {
		case resumed:
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
			octet := LastOctet(existing.Notes.IP)
			switch {
			case resumed && octet != 0:
			case h.Octet != 0: // checkHosts found it claimed by this host
				octet = h.Octet
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
			ip.Guest = &Guest{OS: im.Kind, Hostname: h.Host, Domain: env.Domain, IP: addr, Prefix: HostPrefix, Gateway: env.Gateway, DNS: dnsAddr}
			if err := ip.Guest.Check(); err != nil {
				ip.Blocked = err.Error()
				continue
			}
			ip.Steps = append(ip.Steps, StepWaitAgent, StepGuest, StepVerify)
		}
		ip.Notes = &notes
		desc := cfg["description"]
		if resumed {
			desc = existing.desc
		}
		if err := ip.blockLongNotes(desc); err != nil {
			return nil, err
		}
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
// req.DNS, and recording the choice on every host. Each item keeps the DNS
// its host had (DNSWas).
func (p Planner) EnvDNS(ctx context.Context, req EnvRequest) (*Plan, error) {
	env, _, err := p.findEnv(ctx, req.Domain)
	if err != nil {
		return nil, err
	}
	choice := strings.TrimSpace(req.DNS)
	if choice == "" {
		choice = DNSRouter
	}
	addr, err := env.DNSAddress(choice)
	if err != nil {
		return nil, err
	}
	plan := p.envPlanIn(KindEnvDNS, env.Slot, env.Domain)
	for _, h := range env.Hosts {
		notes := h.Notes
		notes.DNS = choice
		was := &DNSServer{Choice: h.Notes.DNS}
		was.Addr, _ = env.DNSAddress(was.Choice)
		it := Item{Name: h.Name, Host: h.Host, VMID: h.VMID, Node: h.Node, Tags: h.Tags, Notes: &notes, DNSWas: was, Steps: []Step{StepLabel}}
		if h.servesDNS() {
			it.Guest = &Guest{OS: h.Notes.OS, Hostname: h.Host, Domain: env.Domain, IP: h.Notes.IP, Prefix: HostPrefix,
				Gateway: env.Gateway, DNS: addr, DNSOnly: true}
			if err := it.Guest.Check(); err != nil {
				it.Blocked = err.Error()
			}
			it.Steps = []Step{StepGuest, StepVerify, StepLabel}
			if it.Blocked == "" && h.Status != "running" {
				it.Blocked = h.Name + " is " + h.Status + "; start it first so its DNS can be set"
			}
		}
		if err := it.blockLongNotes(h.desc); err != nil {
			return nil, err
		}
		plan.Items = append(plan.Items, it)
	}
	return plan, nil
}
