package pods

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// teamVMModel is whether vm is one of team's VMs, from the rule: named
// team<NN>-<host>, not a template, and in team's own clone VMID block.
func teamVMModel(cfg config.Naming, vm proxmox.VM, team string) bool {
	host, ok := strings.CutPrefix(vm.Name, "team"+team+"-")
	return ok && host != "" && !vm.Template && !strings.HasSuffix(vm.Name, cfg.TemplateSuffix) && inTeamBlock(cfg, team, vm.VMID)
}

// Teardown and power act on exactly the asked teams' VMs, and never on a VM
// that only has a team's name: one outside the team's VMID block (say,
// someone's "team05-test" in a dev pool) isn't in the plan at all, and one
// in a known pool other than the team's is blocked.
func TestPropTeamVMsAreNamedAndNumbered(t *testing.T) {
	cfg := config.Default()
	rapid.Check(t, func(t *rapid.T) {
		f := podstest.New("n1")
		teams := []string{"04", "05", "06"}
		used := map[int]bool{}
		var vms []proxmox.VM
		for i := range rapid.IntRange(0, 12).Draw(t, "vm count") {
			label := fmt.Sprintf("vm %d", i)
			team := rapid.SampledFrom(teams).Draw(t, label+" team")
			vmid := rapid.SampledFrom(blockVMIDs(cfg.Naming, rapid.SampledFrom(teams).Draw(t, label+" block of"))).Draw(t, label+" vmid")
			if used[vmid] {
				continue
			}
			used[vmid] = true
			name := rapid.SampledFrom([]string{"team" + team + "-dc", "team" + team + "-test", "team" + team + "-dc" + cfg.Naming.TemplateSuffix, "team" + team[1:] + "-dc"}).Draw(t, label+" name")
			vm := proxmox.VM{VMID: vmid, Name: name, Node: "n1", Status: "running",
				Pool:     rapid.SampledFrom([]string{"", "pool-" + team, "pool-99", "dev-05"}).Draw(t, label+" pool"),
				Template: rapid.IntRange(0, 5).Draw(t, label+" template") == 0}
			f.Add(vm, nil)
			vms = append(vms, vm)
		}
		asked := rapid.SliceOfNDistinct(rapid.SampledFrom(teams), 1, 3, rapid.ID).Draw(t, "asked")
		p := NewPlanner(f, cfg)
		td, err := p.Teardown(context.Background(), asked, nil)
		if err != nil {
			t.Fatal(err)
		}
		pw, err := p.Power(context.Background(), asked, nil, "stop")
		if err != nil {
			t.Fatal(err)
		}
		for _, plan := range []*Plan{td, pw} {
			got := map[int]Item{}
			for _, it := range plan.Items {
				got[it.VMID] = it
			}
			// The model: the asked teams' VMs are acted on unless in a known
			// other pool; VMs with their names but not team VMs (outside the
			// block) are listed blocked, unless a team VM has the name.
			isTeam := map[int]bool{}
			teamNames := map[string]bool{}
			for _, vm := range vms {
				for _, team := range asked {
					if teamVMModel(cfg.Naming, vm, team) {
						isTeam[vm.VMID], teamNames[vm.Name] = true, true
					}
				}
			}
			for _, vm := range vms {
				it, in := got[vm.VMID]
				named := false
				for _, team := range asked {
					host, ok := strings.CutPrefix(vm.Name, "team"+team+"-")
					named = named || ok && host != "" && !vm.Template && !strings.HasSuffix(vm.Name, cfg.Naming.TemplateSuffix)
				}
				stray := named && !isTeam[vm.VMID] && !teamNames[vm.Name]
				// Items are known by name: of strays sharing one, one is listed.
				listed := slices.ContainsFunc(plan.Items, func(it Item) bool { return it.Name == vm.Name && it.VMID != vm.VMID })
				if want := isTeam[vm.VMID] || stray && !listed; in != want && !(stray && listed) {
					t.Fatalf("%s plan has %s (VMID %d, pool %q, template %v): %v, want %v", plan.Kind, vm.Name, vm.VMID, vm.Pool, vm.Template, in, want)
				}
				if !in {
					continue
				}
				team := vm.Name[4:6]
				wantBlocked := stray || vm.Pool != "" && vm.Pool != "pool-"+team
				if (it.Blocked != "") != wantBlocked {
					t.Fatalf("%s of %s (VMID %d) in pool %q: blocked %q, want blocked %v", plan.Kind, vm.Name, vm.VMID, vm.Pool, it.Blocked, wantBlocked)
				}
				if stray && len(it.Steps) != 0 {
					t.Fatalf("%s would act on %s outside the team's block: %v", plan.Kind, vm.Name, it.Steps)
				}
			}
			if len(got) != len(plan.Items) {
				t.Fatalf("%s plan has a VM twice: %+v", plan.Kind, plan.Items)
			}
		}
	})
}

// A deploy whose VM name is taken by a VM outside the team's block is
// blocked, and says to deal with it in Proxmox: teardown lists it, blocked.
func TestDeployBlocksNameTakenOutsideTeamBlock(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 8005, Name: "team01-teak", Node: "cedar", Pool: "dev-05"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "has a team 01 name but is VMID 8005, outside the team's VMIDs, so it isn't a team VM; rename or remove it in Proxmox"; len(plan.Items) != 1 || plan.Items[0].Blocked != want {
		t.Fatalf("items = %+v", plan.Items)
	}
	td, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(td.Items) != 1 || td.Items[0].Blocked == "" || len(td.Items[0].Steps) != 0 {
		t.Fatalf("teardown of team 01 = %+v, want team01-teak listed, blocked", td.Items)
	}
}

// A deploy leaves alone a team VM in a known pool other than the team's,
// as teardown and power do.
func TestDeployBlocksTeamVMInAnotherPool(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Pool: "dev-05"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "is in pool dev-05, not pool-01; move it back to the team pool in Proxmox first"; len(plan.Items) != 1 || plan.Items[0].Blocked != want {
		t.Fatalf("items = %+v", plan.Items)
	}
}

// Rebuilding a template is held back only by team VMs, not by any VM with
// a team name.
func TestRebuildIgnoresTeamNamesOutsideTheBlock(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 8005, Name: "team05-teak", Node: "cedar", Pool: "dev-05"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Templates[0].Blocked != "" {
		t.Errorf("rebuild blocked by a VM outside every team's block: %q", plan.Templates[0].Blocked)
	}
	f.Add(proxmox.VM{VMID: 10521, Name: "team05-teak", Node: "cedar", Pool: "pool-05"}, nil)
	plan, err = testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Templates[0].Blocked, "team05-teak") {
		t.Errorf("rebuild not blocked by team 05's VM: %q", plan.Templates[0].Blocked)
	}
}
