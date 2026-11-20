package pods

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// newCluster returns a fake with two tagged masters (teak: 2 NICs,
// cloud-init; oak: GPU) and one untagged VM.
func newCluster() *podstest.Fake {
	f := podstest.New("cedar", "birch", "spruce")
	f.Add(proxmox.VM{VMID: 121, Name: "teak.tango.delta", Node: "cedar", Tags: "dev;tango.delta"}, map[string]string{
		"net0":  "virtio=BC:24:11:00:01:21,bridge=vmbr0",
		"net1":  "virtio=BC:24:11:00:01:22,bridge=vmbr1",
		"ide2":  "competitions:vm-121-cloudinit,media=cdrom",
		"scsi0": "competitions:121/vm-121-disk-0.qcow2,size=32G",
	})
	f.Add(proxmox.VM{VMID: 125, Name: "oak.tango.delta", Node: "birch", Tags: "dev"}, map[string]string{
		"net0": "virtio=BC:24:11:00:01:25,bridge=vmbr0",
		"vga":  "virtio-gl,memory=256",
	})
	f.Add(proxmox.VM{VMID: 130, Name: "notes.tango.delta", Node: "cedar", Tags: "docs"}, nil)
	return f
}

func testPlanner(f *podstest.Fake) Planner {
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
	wantSteps := []Step{StepClone, StepNetwork, StepDiskLimits, StepCDROM, StepNotes, StepSetTag, StepSnapshot, StepStart}
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
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "spruce"}, nil)
	f.Add(proxmox.VM{VMID: 10110, Name: "team01-elmer", Node: "spruce"}, nil)

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
	// Converging keeps the VM: no stop, delete or clone.
	if want := []Step{StepNetwork, StepDiskLimits, StepCDROM, StepNotes, StepSetTag, StepStart}; it.Node != "spruce" || !reflect.DeepEqual(it.Steps, want) {
		t.Errorf("existing VM should converge without cloning: %+v", it)
	}

	delete(f.VMs, 10121)
	plan, err = testPlanner(f).Deploy(context.Background(), DeployRequest{
		Pattern: "teak.*", Teams: []string{"01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if it := plan.Items[0]; it.Node != "spruce" || it.Steps[0] != StepClone {
		t.Errorf("redeployed host should join its pod on spruce: %+v", it)
	}
}

func TestDeployPlanBlocksVMIDConflict(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "someone-elses-vm", Node: "cedar"}, nil)

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
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 10521, Name: "team05-teak", Node: "cedar"}, nil)
	f.Add(proxmox.VM{VMID: 10621, Name: "team06-teak", Node: "cedar"}, nil)
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "spruce"}, nil)

	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	// The first clone found (by VMID) is named.
	if got, want := plan.Templates[0].Blocked, "pod VMs such as team05-teak still use this template; tear them down first"; got != want {
		t.Errorf("template Blocked = %q, want %q", got, want)
	}
	// The pod's own VM is blocked where it is.
	if it := plan.Items[0]; !strings.HasPrefix(it.Blocked, "template teak.tango.delta.tpl:") || it.Node != "spruce" {
		t.Errorf("item = %+v", it)
	}
}

// A rebuild is held up only by other pods' VMs of the template's host.
func TestDeployPlanRebuildIgnoresOtherHostsClones(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 10225, Name: "team02-oak", Node: "birch"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if tpl := plan.Templates[0]; tpl.Blocked != "" || !tpl.Rebuild {
		t.Errorf("template = %+v", tpl)
	}
}

// A rebuild whose pod VMs can't be deleted blocks the template, naming the
// first such VM, past a template that isn't rebuilt.
func TestDeployPlanRebuildBlockedByUndeletableVMs(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 10150, Name: "team01-teak", Node: "cedar"}, nil)
	f.Add(proxmox.VM{VMID: 10250, Name: "team02-teak", Node: "cedar"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01", "02"}, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if oak := plan.Templates[0]; oak.Name != "oak.tango.delta.tpl" || oak.Rebuild || oak.Blocked != "" {
		t.Fatalf("oak = %+v", oak)
	}
	if got, want := plan.Templates[1].Blocked, "pod VMs such as team01-teak still use this template; tear them down first"; got != want {
		t.Errorf("teak Blocked = %q, want %q", got, want)
	}
}

