package pods

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/proxmox"
)

// Base-image kinds decide how an environment wires and configures a clone.
const (
	ImageRouter  = "router"
	ImageWindows = "windows"
	ImageLinux   = "linux"
	ImageOther   = "other"
)

// ImageKinds are the base-image kinds, in the order forms list them.
var ImageKinds = []string{ImageRouter, ImageWindows, ImageLinux, ImageOther}

const (
	BaseTag       = "base"
	DeprecatedTag = "deprecated"
)

// BaseImage is one version of a family: a template tagged base and one kind.
type BaseImage struct {
	Name       string // debian-12-v3
	Family     string // debian-12
	Version    int
	Kind       string
	VMID       int
	Node       string
	Deprecated bool
	Notes      string // its Proxmox notes, for people: logins, what's installed
	Source     int    // the VMID it was registered from, if its notes say
}

// MaxImageVersion is the highest version a base image name carries.
const MaxImageVersion = 9999

var (
	familyRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	imageNameRE = regexp.MustCompile(`^(.+)-v([1-9][0-9]{0,3})$`)
)

// CheckFamily checks a base-image family name.
func CheckFamily(f string) error {
	if !familyRE.MatchString(f) {
		return fmt.Errorf("image family %q must be 1-40 lower-case letters, digits and dashes, starting and ending with a letter or digit", f)
	}
	if imageNameRE.MatchString(f) {
		return fmt.Errorf("image family %q must not end in -v<number>: Battleship adds the version", f)
	}
	return nil
}

func ImageName(family string, version int) string { return family + "-v" + strconv.Itoa(version) }

// ParseImageName splits "debian-12-v3" into its family and version.
func ParseImageName(name string) (family string, version int, ok bool) {
	m := imageNameRE.FindStringSubmatch(name)
	if m == nil || CheckFamily(m[1]) != nil {
		return "", 0, false
	}
	v, _ := strconv.Atoi(m[2])
	return m[1], v, true
}

// Tags splits a Proxmox tag list, lower-cased.
func Tags(s string) []string {
	var out []string
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' || unicode.IsSpace(r) }) {
		out = append(out, strings.ToLower(t))
	}
	return out
}

// ImageTags is the tag list of a base image of kind.
func ImageTags(kind string, deprecated bool) string {
	s := BaseTag + ";" + kind
	if deprecated {
		s += ";" + DeprecatedTag
	}
	return s
}

