package pods

import (
	"context"
	"fmt"
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
			for _, vm := range vms {
				it, in := got[vm.VMID]
				var want bool
				for _, team := range asked {
					want = want || teamVMModel(cfg.Naming, vm, team)
				}
				if in != want {
					t.Fatalf("%s plan has %s (VMID %d, pool %q, template %v): %v, want %v", plan.Kind, vm.Name, vm.VMID, vm.Pool, vm.Template, in, want)
				}
				if !in {
					continue
				}
				team := vm.Name[4:6]
				if wrongPool := vm.Pool != "" && vm.Pool != "pool-"+team; (it.Blocked != "") != wrongPool {
					t.Fatalf("%s of %s in pool %q: blocked %q", plan.Kind, vm.Name, vm.Pool, it.Blocked)
				}
			}
			if len(got) != len(plan.Items) {
				t.Fatalf("%s plan has a VM twice: %+v", plan.Kind, plan.Items)
			}
		}
	})
}

// A deploy whose VM name is taken by a VM outside the team's block is
// blocked, and says to deal with it in Proxmox: teardown won't touch it.
func TestDeployBlocksNameTakenOutsideTeamBlock(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 8005, Name: "team01-teak", Node: "cedar", Pool: "dev-05"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "the name is taken by VMID 8005, which is outside team 01's VMIDs, so it isn't a team VM; rename or remove it in Proxmox"; len(plan.Items) != 1 || plan.Items[0].Blocked != want {
		t.Fatalf("items = %+v", plan.Items)
	}
	td, err := testPlanner(f).Teardown(context.Background(), []string{"01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(td.Items) != 0 {
		t.Fatalf("teardown of team 01 = %+v, want nothing", td.Items)
	}
}