// A pod VM that is converged, not rebuilt, stays on its node even when the
// pod's other VMs are mostly elsewhere.
func TestDeployPlanConvergedVMStaysOnItsNode(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "spruce"}, nil)
	f.Add(proxmox.VM{VMID: 10110, Name: "team01-elm", Node: "birch"}, nil)
	f.Add(proxmox.VM{VMID: 10111, Name: "team01-ash", Node: "birch"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if it := plan.Items[0]; it.Node != "spruce" || it.Blocked != "" {
		t.Errorf("item = %+v", it)
	}
}

// A template may sit at template_vmid_base itself; a clean master gives no
// notices.
func TestDeployPlanTemplateAtVMIDBase(t *testing.T) {
	f := podstest.New("cedar")
	f.Add(proxmox.VM{VMID: 100, Name: "edge.x", Node: "cedar", Tags: "dev"}, map[string]string{"net0": "virtio=BC:24:11:00:01:00,bridge=vmbr0", "agent": "1"})
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "edge.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if tpl := plan.Templates[0]; tpl.VMID != 9000 || tpl.Blocked != "" {
		t.Errorf("template = %+v", tpl)
	}
	if it := plan.Items[0]; it.VMID != 10100 || it.Blocked != "" {
		t.Errorf("item = %+v", it)
	}
	if plan.Notices != nil {
		t.Errorf("Notices = %q", plan.Notices)
	}
}

// New pods are spread by the size of the pod being deployed, which a
// blocked template's master doesn't add to.
func TestDeployPlanSpreadsPodsBySizeOfUnblockedTemplates(t *testing.T) {
	f := podstest.New("cedar", "birch")
	f.Add(proxmox.VM{VMID: 50, Name: "load", Node: "cedar", Status: "running", MaxMem: 4 << 30}, nil)
	f.Add(proxmox.VM{VMID: 101, Name: "a.x", Node: "cedar", Tags: "dev", MaxMem: 8 << 30}, nil)
	f.Add(proxmox.VM{VMID: 102, Name: "b.x", Node: "cedar", Tags: "dev"}, nil)
	f.Add(proxmox.VM{VMID: 9002, Name: "b.x.tpl", Node: "cedar"}, nil) // not a template: b is blocked
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "*.x", Teams: []string{"01", "02", "03"}})
	if err != nil {
		t.Fatal(err)
	}
	// Pods of 8 GiB: birch (0), then cedar (4 GiB), then birch (8 GiB).
	nodes := map[string]string{}
	for _, it := range plan.Items {
		if it.Host == "a" {
			nodes[it.Team] = it.Node
		}
	}
	if want := map[string]string{"01": "birch", "02": "cedar", "03": "birch"}; !reflect.DeepEqual(nodes, want) {
		t.Errorf("nodes = %v, want %v", nodes, want)
	}
}

func TestDeployPlanExplainsUntaggedMasters(t *testing.T) {
	_, err := testPlanner(newCluster()).Deploy(context.Background(), DeployRequest{Pattern: "notes.*", Teams: []string{"01"}})
	if want := `no master VMs match "notes.*" with tag "dev" (1 match but lack the tag, e.g. notes.tango.delta)`; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
	_, err = testPlanner(newCluster()).Deploy(context.Background(), DeployRequest{Pattern: "pine.*", Teams: []string{"01"}})
	if want := `no master VMs match "pine.*" with tag "dev"`; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}