// BaseImages lists the templates tagged base with exactly one kind and a
// versioned name, by family then version.
func BaseImages(vms []proxmox.VM) []BaseImage {
	var out []BaseImage
	for _, vm := range vms {
		tags := Tags(vm.Tags)
		if !vm.Template || !slices.Contains(tags, BaseTag) {
			continue
		}
		var kinds []string
		for _, k := range ImageKinds {
			if slices.Contains(tags, k) {
				kinds = append(kinds, k)
			}
		}
		family, version, ok := ParseImageName(vm.Name)
		if len(kinds) != 1 || !ok {
			continue
		}
		out = append(out, BaseImage{Name: vm.Name, Family: family, Version: version, Kind: kinds[0],
			VMID: vm.VMID, Node: vm.Node, Deprecated: slices.Contains(tags, DeprecatedTag)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Family != out[j].Family {
			return out[i].Family < out[j].Family
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// Newest is family's highest version that isn't deprecated.
func Newest(images []BaseImage, family string) (BaseImage, bool) {
	var best BaseImage
	for _, im := range images {
		if im.Family == family && !im.Deprecated && im.Version > best.Version {
			best = im
		}
	}
	return best, best.Version > 0
}

// Latest is family's highest version, deprecated or not: the next version
// number follows it.
func Latest(images []BaseImage, family string) (BaseImage, bool) {
	var best BaseImage
	for _, im := range images {
		if im.Family == family && im.Version > best.Version {
			best = im
		}
	}
	return best, best.Version > 0
}

// Families is each family's newest usable version, of kind if not "".
func Families(images []BaseImage, kind string) []BaseImage {
	var out []BaseImage
	for _, im := range images {
		if kind != "" && im.Kind != kind {
			continue
		}
		if n, ok := Newest(images, im.Family); ok && n.VMID == im.VMID {
			out = append(out, im)
		}
	}
	return out
}

// ImageByName finds a base image by its full name.
func ImageByName(images []BaseImage, name string) (BaseImage, bool) {
	for _, im := range images {
		if im.Name == name {
			return im, true
		}
	}
	return BaseImage{}, false
}

// ReadNotes reads each image's notes as the user. Notes are only shown, so an
// image whose config can't be read keeps none.
func (p Planner) ReadNotes(ctx context.Context, images []BaseImage) []BaseImage {
	out := slices.Clone(images)
	vms := make([]proxmox.VM, len(out))
	for i, im := range out {
		vms[i] = proxmox.VM{VMID: im.VMID, Node: im.Node}
	}
	p.eachConfig(ctx, vms, func(i int, cfg map[string]string, err error) {
		if err == nil {
			out[i].Notes = UserNotes(cfg["description"])
			out[i].Source = sourceVMID(cfg["description"])
		}
	})
	return out
}

// sourceVMID is the VMID desc's notes line says an image was registered
// from, or 0.
func sourceVMID(desc string) int {
	n, _ := ParseNotes(desc)
	id, err := strconv.Atoi(n.Source)
	if err != nil || strconv.Itoa(id) != n.Source || id < 100 {
		return 0
	}
	return id
}

// FamilyNames are the families of images, each once, sorted.
func FamilyNames(images []BaseImage) []string {
	var out []string
	for _, im := range images {
		if !slices.Contains(out, im.Family) {
			out = append(out, im.Family)
		}
	}
	slices.Sort(out)
	return out
}

// RegisterCandidate is a VM in the images pool that could be copied in as
// a base image, with what the register form would be filled in with.
type RegisterCandidate struct {
	VMID       int
	Name, Node string
	Family     string    // suggested (see RegisterCandidates); "" if nothing says
	Kind       string    // its family's kind, or by its OS type; "" if neither says
	From       BaseImage // the highest version registered from it, if any
	Notes      string
	Blocked    string // why it can't be registered as it is (see ImageRegister)
}

// SuggestFamily is the family a VM named name likely is: the name
// lower-cased, less a -build suffix (or an edit copy's -edit), or "" if
// that isn't a valid family.
func SuggestFamily(name string) string {
	f := strings.ToLower(name)
	for _, suffix := range []string{"-build", EditSuffix} {
		if t, ok := strings.CutSuffix(f, suffix); ok {
			f = t
			break
		}
	}
	if CheckFamily(f) != nil {
		return ""
	}
	return f
}

// KindForOSType is the image kind Proxmox's ostype implies: windows for its
// Windows types (wxp through win11), linux for l24 and l26, else "". A
// router reports its own OS's type, so its kind is never implied.
func KindForOSType(ostype string) string {
	switch {
	case strings.HasPrefix(ostype, "w"):
		return ImageWindows
	case ostype == "l24" || ostype == "l26":
		return ImageLinux
	}
	return ""
}

// RegisterCandidates lists the VMs in the images pool that aren't templates
// or busy (a lock, such as a register's clone in progress), by VMID, reading
// each one's config as the user. Each gets a family: the one it was
// registered as (images must carry their Source; see ReadNotes), else the
// one its notes say an edit copy came from, else SuggestFamily; a kind; and
// why ImageRegister would refuse it, if it would.
func (p Planner) RegisterCandidates(ctx context.Context, vms []proxmox.VM, images []BaseImage) []RegisterCandidate {
	var out []RegisterCandidate
	var srcs []proxmox.VM
	for _, vm := range vms {
		if vm.Pool != p.Cfg.Images.Pool || vm.Template || vm.Lock != "" {
			continue
		}
		c := RegisterCandidate{VMID: vm.VMID, Name: vm.Name, Node: vm.Node, Blocked: p.sourceBlock(vm)}
		for _, im := range images {
			if im.Source == vm.VMID && im.Version > c.From.Version {
				c.From = im
			}
		}
		out, srcs = append(out, c), append(srcs, vm)
	}
	p.eachConfig(ctx, srcs, func(i int, cfg map[string]string, err error) {
		c := &out[i]
		if err == nil {
			c.Notes = UserNotes(cfg["description"])
		}
		c.Family = c.From.Family
		if f, _, ok := ParseImageName(baseNote(cfg)); c.Family == "" && ok {
			c.Family = f
		}
		if c.Family == "" {
			c.Family = SuggestFamily(c.Name)
		}
		if latest, ok := Latest(images, c.Family); ok {
			c.Kind = latest.Kind
		} else {
			c.Kind = KindForOSType(cfg["ostype"])
		}
		if c.Blocked == "" && err == nil {
			c.Blocked = agentBlock(srcs[i], cfg, c.Kind)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out
}

func baseNote(cfg map[string]string) string {
	n, _ := ParseNotes(cfg["description"])
	return n.Base
}

// eachConfig reads each VM's config as the user, envReads at a time, and
// calls fn with it, or with the error (and a nil config).
func (p Planner) eachConfig(ctx context.Context, vms []proxmox.VM, fn func(i int, cfg map[string]string, err error)) {
	var mu sync.Mutex
	var g errgroup.Group
	g.SetLimit(envReads)
	for i, vm := range vms {
		g.Go(func() error {
			cfg, err := p.API.VMConfig(ctx, vm.Node, vm.VMID)
			mu.Lock()
			defer mu.Unlock()
			fn(i, cfg, err)
			return nil
		})
	}
	_ = g.Wait()
}
