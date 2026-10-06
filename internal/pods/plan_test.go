package pods

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// newCluster returns a fake with two tagged masters (teak: 2 NICs,
// cloud-init; oak: GPU) and one untagged VM.
func newCluster() *fakeAPI {
	f := newFakeAPI("cedar", "birch", "spruce")
	f.add(proxmox.VM{VMID: 121, Name: "teak.tango.delta", Node: "cedar", Tags: "dev;tango.delta"}, map[string]string{
		"net0":  "virtio=BC:24:11:00:01:21,bridge=vmbr0",
		"net1":  "virtio=BC:24:11:00:01:22,bridge=vmbr1",
		"ide2":  "competitions:vm-121-cloudinit,media=cdrom",
		"scsi0": "competitions:121/vm-121-disk-0.qcow2,size=32G",
	})
	f.add(proxmox.VM{VMID: 125, Name: "oak.tango.delta", Node: "birch", Tags: "dev"}, map[string]string{
		"net0": "virtio=BC:24:11:00:01:25,bridge=vmbr0",
		"vga":  "virtio-gl,memory=256",
	})
	f.add(proxmox.VM{VMID: 130, Name: "notes.tango.delta", Node: "cedar", Tags: "docs"}, nil)
	return f
}

func testPlanner(f *fakeAPI) Planner {
	cfg := config.Default()
	return NewPlanner(f, cfg)
}

func TestDeployPlanFromScratch(t *testing.T) {
	f := newCluster()
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{
		Pattern: "*.tango.delta", Teams: []string{"01", "02"}, Snapshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Templates) != 2 {
		t.Fatalf("templates = %+v", plan.Templates)
	}
	oak, teak := plan.Templates[0], plan.Templates[1]
	if teak.Name != "teak.tango.delta.tpl" || teak.VMID != 9021 || teak.Exists || teak.Interfaces != 2 || teak.GPU {
		t.Errorf("teak template = %+v", teak)
	}
	if !oak.GPU || oak.VMID != 9025 || oak.Node != "birch" {
		t.Errorf("oak template = %+v", oak)
	}

	names := []string{}
	for _, it := range plan.Items {
		names = append(names, it.Name)
	}
	if want := []string{"team01-oak", "team01-teak", "team02-oak", "team02-teak"}; !reflect.DeepEqual(names, want) {
		t.Errorf("items = %v, want %v", names, want)
	}

	t1 := plan.Items[1]
	if t1.VMID != 10121 || t1.Blocked != "" {
		t.Errorf("team01-teak = %+v", t1)
	}
	wantSteps := []Step{StepClone, StepNetwork, StepDiskLimits, StepCDROM, StepSnapshot, StepStart}
	if !reflect.DeepEqual(t1.Steps, wantSteps) {
		t.Errorf("steps = %v, want %v", t1.Steps, wantSteps)
	}
	// Teams are spread across nodes; GPU clones stay on the template's node.
	if plan.Items[1].Node == plan.Items[3].Node {
		t.Errorf("teams 01 and 02 both assigned to %s", plan.Items[1].Node)
	}
	if plan.Items[0].Node != "birch" || plan.Items[2].Node != "birch" {
		t.Errorf("GPU clones not pinned: %s, %s", plan.Items[0].Node, plan.Items[2].Node)
	}
}

func TestDeployPlanReusesTemplateAndConvergesExistingVM(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "spruce"}, nil)

	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{
		Pattern: "teak.*", Teams: []string{"01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Templates[0].Exists {
		t.Errorf("template not reused: %+v", plan.Templates[0])
	}
	it := plan.Items[0]
	if it.Node != "spruce" || it.Steps[0] == StepClone {
		t.Errorf("existing VM should converge without cloning: %+v", it)
	}
}

func TestDeployPlanBlocksVMIDConflict(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "someone-elses-vm", Node: "cedar"}, nil)

	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Items[0].Blocked; got != "VMID 10121 is used by someone-elses-vm" {
		t.Errorf("Blocked = %q", got)
	}
	if len(plan.Runnable()) != 0 {
		t.Errorf("Runnable = %v, want none", plan.Runnable())
	}
}

