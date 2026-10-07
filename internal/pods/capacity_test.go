package pods

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

const gib = int64(1) << 30

// capCluster: cedar has 40 GiB memory free, birch 224 GiB; competitions is
// node-local with 100 GiB free on cedar, 900 GiB on birch. Master teak (121)
// and team VMs are 8 GiB memory, 32 GiB disk; team03-teak is stopped on
// birch, team04-teak runs there.
func capCluster() proxmox.Resources {
	return proxmox.Resources{
		VMs: []proxmox.VM{
			{VMID: 121, Name: "teak.x", Node: "cedar", Status: "running", MaxMem: 8 * gib, MaxDisk: 32 * gib},
			{VMID: 9021, Name: "teak.x.tpl", Node: "cedar", Status: "stopped", Template: true, MaxMem: 6 * gib, MaxDisk: 20 * gib},
			{VMID: 10321, Name: "team03-teak", Node: "birch", Status: "stopped", MaxMem: 4 * gib, MaxDisk: 32 * gib},
			{VMID: 10421, Name: "team04-teak", Node: "birch", Status: "running", MaxMem: 4 * gib, MaxDisk: 32 * gib},
		},
		Nodes: []proxmox.NodeResource{
			{Node: "cedar", Status: "online", Mem: 216 * gib, MaxMem: 256 * gib},
			{Node: "birch", Status: "online", Mem: 32 * gib, MaxMem: 256 * gib},
			{Node: "spruce", Status: "online", Mem: 0, MaxMem: 256 * gib},
		},
		Storage: []proxmox.StorageResource{
			{Storage: "competitions", Node: "cedar", Disk: 900 * gib, MaxDisk: 1000 * gib},
			{Storage: "competitions", Node: "birch", Disk: 100 * gib, MaxDisk: 1000 * gib},
			{Storage: "local", Node: "cedar", Disk: 0, MaxDisk: 100 * gib},
		},
	}
}

func deployItemOn(team, node string, vmid int, clone bool) Item {
	it := Item{Team: team, Host: "teak", Name: "team" + team + "-teak", VMID: vmid, Node: node, Template: "teak.x.tpl"}
	if clone {
		it.Steps = append(it.Steps, StepClone)
	}
	it.Steps = append(it.Steps, StepNetwork, StepDiskLimits, StepCDROM, StepSnapshot, StepStart)
	return it
}

// overcommitPlan builds teak's template, clones 12 team VMs onto cedar and
// starts team03; team04 already runs and team05 is blocked.
func overcommitPlan() *Plan {
	p := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{{
		Name: "teak.x.tpl", Host: "teak", VMID: 9021, Node: "cedar", MasterName: "teak.x", MasterVMID: 121, MasterNode: "cedar",
	}}}
	for i := range 12 {
		team := FormatTeam(10 + i)
		p.Items = append(p.Items, deployItemOn(team, "cedar", 11000+i, true))
	}
	p.Items = append(p.Items, deployItemOn("03", "birch", 10321, false), deployItemOn("04", "birch", 10421, false))
	blocked := deployItemOn("05", "cedar", 10521, true)
	blocked.Blocked = "VMID 10521 is used by something"
	p.Items = append(p.Items, blocked)
	return p
}

func deployCfg(linked bool) config.Deploy {
	d := config.Default().Deploy
	d.Linked = linked
	return d
}

func TestCapacityOvercommitWarns(t *testing.T) {
	c := EstimateCapacity(overcommitPlan(), capCluster(), deployCfg(false))
	if c == nil {
		t.Fatal("no capacity for a deploy")
	}
	want := []Usage{
		{Kind: UsageMemory, Node: "birch", Used: 32 * gib, Total: 256 * gib, Needed: 4 * gib},
		{Kind: UsageMemory, Node: "cedar", Used: 216 * gib, Total: 256 * gib, Needed: 96 * gib},
		// Full clones: the build copies the master's 32 GiB, and each of the 12
		// clones copies the template (master-sized, as it's built in this plan).
		{Kind: UsageStorage, Node: "cedar", Storage: "competitions", Used: 900 * gib, Total: 1000 * gib, Needed: 13 * 32 * gib},
	}
	if !reflect.DeepEqual(c.Usage, want) {
		t.Errorf("usage =\n %+v\nwant\n %+v", c.Usage, want)
	}
	ws := c.Warnings()
	wantWarn := []CapacityWarning{
		{Over: true, Text: "cedar: needs 96 GiB of memory, 40 GiB free"},
		{Over: true, Text: "competitions on cedar: needs 416 GiB of disk, 100 GiB free"},
	}
	if !reflect.DeepEqual(ws, wantWarn) {
		t.Errorf("warnings = %+v, want %+v", ws, wantWarn)
	}
	if len(c.Notes) != 0 {
		t.Errorf("notes = %q, want none for full clones", c.Notes)
	}
}

