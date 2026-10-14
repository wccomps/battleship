package pods

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

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
}

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