func TestDeployPlanRebuildBlockedByClones(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.add(proxmox.VM{VMID: 10521, Name: "team05-teak", Node: "cedar"}, nil)

	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Templates[0].Blocked, "team05-teak") {
		t.Errorf("template Blocked = %q", plan.Templates[0].Blocked)
	}
	if !strings.HasPrefix(plan.Items[0].Blocked, "template teak.tango.delta.tpl:") {
		t.Errorf("item Blocked = %q", plan.Items[0].Blocked)
	}
}

func TestDeployPlanExplainsUntaggedMasters(t *testing.T) {
	_, err := testPlanner(newCluster()).Deploy(context.Background(), DeployRequest{Pattern: "notes.*", Teams: []string{"01"}})
	if err == nil || !strings.Contains(err.Error(), "lack the tag") {
		t.Errorf("err = %v", err)
	}
}

func TestTeardownAndResetPlans(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial")
	f.add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil)
	f.add(proxmox.VM{VMID: 10221, Name: "team02-teak", Node: "cedar"}, nil, "initial")
	p := testPlanner(f)

	td, err := p.Teardown(context.Background(), []string{"01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(td.Items) != 2 || td.Items[0].Name != "team01-oak" || !reflect.DeepEqual(td.Items[0].Steps, []Step{StepStop, StepDelete}) {
		t.Errorf("teardown = %+v", td.Items)
	}

	rs, err := p.Reset(context.Background(), []string{"01"}, nil, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rs.Items[0].Blocked, `no snapshot "initial"`) || rs.Items[1].Blocked != "" {
		t.Errorf("reset = %+v", rs.Items)
	}

	pw, err := p.Power(context.Background(), []string{"01", "02"}, []string{"TEAK"}, "reboot")
	if err != nil {
		t.Fatal(err)
	}
	if len(pw.Items) != 2 || pw.Items[1].Action != "reboot" {
		t.Errorf("power = %+v", pw.Items)
	}
}

func TestDeployPlanBlocksTemplateOutsideVMIDRange(t *testing.T) {
	f := newCluster()
	// A hand-made template at 9121 would give teak the same clone VMIDs as a
	// template at 9021.
	f.add(proxmox.VM{VMID: 9121, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)

	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Templates[0].Blocked, "outside 9000-9099") {
		t.Errorf("template Blocked = %q", plan.Templates[0].Blocked)
	}
	if !strings.HasPrefix(plan.Items[0].Blocked, "template ") {
		t.Errorf("item Blocked = %q", plan.Items[0].Blocked)
	}
}

func TestDeployPlanBlocksTemplateVMIDBumpedPastRange(t *testing.T) {
	f := newFakeAPI("cedar")
	f.add(proxmox.VM{VMID: 199, Name: "edge.x", Node: "cedar", Tags: "dev"}, map[string]string{"net0": "virtio=BC:24:11:00:01:99,bridge=vmbr0"})
	f.add(proxmox.VM{VMID: 9099, Name: "unrelated", Node: "cedar"}, nil)

	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "edge.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Templates[0].VMID != 9100 || !strings.Contains(plan.Templates[0].Blocked, "outside") {
		t.Errorf("template = %+v", plan.Templates[0])
	}
}

func TestDeployPlanRebuildDeletesOldTemplateOnItsNode(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "spruce", Template: true}, nil)

	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	tpl := plan.Templates[0]
	if tpl.OldNode != "spruce" || tpl.Node != "cedar" || !tpl.Rebuild || tpl.VMID != 9021 || tpl.Blocked != "" {
		t.Errorf("template = %+v", tpl)
	}
}

func TestDeployPlanReusedTemplateDescribesItself(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "spruce", Template: true}, map[string]string{
		"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0",
	})
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Templates[0].Interfaces; got != 1 {
		t.Errorf("Interfaces = %d, want 1", got)
	}
}

