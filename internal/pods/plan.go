package pods

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

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

	// Base images and environments: no teams.
	KindImageRegister  Kind = "image-register"
	KindImageEdit      Kind = "image-edit"
	KindImageDeprecate Kind = "image-deprecate"
	KindImageDelete    Kind = "image-delete"
	KindImageDetails   Kind = "image-details"
	KindEnvCreate      Kind = "env-create"
	KindEnvAdd         Kind = "env-add"
	KindEnvTeardown    Kind = "env-teardown"
	KindEnvEdit        Kind = "env-edit"
	KindEnvPower       Kind = "env-power"
	KindEnvDeleteHost  Kind = "env-delete-host"
)

// Kinds are all known job kinds.
var Kinds = []Kind{
	KindDeploy, KindTeardown, KindReset, KindPower, KindSnapshot,
	KindImageRegister, KindImageEdit, KindImageDeprecate, KindImageDelete, KindImageDetails,
	KindEnvCreate, KindEnvAdd, KindEnvTeardown, KindEnvEdit, KindEnvPower, KindEnvDeleteHost,
}

type subject uint8

const (
	subjectTeams subject = iota
	subjectImage
	subjectNewEnvironment
	subjectEnvironment
	subjectEnvHost
)

type retryMode uint8

const (
	retryNone retryMode = iota
	retryNarrowed
	retryWhole
)

// spec records a Kind's model facts: what it acts on, which privileges make
// it worth offering, which config settings ConfigHash covers, whether it
// tears VMs down, and how RetryInputs retries it.
type spec struct {
	subject   subject
	offer     []string
	config    func(config.Config) any
	tearsDown bool
	retry     retryMode
}

var specs = map[Kind]spec{
	KindDeploy:   {subject: subjectTeams, offer: []string{"VM.Clone"}, config: deployConfigHash, retry: retryNarrowed},
	KindTeardown: {subject: subjectTeams, offer: []string{"VM.Allocate"}, tearsDown: true, retry: retryNarrowed},
	KindReset:    {subject: subjectTeams, offer: []string{"VM.Snapshot", "VM.Snapshot.Rollback"}, retry: retryNarrowed},
	KindPower:    {subject: subjectTeams, offer: []string{"VM.PowerMgmt"}, retry: retryNarrowed},
	KindSnapshot: {subject: subjectTeams, offer: []string{"VM.Snapshot"}, retry: retryNarrowed},

	KindImageRegister:  {subject: subjectImage, offer: []string{"VM.Clone"}, config: imageConfigHash},
	KindImageEdit:      {subject: subjectImage, offer: []string{"VM.Clone"}, config: imageConfigHash},
	KindImageDeprecate: {subject: subjectImage, offer: []string{"VM.Config.Options"}, config: imageConfigHash},
	KindImageDelete:    {subject: subjectImage, offer: []string{"VM.Allocate"}, config: imageConfigHash},
	KindImageDetails:   {subject: subjectImage, offer: []string{"VM.Config.Options"}},

	KindEnvCreate:     {subject: subjectNewEnvironment, offer: []string{"VM.Clone"}, config: envConfigHash, retry: retryNarrowed},
	KindEnvAdd:        {subject: subjectEnvironment, offer: []string{"VM.Clone"}, config: envConfigHash, retry: retryNarrowed},
	KindEnvTeardown:   {subject: subjectEnvironment, offer: []string{"VM.Allocate"}, tearsDown: true, retry: retryWhole},
	KindEnvEdit:       {subject: subjectEnvHost, offer: []string{"VM.Config.Options"}, config: envConfigHash},
	KindEnvPower:      {subject: subjectEnvHost, offer: []string{"VM.PowerMgmt"}},
	KindEnvDeleteHost: {subject: subjectEnvHost, offer: []string{"VM.Allocate"}, tearsDown: true},
}

// Teamless reports whether kind acts on base images or environments rather
// than team VMs.
func (k Kind) Teamless() bool { return specs[k].subject != subjectTeams }

// OnEnvironment is whether the kind works on an existing environment, named
// by its domain.
func (k Kind) OnEnvironment() bool { return specs[k].subject == subjectEnvironment }

