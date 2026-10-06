package pods

import (
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/wccomps/battleship/internal/proxmox"
)

// WildcardRegexp turns "*.kilo.alpha" into a case-insensitive, fully
// anchored regexp. (The Python tool only anchored the start.)
func WildcardRegexp(pattern string) *regexp.Regexp {
	quoted := strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, `.*`)
	return regexp.MustCompile("(?i)^" + quoted + "$")
}

func hasTag(tags, tag string) bool {
	if strings.TrimSpace(tag) == "" {
		return false
	}
	for _, t := range strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || unicode.IsSpace(r) }) {
		if strings.EqualFold(t, strings.TrimSpace(tag)) {
			return true
		}
	}
	return false
}

// FindMasters returns non-template VMs matching pattern that carry tag, sorted
// by name, plus pattern matches that lack the tag (to explain empty results).
// Names that parse as team VMs are skipped, since team clones inherit the
// master tag; with a custom naming.vm_name, make sure master names can't
// match it.
func FindMasters(vms []proxmox.VM, n Naming, pattern, tag string) (masters, untagged []proxmox.VM) {
	re := WildcardRegexp(pattern)
	for _, vm := range vms {
		_, _, isTeam := n.ParseVMName(vm.Name)
		if vm.Template || n.IsTemplateName(vm.Name) || isTeam || !re.MatchString(vm.Name) {
			continue
		}
		if hasTag(vm.Tags, tag) {
			masters = append(masters, vm)
		} else {
			untagged = append(untagged, vm)
		}
	}
	sort.Slice(masters, func(i, j int) bool { return masters[i].Name < masters[j].Name })
	return masters, untagged
}

// FindTeamVMs returns non-template team VMs for the given teams. hosts, if
// non-empty, keeps only VMs whose host contains one of the filters
// (case-insensitive substring match). Blank filters are ignored; if all
// filters are blank, no VMs match. Matching the Python tool's --hostname-filter.
func FindTeamVMs(vms []proxmox.VM, n Naming, teams, hosts []string) []proxmox.VM {
	want := map[string]bool{}
	for _, t := range teams {
		want[t] = true
	}
	var out []proxmox.VM
	for _, vm := range vms {
		team, host, ok := n.teamVM(vm)
		if !ok || !want[team] || !MatchesHost(host, hosts) {
			continue
		}
		out = append(out, vm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// teamVM is the team and host of vm if it is a team VM: named as VMName
// names them, and not a template.
func (n Naming) teamVM(vm proxmox.VM) (team, host string, ok bool) {
	team, host, ok = n.ParseVMName(vm.Name)
	if !ok || vm.Template || n.IsTemplateName(vm.Name) {
		return "", "", false
	}
	return team, host, true
}

// MatchesHost is the host filter of every operation: with no filters every
// host matches; otherwise a host matches if it contains one of the non-blank
// filters, ignoring case.
func MatchesHost(host string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		trimmed := strings.TrimSpace(f)
		if trimmed != "" && strings.Contains(strings.ToLower(host), strings.ToLower(trimmed)) {
			return true
		}
	}
	return false
}

// AssignNodes spreads teams across nodes: each team goes to the node with the
// fewest VMs, which then counts 10 more. Ties go to the alphabetically first
// node (Python used API order). No nodes gives an empty map.
func AssignNodes(teams, nodes []string, vms []proxmox.VM) map[string]string {
	if len(nodes) == 0 {
		return map[string]string{}
	}
	sorted := append([]string(nil), nodes...)
	sort.Strings(sorted)
	load := map[string]int{}
	for _, n := range sorted {
		load[n] = 0
	}
	for _, vm := range vms {
		if _, ok := load[vm.Node]; ok {
			load[vm.Node]++
		}
	}
	out := map[string]string{}
	for _, team := range teams {
		best := sorted[0]
		for _, n := range sorted[1:] {
			if load[n] < load[best] {
				best = n
			}
		}
		out[team] = best
		load[best] += 10
	}
	return out
}

// MasterSet is a template set: the masters whose names share the part after
// the host, e.g. dc.kilo.alpha and web.kilo.alpha form set "kilo.alpha",
// which a deploy builds from with the pattern "*.kilo.alpha".
type MasterSet struct {
	Name    string   // e.g. "kilo.alpha"
	Hosts   []string // sorted, each once
	Masters int      // master VMs, counting any that share a name
	Running int      // of them, those running; a template build stops them
}

// MasterSets groups the masters tagged tag (as FindMasters finds them) into
// template sets, sorted by name. Masters whose names have no part after the
// host, like "y" or "tern.", are in no set: others has their names, sorted
// and each once.
func MasterSets(vms []proxmox.VM, n Naming, tag string) (sets []MasterSet, others []string) {
	masters, _ := FindMasters(vms, n, "*", tag)
	index := map[string]int{}
	for _, m := range masters {
		host, set, _ := strings.Cut(m.Name, ".")
		if host == "" || set == "" {
			if len(others) == 0 || others[len(others)-1] != m.Name { // masters are sorted by name
				others = append(others, m.Name)
			}
			continue
		}
		i, ok := index[set]
		if !ok {
			i = len(sets)
			index[set] = i
			sets = append(sets, MasterSet{Name: set})
		}
		s := &sets[i]
		if !slices.Contains(s.Hosts, host) {
			s.Hosts = append(s.Hosts, host)
		}
		s.Masters++
		if m.Status == "running" {
			s.Running++
		}
	}
	for i := range sets {
		sort.Strings(sets[i].Hosts)
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].Name < sets[j].Name })
	return sets, others
}

// AllTeamVMs is every non-template team VM, as FindTeamVMs finds them,
// sorted by name.
func AllTeamVMs(vms []proxmox.VM, n Naming) []proxmox.VM {
	var out []proxmox.VM
	for _, vm := range vms {
		if _, _, ok := n.teamVM(vm); ok {
			out = append(out, vm)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TeamsWithVMs is the teams that have team VMs, as FindTeamVMs finds them
// (templates aside), sorted and each once. These are every team that
// exists as far as the caller can see VMs: "all teams" means them, so
// acting on them leaves none of the caller's team VMs behind.
func TeamsWithVMs(vms []proxmox.VM, n Naming) []string {
	seen := map[string]bool{}
	for _, vm := range vms {
		if team, _, ok := n.teamVM(vm); ok {
			seen[team] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