func TestDeployPlanGPUDetectionIsExact(t *testing.T) {
	for vga, want := range map[string]bool{"type=virtio-gl,memory=256": true, "virtio-gl": true, "virtio-gl2": false, "std": false} {
		f := newFakeAPI("cedar")
		f.add(proxmox.VM{VMID: 121, Name: "teak.x", Node: "cedar", Tags: "dev"}, map[string]string{"vga": vga})
		plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := plan.Templates[0].GPU; got != want {
			t.Errorf("vga %q: GPU = %v, want %v", vga, got, want)
		}
	}
}

func TestDeployPlanBlocksNonTemplateTpl(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	tpl := plan.Templates[0]
	if !strings.Contains(tpl.Blocked, "exists but is not a template") || tpl.VMID != 9021 || tpl.Node != "cedar" {
		t.Errorf("template = %+v", tpl)
	}
	if plan.Items[0].Blocked == "" || len(plan.Runnable()) != 0 {
		t.Errorf("items = %+v", plan.Items)
	}
}

func TestDeployPlanBlocksDuplicateNames(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9099, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "several VMs are named teak.tango.delta.tpl (VMIDs 9021, 9099); rename or delete the extras"
	if got := plan.Templates[0].Blocked; got != want {
		t.Errorf("template Blocked = %q", got)
	}

	f = newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.add(proxmox.VM{VMID: 10221, Name: "team01-teak", Node: "cedar"}, nil)
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
	plan, err = testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Items[0].Blocked; got != "several VMs are named team01-teak (VMIDs 10121, 10221)" {
		t.Errorf("item Blocked = %q", got)
	}
}

func TestDeployPlanBlocksMismatchedExistingVM(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.add(proxmox.VM{VMID: 10105, Name: "team01-teak", Node: "spruce"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	it := plan.Items[0]
	want := "exists as VMID 10105, expected 10121 (built from a different template?); tear it down first"
	if it.Blocked != want || it.VMID != 10105 || it.Node != "spruce" {
		t.Errorf("item = %+v", it)
	}
}

func TestResetSurvivesSnapshotErrors(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial")
	f.add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil)
	f.add(proxmox.VM{VMID: 10225, Name: "team02-oak", Node: "birch"}, nil, "initial")
	f.failOn("snapshots:10225", errors.New("boom"))
	rs, err := testPlanner(f).Reset(context.Background(), []string{"01", "02"}, nil, "initial")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Item{}
	for _, it := range rs.Items {
		byName[it.Name] = it
	}
	if !strings.HasPrefix(byName["team02-oak"].Blocked, "cannot list snapshots: ") {
		t.Errorf("team02-oak = %+v", byName["team02-oak"])
	}
	if byName["team01-teak"].Blocked != "" {
		t.Errorf("team01-teak = %+v", byName["team01-teak"])
	}
	if got := byName["team01-oak"].Blocked; got != `no snapshot "initial" (has none)` {
		t.Errorf("team01-oak Blocked = %q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.failOn("snapshots:10121", context.Canceled)
	if _, err := testPlanner(f).Reset(ctx, []string{"01"}, nil, "initial"); err == nil {
		t.Error("cancelled context should return an error")
	}
}

func TestNormalizeTeams(t *testing.T) {
	f := newCluster()
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"2", "01", "01"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Teams, []string{"01", "02"}) || len(plan.Items) != 2 {
		t.Errorf("teams = %v, items = %d", plan.Teams, len(plan.Items))
	}
	for _, bad := range [][]string{{"abc"}, {"100"}, {""}, {"-1"}, {"1 "}} {
		if _, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: bad}); err == nil {
			t.Errorf("Deploy(%q) should fail", bad)
		}
		if _, err := testPlanner(f).Teardown(context.Background(), bad, nil); err == nil {
			t.Errorf("Teardown(%q) should fail", bad)
		}
	}
	if _, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*"}); err == nil {
		t.Error("Deploy with no teams should fail")
	}
}