func TestCapacityWithoutOvercommitIsQuiet(t *testing.T) {
	p := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{{
		Name: "teak.x.tpl", Host: "teak", VMID: 9021, Node: "cedar", Exists: true, MasterName: "teak.x", MasterVMID: 121, MasterNode: "cedar",
	}}}
	p.Items = []Item{deployItemOn("01", "birch", 10121, true), deployItemOn("02", "spruce", 10221, true)}
	c := EstimateCapacity(p, capCluster(), deployCfg(true))
	// The existing template is reused: clones get its 6 GiB; linked clones need
	// no disk up front.
	want := []Usage{
		{Kind: UsageMemory, Node: "birch", Used: 32 * gib, Total: 256 * gib, Needed: 6 * gib},
		{Kind: UsageMemory, Node: "spruce", Used: 0, Total: 256 * gib, Needed: 6 * gib},
	}
	if !reflect.DeepEqual(c.Usage, want) {
		t.Errorf("usage = %+v, want %+v", c.Usage, want)
	}
	if ws := c.Warnings(); len(ws) != 0 {
		t.Errorf("warnings = %+v, want none", ws)
	}
	if len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "Linked clones") || !strings.Contains(c.Notes[0], "grow") {
		t.Errorf("notes = %q, want the linked-clone note", c.Notes)
	}
}

func TestCapacityNearFullIsASofterNotice(t *testing.T) {
	res := capCluster()
	res.Nodes[1].Mem = 200 * gib // birch: 56 GiB free
	p := &Plan{Kind: KindPower}
	for i := range 4 {
		p.Items = append(p.Items, Item{Name: "x", VMID: 20000 + i, Node: "birch", Action: "start", Steps: []Step{StepPower}})
		res.VMs = append(res.VMs, proxmox.VM{VMID: 20000 + i, Node: "birch", Status: "stopped", MaxMem: 8 * gib})
	}
	c := EstimateCapacity(p, res, deployCfg(true))
	ws := c.Warnings()
	if len(ws) != 1 || ws[0].Over || ws[0].Text != "birch: memory would be 91% used (needs 32 GiB, 56 GiB free)" {
		t.Errorf("warnings = %+v", ws)
	}
}

func TestCapacityPowerStartCountsStoppedVMsOnly(t *testing.T) {
	p := &Plan{Kind: KindPower, Items: []Item{
		{Name: "team03-teak", VMID: 10321, Node: "birch", Action: "start", Steps: []Step{StepPower}},
		{Name: "team04-teak", VMID: 10421, Node: "birch", Action: "start", Steps: []Step{StepPower}},
		{Name: "team05-teak", VMID: 121, Node: "cedar", Action: "start", Steps: []Step{StepPower}, Blocked: "x"},
	}}
	c := EstimateCapacity(p, capCluster(), deployCfg(true))
	want := []Usage{{Kind: UsageMemory, Node: "birch", Used: 32 * gib, Total: 256 * gib, Needed: 4 * gib}}
	if !reflect.DeepEqual(c.Usage, want) {
		t.Errorf("usage = %+v, want %+v", c.Usage, want)
	}
	if len(c.Notes) != 0 {
		t.Errorf("notes = %q", c.Notes)
	}
}

func TestCapacityOnlyForDeployAndStart(t *testing.T) {
	for _, p := range []*Plan{
		{Kind: KindPower, Items: []Item{{VMID: 10321, Node: "birch", Action: "stop"}}},
		{Kind: KindTeardown, Items: []Item{{VMID: 10321, Node: "birch"}}},
		{Kind: KindReset, Items: []Item{{VMID: 10321, Node: "birch"}}},
	} {
		if NeedsCapacity(p) {
			t.Errorf("%s %v needs capacity", p.Kind, p.Items[0].Action)
		}
		if c := EstimateCapacity(p, capCluster(), deployCfg(true)); c != nil {
			t.Errorf("%s: capacity = %+v, want nil", p.Kind, c)
		}
	}
	if !NeedsCapacity(&Plan{Kind: KindDeploy}) || !NeedsCapacity(&Plan{Kind: KindPower, Items: []Item{{Action: "start"}}}) {
		t.Error("deploy and power start need capacity")
	}
}

func TestCapacitySharedStorageIsOneRow(t *testing.T) {
	res := capCluster()
	res.Storage = []proxmox.StorageResource{
		{Storage: "competitions", Node: "cedar", Shared: true, Disk: 100 * gib, MaxDisk: 1000 * gib},
		{Storage: "competitions", Node: "birch", Shared: true, Disk: 100 * gib, MaxDisk: 1000 * gib},
	}
	p := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{{Name: "teak.x.tpl", VMID: 9021, Exists: true, MasterVMID: 121, MasterNode: "cedar"}},
		Items: []Item{deployItemOn("01", "birch", 10121, true), deployItemOn("02", "cedar", 10221, true)}}
	c := EstimateCapacity(p, res, deployCfg(false))
	var storage []Usage
	for _, u := range c.Usage {
		if u.Kind == UsageStorage {
			storage = append(storage, u)
		}
	}
	want := []Usage{{Kind: UsageStorage, Storage: "competitions", Used: 100 * gib, Total: 1000 * gib, Needed: 40 * gib}}
	if !reflect.DeepEqual(storage, want) {
		t.Errorf("storage = %+v, want %+v", storage, want)
	}
	if got := storage[0].Label(); got != "competitions" {
		t.Errorf("label = %q", got)
	}
}