func (k Kind) TearsDown() bool { return specs[k].tearsDown }

type Step string

const (
	StepClone      Step = "clone"
	StepNetwork    Step = "network"
	StepDiskLimits Step = "disk-limits"
	StepCDROM      Step = "cdrom"
	StepNotes      Step = "clear-notes" // empty a team VM's Proxmox notes (NotesChanges)
	StepSetTag     Step = "tag-set"     // record a team VM's template set in its tags (SetTagChanges)
	StepSnapshot   Step = "snapshot"
	StepStart      Step = "start"
	StepStop       Step = "stop"
	StepDelete     Step = "delete"
	StepRollback   Step = "rollback"
	StepPower      Step = "power"
	// StepFreeDisks frees disks a gone VM left on deploy.storage
	// (see Planner.orphanedDisks).
	StepFreeDisks Step = "free-disks"

	// Base images and environments.
	StepLabel     Step = "label"              // set tags and the notes line
	StepConvert   Step = "convert"            // make the VM a template
	StepRetire    Step = "deprecate-previous" // tag the version this one replaces deprecated
	StepDetails   Step = "details"            // write the details, and free text if asked, into the notes (ImageNotes.Changes)
	StepCPU       Step = "cpu"                // set a custom CPU model (CPUChanges)
	StepWaitAgent Step = "wait-agent"
	StepGuest     Step = "configure-guest"
	StepVerify    Step = "verify"
)

// TypedConfirm reports whether kind k needs what it deletes typed to confirm
// (CLI and web): a teardown's team range, an environment's domain, or an
// environment host's VM name.
func (k Kind) TypedConfirm() bool { return specs[k].tearsDown }

// Retryable reports whether a job of kind k can be retried with RetryInputs
// rather than started again from its form.
func (k Kind) Retryable() bool { return specs[k].retry != retryNone }

// RetriedWhole reports whether a retry of a job of kind k plans it again
// whole, not narrowed to the VMs that didn't finish: an environment's
// teardown, in its slot, where what a failed delete left has new names.
func (k Kind) RetriedWhole() bool { return specs[k].retry == retryWhole }

// ChangesConfig reports whether the step can change the VM's config (its
// disks, NICs, pool or snapshots); the power and guest steps can't.
func (s Step) ChangesConfig() bool {
	switch s {
	case StepStart, StepStop, StepPower, StepWaitAgent, StepGuest, StepVerify:
		return false
	}
	return true
}

// Source is the VM a base-image or environment clone copies.
type Source struct {
	VMID int    `json:"vmid"`
	Node string `json:"node"`
	Name string `json:"name"`
	// Bridges are its NICs' bridges: Proxmox checks SDN.Use on them for a
	// clone.
	Bridges []string `json:"bridges,omitempty"`
	// Base, Notes and Unreadable are the source's notes, for its review: the
	// version it edits (ImageEdit), and the user notes the copy keeps, less
	// their details block (see ParseDetails), or all of them when Unreadable
	// says the block is.
	Base       string `json:"base,omitempty"`
	Notes      string `json:"notes,omitempty"`
	Unreadable bool   `json:"details_unreadable,omitempty"`
}

