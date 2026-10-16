package pods

import (
	"maps"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

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

// FindMasters returns non-template VMs matching pattern that carry tag,
// sorted by name, plus untagged matches (to explain empty results). Team VM
// names are skipped since clones inherit the tag; a custom naming.vm_name
// must not match master names.
func FindMasters(vms []proxmox.VM, n Naming, pattern, tag string) (masters, untagged []proxmox.VM, err error) {
	re, err := config.WildcardRegexp(pattern)
	if err != nil {
		return nil, nil, err
	}
	for _, vm := range vms {
		kind, _, _ := n.Classify(vm)
		if vm.Template || n.IsTemplateName(vm.Name) || kind != NotTeamNamed || !re.MatchString(vm.Name) {
			continue
		}
		if hasTag(vm.Tags, tag) {
			masters = append(masters, vm)
		} else {
			untagged = append(untagged, vm)
		}
	}
	sort.Slice(masters, func(i, j int) bool { return masters[i].Name < masters[j].Name })
	return masters, untagged, nil
}

// findForTeams is the team VMs of teams, filtered by hosts (see
// MatchesHost; all-blank filters match nothing), and the OutsideBlock VMs
// of the same teams and hosts (see Classify), each sorted by name.
func findForTeams(vms []proxmox.VM, n Naming, teams, hosts []string) (team, outside []proxmox.VM) {
	for _, vm := range vms {
		kind, t, host := n.Classify(vm)
		if !slices.Contains(teams, t) || !MatchesHost(host, hosts) {
			continue
		}
		switch kind {
		case TeamVMKind:
			team = append(team, vm)
		case OutsideBlock:
			outside = append(outside, vm)
		}
	}
	byName := func(a, b proxmox.VM) int { return strings.Compare(a.Name, b.Name) }
	slices.SortStableFunc(team, byName)
	slices.SortStableFunc(outside, byName)
	return team, outside
}

// VMKind is what a VM is to team naming (see Naming.Classify).
type VMKind int

const (
	NotTeamNamed VMKind = iota // not named as VMName names team VMs
	TeamTemplate               // team-named, but a template
	OutsideBlock               // team-named, outside that team's clone VMIDs
	TeamVMKind                 // a team VM
)

// Classify is what vm is, and the team and host its name gives if it is
// team-named. A team VM is named as VMName names them, in that team's clone
// VMID block (where deploys put them), and not a template. The name alone
// isn't enough: anyone can name a VM "team05-test" in their own pool, and
// teardown would delete it.
func (n Naming) Classify(vm proxmox.VM) (kind VMKind, team, host string) {
	team, host, ok := n.ParseVMName(vm.Name)
	switch {
	case !ok:
		return NotTeamNamed, "", ""
	case vm.Template || n.IsTemplateName(vm.Name):
		return TeamTemplate, team, host
	}
	if t, in := n.TeamOfCloneVMID(vm.VMID); !in || t != team {
		return OutsideBlock, team, host
	}
	return TeamVMKind, team, host
}

// TeamVM is the team and host of vm if it is a team VM (see Classify).
func (n Naming) TeamVM(vm proxmox.VM) (team, host string, ok bool) {
	kind, team, host := n.Classify(vm)
	if kind != TeamVMKind {
		return "", "", false
	}
	return team, host, true
}

// MatchesHost is every operation's host filter: no filters match all;
// otherwise a host matches if it contains a non-blank filter, ignoring case.
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

// AssignNodes gives each team the node with the fewest VMs, which then
// counts 10 more. Ties go to the alphabetically first node.
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

// MasterSet is the masters sharing the part after the host, e.g.
// dc.kilo.alpha and web.kilo.alpha form "kilo.alpha" (pattern "*.kilo.alpha").
type MasterSet struct {
	Name    string   // e.g. "kilo.alpha"
	Hosts   []string // sorted, each once
	Masters int      // master VMs, counting any that share a name
	Running int      // of them, those running; a template build stops them
}

// MasterSets groups masters tagged tag into sets, sorted by name. Masters
// with nothing after the host (like "y" or "tern.") go in others, sorted
// and unique.
func MasterSets(vms []proxmox.VM, n Naming, tag string) (sets []MasterSet, others []string) {
	masters, _, _ := FindMasters(vms, n, "*", tag)
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

// AllTeamVMs is every non-template team VM, as findForTeams finds them,
// sorted by name.
func AllTeamVMs(vms []proxmox.VM, n Naming) []proxmox.VM {
	var out []proxmox.VM
	for _, vm := range vms {
		if _, _, ok := n.TeamVM(vm); ok {
			out = append(out, vm)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TeamsWithVMs is the teams with non-template team VMs, sorted and unique.
// "All teams" means these, so acting on them leaves none of the caller's
// team VMs behind.
func TeamsWithVMs(vms []proxmox.VM, n Naming) []string {
	seen := map[string]bool{}
	for _, vm := range vms {
		if team, _, ok := n.TeamVM(vm); ok {
			seen[team] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
