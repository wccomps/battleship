package pods

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

type Kind string

const (
	KindDeploy   Kind = "deploy"
	KindTeardown Kind = "teardown"
	KindReset    Kind = "reset"
	KindPower    Kind = "power"
	KindSnapshot Kind = "snapshot"
)

type Step string

const (
	StepClone      Step = "clone"
	StepNetwork    Step = "network"
	StepDiskLimits Step = "disk-limits"
	StepCDROM      Step = "cdrom"
	StepSnapshot   Step = "snapshot"
	StepStart      Step = "start"
	StepStop       Step = "stop"
	StepDelete     Step = "delete"
	StepRollback   Step = "rollback"
	StepPower      Step = "power"
	// StepFreeDisks frees the disks a gone VM left on deploy.storage
	// (Planner.orphanedDisks).
	StepFreeDisks Step = "free-disks"
)

// TypedConfirm reports whether confirming an operation of kind k needs the
// team range typed rather than a plain yes, in the CLI and the web alike:
// only a teardown, which deletes VMs.
func (k Kind) TypedConfirm() bool { return k == KindTeardown }

// ChangesConfig reports whether the step can change the VM's config (its
// disks, NICs, pool or snapshots); the power steps can't.
func (s Step) ChangesConfig() bool {
	return s != StepStart && s != StepStop && s != StepPower
}

// TemplateSpec is a .tpl template a deploy clones from.
type TemplateSpec struct {
	Name       string `json:"name"` // teak.tango.delta.tpl
	Host       string `json:"host"` // teak
	VMID       int    `json:"vmid"`
	Node       string `json:"node"`     // node the template is on, or will be built on (the master's node when rebuilding)
	Exists     bool   `json:"exists"`   // a template with this name exists (reused, or deleted and recreated when Rebuild)
	Rebuild    bool   `json:"rebuild"`  // delete the existing template and create it again
	OldNode    string `json:"old_node"` // rebuild: node the existing template is on
	MasterName string `json:"master_name"`
	MasterVMID int    `json:"master_vmid"`
	MasterNode string `json:"master_node"`
	Interfaces int    `json:"interfaces"` // NIC count, from the master
	GPU        bool   `json:"gpu"`        // clones are pinned to Node
	Blocked    string `json:"blocked"`
	// WillStopMaster: building the template stops the master, which is
	// running, until the clone finishes.
	WillStopMaster bool `json:"will_stop_master"`
	// CPU is the master's cpu setting (a custom-<name> model needs
	// Mapping.Use to clone), and CloudInit whether it has a cloud-init
	// drive (configuring its clones regenerates it). They only decide the
	// privileges a deploy needs.
	CPU       string `json:"cpu,omitempty"`
	CloudInit bool   `json:"cloud_init,omitempty"`
	// Bridges are the bridges of the template's net0 and net1, which its
	// clones' NICs start on before the network step rewires them.
	Bridges []string `json:"bridges,omitempty"`
}

// Item is one VM and the steps to run on it, in order.
type Item struct {
	Team     string `json:"team"`
	Host     string `json:"host"`
	Name     string `json:"name"`
	VMID     int    `json:"vmid"`
	Node     string `json:"node"`
	Template string `json:"template"` // deploy: TemplateSpec.Name
	Steps    []Step `json:"steps"`
	Snapshot string `json:"snapshot"` // reset: snapshot to roll back to; snapshot: the one to take
	// Baseline: the reset named no snapshot, so Snapshot is this VM's own
	// baseline (see BaselineSnapshot).
	Baseline bool   `json:"baseline,omitempty"`
	Action   string `json:"action"`  // power: start, shutdown, stop, reboot
	Blocked  string `json:"blocked"` // non-empty: the item will not run
	// Unpermitted: Blocked because the planning user lacks a privilege
	// (BlockUnpermitted), not because of the cluster's state.
	Unpermitted bool `json:"unpermitted,omitempty"`
	// Description and VMState are a snapshot's: its note in Proxmox, and
	// whether it saves a running VM's RAM.
	Description string `json:"description,omitempty"`
	VMState     bool   `json:"vmstate,omitempty"`
}