// Builds is whether a deploy builds the template from its master (it
// doesn't exist yet, or is rebuilt) rather than reusing it.
func (t TemplateSpec) Builds() bool { return !t.Exists || t.Rebuild }

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
	// CPU (a custom-<name> model needs Mapping.Use to clone) and CloudInit
	// (clones regenerate the drive) only decide the privileges a deploy needs.
	CPU       string `json:"cpu,omitempty"`
	CloudInit bool   `json:"cloud_init,omitempty"`
	// Bridges are the template's net0 and net1 bridges, which clones start on
	// until the network step rewires them.
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
	// Set is a team VM's template set: the one a deploy's tag-set step
	// records, or the one a teardown found (SetOf), for its review. Sets are
	// all the set tags a teardown found on it (SetTags).
	Set      Set       `json:"set,omitempty"`
	Sets     SetRecord `json:"sets,omitempty"`
	Steps    []Step    `json:"steps"`
	Snapshot string    `json:"snapshot"` // reset: snapshot to roll back to; snapshot: the one to take
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

	// Base-image and environment items (Team is ""): Source is cloned instead
	// of a template, into Pool (for image-register, image-edit, env-create and
	// env-add). Tags is the whole tag list the label step sets ("" clears
	// them); Notes, if set, is the notes line it writes. An environment
	// teardown's or host delete's items carry the VM's tags and notes as they
	// are, for its review. NICs maps NIC keys to the bridge the network step
	// puts them on. Guest is what configure-guest sets inside the VM.
	// Supersedes is the version a registered image replaces, tagged deprecated
	// once it is a template. Details are what the details step writes.
	Source     *Source           `json:"source,omitempty"`
	Pool       string            `json:"pool,omitempty"`
	Rename     string            `json:"rename,omitempty"`
	Tags       string            `json:"tags,omitempty"`
	Notes      *Notes            `json:"notes,omitempty"`
	NICs       map[string]string `json:"nics,omitempty"`
	CPU        string            `json:"cpu,omitempty"`
	Guest      *Guest            `json:"guest,omitempty"`
	Supersedes *Source           `json:"supersedes,omitempty"`
	Details    *ImageNotes       `json:"details,omitempty"`
	// DetailsWas is an Edit notes's or Edit host's notes as read, for its
	// review and fingerprint.
	DetailsWas *NotesRead `json:"details_was,omitempty"`
}

// TargetPool is the Proxmox pool of it: its Pool if set, else its team's
// pool when it has a Team.
func (it Item) TargetPool(naming Naming) string {
	if it.Pool != "" {
		return it.Pool
	}
	if it.Team != "" {
		return naming.Pool(it.Team)
	}
	return ""
}

// ItemPool is the Proxmox pool of the planned item with name or team.
func (plan *Plan) ItemPool(naming Naming, name, team string) string {
	if team != "" {
		return naming.Pool(team)
	}
	for _, it := range plan.Items {
		if it.Name == name && it.Pool != "" {
			return it.Pool
		}
	}
	return ""
}

// Plan is what an operation will do: the templates a deploy needs and one
// item per VM. It is stored with each job, so its JSON keys are a stored
// format.
type Plan struct {
	Kind      Kind           `json:"kind"`
	Teams     []string       `json:"teams"`
	Templates []TemplateSpec `json:"templates"`
	Items     []Item         `json:"items"`
	// Config is the ConfigHash the plan was made under; plans stored without
	// it are stale.
	Config string `json:"config,omitempty"`
	// Locks are lock keys beyond teams, templates and VMs (slot:NN,
	// domain:…, family:…), so jobs over the same slot, domain or family wait.
	Locks []string `json:"locks,omitempty"`
	// Notices are what a person should know the plan leaves alone, such as
	// VMs named like team VMs that aren't; nothing acts on them.
	Notices []string `json:"notices,omitempty"`
	// Slot is an environment plan's dev slot (SlotName); Locks holds it.
	// Plans stored before it have it only in Locks: read it with DevSlot.
	Slot string `json:"slot,omitempty"`
	// Kept are the VMs a teardown leaves alone, and why: an environment's,
	// those outside its pool on its networks; a set's, the pods' other VMs.
	Kept []KeptVM `json:"kept,omitempty"`
}

// KeptVM is a VM a plan leaves alone: Why completes "Name is ...".
type KeptVM struct {
	Name string `json:"name"`
	Why  string `json:"why"`
}

// LockKeys are what a plan changes and, apart, what it only reads (its
// items' Sources), as lock keys (see the store's blocksSQL). Besides its own
// VMs, a plan changes the image a register retires and the master of a
// template it builds (the build may stop it).
func (plan *Plan) LockKeys() (writes, reads []string) {
	w, r := map[string]bool{}, map[string]bool{}
	vmid := func(set map[string]bool, id int) {
		if id != 0 {
			set["vmid:"+strconv.Itoa(id)] = true
		}
	}
	for _, t := range plan.Teams {
		w["team:"+t] = true
	}
	for _, t := range plan.Templates {
		w["template:"+t.Name] = true
		vmid(w, t.VMID)
		if t.Builds() {
			vmid(w, t.MasterVMID)
		}
	}
	for _, it := range plan.Items {
		vmid(w, it.VMID)
		if it.Source != nil {
			vmid(r, it.Source.VMID)
		}
		if it.Supersedes != nil {
			vmid(w, it.Supersedes.VMID)
		}
	}
	for _, k := range plan.Locks {
		w[k] = true
	}
	for k := range w {
		delete(r, k) // a VM the plan changes is locked as such
	}
	return slices.Sorted(maps.Keys(w)), slices.Sorted(maps.Keys(r))
}

