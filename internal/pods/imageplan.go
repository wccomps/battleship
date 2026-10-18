package pods

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// ImageRequest is a base-image job's input.
type ImageRequest struct {
	VMID   int    // register: the VM to copy
	Family string // register, edit
	Kind   string // register a new family; a new version keeps its family's
	Image  string // deprecate, delete: e.g. debian-12-v1
	// Details are what Edit notes writes, and what register writes: nil
	// is those of its source's notes (see RegisterDetails), and empty is
	// none, the copy keeping its source's notes as they are.
	Details *ImageNotes
}

// agentEnabled reads a VM's agent option ("1", "enabled=1,…", "1,fstrim…").
func agentEnabled(v string) bool {
	first, _, _ := strings.Cut(v, ",")
	return first == "1" || strings.Contains(","+v+",", ",enabled=1,")
}

// freeVMID is the lowest VMID in lo..hi that no listed VM, no VM the user
// can't see (VMIDHeld) and nothing in taken holds.
func (p Planner) freeVMID(ctx context.Context, vms []proxmox.VM, lo, hi int, taken map[int]bool) (int, error) {
	use, err := p.vmidUse(ctx, vms)
	if err != nil {
		return 0, err
	}
	for id := lo; id <= hi; id++ {
		if taken[id] {
			continue
		}
		held, err := use.inUse(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("checking whether VMID %d is free: %w", id, err)
		}
		if !held {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free VMID in %d-%d", lo, hi)
}

func vmByID(vms []proxmox.VM, vmid int) (proxmox.VM, bool) {
	i := slices.IndexFunc(vms, func(v proxmox.VM) bool { return v.VMID == vmid })
	if i < 0 {
		return proxmox.VM{}, false
	}
	return vms[i], true
}

func vmsNamed(vms []proxmox.VM, name string) []proxmox.VM {
	var out []proxmox.VM
	for _, v := range vms {
		if v.Name == name {
			out = append(out, v)
		}
	}
	return out
}

// sourceBridges are the distinct bridges of a clone source's NICs.
func sourceBridges(cfg map[string]string) []string {
	return slices.Compact(slices.Sorted(slices.Values(nicBridgesAll(cfg))))
}

// ImageRegister plans copying a VM in as its family's next version.
func (p Planner) ImageRegister(ctx context.Context, req ImageRequest) (*Plan, error) {
	if err := CheckFamily(req.Family); err != nil {
		return nil, err
	}
	if req.Details != nil {
		if err := req.Details.check(); err != nil {
			return nil, err
		}
	}
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		return nil, err
	}
	src, ok := vmByID(vms, req.VMID)
	if !ok {
		return nil, fmt.Errorf("there is no VM %d, or you can't see it", req.VMID)
	}
	images := BaseImages(vms)
	latest, exists := Latest(images, req.Family)
	kind := req.Kind
	if exists {
		if kind != "" && kind != latest.Kind {
			return nil, fmt.Errorf("family %s is %s; a new version keeps its family's kind", req.Family, latest.Kind)
		}
		kind = latest.Kind
	}
	if !slices.Contains(ImageKinds, kind) {
		return nil, fmt.Errorf("the image kind must be one of %s, not %q", strings.Join(ImageKinds, ", "), kind)
	}
	version := 1
	if exists {
		version = latest.Version + 1
	}
	lo, hi, _ := config.ParseRange(p.Cfg.Images.VMIDRange)
	vmid, err := p.freeVMID(ctx, vms, lo, hi, nil)
	if err != nil {
		return nil, err
	}
	it := Item{Name: ImageName(req.Family, version), VMID: vmid, Node: src.Node, Pool: p.Cfg.Images.Pool,
		Source: &Source{VMID: src.VMID, Node: src.Node, Name: src.Name}, Tags: ImageTags(kind, false),
		Notes: &Notes{Source: strconv.Itoa(src.VMID)}, Steps: []Step{StepClone, StepLabel, StepCDROM, StepConvert}}
	if newest, ok := Newest(images, req.Family); ok {
		it.Supersedes = &Source{VMID: newest.VMID, Node: newest.Node, Name: newest.Name}
		it.Steps = append(it.Steps, StepRetire)
	}
	switch {
	case version > MaxImageVersion:
		it.Blocked = fmt.Sprintf("family %s is at v%d, the last version a base image name holds; register it as a new family", req.Family, MaxImageVersion)
	case p.sourceBlock(src) != "":
		it.Blocked = p.sourceBlock(src)
	case len(vmsNamed(vms, it.Name)) > 0:
		it.Blocked = "a VM named " + it.Name + " already exists"
	}
	var desc string // the source's notes, read unless it is blocked
	if it.Blocked == "" {
		cfg, err := p.API.VMConfig(ctx, src.Node, src.VMID)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", src.Name, err)
		}
		desc = cfg["description"]
		var derr error
		it.Source.Bridges, it.Source.Base = sourceBridges(cfg), baseNote(cfg)
		_, it.Source.Notes, derr = ParseDetails(desc)
		it.Source.Unreadable = derr != nil
		it.Blocked = agentBlock(src, cfg, kind)
		if d, ok := RegisterDetails(desc); ok && !d.Empty() && req.Details == nil {
			it.Details = &ImageNotes{ImageDetails: d}
		}
	}
	if req.Details != nil && !req.Details.ImageDetails.Empty() {
		d := *req.Details
		it.Details = &d
	}
	if it.Details != nil {
		it.Steps = slices.Insert(it.Steps, slices.Index(it.Steps, StepLabel)+1, StepDetails)
	}
	it.blockLongNotes(desc)
	return &Plan{Kind: KindImageRegister, Config: ConfigHash(KindImageRegister, p.Cfg), Items: []Item{it},
		Locks: []string{"family:" + req.Family}}, nil
}