func TestTeardownAndResetPlans(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial")
	f.Add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil)
	f.Add(proxmox.VM{VMID: 10221, Name: "team02-teak", Node: "cedar"}, nil, "initial")
	p := testPlanner(f)

	td, err := p.Teardown(context.Background(), []string{"01"}, nil, "")
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
	f.Add(proxmox.VM{VMID: 9121, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)

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
	f := podstest.New("cedar")
	f.Add(proxmox.VM{VMID: 199, Name: "edge.x", Node: "cedar", Tags: "dev"}, map[string]string{"net0": "virtio=BC:24:11:00:01:99,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 9099, Name: "unrelated", Node: "cedar"}, nil)

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
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "spruce", Template: true}, nil)

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
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "spruce", Template: true}, map[string]string{
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
		f := podstest.New("cedar")
		f.Add(proxmox.VM{VMID: 121, Name: "teak.x", Node: "cedar", Tags: "dev"}, map[string]string{"vga": vga})
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
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar"}, nil)
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
	f.Add(proxmox.VM{VMID: 9099, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "several VMs are named teak.tango.delta.tpl (VMIDs 9021, 9099); rename or delete the extras"
	if got := plan.Templates[0].Blocked; got != want {
		t.Errorf("template Blocked = %q", got)
	}

	f = newCluster()
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 10221, Name: "team01-teak", Node: "cedar"}, nil)
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
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
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 10105, Name: "team01-teak", Node: "spruce"}, nil)
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
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial")
	f.Add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil)
	f.Add(proxmox.VM{VMID: 10225, Name: "team02-oak", Node: "birch"}, nil, "initial")
	f.FailOn("snapshots:10225", errors.New("boom"))
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
	f.FailOn("snapshots:10121", context.Canceled)
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
		if _, err := testPlanner(f).Teardown(context.Background(), bad, nil, ""); err == nil {
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
	f.Add(proxmox.VM{VMID: 9050, Name: "teak.tango.delta.tpl", Node: "spruce", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 10150, Name: "team01-teak", Node: "cedar"}, nil)
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
	if _, err := p.Teardown(ctx, nil, nil, ""); err == nil {
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
	f.Add(proxmox.VM{VMID: 131, Name: "teak.kilo.delta", Node: "cedar", Tags: "dev"}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Templates) != 2 {
		t.Fatalf("templates = %+v", plan.Templates)
	}
	for _, tpl := range plan.Templates {
		if !strings.HasPrefix(tpl.Blocked, "hosts of ") || !strings.Contains(tpl.Blocked, "both map to pod VM names like team01-teak") ||
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
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Template: true}, nil)
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Items[0].Blocked; !strings.Contains(got, "is a template, not a pod VM") {
		t.Errorf("Blocked = %q", got)
	}
}

func TestWillStopMasterOnlyWhenBuildingFromRunningMaster(t *testing.T) {
	f := newCluster()
	f.VMs[121].Status = "running"
	p := testPlanner(f)
	plan, _ := p.Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if !plan.Templates[0].WillStopMaster {
		t.Error("new template from a running master: WillStopMaster = false")
	}
	f.VMs[121].Status = "stopped"
	plan, _ = p.Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if plan.Templates[0].WillStopMaster {
		t.Error("stopped master: WillStopMaster = true")
	}
	f.VMs[121].Status = "running"
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	plan, _ = p.Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if plan.Templates[0].WillStopMaster {
		t.Error("reused template: WillStopMaster = true")
	}
	plan, _ = p.Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
	if !plan.Templates[0].WillStopMaster {
		t.Error("rebuild from a running master: WillStopMaster = false")
	}
}