// resourceAPI is the fake with /cluster/resources, counting calls.
type resourceAPI struct {
	*podstest.Fake
	res   proxmox.Resources
	calls int
}

func (r *resourceAPI) ClusterResources(context.Context) (proxmox.Resources, error) {
	r.calls++
	return r.res, nil
}

// The capacity check reads /cluster/resources once, not per VM, using a
// real deploy plan's node assignment.
func TestReadCapacityOneCallFromPlannerPlan(t *testing.T) {
	f := newCluster()
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01", "02", "03"}})
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.Calls)
	api := &resourceAPI{Fake: f, res: proxmox.Resources{
		VMs: []proxmox.VM{
			{VMID: 121, Node: "cedar", MaxMem: 4 * gib, MaxDisk: 10 * gib},
			{VMID: 125, Node: "birch", MaxMem: 2 * gib, MaxDisk: 10 * gib},
		},
		Nodes: []proxmox.NodeResource{
			{Node: "cedar", Status: "online", MaxMem: 64 * gib},
			{Node: "birch", Status: "online", MaxMem: 64 * gib},
			{Node: "spruce", Status: "online", MaxMem: 64 * gib},
		},
	}}
	c, err := ReadCapacity(context.Background(), api, plan, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if api.calls != 1 || len(f.Calls) != before {
		t.Errorf("ClusterResources called %d times and %d other calls, want 1 and 0", api.calls, len(f.Calls)-before)
	}
	var total int64
	nodes := map[string]int64{}
	for _, u := range c.Usage {
		if u.Kind == UsageMemory {
			total += u.Needed
			nodes[u.Node] += u.Needed
		}
	}
	// 3 teak clones (4 GiB) spread over nodes, 3 GPU oak clones (2 GiB) pinned
	// to birch.
	if total != 18*gib || nodes["birch"] < 6*gib {
		t.Errorf("memory needs = %v (total %d GiB)", nodes, total/gib)
	}

	// Without /cluster/resources (an API that can't), there is no check.
	if c, err := ReadCapacity(context.Background(), f, plan, config.Default()); c != nil || err != nil {
		t.Errorf("without ClusterResources: %+v, %v", c, err)
	}
	// Nor for a plan that starts nothing.
	api.calls = 0
	if c, _ := ReadCapacity(context.Background(), api, &Plan{Kind: KindTeardown}, config.Default()); c != nil || api.calls != 0 {
		t.Errorf("teardown read capacity")
	}
}

func TestGiBText(t *testing.T) {
	for in, want := range map[int64]string{96 * gib: "96 GiB", gib + gib/2: "1.5 GiB", 512 << 20: "0.5 GiB", 10 * gib: "10 GiB", 0: "0 GiB"} {
		if got := GiB(in); got != want {
			t.Errorf("GiB(%d) = %q, want %q", in, got, want)
		}
	}
}

// Without Sys.Audit, online nodes lack mem and maxmem: one note names them
// all and the fix. Only missing or offline nodes get an offline note.
func TestCapacityOnlineNodesWithoutMemoryNeedSysAudit(t *testing.T) {
	res := capCluster()
	res.Nodes = []proxmox.NodeResource{
		{Node: "cedar", Status: "online"},
		{Node: "birch", Status: "online"},
		{Node: "spruce", Status: "offline"},
	}
	p := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{{
		Name: "teak.x.tpl", Host: "teak", VMID: 9021, Node: "cedar", Exists: true, MasterName: "teak.x", MasterVMID: 121, MasterNode: "cedar",
	}}}
	p.Items = []Item{
		deployItemOn("01", "birch", 10121, true),
		deployItemOn("02", "cedar", 10221, true),
		deployItemOn("03", "spruce", 10321, true),
		deployItemOn("04", "psyduck", 10421, true),
	}
	c := EstimateCapacity(p, res, deployCfg(false))
	var mem []string
	for _, n := range c.Notes {
		if strings.Contains(n, "memory") {
			mem = append(mem, n)
		}
	}
	want := []string{
		"Memory on birch, cedar is unknown: you don't have Sys.Audit on /nodes there, so Proxmox shows no memory figures. Nothing is blocked for it.",
		"psyduck reports no memory (it may be offline), so its 6 GiB can't be checked.",
		"spruce reports no memory (it may be offline), so its 6 GiB can't be checked.",
	}
	if !reflect.DeepEqual(mem, want) {
		t.Errorf("memory notes =\n %q\nwant\n %q", mem, want)
	}
}
