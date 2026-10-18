package pods

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/wccomps/battleship/internal/proxmox"
)

// Set is a template set: what masters share after their host, as
// "kilo.alpha" of dc.kilo.alpha and web.Kilo.Alpha.tpl. It is lower-case,
// as Tags reads the tag that records it on a pod VM.
type Set string

// setTagPrefix starts a set tag, the tag a deploy gives a pod VM to record
// its set: "set-kilo.alpha" (StepSetTag).
const setTagPrefix = "set-"

// Tag is the tag that records s on a pod VM.
func (s Set) Tag() string { return setTagPrefix + string(s) }

// Pattern is the master pattern of s's masters: "*.kilo.alpha".
func (s Set) Pattern() string { return "*." + string(s) }

// SetOfPattern is the set a "*.<set>" deploy pattern names, case-insensitively,
// or false for any other pattern.
func SetOfPattern(pattern string) (Set, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(pattern), "*.")
	if !ok || rest == "" {
		return "", false
	}
	set, err := ParseSet(rest)
	return set, err == nil && set != ""
}

// SplitMaster splits a master or template name into its host and set:
// "dc.Kilo.Alpha.tpl" is dc and kilo.alpha. A name with nothing after its
// host has set "".
func (n Naming) SplitMaster(name string) (host string, set Set) {
	host, rest, _ := strings.Cut(strings.TrimSuffix(name, n.cfg.TemplateSuffix), ".")
	return host, Set(strings.ToLower(rest))
}

// ParseSet reads a set's name as typed, trimmed and lower-cased; "" is no
// set. It is a name, not a pattern, so "*" is refused, and it must fit in
// a Proxmox tag, as its set tag records it.
func ParseSet(s string) (Set, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.Contains(s, "*"):
		return "", fmt.Errorf("set %q is a pattern; give one set's name, e.g. kilo.alpha", s)
	case !setNameRE.MatchString(s):
		return "", fmt.Errorf("set %q isn't a set's name: letters, digits, '.', '-', '_' and '+' only", s)
	}
	return Set(strings.ToLower(s)), nil
}

// setNameRE is what fits after "set-" in a Proxmox tag (pve-tag).
var setNameRE = regexp.MustCompile(`^[A-Za-z0-9_+.-]*$`)

func isTagSeparator(r rune) bool { return r == ';' || r == ',' || unicode.IsSpace(r) }

// SetTagChanges sets the set tags in cur's tag list to set's alone (none
// for ""), keeping every other tag.
func SetTagChanges(cur map[string]string, set Set) map[string]string {
	var keep []string
	for _, t := range strings.FieldsFunc(cur["tags"], isTagSeparator) {
		if !strings.HasPrefix(strings.ToLower(t), setTagPrefix) {
			keep = append(keep, t)
		}
	}
	if set != "" {
		keep = append(keep, set.Tag())
	}
	return LabelChanges(cur, strings.Join(keep, ";"), nil)
}

// SetRecord are the sets a pod VM's set tags name, sorted, each once
// (SetTags). One non-empty set tag records the VM's set (Of); none is a VM
// with no set recorded; several, or a bare "set-" tag, are drift (Several).
type SetRecord []Set