func TestDeployMasterAndUntaggedWarnings(t *testing.T) {
	f := podstest.New("cedar")
	f.CPUModels = []string{"baseline"}
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "cedar", Pool: "dev-01", Tags: "dev;kilo.alpha", MaxMem: 16 << 30},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devint02", "net1": "virtio=BC:24:11:00:00:02,bridge=devext01",
			"cpu": "x86-64-v2-AES", "agent": "0", "ide2": "local:iso/debian.iso,media=cdrom", "ostype": "win11"})
	f.Add(proxmox.VM{VMID: 2102, Name: "elm.kilo.alpha", Node: "cedar", Pool: "dev-01", Tags: ""},
		map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=devint01"})

	cfg := config.Default()
	plan, err := NewPlanner(f, cfg).Deploy(context.Background(), DeployRequest{Pattern: "*.kilo.alpha", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`Left out: elm.kilo.alpha in dev-01 lacks tag "dev".`,
		"teak.kilo.alpha: net0 on devint02, expected devint01; 2 NICs, expected 1; CPU x86-64-v2-AES, expected baseline; guest agent off; CD-ROM attached; 16 GiB of memory (Windows VMs are usually at most 12 GiB).",
	}
	if !reflect.DeepEqual(plan.Notices, want) {
		t.Errorf("Notices = %v, want %v", plan.Notices, want)
	}
}

func TestDeployPlanRebuildReplacesInTeamVMsAndBlocksOnBlockedVM(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "spruce"}, nil)

	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{
		Pattern: "teak.*", Teams: []string{"01", "02"}, Rebuild: true, Snapshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Templates[0].Blocked != "" || !plan.Templates[0].Rebuild {
		t.Fatalf("template = %+v", plan.Templates[0])
	}
	wantSteps := []Step{StepStop, StepDelete, StepClone, StepNetwork, StepDiskLimits, StepCDROM, StepNotes, StepSetTag, StepSnapshot, StepStart}
	if !reflect.DeepEqual(plan.Items[0].Steps, wantSteps) || plan.Items[0].Blocked != "" {
		t.Errorf("team01-teak = %+v, want steps %v", plan.Items[0], wantSteps)
	}

	f.Add(proxmox.VM{VMID: 10110, Name: "team01-elmer", Node: "birch"}, nil)
	plan, err = testPlanner(f).Deploy(context.Background(), DeployRequest{
		Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.Items[0].Blocked = "you don't have VM.Allocate on /vms/10121"
	plan.BlockUnrebuildable()
	if plan.Items[0].Node != "spruce" {
		t.Errorf("blocked existing item Node = %q, want spruce", plan.Items[0].Node)
	}

	f.Add(proxmox.VM{VMID: 10299, Name: "team02-teak", Node: "birch"}, nil)
	plan, err = testPlanner(f).Deploy(context.Background(), DeployRequest{
		Pattern: "teak.*", Teams: []string{"01", "02"}, Rebuild: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Templates[0].Blocked, "team02-teak") {
		t.Errorf("template Blocked = %q", plan.Templates[0].Blocked)
	}
	if !strings.HasPrefix(plan.Items[0].Blocked, "template teak.tango.delta.tpl:") || plan.Items[0].Node != "spruce" {
		t.Errorf("team01-teak = %+v", plan.Items[0])
	}
}

func TestKindFacts(t *testing.T) {
	for _, k := range Kinds {
		onEnv := k == KindEnvAdd || k == KindEnvTeardown
		retryable := !k.Teamless() || k == KindEnvCreate || k == KindEnvAdd || k == KindEnvTeardown
		if k.OnEnvironment() != onEnv || k.Retryable() != retryable || k.RetriedWhole() != (k == KindEnvTeardown) {
			t.Errorf("%s: OnEnvironment %v, Retryable %v, RetriedWhole %v", k, k.OnEnvironment(), k.Retryable(), k.RetriedWhole())
		}
	}
}

func TestItemPool(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	plan := &Plan{Items: []Item{{Name: "gw", Pool: "dev-02"}, {Name: "dc"}, {Name: "dc", Pool: "dev-01"}}}
	for _, c := range []struct{ name, team, want string }{
		{"dc", "", "dev-01"}, // the first item of the name with a pool
		{"gw", "07", "pool-07"},
		{"web", "", ""},
	} {
		if got := plan.ItemPool(n, c.name, c.team); got != c.want {
			t.Errorf("ItemPool(%q, %q) = %q, want %q", c.name, c.team, got, c.want)
		}
	}
}

// orphanFake is a one-node cluster with template 9001, whose pod 01 and 02
// clones (10101, 10201) are gone; vols puts volumes on deploy.storage, each
// "<owner>/<file>".
func orphanFake(vols ...string) *podstest.Fake {
	f := podstest.New("cedar")
	f.Add(proxmox.VM{VMID: 9001, Name: "dc.kilo.alpha.tpl", Node: "cedar", Template: true}, nil)
	for _, v := range vols {
		owner, _, _ := strings.Cut(v, "/")
		id, err := strconv.Atoi(owner)
		if err != nil {
			panic(err)
		}
		f.Vols[config.Default().Deploy.Storage+":"+v] = id
	}
	return f
}

func teardownItems(t *testing.T, api Reads) []string {
	t.Helper()
	plan, err := NewPlanner(api, config.Default()).Teardown(context.Background(), []string{"01", "02"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range plan.Items {
		out = append(out, it.Name+"@"+it.Node)
	}
	return out
}

// Every gone VM's disks are freed, however many disks each left.
func TestTeardownFreesEveryGoneVMsDisks(t *testing.T) {
	f := orphanFake("10101/vm-10101-disk-0.qcow2", "10101/vm-10101-disk-1.qcow2", "10201/vm-10201-disk-0.qcow2", "10201/vm-10201-disk-1.qcow2")
	if got, want := teardownItems(t, f), []string{"team01-disks-10101@cedar", "team02-disks-10201@cedar"}; !reflect.DeepEqual(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
}

// Disks at a VMID a VM the user can't see holds are that VM's: not freed.
func TestTeardownKeepsDisksOfUnseenVM(t *testing.T) {
	f := orphanFake("10101/vm-10101-disk-0.qcow2", "10201/vm-10201-disk-0.qcow2")
	f.Unseen = map[int]bool{10101: true}
	if got, want := teardownItems(t, f), []string{"team02-disks-10201@cedar"}; !reflect.DeepEqual(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	// A disk of 10201's filed under 10101 is still 10201's to free.
	f = orphanFake("10101/vm-10101-disk-0.qcow2", "10101/vm-10201-disk-0.qcow2")
	f.Unseen = map[int]bool{10101: true}
	if got, want := teardownItems(t, f), []string{"team02-disks-10201@cedar"}; !reflect.DeepEqual(got, want) {
		t.Errorf("filed under 10101: items = %v, want %v", got, want)
	}
}

// A teardown that can't tell whether a VMID is free fails rather than
// freeing its disks.
func TestTeardownFailsWhenVMIDUseUnknown(t *testing.T) {
	f := orphanFake("10101/vm-10101-disk-0.qcow2")
	f.FailOn("vmid-held:10101", errors.New("boom"))
	_, err := NewPlanner(f, config.Default()).Teardown(context.Background(), []string{"01"}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "checking whether VMID 10101 is free: boom") {
		t.Errorf("err = %v", err)
	}
}

// Disks are looked for where deploy.storage is: on one node if it is shared,
// else on each node that has it; with no node reporting it, on every node.
func TestTeardownListsDisksWhereTheStorageIs(t *testing.T) {
	storage := config.Default().Deploy.Storage
	for _, c := range []struct {
		name string
		res  []proxmox.StorageResource
		want string
	}{
		{"shared", []proxmox.StorageResource{{Storage: "local", Node: "cedar"}, {Storage: storage, Node: "birch", Shared: true}}, "birch"},
		{"node-local", []proxmox.StorageResource{{Storage: storage, Node: "spruce"}, {Storage: storage, Node: "birch"}}, "birch"},
		{"unreported", []proxmox.StorageResource{{Storage: "local", Node: "spruce"}}, "cedar"},
	} {
		f := orphanFake("10101/vm-10101-disk-0.qcow2")
		f.Nodes = []string{"cedar", "birch", "spruce"}
		api := &resourceAPI{Fake: f, res: proxmox.Resources{Storage: c.res}}
		if got, want := teardownItems(t, api), []string{"team01-disks-10101@" + c.want}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: items = %v, want %v", c.name, got, want)
		}
	}
}

// VMs sharing a name stay in VMID order.
func TestTeardownKeepsSameNamedVMsInVMIDOrder(t *testing.T) {
	f := orphanFake()
	f.Add(proxmox.VM{VMID: 10102, Name: "team01-dc", Node: "cedar", Pool: "pool-01"}, nil)
	f.Add(proxmox.VM{VMID: 10101, Name: "team01-dc", Node: "cedar", Pool: "pool-01"}, nil)
	plan, err := NewPlanner(f, config.Default()).Teardown(context.Background(), []string{"01"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, it := range plan.Items {
		ids = append(ids, it.VMID)
	}
	if want := []int{10101, 10102}; !reflect.DeepEqual(ids, want) {
		t.Errorf("VMIDs = %v, want %v", ids, want)
	}
}

// Disks are looked for only at the clone VMIDs of templates: .tpl-named
// templates in template_vmid_base..+99.
func TestTeardownLooksForDisksOfTemplatesInRange(t *testing.T) {
	for _, c := range []struct {
		name string
		tpl  proxmox.VM
		want bool
	}{
		{"at the base", proxmox.VM{VMID: 9000, Name: "dc.x.tpl", Template: true}, true},
		{"past the range", proxmox.VM{VMID: 9100, Name: "dc.x.tpl", Template: true}, false},
		{"not a template", proxmox.VM{VMID: 9000, Name: "dc.x.tpl"}, false},
		{"not .tpl", proxmox.VM{VMID: 9000, Name: "dc.x", Template: true}, false},
	} {
		f := podstest.New("cedar")
		f.Add(proxmox.VM{VMID: 50, Name: "other", Node: "cedar"}, nil) // listed before the template
		c.tpl.Node = "cedar"
		f.Add(c.tpl, nil)
		f.Vols[config.Default().Deploy.Storage+":10100/vm-10100-disk-0.qcow2"] = 10100
		got := teardownItems(t, f)
		if want := []string{"team01-disks-10100@cedar"}; c.want != reflect.DeepEqual(got, want) {
			t.Errorf("%s: items = %v", c.name, got)
		}
	}
}

// A VM blocked for its pool doesn't stop the VMs after it being checked.
func TestResetChecksVMsPastABlockedOne(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-ash", Node: "cedar", Pool: "pool-02"}, nil, "initial")
	f.Add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil)
	rs, err := testPlanner(f).Reset(context.Background(), []string{"01"}, nil, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rs.Items[0].Blocked, "is in pool pool-02") || rs.Items[1].Blocked != `no snapshot "initial" (has none)` {
		t.Errorf("items = %+v", rs.Items)
	}
}

// A rebuilt pod VM is recloned on its pod's node, not where the old one
// was; one the rebuild can't delete is reported where it is.
func TestDeployPlanRebuildPlacesVMOnPodNode(t *testing.T) {
	for _, tc := range []struct {
		vmid        int
		node        string
		wantBlocked bool
	}{
		{10121, "birch", false},
		{10150, "spruce", true}, // wrong VMID: blocked
	} {
		f := newCluster()
		f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nil)
		f.Add(proxmox.VM{VMID: tc.vmid, Name: "team01-teak", Node: "spruce"}, nil)
		f.Add(proxmox.VM{VMID: 10110, Name: "team01-elm", Node: "birch"}, nil)
		f.Add(proxmox.VM{VMID: 10111, Name: "team01-ash", Node: "birch"}, nil)
		plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Rebuild: true})
		if err != nil {
			t.Fatal(err)
		}
		it := plan.Items[0]
		if it.Node != tc.node || it.OldNode != "spruce" || (it.Blocked != "") != tc.wantBlocked {
			t.Errorf("VMID %d: item = %+v, want Node %s", tc.vmid, it, tc.node)
		}
	}
}

// A host filter that leaves an approved set no master names the set's
// pattern.
func TestDeployApprovedSetNamesItWhenNoHostMatches(t *testing.T) {
	cfg := config.Default()
	cfg.Deploy.ApprovedTag = "approved"
	_, err := approvedDeploy(approvedCluster("dev;approved", "dev;approved"), cfg, "pine")
	if want := `no master VMs match "*.tango.delta" with tag "dev" and hosts pine`; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}