// Plan is what an operation will do: the templates a deploy needs and one
// item per team VM. It is stored with each job, so its JSON keys are part of
// the stored format.
type Plan struct {
	Kind      Kind           `json:"kind"`
	Teams     []string       `json:"teams"`
	Templates []TemplateSpec `json:"templates"`
	Items     []Item         `json:"items"`
	// Config is ConfigHash of the config the plan was made under. Plans
	// stored before it existed lack it, and their jobs are stale.
	Config string `json:"config,omitempty"`
}

// ConfigHash identifies the settings that decide what running a plan of
// kind does to VMs beyond what the plan says: a deploy's NICs, cloud-init,
// disk limits, baseline snapshot, clone mode, storage and pool, and a
// teardown's shutdown timeout. A job runs under the config of the process
// that claims it, so a plan made under other settings must not pass for
// it. It is "" for kinds no setting changes. confighash_test.go classifies
// every field of the naming, network, deploy and teardown sections.
func ConfigHash(kind Kind, c config.Config) string {
	var v any
	switch kind {
	case KindDeploy:
		v = struct {
			Network          config.Network
			Storage          string
			Linked           bool
			DiskRead, Write  int
			Snapshot         string
			BaselinePatterns []string
			Pool             string
		}{c.Network, c.Deploy.Storage, c.Deploy.Linked, c.Deploy.DiskMBpsRead, c.Deploy.DiskMBpsWrite,
			c.Deploy.SnapshotName, c.Deploy.BaselinePatterns, c.Naming.Pool}
	case KindTeardown:
		v = c.Teardown.ShutdownTimeout
	default:
		return ""
	}
	b, _ := json.Marshal(v) // strings, ints, bools and a duration can't fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Runnable returns items that are not blocked.
func (p *Plan) Runnable() []Item {
	var out []Item
	for _, it := range p.Items {
		if it.Blocked == "" {
			out = append(out, it)
		}
	}
	return out
}

// Planner builds plans from the cluster's current state. It only reads.
type Planner struct {
	API    API
	Cfg    config.Config
	Naming Naming
	// Access, if set, is the planning user's privileges: jobs.BuildPlan
	// blocks what they lack (BlockUnpermitted).
	Access Access
}

// NewPlanner plans with api. If api can read the caller's privileges (a
// Proxmox client acting as someone), plans are checked against them.
func NewPlanner(api API, cfg config.Config) Planner {
	p := Planner{API: api, Cfg: cfg, Naming: NewNaming(cfg.Naming)}
	if r, ok := api.(PermissionReader); ok {
		p.Access = NewAccess(r)
	}
	return p
}

type DeployRequest struct {
	Pattern  string   // master name pattern, e.g. "*.kilo.alpha"
	Teams    []string // two-digit team numbers
	Hosts    []string // optional host filter
	Rebuild  bool     // recreate templates
	Snapshot bool     // take the baseline snapshot
}

func (p Planner) Deploy(ctx context.Context, req DeployRequest) (*Plan, error) {
	teams, err := normalizeTeams(req.Teams)
	if err != nil {
		return nil, err
	}
	if len(teams) == 0 {
		return nil, errors.New("no teams given")
	}
	req.Teams = teams
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := p.API.OnlineNodes(ctx)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no online Proxmox nodes")
	}
	masters, untagged := FindMasters(vms, p.Naming, req.Pattern, p.Cfg.Deploy.MasterTag)
	var kept []proxmox.VM
	for _, m := range masters {
		if MatchesHost(Hostname(m.Name), req.Hosts) {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		msg := fmt.Sprintf("no master VMs match %q with tag %q", req.Pattern, p.Cfg.Deploy.MasterTag)
		if len(req.Hosts) > 0 {
			msg = fmt.Sprintf("no master VMs match %q with tag %q and hosts %s", req.Pattern, p.Cfg.Deploy.MasterTag, strings.Join(req.Hosts, ", "))
		}
		if len(untagged) > 0 {
			msg += fmt.Sprintf(" (%d match but lack the tag, e.g. %s)", len(untagged), untagged[0].Name)
		}
		return nil, fmt.Errorf("%s", msg)
	}

	byName := map[string][]proxmox.VM{}
	used := map[int]string{}
	for _, vm := range vms {
		byName[vm.Name] = append(byName[vm.Name], vm)
		used[vm.VMID] = vm.Name
	}

	plan := &Plan{Kind: KindDeploy, Teams: req.Teams, Config: ConfigHash(KindDeploy, p.Cfg)}
	for _, m := range kept {
		spec, err := p.templateSpec(ctx, m, req.Rebuild, byName, used, vms)
		if err != nil {
			return nil, err
		}
		spec.WillStopMaster = spec.Blocked == "" && (!spec.Exists || spec.Rebuild) && m.Status == "running"
		plan.Templates = append(plan.Templates, spec)
	}
	p.blockDuplicateHosts(plan.Templates, req.Teams[0])

	assign := AssignNodes(req.Teams, nodes, vms)
	for _, team := range req.Teams {
		for _, tpl := range plan.Templates {
			plan.Items = append(plan.Items, p.deployItem(team, tpl, assign[team], req.Snapshot, byName, used))
		}
	}
	return plan, nil
}

// blockDuplicateHosts blocks templates whose hostnames collide: their team
// VMs would get the same name.
func (p Planner) blockDuplicateHosts(specs []TemplateSpec, team string) {
	first := map[string]int{}
	for i := range specs {
		j, seen := first[specs[i].Host]
		if !seen {
			first[specs[i].Host] = i
			continue
		}
		msg := fmt.Sprintf("hosts of %s and %s both map to team VM names like %s; narrow the template pattern",
			specs[j].MasterName, specs[i].MasterName, p.Naming.VMName(team, specs[i].Host))
		for _, k := range []int{j, i} {
			if specs[k].Blocked == "" {
				specs[k].Blocked = msg
			} else if !strings.Contains(specs[k].Blocked, msg) {
				specs[k].Blocked += "; " + msg
			}
			specs[k].WillStopMaster = false
		}
	}
}

func (p Planner) templateSpec(ctx context.Context, m proxmox.VM, rebuild bool,
	byName map[string][]proxmox.VM, used map[int]string, vms []proxmox.VM) (TemplateSpec, error) {
	cfg, err := p.API.VMConfig(ctx, m.Node, m.VMID)
	if err != nil {
		return TemplateSpec{}, fmt.Errorf("reading master %s: %w", m.Name, err)
	}
	spec := TemplateSpec{
		Name:       p.Naming.TemplateName(m.Name),
		Host:       Hostname(m.Name),
		MasterName: m.Name,
		MasterVMID: m.VMID,
		MasterNode: m.Node,
	}
	spec.Interfaces, spec.GPU = p.describe(cfg)
	spec.CPU, spec.CloudInit, spec.Bridges = cfg["cpu"], HasCloudInit(cfg), NICBridges(cfg)
	named := byName[spec.Name]
	var existing proxmox.VM
	exists := len(named) > 0
	if exists {
		existing = lowestVMID(named)
	}
	switch {
	case len(named) > 1:
		spec.VMID, spec.Node = existing.VMID, existing.Node
		spec.Blocked = fmt.Sprintf("several VMs are named %s (VMIDs %s); rename or delete the extras", spec.Name, vmidList(named))
	case exists && !rebuild && !existing.Template:
		spec.VMID, spec.Node = existing.VMID, existing.Node
		spec.Blocked = "exists but is not a template (an earlier build may have been interrupted); deploy with rebuild"
	case exists && !rebuild:
		spec.VMID, spec.Node, spec.Exists = existing.VMID, existing.Node, true
		// Clones copy the template, so it describes the pod, not the master.
		tcfg, err := p.API.VMConfig(ctx, existing.Node, existing.VMID)
		if err != nil {
			return TemplateSpec{}, fmt.Errorf("reading template %s: %w", spec.Name, err)
		}
		spec.Interfaces, spec.GPU = p.describe(tcfg)
		spec.CPU, spec.CloudInit, spec.Bridges = tcfg["cpu"], HasCloudInit(tcfg), NICBridges(tcfg)
	case exists && rebuild:
		// Rebuilding deletes the template, which would break its linked clones.
		for _, vm := range vms {
			if _, host, ok := p.Naming.ParseVMName(vm.Name); ok && host == spec.Host && !vm.Template {
				spec.Blocked = fmt.Sprintf("team VMs such as %s still use this template; tear them down first", vm.Name)
				break
			}
		}
		spec.VMID, spec.Node, spec.OldNode, spec.Exists, spec.Rebuild = existing.VMID, m.Node, existing.Node, true, true
	default:
		id := p.Naming.TemplateVMID(m.VMID)
		for used[id] != "" {
			id++
		}
		used[id] = spec.Name
		spec.VMID, spec.Node = id, m.Node
	}
	base := p.Cfg.Naming.TemplateVMIDBase
	if spec.Blocked == "" && (spec.VMID < base || spec.VMID >= base+100) {
		spec.Blocked = fmt.Sprintf("template VMID %d is outside %d-%d, so its clone VMIDs could collide with another template's", spec.VMID, base, base+99)
	}
	return spec, nil
}

// describe reads the NIC count and GPU flag from a VM config.
func (p Planner) describe(cfg map[string]string) (interfaces int, gpu bool) {
	t, _, _ := strings.Cut(strings.TrimPrefix(cfg["vga"], "type="), ",")
	gpu = p.Cfg.Deploy.GPUVGA != "" && t == p.Cfg.Deploy.GPUVGA
	return CountInterfaces(cfg), gpu
}

func lowestVMID(vms []proxmox.VM) proxmox.VM {
	return slices.MinFunc(vms, func(a, b proxmox.VM) int { return cmp.Compare(a.VMID, b.VMID) })
}

func vmidList(vms []proxmox.VM) string {
	ids := make([]int, len(vms))
	for i, vm := range vms {
		ids[i] = vm.VMID
	}
	sort.Ints(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ", ")
}

// normalizeTeams validates, zero-pads, de-duplicates and sorts team numbers,
// so callers other than ParseTeams (e.g. the web UI) can't produce bad plans.
func normalizeTeams(teams []string) ([]string, error) {
	seen := map[int]bool{}
	for _, t := range teams {
		if len(t) < 1 || len(t) > 2 || !isAllDigits(t) {
			return nil, fmt.Errorf("invalid team %q", t)
		}
		n, _ := strconv.Atoi(t)
		seen[n] = true
	}
	return teamList(seen), nil
}

func (p Planner) deployItem(team string, tpl TemplateSpec, assigned string, snapshot bool,
	byName map[string][]proxmox.VM, used map[int]string) Item {
	it := Item{
		Team:     team,
		Host:     tpl.Host,
		Name:     p.Naming.VMName(team, tpl.Host),
		Template: tpl.Name,
		Node:     assigned,
	}
	if tpl.Blocked != "" {
		// The template's problem comes first: its VMID is meaningless, so
		// the checks below would give wrong advice about healthy VMs.
		it.Node = ""
		if named := byName[it.Name]; len(named) > 0 {
			vm := lowestVMID(named)
			it.VMID, it.Node = vm.VMID, vm.Node
		}
		it.Blocked = "template " + tpl.Name + ": " + tpl.Blocked
		return it
	}
	if tpl.GPU {
		it.Node = tpl.Node
	}
	if named := byName[it.Name]; len(named) > 0 {
		vm := named[0]
		it.VMID, it.Node = vm.VMID, vm.Node
		if len(named) > 1 {
			it.Blocked = fmt.Sprintf("several VMs are named %s (VMIDs %s)", it.Name, vmidList(named))
		} else if vm.Template {
			it.Blocked = "is a template, not a team VM"
		} else if want := p.Naming.CloneVMID(team, tpl.VMID); vm.VMID != want {
			it.Blocked = fmt.Sprintf("exists as VMID %d, expected %d (built from a different template?); tear it down first", vm.VMID, want)
		}
	} else {
		it.VMID = p.Naming.CloneVMID(team, tpl.VMID)
		if other := used[it.VMID]; other != "" {
			it.Blocked = fmt.Sprintf("VMID %d is used by %s", it.VMID, other)
		}
		if it.Blocked == "" {
			used[it.VMID] = it.Name
		}
		it.Steps = append(it.Steps, StepClone)
	}
	it.Steps = append(it.Steps, StepNetwork, StepDiskLimits, StepCDROM)
	if snapshot {
		it.Steps = append(it.Steps, StepSnapshot)
	}
	it.Steps = append(it.Steps, StepStart)
	return it
}

// Teardown stops and deletes team VMs. A whole-team teardown also reports
// those teams' half-deleted VMs (halfDeleted) and frees the disks gone VMs
// left (orphanedDisks).
func (p Planner) Teardown(ctx context.Context, teams, hosts []string) (*Plan, error) {
	plan, vms, err := p.simple(ctx, KindTeardown, teams, hosts, func(it *Item) {
		it.Steps = []Step{StepStop, StepDelete}
	})
	if err != nil || len(hosts) > 0 {
		return plan, err
	}
	p.halfDeleted(plan, vms)
	if err := p.orphanedDisks(ctx, plan, vms); err != nil {
		return nil, err
	}
	return plan, nil
}

// halfDeleted adds a blocked delete item for each VM in plan's teams' clone
// VMIDs that a failed delete left half-deleted (locked as destroyed): it
// has lost its name, so planning by name misses it. Only an admin can
// finish it (--skiplock is root's), so the item says how rather than run.
func (p Planner) halfDeleted(plan *Plan, vms []proxmox.VM) {
	want := map[string]bool{}
	for _, t := range plan.Teams {
		want[t] = true
	}
	planned := map[int]bool{} // one that kept its name is planned already
	for _, it := range plan.Items {
		planned[it.VMID] = true
	}
	for _, vm := range vms {
		team, ok := p.Naming.TeamOfCloneVMID(vm.VMID)
		if vm.Lock != "destroyed" || !ok || !want[team] || planned[vm.VMID] {
			continue
		}
		plan.Items = append(plan.Items, Item{Team: team, Name: p.Naming.VMName(team, "half-deleted-"+strconv.Itoa(vm.VMID)),
			VMID: vm.VMID, Node: vm.Node, Steps: []Step{StepDelete},
			Blocked: (&halfDeletedError{vmid: vm.VMID, node: vm.Node}).Error()})
	}
	sort.SliceStable(plan.Items, func(i, j int) bool { return plan.Items[i].Name < plan.Items[j].Name })
}

// orphanedDisks adds a free-disks item for each VMID of plan's teams that
// has disks on deploy.storage but no VM: a teardown cut off between
// deleting a VM and freeing its disks leaves them so. The storage is listed
// on every online node, so a node-local one is covered. Proxmox lists a
// gone VM's disks only to a user with Datastore.Allocate on the storage,
// the privilege freeing them needs; to anyone else, as to a user refused
// the listing, there are none.
func (p Planner) orphanedDisks(ctx context.Context, plan *Plan, vms []proxmox.VM) error {
	nodes, err := p.API.OnlineNodes(ctx)
	if err != nil {
		return err
	}
	held := map[int]bool{}
	for _, vm := range vms {
		held[vm.VMID] = true
	}
	want := map[string]bool{}
	for _, t := range plan.Teams {
		want[t] = true
	}
	nodes, err = p.storageNodes(ctx, nodes)
	if err != nil {
		return err
	}
	// A listing takes seconds per node (Proxmox reads every image on it):
	// the nodes are listed at once.
	listed := make([][]string, len(nodes))
	var g errgroup.Group
	for i, node := range nodes {
		g.Go(func() error {
			vols, err := p.API.StorageContent(ctx, node, p.Cfg.Deploy.Storage, 0)
			switch {
			case proxmox.IsForbidden(err):
				return nil
			case err != nil:
				return fmt.Errorf("listing %s on %s for disks gone VMs left: %w", p.Cfg.Deploy.Storage, node, err)
			}
			listed[i] = vols
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	for i, node := range nodes {
		for _, v := range listed[i] {
			vmid, _ := volumeOwner(v)
			team, ok := p.Naming.TeamOfCloneVMID(vmid)
			if !ok || !want[team] || held[vmid] {
				continue
			}
			held[vmid] = true // one item per VMID, from the first node that lists it
			// A VM the user can't see may hold it: the disks are its own.
			if taken, err := p.API.VMIDHeld(ctx, vmid); err != nil || taken {
				if err != nil {
					return fmt.Errorf("checking whether VMID %d is free: %w", vmid, err)
				}
				continue
			}
			plan.Items = append(plan.Items, Item{Team: team, Name: p.Naming.VMName(team, "disks-"+strconv.Itoa(vmid)),
				VMID: vmid, Node: node, Steps: []Step{StepFreeDisks}})
		}
	}
	sort.SliceStable(plan.Items, func(i, j int) bool { return plan.Items[i].Name < plan.Items[j].Name })
	return nil
}

// storageNodes are the online nodes to list deploy.storage on: one, when
// Proxmox says the storage is shared (every node sees the same volumes),
// else those that have it. Without a resources read (an API that can't, or
// a user who may not audit the storage) it is every online node.
func (p Planner) storageNodes(ctx context.Context, online []string) ([]string, error) {
	rr, ok := p.API.(ResourceReader)
	if !ok || len(online) == 0 {
		return online, nil
	}
	res, err := rr.ClusterResources(ctx)
	if err != nil {
		return nil, err
	}
	var has []string
	for _, node := range online {
		for _, st := range res.Storage {
			if st.Storage != p.Cfg.Deploy.Storage || st.Node != node {
				continue
			}
			if st.Shared {
				return []string{node}, nil
			}
			has = append(has, node)
		}
	}
	if len(has) == 0 {
		return online, nil
	}
	return has, nil
}

// Reset rolls team VMs back to snapshot and starts them; an empty snapshot
// means each VM's own baseline (see BaselineSnapshot). VMs without it are
// blocked.
func (p Planner) Reset(ctx context.Context, teams, hosts []string, snapshot string) (*Plan, error) {
	plan, _, err := p.simple(ctx, KindReset, teams, hosts, func(it *Item) {
		it.Steps = []Step{StepStop, StepRollback, StepStart}
		it.Snapshot = snapshot
		it.Baseline = snapshot == ""
	})
	if err != nil {
		return nil, err
	}
	for i := range plan.Items {
		it := &plan.Items[i]
		snaps, err := p.API.Snapshots(ctx, it.Node, it.VMID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			it.Blocked = "cannot list snapshots: " + proxmox.Describe(err)
			continue
		}
		names := SnapshotNames(snaps)
		has := "has none"
		if len(names) > 0 {
			has = "has: " + strings.Join(names, ", ")
		}
		if snapshot == "" {
			if base, ok := BaselineSnapshot(p.Cfg.Deploy, snaps); ok {
				it.Snapshot = base
			} else {
				it.Blocked = fmt.Sprintf("%s (%s)", NoBaseline(p.Cfg.Deploy), has)
			}
			continue
		}
		if !slices.Contains(names, snapshot) {
			it.Blocked = fmt.Sprintf("no snapshot %q (%s)", snapshot, has)
		}
	}
	return plan, nil
}

// Power runs start, shutdown, stop or reboot on team VMs.
func (p Planner) Power(ctx context.Context, teams, hosts []string, action string) (*Plan, error) {
	if !IsPowerAction(action) {
		return nil, fmt.Errorf("unknown power action %q", action)
	}
	plan, _, err := p.simple(ctx, KindPower, teams, hosts, func(it *Item) {
		it.Steps = []Step{StepPower}
		it.Action = action
	})
	return plan, err
}

func (p Planner) simple(ctx context.Context, kind Kind, teams, hosts []string, fill func(*Item)) (*Plan, []proxmox.VM, error) {
	teams, err := normalizeTeams(teams)
	if err != nil {
		return nil, nil, err
	}
	if len(teams) == 0 {
		return nil, nil, errors.New("no teams given")
	}
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		return nil, nil, err
	}
	plan := &Plan{Kind: kind, Teams: teams, Config: ConfigHash(kind, p.Cfg)}
	for _, vm := range FindTeamVMs(vms, p.Naming, teams, hosts) {
		team, host, _ := p.Naming.ParseVMName(vm.Name)
		it := Item{Team: team, Host: host, Name: vm.Name, VMID: vm.VMID, Node: vm.Node}
		fill(&it)
		plan.Items = append(plan.Items, it)
	}
	sort.SliceStable(plan.Items, func(i, j int) bool { return plan.Items[i].Name < plan.Items[j].Name })
	return plan, vms, nil
}