// ConfigHash identifies the settings that change what a plan of kind does
// beyond what the plan says. A job runs under the config of the process
// that claims it, so a plan made under other settings must not pass. It is
// "" for kinds no setting affects (see confighash_test.go).
func ConfigHash(kind Kind, c config.Config) string {
	fn := specs[kind].config
	if fn == nil {
		return ""
	}
	b, _ := json.Marshal(fn(c)) // strings, ints, bools, a duration and a LAN's text can't fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func deployConfigHash(c config.Config) any {
	return struct {
		Network          config.Network
		Storage          string
		Linked           bool
		DiskRead, Write  int
		Snapshot         string
		BaselinePatterns []string
		Pool             string
		TemplatePool     string
	}{c.Network, c.Deploy.Storage, c.Deploy.Linked, c.Deploy.DiskMBpsRead, c.Deploy.DiskMBpsWrite,
		c.Deploy.SnapshotName, c.Deploy.BaselinePatterns, c.Naming.Pool, c.Images.Pool}
}

func imageConfigHash(c config.Config) any {
	return struct {
		Images  config.Images
		Storage string
	}{c.Images, c.Deploy.Storage}
}

func envConfigHash(c config.Config) any {
	return struct {
		Environments config.Environments
		Storage      string
		MasterTag    string
	}{c.Environments, c.Deploy.Storage, c.Deploy.MasterTag}
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

// Planner builds plans and reads live state from the cluster using read-only
// API calls (Reads).
type Planner struct {
	API    Reads
	Cfg    config.Config
	Naming Naming
	// Access, if set, is the planning user's privileges: jobs.BuildPlan
	// blocks what they lack (BlockUnpermitted).
	Access Access
	// Reserved, if set, returns the lock keys other pending or running jobs
	// write ("vmid:8012", "slot:05"): VMIDs and slots they will create but may
	// not have yet, which a free VMID or slot must not be.
	Reserved func(ctx context.Context) (map[string]bool, error)
}

// NewPlanner plans with api. If api can read the caller's privileges (a
// Proxmox client acting as someone), plans are checked against them.
func NewPlanner(api Reads, cfg config.Config) Planner {
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
		return nil, errors.New("no pods given")
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
	masters, untagged, err := FindMasters(vms, p.Naming, req.Pattern, p.Cfg.Deploy.MasterTag)
	if err != nil {
		return nil, err
	}
	var kept []proxmox.VM
	for _, m := range masters {
		if host, _ := p.Naming.SplitMaster(m.Name); MatchesHost(host, req.Hosts) {
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

	slotByPool := map[string]Slot{}
	for _, s := range Slots(p.Cfg.Environments) {
		slotByPool[s.Pool] = s
	}
	plan := &Plan{Kind: KindDeploy, Teams: req.Teams, Config: ConfigHash(KindDeploy, p.Cfg)}
	for _, u := range untagged {
		if _, inSlot := slotByPool[u.Pool]; inSlot {
			if host, _ := p.Naming.SplitMaster(u.Name); MatchesHost(host, req.Hosts) {
				plan.Notices = append(plan.Notices, fmt.Sprintf("Left out: %s in %s lacks tag %q.", u.Name, u.Pool, p.Cfg.Deploy.MasterTag))
			}
		}
	}
	cpuModels := p.customCPUModels(ctx, nodes[0])
	for _, m := range kept {
		spec, ws, err := p.templateSpec(ctx, m, req.Rebuild, byName, used, vms, slotByPool, cpuModels)
		if err != nil {
			return nil, err
		}
		if len(ws) > 0 {
			plan.Notices = append(plan.Notices, fmt.Sprintf("%s: %s.", m.Name, strings.Join(ws, "; ")))
		}
		spec.WillStopMaster = spec.Blocked == "" && spec.Builds() && m.Status == "running"
		plan.Templates = append(plan.Templates, spec)
	}
	p.blockDuplicateHosts(plan.Templates, req.Teams[0])

	byVMID := map[int]proxmox.VM{}
	for _, vm := range vms {
		byVMID[vm.VMID] = vm
	}
	var podVMs []proxmox.VM
	for _, tpl := range plan.Templates {
		if tpl.Blocked == "" {
			id := tpl.VMID
			if tpl.Builds() {
				id = tpl.MasterVMID
			}
			podVMs = append(podVMs, byVMID[id])
		}
	}
	assign := p.Naming.AssignNodes(req.Teams, nodes, vms, podVMs)
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
		msg := fmt.Sprintf("hosts of %s and %s both map to pod VM names like %s; narrow the template pattern",
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
	byName map[string][]proxmox.VM, used map[int]string, vms []proxmox.VM,
	slotByPool map[string]Slot, cpuModels []string) (TemplateSpec, []string, error) {
	cfg, err := p.API.VMConfig(ctx, m.Node, m.VMID)
	if err != nil {
		return TemplateSpec{}, nil, fmt.Errorf("reading master %s: %w", m.Name, err)
	}
	ws := p.masterWarnings(m, cfg, slotByPool, cpuModels)
	host, _ := p.Naming.SplitMaster(m.Name)
	spec := TemplateSpec{
		Name:       p.Naming.TemplateName(m.Name),
		Host:       host,
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
			return TemplateSpec{}, nil, fmt.Errorf("reading template %s: %w", spec.Name, err)
		}
		spec.Interfaces, spec.GPU = p.describe(tcfg)
		spec.CPU, spec.CloudInit, spec.Bridges = tcfg["cpu"], HasCloudInit(tcfg), NICBridges(tcfg)
	case exists && rebuild:
		// Rebuilding deletes the template, which would break team VMs linked
		// to it. Proxmox itself refuses to free a base disk any other linked
		// clone still uses.
		for _, vm := range vms {
			if _, host, ok := p.Naming.TeamVM(vm); ok && host == spec.Host {
				spec.Blocked = fmt.Sprintf("pod VMs such as %s still use this template; tear them down first", vm.Name)
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
	return spec, ws, nil
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
			return nil, fmt.Errorf("invalid pod %q", t)
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
	_, it.Set = p.Naming.SplitMaster(tpl.MasterName)
	if tpl.Blocked != "" {
		// The template's problem comes first: its VMID is meaningless, so the
		// checks below would give wrong advice.
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
		} else if kind, _, _ := p.Naming.Classify(vm); kind == TeamTemplate {
			it.Blocked = "is a template, not a pod VM"
		} else if kind == OutsideBlock {
			it.Blocked = outsideBlock(vm, team)
		} else if want, wrong := p.Naming.WrongPool(vm, team); wrong {
			it.Blocked = wrongPool(vm, want)
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
	it.Steps = append(it.Steps, StepNetwork, StepDiskLimits, StepCDROM, StepNotes, StepSetTag)
	if snapshot {
		it.Steps = append(it.Steps, StepSnapshot)
	}
	it.Steps = append(it.Steps, StepStart)
	return it
}

// Teardown stops and deletes pod VMs among hosts: with set "", every one
// in teams' pods; with a set, only those SetOf says are of it, keeping the
// rest (Kept). Without hosts it also reports half-deleted VMs (each lost
// its tags with its config) and frees the disks gone VMs left at the clone
// VMIDs of the templates, only the set's with one: such disks have no VM or
// tag to go by.
func (p Planner) Teardown(ctx context.Context, teams, hosts []string, set Set) (*Plan, error) {
	plan, vms, err := p.simple(ctx, KindTeardown, teams, hosts, func(it *Item) {
		it.Steps = []Step{StepStop, StepDelete}
	})
	if err != nil {
		return nil, err
	}
	byVMID := map[int]proxmox.VM{}
	for _, vm := range vms {
		byVMID[vm.VMID] = vm
	}
	var items []Item
	for _, it := range plan.Items {
		vm := byVMID[it.VMID]
		it.Sets = SetTags(vm.Tags)
		it.Set, _ = it.Sets.Of()
		if set != "" && it.Set != set {
			plan.Kept = append(plan.Kept, KeptVM{Name: it.Name, Why: keptBySet(it.Sets)})
			continue
		}
		items = append(items, it)
	}
	plan.Items = items
	if len(hosts) > 0 {
		return plan, nil
	}
	p.halfDeleted(plan, vms)
	err = p.orphanedDisks(ctx, plan, vms, p.orphanCandidates(plan.Teams, vms, set), func(vmid int, node string) Item {
		team, _ := p.Naming.TeamOfCloneVMID(vmid) // a candidate is a team's clone VMID
		return Item{Team: team, Name: p.Naming.VMName(team, "disks-"+strconv.Itoa(vmid)), VMID: vmid, Node: node,
			Steps: []Step{StepFreeDisks}}
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// halfDeleted adds a blocked delete item for each clone VMID in plan's pods
// that a failed delete left locked as destroyed. Such a VM has lost its
// name and tags, so planning by name or set misses it, and only root can
// finish it (--skiplock).
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
			Blocked: HalfDeletedText(vm.VMID, vm.Node)})
	}
	sort.SliceStable(plan.Items, func(i, j int) bool { return plan.Items[i].Name < plan.Items[j].Name })
}

// HalfDeletedText says that a failed destroy task left VM vmid on node
// half-deleted, and how an admin finishes it.
func HalfDeletedText(vmid int, node string) string {
	return fmt.Sprintf("Proxmox left VM %d half-deleted (locked as destroyed); an admin must finish it: `qm destroy %d --skiplock --purge` on %s",
		vmid, vmid, node)
}

// orphanedDisks adds a free-disks item, made by item, for each VMID of
// cands with disks on deploy.storage but no VM (a teardown cut off before
// freeing them). Only volumes a candidate owns count, so a file of no VMID
// (VolumeOwner 0) is never an item. It lists every node in storageNodes, so
// node-local storage is covered. Proxmox shows a gone VM's disks only to
// users with Datastore.Allocate, which freeing needs; others see none.
func (p Planner) orphanedDisks(ctx context.Context, plan *Plan, vms []proxmox.VM, cands []int, item func(vmid int, node string) Item) error {
	nodes, err := p.API.OnlineNodes(ctx)
	if err != nil {
		return err
	}
	use, err := p.vmidUse(ctx, vms)
	if err != nil {
		return err
	}
	held := maps.Clone(use.listed) // and, as items are added, VMIDs already itemized
	nodes, err = p.storageNodes(ctx, nodes)
	if err != nil {
		return err
	}
	// Each candidate is asked about on its own: one VMID takes a moment, while
	// a whole listing runs qemu-img on every disk on the storage and outgrows
	// Proxmox's proxy timeout once a few hundred VMs are deployed.
	cands = slices.DeleteFunc(slices.Clone(cands), func(id int) bool { return held[id] })
	listed := make([][]string, len(nodes))
	var mu sync.Mutex
	var g errgroup.Group
	g.SetLimit(orphanListings)
	for i, node := range nodes {
		for _, vmid := range cands {
			g.Go(func() error {
				vols, err := p.API.StorageContent(ctx, node, p.Cfg.Deploy.Storage, vmid)
				switch {
				case proxmox.IsForbidden(err):
					return nil
				case err != nil:
					return fmt.Errorf("listing %s on %s for disks gone VMs left: %w", p.Cfg.Deploy.Storage, node, err)
				}
				mu.Lock()
				listed[i] = append(listed[i], vols...)
				mu.Unlock()
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return err
	}
	for i, node := range nodes {
		for _, v := range listed[i] {
			vmid, _ := VolumeOwner(v)
			if !slices.Contains(cands, vmid) || held[vmid] {
				continue
			}
			held[vmid] = true // one item per VMID, from the first node that lists it
			// A VM the user can't see may hold it: the disks are its own.
			if taken, err := use.inUse(ctx, vmid); err != nil || taken {
				if err != nil {
					return fmt.Errorf("checking whether VMID %d is free: %w", vmid, err)
				}
				continue
			}
			plan.Items = append(plan.Items, item(vmid, node))
		}
	}
	sort.SliceStable(plan.Items, func(i, j int) bool { return plan.Items[i].Name < plan.Items[j].Name })
	return nil
}

// orphanListings is how many storage listings run at once.
const orphanListings = 16

// orphanCandidates are the VMIDs teams' VMs get when cloned from templates
// in template_vmid_base..template_vmid_base+99 (CloneVMID), only set's when
// set is non-empty, where a gone VM's disks would be. Disks of VMs whose
// template is gone or outside the template VMID range aren't looked for.
func (p Planner) orphanCandidates(teams []string, vms []proxmox.VM, set Set) []int {
	base := p.Cfg.Naming.TemplateVMIDBase
	var templates []proxmox.VM
	for _, vm := range vms {
		if !vm.Template || !p.Naming.IsTemplateName(vm.Name) || vm.VMID < base || vm.VMID >= base+100 {
			continue
		}
		if _, of := p.Naming.SplitMaster(vm.Name); set != "" && of != set {
			continue
		}
		templates = append(templates, vm)
	}
	var out []int
	for _, t := range teams {
		for _, vm := range templates {
			if id := p.Naming.CloneVMID(t, vm.VMID); !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	slices.Sort(out)
	return out
}

// storageNodes are the online nodes to list deploy.storage on: one if the
// storage is shared, else those that have it. Without a resources read it
// is every online node.
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

// Reset rolls team VMs back to snapshot and starts them; "" means each VM's
// own baseline (see BaselineSnapshot). VMs without it are blocked.
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
		if it.Blocked != "" {
			continue
		}
		snaps, err := p.API.Snapshots(ctx, it.Node, it.VMID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			it.Blocked = "cannot list snapshots: " + proxmox.Describe(err)
			continue
		}
		names := SnapshotNames(Finished(snaps))
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

// outsideBlock is why a VM named like one of team's isn't treated as one.
func outsideBlock(vm proxmox.VM, team string) string {
	return fmt.Sprintf("has pod %s's name but is VMID %d, outside the pod's VMIDs, so it isn't a pod VM; rename or remove it in Proxmox", team, vm.VMID)
}

// wrongPool is why a team VM in another pool is left alone.
func wrongPool(vm proxmox.VM, want string) string {
	return fmt.Sprintf("is in pool %s, not %s; move it back to the pod's pool in Proxmox first", vm.Pool, want)
}

func (p Planner) simple(ctx context.Context, kind Kind, teams, hosts []string, fill func(*Item)) (*Plan, []proxmox.VM, error) {
	teams, err := normalizeTeams(teams)
	if err != nil {
		return nil, nil, err
	}
	if len(teams) == 0 {
		return nil, nil, errors.New("no pods given")
	}
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		return nil, nil, err
	}
	plan := &Plan{Kind: kind, Teams: teams, Config: ConfigHash(kind, p.Cfg)}
	teamVMs, outside := findForTeams(vms, p.Naming, teams, hosts)
	for _, vm := range teamVMs {
		it := Item{Team: vm.Team, Host: vm.Host, Name: vm.Name, VMID: vm.VMID, Node: vm.Node}
		fill(&it)
		if want, wrong := vm.WrongPool(p.Naming); wrong {
			it.Blocked = wrongPool(vm.VM, want)
		}
		plan.Items = append(plan.Items, it)
	}
	// VMs with a team's name outside its block aren't team VMs: say so, so
	// a teardown doesn't seem to miss them silently.
	for _, vm := range outside {
		plan.Notices = append(plan.Notices, fmt.Sprintf("Left alone: %s %s.", vm.Name, outsideBlock(vm.VM, vm.Team)))
	}
	sort.SliceStable(plan.Items, func(i, j int) bool { return plan.Items[i].Name < plan.Items[j].Name })
	return plan, vms, nil
}
