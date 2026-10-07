package pods

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// grants is an Access from a table: path -> privileges held there. A path
// missing from it holds nothing.
type grants map[string][]string

func (g grants) Privileges(_ context.Context, path string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, p := range g[path] {
		out[p] = true
	}
	return out, nil
}

func blocked(plan *Plan) map[string]string {
	out := map[string]string{}
	for _, it := range plan.Items {
		if it.Blocked != "" {
			out[it.Name] = it.Blocked
		}
	}
	for _, t := range plan.Templates {
		if t.Blocked != "" {
			out[t.Name] = t.Blocked
		}
	}
	return out
}

func powerPlan() *Plan {
	return &Plan{Kind: KindPower, Teams: []string{"01"}, Items: []Item{
		{Team: "01", Host: "dc", Name: "team01-dc", VMID: 10101, Node: "n1", Steps: []Step{StepPower}, Action: "stop"},
		{Team: "01", Host: "web", Name: "team01-web", VMID: 10102, Node: "n1", Steps: []Step{StepPower}, Action: "stop"},
	}}
}

func TestBlockUnpermittedNamesTheMissingPrivilege(t *testing.T) {
	plan := powerPlan()
	acc := grants{"/vms/10101": {"VM.Audit", "VM.PowerMgmt"}, "/vms/10102": {"VM.Audit"}}
	if err := BlockUnpermitted(context.Background(), plan, acc, config.Default()); err != nil {
		t.Fatal(err)
	}
	got := blocked(plan)
	if len(got) != 1 || got["team01-web"] != "you don't have VM.PowerMgmt on /vms/10102" {
		t.Fatalf("blocked = %v", got)
	}
}

func TestBlockUnpermittedKeepsEarlierReasons(t *testing.T) {
	plan := powerPlan()
	plan.Items[1].Blocked = "no snapshot"
	if err := BlockUnpermitted(context.Background(), plan, grants{}, config.Default()); err != nil {
		t.Fatal(err)
	}
	if got := plan.Items[1].Blocked; got != "no snapshot" {
		t.Fatalf("an item blocked for another reason now says %q", got)
	}
}

func TestRollbackAcceptsEitherSnapshotPrivilege(t *testing.T) {
	for _, priv := range []string{"VM.Snapshot", "VM.Snapshot.Rollback"} {
		plan := &Plan{Kind: KindReset, Items: []Item{{Team: "01", Name: "team01-dc", VMID: 10101, Steps: []Step{StepStop, StepRollback, StepStart}}}}
		if err := BlockUnpermitted(context.Background(), plan, grants{"/vms/10101": {priv, "VM.PowerMgmt"}}, config.Default()); err != nil {
			t.Fatal(err)
		}
		if b := plan.Items[0].Blocked; b != "" {
			t.Errorf("with %s: blocked %q", priv, b)
		}
	}
	plan := &Plan{Kind: KindReset, Items: []Item{{Team: "01", Name: "team01-dc", VMID: 10101, Steps: []Step{StepStop, StepRollback, StepStart}}}}
	_ = BlockUnpermitted(context.Background(), plan, grants{"/vms/10101": {"VM.PowerMgmt"}}, config.Default())
	if b := plan.Items[0].Blocked; b != "you don't have VM.Snapshot or VM.Snapshot.Rollback on /vms/10101" {
		t.Fatalf("blocked %q", b)
	}
}

