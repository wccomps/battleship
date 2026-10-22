package pods

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/proxmox"
)

const (
	// NoBaseImage clears an environment host's base image in its notes.
	NoBaseImage = "-"
	// ClearOctet clears an other host's address in its notes.
	ClearOctet = -1
)

// HostEdit is what Edit host starts from: the environment, the host and its
// notes as read.
type HostEdit struct {
	Env  Environment
	Host EnvHost
	Read NotesRead
}

// Kind is the host's kind: router when tagged router, else its notes' OS,
// else other.
func (e HostEdit) Kind() string {
	if e.Host.Router {
		return ImageRouter
	}
	return cmp.Or(e.Host.Notes.OS, ImageOther)
}

// Octet is the host's LAN address octet, or 0 without one.
func (e HostEdit) Octet() int {
	o, _ := e.Env.LAN.Octet(e.Host.Addr)
	return o
}

// Edit is what an Edit host form starts its details and notes fields from,
// with a mark of the notes read (From).
func (e HostEdit) Edit() ImageNotes { return e.Read.Edit() }

// HostEditInput is what EnvEdit changes on an environment host. Empty fields
// keep what the host already has; Image NoBaseImage clears its base image
// and Octet ClearOctet clears an other host's address.
type HostEditInput struct {
	VMID       int
	Host       string      // host label (or host.domain in a mixed slot); "" keeps the current name
	Kind       string      // ImageKinds; "" keeps the current kind
	Octet      int         // FirstHostOctet..LastHostOctet, ClearOctet, or 0 to keep
	Image      string      // base image name, NoBaseImage, or "" to keep
	CPU        string      // custom CPU model name; "" keeps the current CPU
	EjectCDROM bool        // eject attached non-cloudinit CD-ROM media
	Details    *ImageNotes // nil keeps the details block and free notes as they are
}

