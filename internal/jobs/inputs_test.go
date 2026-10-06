package jobs

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

var bg = context.Background()

func TestValidate(t *testing.T) {
	cases := []struct {
		in   Inputs
		want string // "" means valid
	}{
		{Inputs{Kind: pods.KindTeardown, Teams: "1-32"}, ""},
		{Inputs{Kind: pods.KindDeploy, Teams: "1", Pattern: "*.x"}, ""},
		{Inputs{Kind: pods.KindDeploy, Teams: "1"}, "deploy needs a template pattern"},
		{Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, ""},
		{Inputs{Kind: pods.KindPower, Teams: "1", Action: "explode"}, "power action must be"},
		{Inputs{Kind: "nuke", Teams: "1"}, `unknown job kind "nuke"`},
		{Inputs{Kind: pods.KindReset, Teams: ""}, "teams: no teams given"},
		{Inputs{Kind: pods.KindReset, Teams: "1", Hosts: []string{"dc", " "}}, "blank entries"},
	}
	for _, tc := range cases {
		err := tc.in.Validate()
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("Validate(%+v) = %v, want %q", tc.in, err, tc.want)
		}
	}
}

func testCfg() config.Config {
	cfg := config.Default()
	cfg.Retry.Rounds = 0
	return cfg
}

func TestBuildPlanUsesInputs(t *testing.T) {
	f := newFake(
		proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "n1", Status: "running"},
		proxmox.VM{VMID: 10105, Name: "team01-dc", Node: "n1", Status: "running"},
		proxmox.VM{VMID: 10221, Name: "team02-teak", Node: "n1", Status: "running"},
	)
	cfg := testCfg()
	p := pods.NewPlanner(f, cfg)

	plan, err := BuildPlan(context.Background(), p, Inputs{Kind: pods.KindTeardown, Teams: "1", Hosts: []string{"teak"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Name != "team01-teak" {
		t.Errorf("teardown items = %+v", plan.Items)
	}
	reset, err := BuildPlan(context.Background(), p, Inputs{Kind: pods.KindReset, Teams: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reset.Items) != 1 || reset.Items[0].Snapshot != cfg.Deploy.SnapshotName {
		t.Errorf("reset items = %+v, want default snapshot", reset.Items)
	}
	if _, err := BuildPlan(context.Background(), p, Inputs{Kind: pods.KindPower, Teams: "1"}); err == nil {
		t.Error("BuildPlan accepted power with no action")
	}
}

func plan(items ...pods.Item) *pods.Plan {
	return &pods.Plan{Kind: pods.KindDeploy, Teams: []string{"01"}, Items: items,
		Templates: []pods.TemplateSpec{{Name: "teak.x.tpl", VMID: 9021}}}
}

func TestFingerprintIgnoresNodesButNotWork(t *testing.T) {
	base := pods.Item{Name: "team01-teak", VMID: 10121, Node: "n1", Steps: []pods.Step{pods.StepClone, pods.StepStart}}
	fp := Fingerprint(plan(base))

	moved := base
	moved.Node = "n2"
	if Fingerprint(plan(moved)) != fp {
		t.Error("node assignment changed the fingerprint")
	}

	// Precision checks: these should NOT change the fingerprint
	precisionBase := plan(base)
	precisionBase.Templates[0].Blocked = "VMID used"
	precisionFP := Fingerprint(precisionBase)
	precisionMuted := plan(base)
	precisionMuted.Templates[0].Blocked = "VMID used by team02"
	if Fingerprint(precisionMuted) != precisionFP {
		t.Error("changing blocked reason text changed the fingerprint")
	}

	nodeBase := plan(base)
	nodeBase.Templates[0].Node = "n1"
	nodeFP := Fingerprint(nodeBase)
	nodeMuted := plan(base)
	nodeMuted.Templates[0].Node = "n2"
	if Fingerprint(nodeMuted) != nodeFP {
		t.Error("template node assignment changed the fingerprint")
	}

	for name, mut := range map[string]func(*pods.Plan){
		"steps":             func(p *pods.Plan) { p.Items[0].Steps = []pods.Step{pods.StepStart} },
		"blocked":           func(p *pods.Plan) { p.Items[0].Blocked = "VMID used" },
		"vmid":              func(p *pods.Plan) { p.Items[0].VMID = 10122 },
		"extra item":        func(p *pods.Plan) { p.Items = append(p.Items, pods.Item{Name: "team01-dc"}) },
		"template rebuild":  func(p *pods.Plan) { p.Templates[0].Rebuild = true },
		"master stop":       func(p *pods.Plan) { p.Templates[0].WillStopMaster = true },
		"interfaces":        func(p *pods.Plan) { p.Templates[0].Interfaces = 3 },
		"gpu":               func(p *pods.Plan) { p.Templates[0].GPU = true },
		"master vmid":       func(p *pods.Plan) { p.Templates[0].MasterVMID = 1234 },
		"item template":     func(p *pods.Plan) { p.Items[0].Template = "dc.x.tpl" },
		"resolved snapshot": func(p *pods.Plan) { p.Items[0].Snapshot = "fresh_clone_20261002034615" },
	} {
		p := plan(base)
		mut(p)
		if Fingerprint(p) == fp {
			t.Errorf("changing %s didn't change the fingerprint", name)
		}
	}
}

func TestLockKeys(t *testing.T) {
	p := &pods.Plan{
		Teams:     []string{"02", "01"},
		Templates: []pods.TemplateSpec{{Name: "teak.x.tpl"}, {Name: "dc.x.tpl"}},
	}
	want := []string{"team:01", "team:02", "template:dc.x.tpl", "template:teak.x.tpl"}
	if got := LockKeys(p); !reflect.DeepEqual(got, want) {
		t.Errorf("LockKeys = %v, want %v", got, want)
	}
}

// Capacity is advice beside the plan: reading it leaves the plan, and so
// its fingerprint, as it was, however full the cluster is.
func TestCapacityLeavesFingerprintAlone(t *testing.T) {
	p := plan(pods.Item{Name: "team01-teak", VMID: 10121, Node: "n1", Template: "teak.x.tpl", Steps: []pods.Step{pods.StepClone, pods.StepStart}})
	fp := Fingerprint(p)
	before, _ := json.Marshal(p)
	for _, free := range []int64{1, 1000} {
		c := pods.EstimateCapacity(p, proxmox.Resources{
			VMs:   []proxmox.VM{{VMID: p.Templates[0].MasterVMID, MaxMem: 500}},
			Nodes: []proxmox.NodeResource{{Node: "n1", Status: "online", Mem: 1000 - free, MaxMem: 1000}},
		}, config.Default().Deploy)
		if c == nil || len(c.Usage) != 1 {
			t.Fatalf("capacity = %+v, want n1's memory", c)
		}
		after, _ := json.Marshal(p)
		if Fingerprint(p) != fp || string(after) != string(before) {
			t.Errorf("capacity with %d free changed the plan or its fingerprint", free)
		}
	}
}

// Two deploys of different masters can pick the same free VMID for their
// new templates (masters 1021 and 2021 both prefer 9021), so their lock keys
// overlap on it and they run one after the other.
func TestLockKeysCoverTemplateVMIDs(t *testing.T) {
	a := &pods.Plan{Teams: []string{"01"}, Templates: []pods.TemplateSpec{{Name: "teak.a.tpl", VMID: 9021}}}
	b := &pods.Plan{Teams: []string{"02"}, Templates: []pods.TemplateSpec{{Name: "teak.b.tpl", VMID: 9021}}}
	shared := false
	for _, k := range LockKeys(a) {
		shared = shared || slices.Contains(LockKeys(b), k)
	}
	if !shared {
		t.Errorf("lock keys %v and %v don't overlap, though both plans use VMID 9021", LockKeys(a), LockKeys(b))
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList(" dc, web ,,")
	if len(got) != 2 || got[0] != "dc" || got[1] != "web" {
		t.Errorf("SplitList = %q", got)
	}
}
