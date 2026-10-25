package pods

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

func TestFindMastersRequiresExactTag(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	vms := []proxmox.VM{
		{VMID: 1, Name: "a.x", Tags: "dev;x"},
		{VMID: 2, Name: "b.x", Tags: "devops"}, // Python's substring check matched this
		{VMID: 3, Name: "c.x.tpl", Tags: "dev"},
		{VMID: 4, Name: "d.x", Tags: "dev", Template: true},
	}
	masters, untagged, _ := FindMasters(vms, n, "*.x", "dev")
	if len(masters) != 1 || masters[0].Name != "a.x" {
		t.Errorf("masters = %+v", masters)
	}
	if len(untagged) != 1 || untagged[0].Name != "b.x" {
		t.Errorf("untagged = %+v", untagged)
	}
}

func TestFindForTeamsFiltersByTeamAndHost(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	vms := []proxmox.VM{
		{VMID: 10121, Name: "team01-teak"},
		{VMID: 10125, Name: "team01-oak"},
		{VMID: 10221, Name: "team02-teak"},
		{VMID: 500, Name: "pool-01-vpn"},
		{VMID: 10199, Name: "team01-teak.tpl"},
	}
	got := findTeamVMs(vms, n, []string{"01"}, []string{"TEA"})
	if len(got) != 1 || got[0].Name != "team01-teak" {
		t.Errorf("findForTeams = %+v", got)
	}
}

func TestFindMastersSkipsTeamClones(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	vms := []proxmox.VM{
		{VMID: 1, Name: "teak.x", Tags: "dev"},
		{VMID: 10121, Name: "team01-teak", Tags: "dev"},
	}
	masters, untagged, _ := FindMasters(vms, n, "*", "dev")
	if len(masters) != 1 || masters[0].Name != "teak.x" {
		t.Errorf("masters = %+v, want only teak.x", masters)
	}
	if len(untagged) != 0 {
		t.Errorf("untagged = %+v, want empty", untagged)
	}
}