// Check checks req before contacting Proxmox.
func (req HostEditInput) Check() error {
	var errs []error
	if h := strings.TrimSpace(req.Host); h != "" {
		if hostPart, domPart, ok := strings.Cut(h, "."); ok {
			if err := CheckHostname(hostPart); err != nil {
				errs = append(errs, err)
			}
			if err := CheckDomain(domPart); err != nil {
				errs = append(errs, err)
			}
		} else if err := CheckHostname(h); err != nil {
			errs = append(errs, err)
		}
	}
	if req.Kind != "" && !slices.Contains(ImageKinds, req.Kind) {
		errs = append(errs, fmt.Errorf("the host kind must be one of %s, not %q", strings.Join(ImageKinds, ", "), req.Kind))
	}
	if req.Octet != 0 && req.Octet != ClearOctet && (req.Octet < FirstHostOctet || req.Octet > LastHostOctet) {
		errs = append(errs, fmt.Errorf("the address must be %d-%d (.%d is the router, .3 the team VPN)", FirstHostOctet, LastHostOctet, GatewayOctet))
	}
	if req.Image != "" && req.Image != NoBaseImage {
		if _, _, ok := ParseImageName(req.Image); !ok {
			errs = append(errs, fmt.Errorf("%q isn't a base image name like debian-12-v1", req.Image))
		}
	}
	if req.CPU != "" {
		if err := CheckCPUModel(req.CPU); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ErrNoEnvHost marks a VMID that is missing, half-deleted, or outside any
// dev slot pool.
var ErrNoEnvHost = errors.New("not an environment host")

type noEnvHostError struct{ msg string }

func (e noEnvHostError) Error() string        { return e.msg }
func (e noEnvHostError) Is(target error) bool { return target == ErrNoEnvHost }

// HostToEdit reads the dev slot containing vmid from vms and returns vmid's
// HostEdit.
func (p Planner) HostToEdit(ctx context.Context, vms []proxmox.VM, vmid int) (HostEdit, error) {
	at, _, err := p.envHostFrom(ctx, vms, vmid)
	return at, err
}

// envHost reads only the dev slot containing vmid, rather than every slot
// and unpooled VM in the cluster.
func (p Planner) envHost(ctx context.Context, vmid int) (HostEdit, proxmox.VM, []proxmox.VM, error) {
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		return HostEdit{}, proxmox.VM{}, nil, err
	}
	at, vm, err := p.envHostFrom(ctx, vms, vmid)
	return at, vm, vms, err
}

func (p Planner) envHostFrom(ctx context.Context, vms []proxmox.VM, vmid int) (HostEdit, proxmox.VM, error) {
	vm, ok := vmByID(vms, vmid)
	if !ok {
		return HostEdit{}, proxmox.VM{}, noEnvHostError{fmt.Sprintf("there is no environment host with VMID %d, or you can't see it", vmid)}
	}
	if vm.Lock == "destroyed" {
		return HostEdit{}, vm, noEnvHostError{HalfDeletedText(vm.VMID, vm.Node)}
	}
	slots := Slots(p.Cfg.Environments)
	si := slices.IndexFunc(slots, func(s Slot) bool { return s.Pool == vm.Pool })
	if si < 0 {
		return HostEdit{}, vm, noEnvHostError{fmt.Sprintf("%s (%d) is not in a dev slot pool", vm.Name, vmid)}
	}
	slot := slots[si]
	var inSlot []proxmox.VM
	for _, v := range vms {
		if v.Pool == slot.Pool && v.Lock != "destroyed" {
			inSlot = append(inSlot, v)
		}
	}
	cfgs := make([]map[string]string, len(inSlot))
	var g errgroup.Group
	g.SetLimit(envReads)
	for i, v := range inSlot {
		g.Go(func() error {
			cfg, err := p.API.VMConfig(ctx, v.Node, v.VMID)
			switch {
			case proxmox.IsNotFound(err):
				return nil
			case err != nil:
				return fmt.Errorf("reading %s (%d): %w", v.Name, v.VMID, err)
			}
			cfgs[i] = cfg
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return HostEdit{}, proxmox.VM{}, err
	}
	cpuModels := p.customCPUModels(ctx, vm.Node)
	env := Environment{Slot: slot, LAN: p.Cfg.Environments.Subnet}
	for i, v := range inSlot {
		if cfgs[i] != nil {
			env.Hosts = append(env.Hosts, p.buildEnvHost(v, cfgs[i], slot, cpuModels))
		}
	}
	setEnvDomain(&env)
	for _, h := range env.Hosts {
		if h.VMID == vmid {
			return HostEdit{Env: env, Host: h, Read: readVMNotes(h.VMID, h.desc())}, vm, nil
		}
	}
	return HostEdit{}, proxmox.VM{}, noEnvHostError{fmt.Sprintf("there is no environment host with VMID %d, or you can't see it", vmid)}
}

func hostLocks(slot Slot, domains ...string) []string {
	locks := []string{slotLock(slot.Name)}
	for _, d := range domains {
		if CheckDomain(d) == nil && !slices.Contains(locks, "domain:"+d) {
			locks = append(locks, "domain:"+d)
		}
	}
	return locks
}

// EnvEdit plans editing an existing host in a dev slot: its name, kind,
// address, base image, CPU, attached CD-ROM, details and free notes. When a
// Windows or Linux host's name, address or OS kind changes, the guest agent
// configures it inside the VM too.
func (p Planner) EnvEdit(ctx context.Context, req HostEditInput) (*Plan, error) {
	if err := req.Check(); err != nil {
		return nil, err
	}
	at, vm, vms, err := p.envHost(ctx, req.VMID)
	if err != nil {
		return nil, err
	}
	env, h := at.Env, at.Host
	wantName, wantHost, domain, err := resolveHostName(env, h, strings.TrimSpace(req.Host))
	if err != nil {
		return nil, err
	}
	curKind := at.Kind()
	wantKind := cmp.Or(req.Kind, curKind)
	wantBase := h.Notes.Base
	switch req.Image {
	case NoBaseImage:
		wantBase = ""
	case "":
	default:
		wantBase = req.Image
	}
	wantTags := hostTags(h.Tags, p.Cfg.Deploy.MasterTag, Domain(h.Name), domain, wantKind == ImageRouter)
	wantNotes := h.Notes
	wantNotes.Base = wantBase

	it := Item{Name: h.Name, Host: wantHost, VMID: h.VMID, Node: h.Node, Pool: env.Slot.Pool, DetailsWas: &at.Read}
	if vm.Template {
		it.Blocked = "is a template"
	}
	if it.Blocked == "" && wantName != h.Name && len(vmsNamed(vms, wantName)) > 0 {
		it.Blocked = "a VM named " + wantName + " already exists"
	}

	used := map[int]string{}
	for _, other := range env.Hosts {
		if other.VMID == h.VMID {
			continue
		}
		if o, ok := env.LAN.Octet(other.Notes.IP); ok {
			used[o] = other.Name
		}
	}

	switch wantKind {
	case ImageRouter:
		wantNotes.IP, wantNotes.OS, wantNotes.DNS = netip.Addr{}, "", ""
		for _, other := range env.Hosts {
			if other.VMID != h.VMID && other.Router && it.Blocked == "" {
				it.Blocked = "an environment has one router (" + other.Name + ")"
				break
			}
		}
		if it.Blocked == "" && h.Config["net1"] == "" {
			it.Blocked = h.Name + " has one NIC; a router needs two (WAN on net0, LAN on net1)"
		}
	case ImageWindows, ImageLinux:
		wantNotes.OS = wantKind
		if wantNotes.DNS == "" {
			wantNotes.DNS = env.DNS()
		} else if wantHost != h.Host && wantNotes.DNS == h.Host {
			wantNotes.DNS = wantHost
		}
		if wantKind == ImageWindows && len(wantHost) > MaxWindowsHostname && it.Blocked == "" {
			it.Blocked = fmt.Sprintf("Windows host names are at most %d characters", MaxWindowsHostname)
		}
		switch {
		case req.Octet >= FirstHostOctet:
			wantNotes.IP = env.LAN.Host(req.Octet)
			if other := used[req.Octet]; other != "" && it.Blocked == "" {
				it.Blocked = fmt.Sprintf("address .%d is already taken by %s", req.Octet, other)
			}
		case h.Notes.IP.IsValid():
			wantNotes.IP = h.Notes.IP
			if o, ok := env.LAN.Octet(wantNotes.IP); ok && used[o] != "" && it.Blocked == "" {
				it.Blocked = fmt.Sprintf("address .%d is already taken by %s", o, used[o])
			}
		default:
			o, ok := PickOctet(cmp.Or(domain, env.Slot.Pool), wantHost, env.UsedOctets())
			if !ok {
				if it.Blocked == "" {
					it.Blocked = "no free address left in " + cmp.Or(domain, env.Slot.Pool)
				}
			} else {
				wantNotes.IP = env.LAN.Host(o)
			}
		}
	default:
		wantNotes.OS = ""
		switch {
		case req.Octet == ClearOctet:
			wantNotes.IP = netip.Addr{}
		case req.Octet >= FirstHostOctet:
			wantNotes.IP = env.LAN.Host(req.Octet)
			if other := used[req.Octet]; other != "" && it.Blocked == "" {
				it.Blocked = fmt.Sprintf("address .%d is already taken by %s", req.Octet, other)
			}
		}
	}

	if wantName != h.Name {
		it.Rename = wantName
	}
	if it.Rename != "" || !sameTags(h.Tags, wantTags) || WithNotes(h.desc(), wantNotes) != h.desc() {
		it.Tags = wantTags
		it.Notes = &wantNotes
		it.Steps = append(it.Steps, StepLabel)
	}

	if req.Details != nil {
		n, same, stale, err := at.Read.planChanges(*req.Details, h.Name, "Edit host")
		if err != nil {
			return nil, err
		}
		if stale != "" && it.Blocked == "" {
			it.Blocked = stale
		}
		if !same {
			it.Details = &n
			it.Steps = append(it.Steps, StepDetails)
		}
	}

	wantNICs := env.Slot.wantNICs(wantKind == ImageRouter)
	if len(NICChanges(h.Config, wantNICs)) > 0 {
		it.NICs = wantNICs
		it.Steps = append(it.Steps, StepNetwork)
	}

	if req.EjectCDROM {
		if h.CDROM {
			it.Steps = append(it.Steps, StepCDROM)
		} else if len(it.Steps) == 0 && it.Blocked == "" {
			it.Blocked = h.Name + " has no CD-ROM attached"
		}
	}

	if req.CPU != "" {
		model := strings.TrimPrefix(req.CPU, "custom-")
		wantCPU := "custom-" + model
		if len(h.CPUModels) > 0 && !slices.Contains(h.CPUModels, model) && it.Blocked == "" {
			it.Blocked = fmt.Sprintf("CPU model %s is not one of %s", model, strings.Join(h.CPUModels, " or "))
		}
		if len(CPUChanges(h.Config, wantCPU)) > 0 {
			it.CPU = wantCPU
			it.Steps = append(it.Steps, StepCPU)
		}
	}

	guestChanged := (wantKind == ImageWindows || wantKind == ImageLinux) &&
		(wantName != h.Name || wantNotes.IP != h.Notes.IP || wantKind != curKind)
	if guestChanged {
		if !h.Agent && it.Blocked == "" {
			it.Blocked = h.Name + " has no QEMU guest agent enabled (Options, QEMU Guest Agent); environments set hostname and address through it"
		}
		if CheckDomain(domain) != nil && it.Blocked == "" {
			it.Blocked = fmt.Sprintf("%s has no valid domain; write the name as <host>.<domain>, e.g. %s.kilo.alpha", wantName, wantHost)
		}
		dnsAddr, err := env.DNSAddress(wantNotes.DNS)
		if err != nil {
			dnsAddr = env.Gateway()
		}
		if wantNotes.DNS == wantHost && wantNotes.IP.IsValid() {
			dnsAddr = wantNotes.IP
		}
		g, why := p.planGuest(env, wantKind, wantHost, domain, wantNotes.IP, dnsAddr)
		if why != "" && it.Blocked == "" {
			it.Blocked = why
		}
		if wantNotes.IP.IsValid() && CheckDomain(domain) == nil {
			it.Guest = g
			if h.Status != "running" {
				it.Steps = append(it.Steps, StepStart)
			}
			it.Steps = append(it.Steps, StepWaitAgent, StepGuest, StepVerify)
		}
	}

	if len(it.Steps) == 0 {
		it.Steps = []Step{StepLabel}
		if it.Blocked == "" {
			it.Blocked = h.Name + " already has these settings"
		}
	}
	it.blockLongNotes(h.desc())
	return &Plan{Kind: KindEnvEdit, Config: ConfigHash(KindEnvEdit, p.Cfg), Slot: env.Slot.Name,
		Locks: hostLocks(env.Slot, env.Domain, Domain(h.Name), domain), Items: []Item{it}}, nil
}

// resolveHostName resolves input (empty, a host label like "dc", or a full
// name like "dc.kilo.alpha", already checked by HostEditInput.Check) against
// env and h.
func resolveHostName(env Environment, h EnvHost, input string) (full, host, domain string, err error) {
	curDomain := cmp.Or(env.Domain, Domain(h.Name))
	if input == "" {
		return h.Name, h.Host, curDomain, nil
	}
	if hostPart, domPart, ok := strings.Cut(input, "."); ok {
		if env.Domain != "" && domPart != env.Domain && len(env.Hosts) > 1 {
			return "", "", "", fmt.Errorf("dev slot %s holds %s; name the host without a domain, or %s.%s", env.Slot.Name, env.Domain, hostPart, env.Domain)
		}
		return input, hostPart, domPart, nil
	}
	if curDomain == "" {
		return "", "", "", fmt.Errorf("%s has no domain in its name; write the full name as <host>.<domain>, e.g. %s.kilo.alpha", h.Name, input)
	}
	return input + "." + curDomain, input, curDomain, nil
}

// hostTags updates a slot VM's tag list with masterTag, domain and router,
// keeping any other tags on the VM in order.
func hostTags(cur, masterTag, oldDomain, domain string, router bool) string {
	var out []string
	add := func(t string) {
		if t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	add(masterTag)
	if CheckDomain(domain) == nil {
		add(domain)
	}
	if router {
		add(ImageRouter)
	}
	for _, t := range Tags(cur) {
		if t == ImageRouter || (oldDomain != "" && t == oldDomain && domain != oldDomain) {
			continue
		}
		add(t)
	}
	return strings.Join(out, ";")
}

// EnvPower plans a power action on an environment host in a dev slot.
func (p Planner) EnvPower(ctx context.Context, vmid int, action string) (*Plan, error) {
	if !IsPowerAction(action) {
		return nil, fmt.Errorf("power action must be start, shutdown, stop or reboot, not %q", action)
	}
	at, vm, _, err := p.envHost(ctx, vmid)
	if err != nil {
		return nil, err
	}
	env, h := at.Env, at.Host
	it := Item{Name: h.Name, Host: h.Host, VMID: h.VMID, Node: h.Node, Pool: env.Slot.Pool, Action: action, Steps: []Step{StepPower}}
	if vm.Template {
		it.Blocked = "is a template"
	}
	return &Plan{Kind: KindEnvPower, Config: ConfigHash(KindEnvPower, p.Cfg), Slot: env.Slot.Name,
		Locks: hostLocks(env.Slot, env.Domain, Domain(h.Name)), Items: []Item{it}}, nil
}

// EnvDeleteHost plans stopping and deleting a single host in a dev slot.
func (p Planner) EnvDeleteHost(ctx context.Context, vmid int, name string) (*Plan, error) {
	at, vm, _, err := p.envHost(ctx, vmid)
	if err != nil {
		return nil, err
	}
	env, h := at.Env, at.Host
	if name != "" && h.Name != name {
		return nil, fmt.Errorf("VM %d is named %s, not %s", vmid, h.Name, name)
	}
	notes := h.Notes
	it := Item{Name: h.Name, Host: h.Host, VMID: h.VMID, Node: h.Node, Pool: env.Slot.Pool, Tags: h.Tags, Notes: &notes,
		Steps: []Step{StepStop, StepDelete}}
	if vm.Template {
		it.Blocked = "is a template, not an environment host; move it out of " + env.Slot.Pool + " first"
	}
	return &Plan{Kind: KindEnvDeleteHost, Config: ConfigHash(KindEnvDeleteHost, p.Cfg), Slot: env.Slot.Name,
		Locks: hostLocks(env.Slot, env.Domain, Domain(h.Name)), Items: []Item{it}}, nil
}