// sourceBlock is why ImageRegister won't copy src in, whatever its family,
// or "".
func (p Planner) sourceBlock(src proxmox.VM) string {
	srcKind, srcTeam, _ := p.Naming.Classify(src)
	switch {
	case src.Template:
		return src.Name + " is a template; register the VM it was made from, or open the family for editing"
	case srcKind == TeamVMKind:
		return src.Name + " is a pod VM"
	case srcKind == OutsideBlock:
		return src.Name + " " + outsideBlock(src, srcTeam)
	case src.Status != "stopped":
		return fmt.Sprintf("%s is %s; shut it down first so the copy is consistent", src.Name, src.Status)
	}
	return ""
}

// agentBlock is why src, with config cfg, can't be a base image of kind, or
// "": environments set a Windows or Linux host up through the guest agent.
func agentBlock(src proxmox.VM, cfg map[string]string, kind string) string {
	if (kind == ImageWindows || kind == ImageLinux) && !agentEnabled(cfg["agent"]) {
		return src.Name + " has no QEMU guest agent enabled (Options, QEMU Guest Agent); environments set hostname and address through it"
	}
	return ""
}

// EditSuffix ends the name of a family's working copy (see ImageEdit).
const EditSuffix = "-edit"

// ImageEdit plans a working copy of a family's newest version, every NIC on
// the edit bridge, with its tags cleared so it isn't taken for a base image.
func (p Planner) ImageEdit(ctx context.Context, req ImageRequest) (*Plan, error) {
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		return nil, err
	}
	newest, ok := Newest(BaseImages(vms), req.Family)
	if !ok {
		return nil, fmt.Errorf("family %q has no version that isn't deprecated", req.Family)
	}
	lo, hi, _ := config.ParseRange(p.Cfg.Images.VMIDRange)
	vmid, err := p.freeVMID(ctx, vms, lo, hi, nil)
	if err != nil {
		return nil, err
	}
	it := Item{Name: req.Family + EditSuffix, VMID: vmid, Node: newest.Node, Pool: p.Cfg.Images.Pool,
		Source: &Source{VMID: newest.VMID, Node: newest.Node, Name: newest.Name}, Notes: &Notes{Base: newest.Name},
		NICs:  map[string]string{"net0": p.Cfg.Images.EditBridge},
		Steps: []Step{StepClone, StepLabel, StepNetwork}}
	if len(vmsNamed(vms, it.Name)) > 0 {
		it.Blocked = it.Name + " already exists: register it as the next version, or delete it in Proxmox, first"
	} else {
		cfg, err := p.API.VMConfig(ctx, newest.Node, newest.VMID)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", newest.Name, err)
		}
		it.Source.Bridges = sourceBridges(cfg)
		it.blockLongNotes(cfg["description"])
		// Every NIC goes on the edit bridge: a router's LAN left on a slot's
		// bridge would clash with that environment's router.
		for i := range 32 {
			if key := fmt.Sprintf("net%d", i); cfg[key] != "" {
				it.NICs[key] = p.Cfg.Images.EditBridge
			}
		}
	}
	return &Plan{Kind: KindImageEdit, Config: ConfigHash(KindImageEdit, p.Cfg), Items: []Item{it},
		Locks: []string{"family:" + req.Family}}, nil
}