func TestFindMastersTagEdgeCases(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	tests := []struct {
		name string
		tags string
		want bool
	}{
		{"comma-separated with dev", "x,dev", true},
		{"uppercase tag", "DEV", true},
		{"padded tag", " dev ", true},
		{"empty tag", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vms := []proxmox.VM{{VMID: 1, Name: "a.x", Tags: tt.tags}}
			masters, _, _ := FindMasters(vms, n, "*.x", "dev")
			if got := len(masters) > 0; got != tt.want {
				t.Errorf("FindMasters found tag %q: %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}

func TestFindMastersEmptySearchTag(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	vms := []proxmox.VM{
		{VMID: 1, Name: "a.x", Tags: "dev"},
		{VMID: 2, Name: "b.x", Tags: "other"},
	}
	masters, untagged, _ := FindMasters(vms, n, "*.x", "")
	if len(masters) != 0 {
		t.Errorf("FindMasters with empty search tag: masters = %+v, want empty", masters)
	}
	if len(untagged) != 2 {
		t.Errorf("FindMasters with empty search tag: untagged = %+v, want 2 VMs", untagged)
	}
}

func TestMatchesHostBlankFilters(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	vms := []proxmox.VM{
		{VMID: 10121, Name: "team01-teak"},
		{VMID: 10125, Name: "team01-oak"},
	}

	// Empty filter in list matches nothing
	got := findTeamVMs(vms, n, []string{"01"}, []string{""})
	if len(got) != 0 {
		t.Errorf("findForTeams with blank filter: %+v, want empty", got)
	}

	// Mix of blank and valid filters keeps only valid matches
	got = findTeamVMs(vms, n, []string{"01"}, []string{"teak", " "})
	if len(got) != 1 || got[0].Name != "team01-teak" {
		t.Errorf("findForTeams with blank+valid filters: %+v, want team01-teak", got)
	}
}

func TestAssignNodes(t *testing.T) {
	tests := []struct {
		name       string
		teams      []string
		nodes      []string
		vms        []proxmox.VM
		podVMs     []proxmox.VM
		wantAssign map[string]string // team -> node
	}{
		{
			name:  "least loaded wins",
			teams: []string{"01", "02"},
			nodes: []string{"a", "b"},
			vms: []proxmox.VM{
				{Node: "a", VMID: 1},
				{Node: "a", VMID: 2},
				{Node: "a", VMID: 3},
				{Node: "a", VMID: 4},
				{Node: "a", VMID: 5},
				{Node: "b", VMID: 6},
				{Node: "b", VMID: 7},
			},
			wantAssign: map[string]string{"01": "b", "02": "a"},
		},
		{
			name:  "balances by requested RAM and CPU of running VMs, ignoring stopped and templates",
			teams: []string{"01", "02", "03"},
			nodes: []string{"a", "b"},
			vms: []proxmox.VM{
				// Node a has 1 running VM with 16 GiB RAM + 8 vCPUs (load 24 GiB),
				// and many stopped VMs / templates that don't count toward running load.
				{Node: "a", VMID: 1, Status: "running", MaxMem: 16 << 30, MaxCPU: 8},
				{Node: "a", VMID: 2, Status: "stopped", MaxMem: 64 << 30, MaxCPU: 16},
				{Node: "a", VMID: 3, Status: "stopped", Template: true, MaxMem: 64 << 30, MaxCPU: 16},
				// Node b has 4 running VMs with 2 GiB RAM + 1 vCPU each (load 12 GiB).
				{Node: "b", VMID: 4, Status: "running", MaxMem: 2 << 30, MaxCPU: 1},
				{Node: "b", VMID: 5, Status: "running", MaxMem: 2 << 30, MaxCPU: 1},
				{Node: "b", VMID: 6, Status: "running", MaxMem: 2 << 30, MaxCPU: 1},
				{Node: "b", VMID: 7, Status: "running", MaxMem: 2 << 30, MaxCPU: 1},
			},
			// Each pod adds 8 GiB RAM + 2 vCPUs (load 10 GiB):
			// team 01 -> b (12 -> 22), team 02 -> b (22 -> 32), team 03 -> a (24 -> 34).
			podVMs:     []proxmox.VM{{MaxMem: 8 << 30, MaxCPU: 2}},
			wantAssign: map[string]string{"01": "b", "02": "b", "03": "a"},
		},
		{
			name:  "weighting: +10 per assigned team",
			teams: []string{"01", "02"},
			nodes: []string{"a", "b"},
			vms: []proxmox.VM{
				{Node: "b", VMID: 1},
				{Node: "b", VMID: 2},
				{Node: "b", VMID: 3},
				{Node: "b", VMID: 4},
				{Node: "b", VMID: 5},
				{Node: "b", VMID: 6},
				{Node: "b", VMID: 7},
				{Node: "b", VMID: 8},
				{Node: "b", VMID: 9},
				{Node: "b", VMID: 10},
				{Node: "b", VMID: 11},
				{Node: "b", VMID: 12},
				{Node: "b", VMID: 13},
				{Node: "b", VMID: 14},
				{Node: "b", VMID: 15},
			},
			wantAssign: map[string]string{"01": "a", "02": "a"},
		},
		{
			name:       "ties go alphabetically",
			teams:      []string{"01", "02"},
			nodes:      []string{"zebra", "apple"},
			vms:        []proxmox.VM{},
			wantAssign: map[string]string{"01": "apple", "02": "zebra"},
		},
		{
			name:  "unknown nodes ignored",
			teams: []string{"01"},
			nodes: []string{"a", "b"},
			vms: []proxmox.VM{
				{Node: "c", VMID: 1},
				{Node: "c", VMID: 2},
				{Node: "c", VMID: 3},
				{Node: "c", VMID: 4},
				{Node: "c", VMID: 5},
				{Node: "c", VMID: 6},
				{Node: "c", VMID: 7},
				{Node: "c", VMID: 8},
				{Node: "c", VMID: 9},
				{Node: "c", VMID: 10},
				{Node: "c", VMID: 11},
				{Node: "c", VMID: 12},
				{Node: "c", VMID: 13},
				{Node: "c", VMID: 14},
				{Node: "c", VMID: 15},
				{Node: "c", VMID: 16},
				{Node: "c", VMID: 17},
				{Node: "c", VMID: 18},
				{Node: "c", VMID: 19},
				{Node: "c", VMID: 20},
				{Node: "c", VMID: 21},
				{Node: "c", VMID: 22},
				{Node: "c", VMID: 23},
				{Node: "c", VMID: 24},
				{Node: "c", VMID: 25},
				{Node: "c", VMID: 26},
				{Node: "c", VMID: 27},
				{Node: "c", VMID: 28},
				{Node: "c", VMID: 29},
				{Node: "c", VMID: 30},
				{Node: "c", VMID: 31},
				{Node: "c", VMID: 32},
				{Node: "c", VMID: 33},
				{Node: "c", VMID: 34},
				{Node: "c", VMID: 35},
				{Node: "c", VMID: 36},
				{Node: "c", VMID: 37},
				{Node: "c", VMID: 38},
				{Node: "c", VMID: 39},
				{Node: "c", VMID: 40},
				{Node: "c", VMID: 41},
				{Node: "c", VMID: 42},
				{Node: "c", VMID: 43},
				{Node: "c", VMID: 44},
				{Node: "c", VMID: 45},
				{Node: "c", VMID: 46},
				{Node: "c", VMID: 47},
				{Node: "c", VMID: 48},
				{Node: "c", VMID: 49},
				{Node: "c", VMID: 50},
				{Node: "b", VMID: 51},
			},
			wantAssign: map[string]string{"01": "a"},
		},
		{
			name:       "empty nodes returns empty map",
			teams:      []string{"01"},
			nodes:      []string{},
			vms:        []proxmox.VM{},
			wantAssign: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := AssignNodes(tt.teams, tt.nodes, tt.vms, tt.podVMs...)
			if !reflect.DeepEqual(result, tt.wantAssign) {
				t.Errorf("AssignNodes: got %v, want %v", result, tt.wantAssign)
			}
		})
	}
}

// Tagged masters group into sets by the part after the host; others are
// listed apart. Untagged VMs, templates and team VMs are never masters.
func TestMasterSets(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	vms := []proxmox.VM{
		{VMID: 1, Name: "web.kilo.alpha", Tags: "dev", Status: "stopped"},
		{VMID: 2, Name: "dc.kilo.alpha", Tags: "dev", Status: "running"},
		{VMID: 3, Name: "dc.kilo.alpha", Tags: "dev", Status: "running"}, // a duplicate still counts as a master
		{VMID: 4, Name: "bugs.looney.tunes", Tags: "dev;x", Status: "stopped"},
		{VMID: 5, Name: "y", Tags: "dev", Status: "running"},
		{VMID: 6, Name: "blank", Tags: "dev", Status: "stopped"},
		{VMID: 7, Name: "trailing.", Tags: "dev", Status: "stopped"},
		{VMID: 8, Name: "mail.kilo.alpha", Tags: "devops", Status: "running"}, // not the master tag
		{VMID: 9, Name: "dc.kilo.alpha.tpl", Tags: "dev", Template: true},
		{VMID: 10, Name: "team01-dc", Tags: "dev", Status: "running"},
	}
	sets, others := MasterSets(vms, n, "dev")
	want := []MasterSet{
		{Name: "kilo.alpha", Hosts: []string{"dc", "web"}, Masters: 3, Running: 2},
		{Name: "looney.tunes", Hosts: []string{"bugs"}, Masters: 1, Running: 0},
	}
	if !reflect.DeepEqual(sets, want) {
		t.Errorf("sets = %+v\nwant   %+v", sets, want)
	}
	if want := []string{"blank", "trailing.", "y"}; !reflect.DeepEqual(others, want) {
		t.Errorf("others = %v, want %v", others, want)
	}
	if sets, others := MasterSets(nil, n, "dev"); sets != nil || others != nil {
		t.Errorf("no VMs: sets %v, others %v; want none", sets, others)
	}
}

// Every team with a team VM counts, including 00 and teams past it.
// Templates, masters, unpadded and hand-made names don't make a team.
func TestTeamsWithVMs(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	vms := []proxmox.VM{
		{VMID: 10101, Name: "team01-dc"},
		{VMID: 10102, Name: "team01-web"},
		{VMID: 10001, Name: "team00-dc"},
		{VMID: 14001, Name: "team40-dc", Status: "stopped"},
		{VMID: 10701, Name: "team07-dc", Template: true},
		{VMID: 9005, Name: "team08-dc.tpl"},
		{VMID: 105, Name: "dc.kilo.alpha", Tags: "dev"},
		{VMID: 10501, Name: "team5-dc"},
		{VMID: 10601, Name: "team6x-dc"},
	}
	if got, want := TeamsWithVMs(vms, n), []string{"00", "01", "40"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TeamsWithVMs = %v, want %v", got, want)
	}
	if got := TeamsWithVMs(nil, n); len(got) != 0 {
		t.Errorf("TeamsWithVMs(no VMs) = %v, want none", got)
	}
	// AllTeamVMs is what findForTeams finds for every team.
	if got, want := vmsOf(n, AllTeamVMs(vms, n)), findTeamVMs(vms, n, TeamsWithVMs(vms, n), nil); !reflect.DeepEqual(got, want) || len(got) != 4 {
		t.Errorf("AllTeamVMs = %v, want %v", got, want)
	}
	// Every team VM's team is in TeamsWithVMs, so "all" leaves none behind.
	all := TeamsWithVMs(vms, n)
	for _, vm := range vms {
		if team, _, ok := n.ParseVMName(vm.Name); ok && !vm.Template && !n.IsTemplateName(vm.Name) && len(findTeamVMs([]proxmox.VM{vm}, n, all, nil)) != 1 {
			t.Errorf("%s (team %s) is not covered by TeamsWithVMs %v", vm.Name, team, all)
		}
	}
}

// FindMasters passes on the refusal of a pattern that isn't valid UTF-8.
func TestFindMastersRefusesInvalidPattern(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	vms := []proxmox.VM{{VMID: 1, Name: "dc.kilo", Tags: "dev"}}
	for _, pattern := range []string{"\xff", "dc.\xc3", "*\xff*"} {
		if _, _, err := FindMasters(vms, n, pattern, "dev"); err == nil {
			t.Errorf("FindMasters(%q) accepted invalid UTF-8", pattern)
		}
	}
}

func findTeamVMs(vms []proxmox.VM, n Naming, teams, hosts []string) []proxmox.VM {
	team, _ := findForTeams(vms, n, teams, hosts)
	return vmsOf(n, team)
}

// vmsOf is the VMs of named VMs, checking each carries its name's team and
// host.
func vmsOf(n Naming, named []NamedVM) []proxmox.VM {
	var out []proxmox.VM
	for _, v := range named {
		if team, host, _ := n.ParseVMName(v.Name); team != v.Team || host != v.Host {
			panic(fmt.Sprintf("%s carries team %q host %q, its name gives %q %q", v.Name, v.Team, v.Host, team, host))
		}
		out = append(out, v.VM)
	}
	return out
}