// SetTags are the sets a tag list's set tags name, sorted, each once.
func SetTags(tags string) SetRecord {
	var out SetRecord
	for _, t := range Tags(tags) {
		if set, ok := strings.CutPrefix(t, setTagPrefix); ok {
			out = append(out, Set(set))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Of is the one set r records, or false when r has no set tag, several, or a
// bare "set-" tag.
func (r SetRecord) Of() (set Set, ok bool) {
	if len(r) != 1 || r[0] == "" {
		return "", false
	}
	return r[0], true
}

// Several reports whether r has set tags that don't name one set: several
// set tags, or a bare "set-" tag. No set tag is false.
func (r SetRecord) Several() bool {
	_, ok := r.Of()
	return len(r) > 0 && !ok
}

// Label names r's set for people: the set's name, "not recorded" when r has
// no set tag, or UnknownSet.
func (r SetRecord) Label() string {
	if set, ok := r.Of(); ok {
		return string(set)
	}
	if !r.Several() {
		return "not recorded"
	}
	return UnknownSet
}

// SetOf is a pod VM's set, as its one set tag records it. With no set tag,
// several, or a bare "set-", its set is unknown (ok false). Only the tag
// decides: a host name or a VMID may be another set's.
func SetOf(vm proxmox.VM) (set Set, ok bool) {
	return SetTags(vm.Tags).Of()
}

// UnknownSet names the set of a pod VM whose set SetOf can't tell.
const UnknownSet = "unknown set"

// keptBySet is why a teardown of a set leaves a VM with sets r, completing
// "team01-dc is ...".
func keptBySet(r SetRecord) string {
	if set, ok := r.Of(); ok {
		return "of " + string(set)
	}
	return "of an " + UnknownSet + ", with no single set tag, which only a teardown that names no set removes"
}

// DeployedSet is what of one set is deployed in some pods; Set "" is the
// VMs of an unknown set.
type DeployedSet struct {
	Set Set
	// Pods maps each pod with VMs of it to their hosts, sorted.
	Pods map[string][]string
	VMs  int
}

// PodList is the pods with VMs of it, sorted.
func (d DeployedSet) PodList() []string { return slices.Sorted(maps.Keys(d.Pods)) }

// SameHosts is the host count every pod has, or 0 if they differ.
func (d DeployedSet) SameHosts() int {
	n := -1
	for _, hosts := range d.Pods {
		switch {
		case n == -1:
			n = len(hosts)
		case n != len(hosts):
			return 0
		}
	}
	return max(n, 0)
}

// Deployed is what is deployed in teams' pods, among hosts (MatchesHost),
// grouped by set (SetOf): sets by name, then the unknown.
func (n Naming) Deployed(vms []proxmox.VM, teams, hosts []string) []DeployedSet {
	found, _ := findForTeams(vms, n, teams, hosts)
	index := map[Set]int{}
	var out []DeployedSet
	for _, vm := range found {
		set, _ := SetOf(vm.VM)
		i, ok := index[set]
		if !ok {
			i = len(out)
			index[set] = i
			out = append(out, DeployedSet{Set: set, Pods: map[string][]string{}})
		}
		d := &out[i]
		d.VMs++
		if !slices.Contains(d.Pods[vm.Team], vm.Host) {
			d.Pods[vm.Team] = append(d.Pods[vm.Team], vm.Host)
			slices.Sort(d.Pods[vm.Team])
		}
	}
	slices.SortFunc(out, func(a, b DeployedSet) int {
		if (a.Set == "") != (b.Set == "") {
			if a.Set == "" {
				return 1
			}
			return -1
		}
		return strings.Compare(string(a.Set), string(b.Set))
	})
	return out
}

// keptNamed is the most VMs of one reason KeptLines names in full.
const keptNamed = 4

// KeptLines say what a plan leaves alone (Kept), one sentence per reason:
// "team01-web and team02-web are of tango.delta." Up to keptNamed are
// named; past that, keptNamed-1 and a count.
func KeptLines(kept []KeptVM) []string {
	var whys []string
	names := map[string][]string{}
	for _, k := range kept {
		if _, ok := names[k.Why]; !ok {
			whys = append(whys, k.Why)
		}
		names[k.Why] = append(names[k.Why], k.Name)
	}
	var out []string
	for _, why := range whys {
		n := names[why]
		switch {
		case len(n) == 1:
			out = append(out, n[0]+" is "+why+".")
		case len(n) <= keptNamed:
			out = append(out, strings.Join(n[:len(n)-1], ", ")+" and "+n[len(n)-1]+" are "+why+".")
		default:
			out = append(out, strconv.Itoa(len(n))+" VMs are "+why+": "+strings.Join(n[:keptNamed-1], ", ")+
				" and "+strconv.Itoa(len(n)-keptNamed+1)+" more.")
		}
	}
	return out
}