// imageItem finds req.Image for deprecate and delete.
func (p Planner) imageItem(ctx context.Context, req ImageRequest) (BaseImage, error) {
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		return BaseImage{}, err
	}
	im, ok := ImageByName(BaseImages(vms), req.Image)
	if !ok {
		return BaseImage{}, fmt.Errorf("there is no base image %q", req.Image)
	}
	return im, nil
}

// ImageDeprecate plans tagging a version deprecated.
func (p Planner) ImageDeprecate(ctx context.Context, req ImageRequest) (*Plan, error) {
	im, err := p.imageItem(ctx, req)
	if err != nil {
		return nil, err
	}
	it := Item{Name: im.Name, VMID: im.VMID, Node: im.Node, Tags: ImageTags(im.Kind, true), Steps: []Step{StepLabel}}
	if im.Deprecated {
		it.Blocked = "is already deprecated"
	}
	return &Plan{Kind: KindImageDeprecate, Config: ConfigHash(KindImageDeprecate, p.Cfg), Items: []Item{it},
		Locks: []string{"family:" + im.Family}}, nil
}

// ImageDelete plans deleting a version. Environment VMs are full clones, so
// none depend on it.
func (p Planner) ImageDelete(ctx context.Context, req ImageRequest) (*Plan, error) {
	im, err := p.imageItem(ctx, req)
	if err != nil {
		return nil, err
	}
	return &Plan{Kind: KindImageDelete, Config: ConfigHash(KindImageDelete, p.Cfg),
		Items: []Item{{Name: im.Name, VMID: im.VMID, Node: im.Node, Steps: []Step{StepDelete}}},
		Locks: []string{"family:" + im.Family}}, nil
}

// NotesEdit is what Edit notes on a family works on: the family's newest
// version, the one new hosts are made from, and its notes as read.
type NotesEdit struct {
	Image BaseImage
	Read  NotesRead
	desc  string
}

// Edit is what an Edit notes form starts from: the details and free text
// read, and a mark of the notes they were read from (From).
func (e NotesEdit) Edit() ImageNotes {
	free := e.Read.Free
	return ImageNotes{ImageDetails: e.Read.Details, Free: &free, From: e.from()}
}

// from marks e's version and its notes exactly as they were read.
func (e NotesEdit) from() string {
	sum := sha256.Sum256([]byte(strconv.Itoa(e.Image.VMID) + "\n" + e.desc))
	return hex.EncodeToString(sum[:])
}

// NotesToEdit reads what Edit notes on family starts from.
func (p Planner) NotesToEdit(ctx context.Context, family string) (NotesEdit, error) {
	vms, err := p.API.ClusterVMs(ctx)
	if err != nil {
		return NotesEdit{}, err
	}
	newest, ok := Newest(BaseImages(vms), family)
	if !ok {
		return NotesEdit{}, fmt.Errorf("family %q has no version that isn't deprecated", family)
	}
	cfg, err := p.API.VMConfig(ctx, newest.Node, newest.VMID)
	if err != nil {
		return NotesEdit{Image: newest}, fmt.Errorf("reading %s: %w", newest.Name, err)
	}
	return NotesEdit{Image: newest, Read: readImageNotes(cfg["description"]), desc: cfg["description"]}, nil
}

// ImageSetDetails plans Edit notes: writing details, and free text if
// asked, into the notes of a family's newest version (NotesToEdit). Older
// versions keep theirs: each is its own build, and hosts already made from
// them.
func (p Planner) ImageSetDetails(ctx context.Context, req ImageRequest) (*Plan, error) {
	if req.Details == nil {
		return nil, fmt.Errorf("name the details to write")
	}
	at, err := p.NotesToEdit(ctx, req.Family)
	if err != nil {
		return nil, err
	}
	n, same := req.Details.against(at.Read)
	if err := n.check(); err != nil {
		return nil, err
	}
	it := Item{Name: at.Image.Name, VMID: at.Image.VMID, Node: at.Image.Node, Steps: []Step{StepDetails}, Details: &n, DetailsWas: &at.Read}
	switch {
	case req.Details.From != "" && req.Details.From != at.from():
		it.Blocked = "the notes of " + at.Image.Name + " changed since you opened this form; open Edit notes again to start from them as they are"
	case same:
		it.Blocked = "the notes of " + at.Image.Name + " already say this"
	}
	it.blockLongNotes(at.desc)
	return &Plan{Kind: KindImageDetails, Config: ConfigHash(KindImageDetails, p.Cfg), Items: []Item{it},
		Locks: []string{"family:" + req.Family}}, nil
}