// The deploy table: what a clone into a team's pool and its configuration
// need, and what building a template needs.
func TestDeployNeeds(t *testing.T) {
	cfg := config.Default()
	tpl := TemplateSpec{Name: "teak.x.tpl", Host: "teak", VMID: 9021, MasterVMID: 121, MasterName: "teak.x", Interfaces: 2,
		CloudInit: true, CPU: "custom-lab-baseline,flags=+aes"}
	item := Item{Team: "01", Host: "teak", Name: "team01-teak", VMID: 10121, Template: tpl.Name,
		Steps: []Step{StepClone, StepNetwork, StepDiskLimits, StepCDROM, StepSnapshot, StepStart}}
	plan := &Plan{Kind: KindDeploy, Teams: []string{"01"}, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
	everything := grants{
		"/vms/121":                  {"VM.Clone"},
		"/vms/9021":                 {"VM.Clone", "VM.Allocate"},
		"/storage/competitions":     {"Datastore.AllocateSpace"},
		"/mapping/cpu/lab-baseline": {"Mapping.Use"},
		"/pool/pool-01": {"VM.Allocate", "VM.Config.Network", "VM.Config.Cloudinit", "VM.Config.Disk",
			"VM.Config.CDROM", "VM.Snapshot", "VM.PowerMgmt"},
		"/sdn/zones/teams/ext01": {"SDN.Use"},
		"/sdn/zones/teams/int01": {"SDN.Use"},
	}
	if err := BlockUnpermitted(context.Background(), plan, everything, cfg); err != nil {
		t.Fatal(err)
	}
	if got := blocked(plan); len(got) != 0 {
		t.Fatalf("with every privilege, blocked = %v", got)
	}
	// Take each grant away in turn: something must name it.
	for path, privs := range everything {
		for i, priv := range privs {
			less := grants{}
			for p, v := range everything {
				less[p] = v
			}
			less[path] = append(append([]string(nil), privs[:i]...), privs[i+1:]...)
			plan := &Plan{Kind: KindDeploy, Teams: []string{"01"}, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
			if err := BlockUnpermitted(context.Background(), plan, less, cfg); err != nil {
				t.Fatal(err)
			}
			want := "you don't have " + priv + " on " + path
			found := false
			for _, reason := range blocked(plan) {
				if strings.Contains(reason, want) {
					found = true
				}
			}
			if !found {
				t.Errorf("without %s on %s, blocked = %v", priv, path, blocked(plan))
			}
		}
	}
}

// An existing template is reused: nothing is built, so the master's
// privileges don't matter, and a VM that exists is configured at its own
// path.
func TestDeployOfExistingVMs(t *testing.T) {
	tpl := TemplateSpec{Name: "teak.x.tpl", VMID: 9021, MasterVMID: 121, Exists: true, Interfaces: 1}
	item := Item{Team: "01", Name: "team01-teak", VMID: 10121, Template: tpl.Name, Steps: []Step{StepNetwork}}
	plan := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
	acc := grants{"/vms/10121": {"VM.Config.Network"}, "/sdn/zones/teams/int01": {"SDN.Use"}}
	if err := BlockUnpermitted(context.Background(), plan, acc, config.Default()); err != nil {
		t.Fatal(err)
	}
	if got := blocked(plan); len(got) != 0 {
		t.Fatalf("blocked = %v", got)
	}
}

// A template the user can't build blocks the VMs cloned from it.
func TestTemplateBlockBlocksItsClones(t *testing.T) {
	tpl := TemplateSpec{Name: "teak.x.tpl", VMID: 9021, MasterVMID: 121, Interfaces: 1, WillStopMaster: true}
	item := Item{Team: "01", Name: "team01-teak", VMID: 10121, Template: tpl.Name, Steps: []Step{StepClone}}
	plan := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
	acc := grants{"/vms/121": {"VM.Clone"}, "/vms/9021": {"VM.Allocate", "VM.Clone"}, "/storage/competitions": {"Datastore.AllocateSpace"},
		"/pool/pool-01": {"VM.Allocate"}}
	if err := BlockUnpermitted(context.Background(), plan, acc, config.Default()); err != nil {
		t.Fatal(err)
	}
	got := blocked(plan)
	if got["teak.x.tpl"] != "you don't have VM.PowerMgmt on /vms/121" {
		t.Fatalf("template blocked %q", got["teak.x.tpl"])
	}
	if got["team01-teak"] != "template teak.x.tpl: you don't have VM.PowerMgmt on /vms/121" {
		t.Fatalf("item blocked %q", got["team01-teak"])
	}
}

func TestTeardownNeeds(t *testing.T) {
	plan := &Plan{Kind: KindTeardown, Items: []Item{{Team: "01", Name: "team01-dc", VMID: 10101, Steps: []Step{StepStop, StepDelete}}}}
	_ = BlockUnpermitted(context.Background(), plan, grants{"/vms/10101": {"VM.PowerMgmt"}}, config.Default())
	if b := plan.Items[0].Blocked; b != "you don't have VM.Allocate on /vms/10101" {
		t.Fatalf("blocked %q", b)
	}
}

// Every privilege offered for an operation is one that some item of it, as
// the planner builds them, needs: offerPrivileges can't drift from
// itemNeeds unnoticed.
func TestOfferPrivilegesAreItemNeeds(t *testing.T) {
	ctx := context.Background()
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial")
	p := testPlanner(f)
	plans := map[Kind]func() (*Plan, error){
		KindDeploy: func() (*Plan, error) {
			return p.Deploy(ctx, DeployRequest{Pattern: "*.tango.delta", Teams: []string{"02"}, Snapshot: true})
		},
		KindTeardown: func() (*Plan, error) { return p.Teardown(ctx, []string{"01"}, nil) },
		KindReset:    func() (*Plan, error) { return p.Reset(ctx, []string{"01"}, nil, "initial") },
		KindPower:    func() (*Plan, error) { return p.Power(ctx, []string{"01"}, nil, "stop") },
		KindSnapshot: func() (*Plan, error) {
			return p.Snapshot(ctx, SnapshotRequest{Teams: []string{"01"}, Name: "before-scoring"})
		},
	}
	if len(plans) != len(offerPrivileges) {
		t.Fatalf("plans cover %d operations, offerPrivileges %d", len(plans), len(offerPrivileges))
	}
	for kind, build := range plans {
		plan, err := build()
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		templates := map[string]*TemplateSpec{}
		for i := range plan.Templates {
			templates[plan.Templates[i].Name] = &plan.Templates[i]
		}
		needed := map[string]bool{}
		for _, it := range plan.Items {
			for _, n := range itemNeeds(it, templates[it.Template], p.Cfg) {
				for _, g := range n {
					needed[g.priv] = true
				}
			}
		}
		for _, priv := range OfferPrivileges(kind) {
			if !needed[priv] {
				t.Errorf("%s offers %s, which none of its items needs", kind, priv)
			}
		}
	}
}

// Freeing a gone VM's disks needs Datastore.Allocate on the storage, as
// Proxmox's volume delete does, and nothing on the VM's path.
func TestFreeDisksNeeds(t *testing.T) {
	cfg := config.Default()
	plan := &Plan{Kind: KindTeardown, Items: []Item{{Team: "01", Name: "team01-disks-10121", VMID: 10121, Steps: []Step{StepFreeDisks}}}}
	_ = BlockUnpermitted(context.Background(), plan, grants{"/storage/" + cfg.Deploy.Storage: {"Datastore.Audit"}}, cfg)
	if b, want := plan.Items[0].Blocked, "you don't have Datastore.Allocate on /storage/"+cfg.Deploy.Storage; b != want {
		t.Fatalf("blocked %q, want %q", b, want)
	}
	plan.Items[0].Blocked = ""
	_ = BlockUnpermitted(context.Background(), plan, grants{"/storage/" + cfg.Deploy.Storage: {"Datastore.Allocate"}}, cfg)
	if b := plan.Items[0].Blocked; b != "" {
		t.Fatalf("blocked %q with Datastore.Allocate", b)
	}
}

// fakePerms is Proxmox's two answers: the listed paths, and one path's
// effective privileges.
type fakePerms struct {
	err    error // Permissions fails with it
	listed proxmox.Permissions
	at     map[string]map[string]bool
	calls  []string
}

func (f *fakePerms) Permissions(context.Context) (proxmox.Permissions, error) {
	f.calls = append(f.calls, "all")
	return f.listed, f.err
}

func (f *fakePerms) PermissionsAt(_ context.Context, path string) (map[string]bool, error) {
	f.calls = append(f.calls, path)
	return f.at[path], nil
}

// Listed paths come from one read; others are asked one by one, once, not
// guessed from their parents.
func TestAccessReadsListedPathsOnceAndAsksForOthers(t *testing.T) {
	f := &fakePerms{
		listed: proxmox.Permissions{"/vms/10101": {"VM.PowerMgmt": false}, "/vms": {"VM.Audit": true}},
		at:     map[string]map[string]bool{"/vms/9021": {"VM.Clone": false}},
	}
	acc := NewAccess(f)
	for range 2 {
		if p, _ := acc.Privileges(context.Background(), "/vms/10101"); !p["VM.PowerMgmt"] && len(p) == 0 {
			t.Fatalf("/vms/10101 = %v", p)
		}
		if p, _ := acc.Privileges(context.Background(), "/vms/9021"); len(p) != 1 {
			t.Fatalf("/vms/9021 = %v", p)
		}
	}
	if strings.Join(f.calls, ",") != "all,/vms/9021" {
		t.Fatalf("calls = %v", f.calls)
	}
	if ok, _ := acc.Anywhere(context.Background(), "VM.PowerMgmt"); !ok {
		t.Fatal("VM.PowerMgmt anywhere = false")
	}
	if ok, _ := acc.Anywhere(context.Background(), "VM.Clone"); ok {
		t.Fatal("VM.Clone anywhere from an unlisted path")
	}
}

func TestApproxUsesListedPathsOnly(t *testing.T) {
	f := &fakePerms{listed: proxmox.Permissions{
		"/vms":       {"VM.PowerMgmt": true, "VM.Clone": false},
		"/vms/10101": {"VM.Snapshot": false},
	}}
	acc := NewAccess(f)
	got, _ := acc.Approx(context.Background(), "/vms/10102")
	if _, ok := got["VM.PowerMgmt"]; !ok || len(got) != 1 {
		t.Fatalf("/vms/10102 ~ %v, want what /vms propagates", got)
	}
	got, _ = acc.Approx(context.Background(), "/vms/10101")
	if _, ok := got["VM.Snapshot"]; !ok || len(got) != 1 {
		t.Fatalf("/vms/10101 ~ %v, want its own entry", got)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %v, want only the listing", f.calls)
	}
}

func TestIsTeamBridge(t *testing.T) {
	net := config.Default().Network // ext{team}, int{team}
	for bridge, want := range map[string]bool{
		"ext01": true, "int99": true,
		"ext1": false, "ext011": false, "extab": false, "vmbr0": false, "xext01": false, "ext01x": false,
	} {
		if got := isTeamBridge(net, bridge); got != want {
			t.Errorf("isTeamBridge(%q) = %v, want %v", bridge, got, want)
		}
	}
}

// A read cut off by its caller (a request the browser abandoned) is not
// Proxmox's answer: the next caller asks again. A refusal is remembered.
func TestAccessDoesNotRememberACancelledRead(t *testing.T) {
	f := &fakePerms{err: context.Canceled}
	acc := NewAccess(f)
	if _, err := acc.Privileges(context.Background(), "/vms/1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first read: %v", err)
	}
	f.err, f.listed = nil, proxmox.Permissions{"/vms/1": {"VM.Audit": false}}
	if p, err := acc.Privileges(context.Background(), "/vms/1"); err != nil || len(p) != 1 {
		t.Fatalf("after a cancelled read: %v, %v; want it asked again", p, err)
	}

	refused := &fakePerms{err: &proxmox.APIError{Status: 403, Message: "forbidden"}}
	acc = NewAccess(refused)
	_, _ = acc.Privileges(context.Background(), "/vms/1")
	_, _ = acc.Privileges(context.Background(), "/vms/1")
	if n := len(refused.calls); n != 1 {
		t.Errorf("a refused listing was asked %d times, want 1", n)
	}
}