func TestDeployNoMastersWithHostFilter(t *testing.T) {
	_, err := testPlanner(newCluster()).Deploy(context.Background(), DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01"}, Hosts: []string{"tahoo"}})
	if err == nil || !strings.Contains(err.Error(), "hosts tahoo") {
		t.Errorf("err = %v", err)
	}
}

func TestDeployPlanBlockedTemplateReasonWins(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 9050, Name: "teak.tango.delta.tpl", Node: "spruce", Template: true}, nil)
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.add(proxmox.VM{VMID: 10150, Name: "team01-teak", Node: "cedar"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01", "02"}})
	if err != nil {
		t.Fatal(err)
	}
	if tpl := plan.Templates[0]; tpl.VMID != 9021 || tpl.Node != "cedar" {
		t.Errorf("template = %+v, want lowest VMID shown", tpl)
	}
	for _, it := range plan.Items {
		if !strings.HasPrefix(it.Blocked, "template ") || strings.Contains(it.Blocked, "tear it down") {
			t.Errorf("item = %+v", it)
		}
	}
	if it := plan.Items[0]; it.VMID != 10150 || it.Node != "cedar" {
		t.Errorf("existing VM not shown: %+v", it)
	}
}

func TestSimplePlansRejectEmptyTeams(t *testing.T) {
	p := testPlanner(newCluster())
	ctx := context.Background()
	if _, err := p.Teardown(ctx, nil, nil); err == nil {
		t.Error("Teardown")
	}
	if _, err := p.Reset(ctx, []string{}, nil, "initial"); err == nil {
		t.Error("Reset")
	}
	if _, err := p.Power(ctx, nil, nil, "start"); err == nil {
		t.Error("Power")
	}
}

func TestFakeRollbackAndCloudInitCheckNode(t *testing.T) {
	f := newCluster()
	if _, err := f.Rollback(context.Background(), "spruce", 121, "x"); err == nil {
		t.Error("Rollback on wrong node should fail")
	}
	if err := f.RegenerateCloudInit(context.Background(), "spruce", 121); err == nil {
		t.Error("RegenerateCloudInit on wrong node should fail")
	}
}

func TestDuplicateHostAcrossMastersBlocksBothTemplates(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 131, Name: "teak.kilo.delta", Node: "cedar", Tags: "dev"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Templates) != 2 {
		t.Fatalf("templates = %+v", plan.Templates)
	}
	for _, tpl := range plan.Templates {
		if !strings.Contains(tpl.Blocked, "both map to team VM names like team01-teak") ||
			!strings.Contains(tpl.Blocked, "teak.tango.delta") || !strings.Contains(tpl.Blocked, "teak.kilo.delta") {
			t.Errorf("%s Blocked = %q", tpl.Name, tpl.Blocked)
		}
	}
	for _, it := range plan.Items {
		if it.Blocked == "" {
			t.Errorf("item %s not blocked", it.Name)
		}
	}
}

func TestDeployBlocksExistingTemplateNamedAsTeamVM(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Template: true}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Items[0].Blocked; !strings.Contains(got, "is a template, not a team VM") {
		t.Errorf("Blocked = %q", got)
	}
}

func TestWillStopMasterOnlyWhenBuildingFromRunningMaster(t *testing.T) {
	f := newCluster()
	f.vms[121].Status = "running"
	p := testPlanner(f)
	plan, _ := p.Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if !plan.Templates[0].WillStopMaster {
		t.Error("new template from a running master: WillStopMaster = false")
	}
	f.vms[121].Status = "stopped"
	plan, _ = p.Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if plan.Templates[0].WillStopMaster {
		t.Error("stopped master: WillStopMaster = true")
	}
	f.vms[121].Status = "running"
	f.add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	plan, _ = p.Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if plan.Templates[0].WillStopMaster {
		t.Error("reused template: WillStopMaster = true")
	}
	plan, _ = p.Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if !plan.Templates[0].WillStopMaster {
		t.Error("rebuild from a running master: WillStopMaster = false")
	}
}
