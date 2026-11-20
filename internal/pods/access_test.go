package pods

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// grants is a table Access: path -> privileges; missing paths hold nothing.
type grants map[string][]string

func (g grants) Privileges(_ context.Context, path string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, p := range g[path] {
		out[p] = true
	}
	return out, nil
}

func (grants) TagAccess(context.Context) (proxmox.TagAccess, error) { return proxmox.TagAccess{}, nil }

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

// What a clone into a team's pool, its configuration and a template build
// need.
func TestDeployNeeds(t *testing.T) {
	cfg := config.Default()
	cfg.Images.Pool = "images"
	tpl := TemplateSpec{Name: "teak.x.tpl", Host: "teak", VMID: 9021, MasterVMID: 121, MasterName: "teak.x", Interfaces: 2,
		CloudInit: true, CPU: "custom-lab-baseline,flags=+aes"}
	item := Item{Team: "01", Host: "teak", Name: "team01-teak", VMID: 10121, Template: tpl.Name,
		Steps:  []Step{StepClone, StepNetwork, StepDiskLimits, StepCDROM, StepNotes, StepSnapshot, StepStart},
		AsRead: map[string]string{"net0": "virtio,bridge=vmbr0", "net1": "virtio,bridge=vmbr1"}}
	plan := &Plan{Kind: KindDeploy, Teams: []string{"01"}, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
	everything := grants{
		"/vms/121":                  {"VM.Clone"},
		"/vms/9021":                 {"VM.Clone"},
		"/pool/images":              {"VM.Allocate"},
		"/storage/competitions":     {"Datastore.AllocateSpace"},
		"/mapping/cpu/lab-baseline": {"Mapping.Use"},
		"/pool/pool-01": {"VM.Allocate", "VM.Config.Network", "VM.Config.Cloudinit", "VM.Config.Disk",
			"VM.Config.CDROM", "VM.Config.Options", "VM.Snapshot", "VM.PowerMgmt"},
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

// A build copies the master into the images pool, which, as Proxmox
// checks a clone, needs VM.Allocate on the pool or on the new VMID; a
// rebuild deletes the old template first, which needs it on that VMID.
func TestTemplateBuildNeedsAllocateInImagesPool(t *testing.T) {
	cfg := config.Default()
	cfg.Images.Pool = "images"
	base := grants{"/vms/121": {"VM.Clone"}, "/storage/competitions": {"Datastore.AllocateSpace"}}
	with := func(path string) grants {
		g := grants{path: {"VM.Allocate"}}
		for p, v := range base {
			g[p] = v
		}
		return g
	}
	cases := []struct {
		name    string
		rebuild bool
		acc     grants
		want    string
	}{
		{"pool grant", false, with("/pool/images"), ""},
		{"VMID grant", false, with("/vms/9021"), ""},
		{"neither", false, base, "you don't have VM.Allocate on /pool/images"},
		{"another pool", false, with("/pool/pool-01"), "you don't have VM.Allocate on /pool/images"},
		{"rebuild, pool grant only", true, with("/pool/images"), "you don't have VM.Allocate on /vms/9021"},
		{"rebuild, VMID grant", true, with("/vms/9021"), ""},
	}
	for _, c := range cases {
		tpl := TemplateSpec{Name: "teak.x.tpl", Host: "teak", VMID: 9021, MasterVMID: 121, Interfaces: 1, Exists: c.rebuild, Rebuild: c.rebuild}
		item := Item{Team: "01", Name: "team01-teak", VMID: 10121, Template: tpl.Name, Steps: []Step{StepClone}}
		plan := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
		if err := BlockUnpermitted(context.Background(), plan, c.acc, cfg); err != nil {
			t.Fatal(err)
		}
		if got := plan.Templates[0].Blocked; got != c.want {
			t.Errorf("%s: template blocked %q, want %q", c.name, got, c.want)
		}
		if c.want != "" && plan.Items[0].Blocked != "template teak.x.tpl: "+c.want {
			t.Errorf("%s: item blocked %q", c.name, plan.Items[0].Blocked)
		}
	}
}

// A reused template needs no master privileges, and an existing VM is
// configured at its own path.
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

// Every offered privilege is needed by some item the planner builds, so
// offerPrivileges can't drift from itemNeeds.
func TestOfferPrivilegesAreItemNeeds(t *testing.T) {
	ctx := context.Background()
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial")
	p := testPlanner(f)
	plans := map[Kind]func() (*Plan, error){
		KindDeploy: func() (*Plan, error) {
			return p.Deploy(ctx, DeployRequest{Pattern: "*.tango.delta", Teams: []string{"02"}, Snapshot: true})
		},
		KindTeardown: func() (*Plan, error) { return p.Teardown(ctx, []string{"01"}, nil, "") },
		KindReset:    func() (*Plan, error) { return p.Reset(ctx, []string{"01"}, nil, "initial") },
		KindPower:    func() (*Plan, error) { return p.Power(ctx, []string{"01"}, nil, "stop") },
		KindSnapshot: func() (*Plan, error) {
			return p.Snapshot(ctx, SnapshotRequest{Teams: []string{"01"}, Name: "before-scoring"})
		},
	}
	for kind := range specs {
		if plans[kind] == nil && !kind.Teamless() {
			t.Fatalf("no plan built for %s", kind)
		}
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
			for _, n := range itemNeeds(it, templates[it.Template], p.Cfg, proxmox.TagAccess{}) {
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

// Freeing a gone VM's disks needs Datastore.Allocate on the storage and
// nothing on the VM's path.
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

// fakePerms answers Proxmox's two reads: the listed paths, and one path's
// effective privileges.
type fakePerms struct {
	mu      sync.Mutex
	err     error // Permissions fails with it
	listed  proxmox.Permissions
	at      map[string]map[string]bool
	tags    proxmox.TagAccess
	tagsErr error // TagAccess fails with it
	calls   []string
}

func (f *fakePerms) Permissions(context.Context) (proxmox.Permissions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "all")
	return f.listed, f.err
}

func (f *fakePerms) PermissionsAt(_ context.Context, path string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, path)
	return f.at[path], nil
}

func (f *fakePerms) TagAccess(context.Context) (proxmox.TagAccess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "tags")
	return f.tags, f.tagsErr
}

// When datacenter user-tag-access restricts tags (or registers set-<name>),
// Proxmox refuses StepSetTag unless the tag is in allowed-tags or the user
// holds Sys.Modify on /.
func TestDeploySetTagRespectsUserTagAccess(t *testing.T) {
	cfg := config.Default()
	for _, tc := range []struct {
		name      string
		set       Set
		tags      proxmox.TagAccess
		sysModify bool
		want      string
	}{
		{"free mode", "kilo.alpha", proxmox.TagAccess{Mode: "free"}, false, ""},
		{"unset mode", "kilo.alpha", proxmox.TagAccess{}, false, ""},
		{"list mode, tag allowed", "kilo.alpha", proxmox.TagAccess{Mode: "list", Allowed: []string{"set-kilo.alpha"}}, false, ""},
		{"list mode, tag not allowed", "kilo.alpha", proxmox.TagAccess{Mode: "list", Allowed: []string{"dev"}}, false, "you don't have Sys.Modify on /"},
		{"existing mode, tag not in allowed-tags", "kilo.alpha", proxmox.TagAccess{Mode: "existing", Allowed: []string{"dev"}}, false, "you don't have Sys.Modify on /"},
		{"existing mode, with Sys.Modify on /", "kilo.alpha", proxmox.TagAccess{Mode: "existing", Allowed: []string{"dev"}}, true, ""},
		{"none mode, without Sys.Modify", "kilo.alpha", proxmox.TagAccess{Mode: "none"}, false, "you don't have Sys.Modify on /"},
		{"registered tag in free mode", "kilo.alpha", proxmox.TagAccess{Mode: "free", Registered: []string{"set-kilo.alpha"}}, false, "you don't have Sys.Modify on /"},
		{"registered tag with Sys.Modify", "kilo.alpha", proxmox.TagAccess{Mode: "free", Registered: []string{"set-kilo.alpha"}}, true, ""},
		{"single-label master (no set tag written)", "", proxmox.TagAccess{Mode: "none"}, false, ""},
	} {
		listed := proxmox.Permissions{"/vms/10121": {"VM.Config.Options": false}}
		if tc.sysModify {
			listed["/"] = map[string]bool{"Sys.Modify": false}
		}
		acc := NewAccess(&fakePerms{listed: listed, tags: tc.tags})
		plan := &Plan{Kind: KindDeploy, Items: []Item{{Team: "01", Name: "team01-teak", VMID: 10121, Set: tc.set, Steps: []Step{StepSetTag}}}}
		if err := BlockUnpermitted(context.Background(), plan, acc, cfg); err != nil {
			t.Fatal(err)
		}
		if got := plan.Items[0].Blocked; got != tc.want {
			t.Errorf("%s: blocked = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Listed paths come from one read; others are asked once each, not guessed
// from parents.
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

func TestBridgeZoneKnowsTeamBridges(t *testing.T) {
	cfg := config.Default() // ext{team}, int{team} in zone teams
	for bridge, want := range map[string]string{
		"ext01": "teams", "int99": "teams", "devint03": "devnet",
		"ext1": "", "ext011": "", "extab": "", "vmbr0": "", "xext01": "", "ext01x": "",
	} {
		if got := bridgeZone(cfg, bridge); got != want {
			t.Errorf("bridgeZone(%q) = %q, want %q", bridge, got, want)
		}
	}
}

// A read the caller cancelled isn't remembered, so the next caller asks
// again; a refusal is remembered.
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

func TestEnvItemNeeds(t *testing.T) {
	cfg := envConfig()
	it := Item{Name: "oak.kilo.alpha", VMID: 2101, Pool: "dev-01", Source: &Source{VMID: 8002, Node: "birch"},
		NICs: map[string]string{"net0": "devint01"}, Guest: &Guest{OS: ImageWindows},
		Steps: []Step{StepClone, StepLabel, StepNetwork, StepStart, StepWaitAgent, StepGuest, StepVerify}}
	pool := []string{"VM.Allocate", "VM.Config.Options", "VM.Config.Network", "VM.PowerMgmt", "VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted"}
	full := func(poolPrivs []string) grants {
		return grants{
			"/vms/8002":                      {"VM.Clone"},
			"/pool/dev-01":                   poolPrivs,
			"/storage/" + cfg.Deploy.Storage: {"Datastore.AllocateSpace"},
			"/sdn/zones/" + cfg.Environments.SDNZone + "/devint01": {"SDN.Use"},
		}
	}
	plan := &Plan{Kind: KindEnvAdd, Items: []Item{it}}
	if err := BlockUnpermitted(context.Background(), plan, full(pool), cfg); err != nil || plan.Items[0].Blocked != "" {
		t.Fatalf("full access blocked: %q, %v", plan.Items[0].Blocked, err)
	}
	plan = &Plan{Kind: KindEnvAdd, Items: []Item{it}}
	_ = BlockUnpermitted(context.Background(), plan, full(pool[:5]), cfg)
	if !strings.Contains(plan.Items[0].Blocked, "VM.GuestAgent.Unrestricted") {
		t.Errorf("blocked = %q", plan.Items[0].Blocked)
	}
}

// After an edit and register, a base image sits on the edit bridge, and
// Proxmox checks SDN.Use on a clone source's bridges.
func TestCloneNeedsSDNUseOnSourceBridges(t *testing.T) {
	cfg := envConfig()
	cfg.Images.EditBridge, cfg.Images.EditZone = "devedit", "edits"
	f := envFake()
	f.Add(proxmox.VM{VMID: 8003, Name: "debian-13-v1", Node: "alder", Template: true, Tags: "base;linux"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:80:04,bridge=devedit"})
	plan := create(t, NewPlanner(f, cfg), "teak=debian-13")
	teak := plan.Items[1]
	if nicBridge(teak.Source.AsRead["net0"]) != "devedit" {
		t.Fatalf("source = %+v", teak.Source)
	}
	g := grants{
		"/vms/8003":                      {"VM.Clone"},
		"/pool/dev-01":                   {"VM.Allocate", "VM.Config.Options", "VM.Config.Network", "VM.PowerMgmt", "VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted"},
		"/storage/" + cfg.Deploy.Storage: {"Datastore.AllocateSpace"},
		"/sdn/zones/" + cfg.Environments.SDNZone + "/devint01": {"SDN.Use"},
	}
	p := &Plan{Kind: KindEnvCreate, Items: []Item{teak}}
	if err := BlockUnpermitted(context.Background(), p, g, cfg); err != nil {
		t.Fatal(err)
	}
	if b := p.Items[0].Blocked; !strings.Contains(b, "SDN.Use on /sdn/zones/edits/devedit") {
		t.Errorf("blocked = %q", b)
	}
	g["/sdn/zones/edits/devedit"] = []string{"SDN.Use"}
	p = &Plan{Kind: KindEnvCreate, Items: []Item{teak}}
	if err := BlockUnpermitted(context.Background(), p, g, cfg); err != nil || p.Items[0].Blocked != "" {
		t.Errorf("with SDN.Use on the edit bridge: %q, %v", p.Items[0].Blocked, err)
	}
}

func TestHoldsOfferedSkipsEmptyPool(t *testing.T) {
	// A bogus grant on "/pool/" must not satisfy an empty pool.
	acc := grants{"/pool/": {"VM.PowerMgmt"}}
	if HoldsOffered(context.Background(), acc, KindPower, 10101, "") {
		t.Fatal("HoldsOffered checked /pool/ for an empty pool")
	}
	if !HoldsOffered(context.Background(), grants{"/pool/dev-01": {"VM.Clone"}}, KindEnvCreate, 2100, "dev-01") {
		t.Fatal("HoldsOffered missed the privilege on /pool/dev-01")
	}
}

func TestBlockUnpermittedClearsWillStopMaster(t *testing.T) {
	plan := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{
		{Name: "teak.x.tpl", VMID: 9021, MasterVMID: 121, MasterName: "teak.x", WillStopMaster: true},
	}}
	if err := BlockUnpermitted(context.Background(), plan, grants{}, config.Default()); err != nil {
		t.Fatal(err)
	}
	if plan.Templates[0].Blocked == "" || plan.Templates[0].WillStopMaster {
		t.Fatalf("template = %+v, want Blocked set and WillStopMaster cleared", plan.Templates[0])
	}
}

// Callers asking for one unlisted path at once share a read: the later one
// waits for it and gets its answer.
func TestAccessConcurrentCallersShareARead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &gatedPerms{fakePerms: fakePerms{listed: proxmox.Permissions{}, at: map[string]map[string]bool{"/vms/9021": {"VM.Clone": false}}},
			release: make(chan struct{})}
		acc := NewAccess(f)
		got := make([]map[string]bool, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() { got[i], errs[i] = acc.Privileges(context.Background(), "/vms/9021") })
			synctest.Wait() // the first is reading; the second waits for it
		}
		close(f.release)
		wg.Wait()
		for i := range 2 {
			if _, ok := got[i]["VM.Clone"]; !ok || errs[i] != nil {
				t.Errorf("caller %d: %v, %v", i, got[i], errs[i])
			}
		}
		if strings.Join(f.calls, ",") != "all,/vms/9021" {
			t.Errorf("calls = %v, want one read of the path", f.calls)
		}
	})
}

// gatedPerms is fakePerms whose path reads wait for release.
type gatedPerms struct {
	fakePerms
	release chan struct{}
}

func (g *gatedPerms) PermissionsAt(ctx context.Context, path string) (map[string]bool, error) {
	<-g.release
	return g.fakePerms.PermissionsAt(ctx, path)
}

// Tag access is read once; a refusal is remembered, while a read the
// caller cancelled or timed out isn't.
func TestAccessTagAccessIsReadOnce(t *testing.T) {
	ctx := context.Background()
	f := &fakePerms{tags: proxmox.TagAccess{Mode: "list"}}
	acc := NewAccess(f)
	for range 2 {
		if ta, err := acc.TagAccess(ctx); err != nil || ta.Mode != "list" {
			t.Fatalf("tag access = %+v, %v", ta, err)
		}
	}
	if len(f.calls) != 1 {
		t.Errorf("read %d times, want once", len(f.calls))
	}

	refused := &fakePerms{tagsErr: &proxmox.APIError{Status: 403, Message: "forbidden"}}
	acc = NewAccess(refused)
	for range 2 {
		if _, err := acc.TagAccess(ctx); err == nil {
			t.Fatal("a refused read succeeded")
		}
	}
	if len(refused.calls) != 1 {
		t.Errorf("a refused read was asked %d times, want once", len(refused.calls))
	}

	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		f := &fakePerms{tagsErr: cause}
		acc := NewAccess(f)
		if _, err := acc.TagAccess(ctx); !errors.Is(err, cause) {
			t.Fatalf("first read: %v", err)
		}
		f.tagsErr, f.tags = nil, proxmox.TagAccess{Mode: "free"}
		if ta, err := acc.TagAccess(ctx); err != nil || ta.Mode != "free" {
			t.Errorf("after %v: %+v, %v; want it asked again", cause, ta, err)
		}
	}
}

// Only a custom CPU model needs Mapping.Use on its mapping.
func TestCPUNeedsMappingOnlyForCustomModels(t *testing.T) {
	for cpu, want := range map[string]string{
		"host":                 "",
		"x86-64-v2-AES,flags=": "",
		"custom-lab":           "you don't have Mapping.Use on /mapping/cpu/lab",
	} {
		plan := &Plan{Kind: KindDeploy, Items: []Item{{Team: "01", Name: "team01-teak", VMID: 10121, CPU: cpu, Steps: []Step{StepCPU}}}}
		if err := BlockUnpermitted(context.Background(), plan, grants{"/vms/10121": {"VM.Config.CPU"}}, config.Default()); err != nil {
			t.Fatal(err)
		}
		if got := plan.Items[0].Blocked; got != want {
			t.Errorf("cpu %q: blocked %q, want %q", cpu, got, want)
		}
	}
}

// A clone source's NICs need SDN.Use on a team vnet in the teams zone; a
// plain bridge, whose zone Battleship doesn't know, is left to Proxmox.
func TestCloneNeedsSDNUseOnlyOnKnownZoneBridges(t *testing.T) {
	cfg := config.Default()
	vnet := "/sdn/zones/teams/ext01"
	g := grants{
		"/vms/8002":                      {"VM.Clone"},
		"/pool/pool-01":                  {"VM.Allocate"},
		"/storage/" + cfg.Deploy.Storage: {"Datastore.AllocateSpace"},
		vnet:                             {"SDN.Use"},
	}
	plan := func() *Plan {
		return &Plan{Kind: KindDeploy, Items: []Item{{Team: "01", Name: "team01-teak", VMID: 10121,
			Source: &Source{VMID: 8002, AsRead: map[string]string{"net0": "virtio,bridge=ext01", "net1": "virtio,bridge=vmbr0"}}, Steps: []Step{StepClone}}}}
	}
	p := plan()
	if err := BlockUnpermitted(context.Background(), p, g, cfg); err != nil || p.Items[0].Blocked != "" {
		t.Errorf("with SDN.Use on ext01 alone: blocked %q, %v", p.Items[0].Blocked, err)
	}
	delete(g, vnet)
	p = plan()
	if err := BlockUnpermitted(context.Background(), p, g, cfg); err != nil || p.Items[0].Blocked != "you don't have SDN.Use on "+vnet {
		t.Errorf("without SDN.Use on ext01: blocked %q, %v", p.Items[0].Blocked, err)
	}
}

// A clone asks SDN.Use on every NIC of its template whose zone Battleship
// knows, as Proxmox does: net2 as well as net0, and a VLAN-tagged NIC on its
// tag; a plain bridge is left to Proxmox.
func TestCloneNeedsSDNUseOnEveryTemplateNIC(t *testing.T) {
	ctx := context.Background()
	cfg := envConfig()
	f := podstest.New("cedar")
	nics := map[string]string{
		"net0": "virtio=BC:24:11:00:01:21,bridge=ext01",
		"net2": "virtio=BC:24:11:00:01:22,bridge=devint01",
		"net3": "e1000=BC:24:11:00:01:23,bridge=devint02,tag=20",
		"net4": "e1000=BC:24:11:00:01:24,bridge=vmbr0",
	}
	f.Add(proxmox.VM{VMID: 121, Name: "teak.tango.delta", Node: "cedar", Tags: "dev"}, nics)
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true}, nics)
	plan, err := NewPlanner(f, cfg).Deploy(ctx, DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01"}})
	if err != nil || len(plan.Templates) != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	zone := "/sdn/zones/" + cfg.Environments.SDNZone
	vnets := []string{"/sdn/zones/" + cfg.Proxmox.SDNZone + "/ext01", zone + "/devint01", zone + "/devint02/20"}
	g := grants{
		"/vms/9021":                      {"VM.Clone"},
		"/pool/pool-01":                  {"VM.Allocate"},
		"/storage/" + cfg.Deploy.Storage: {"Datastore.AllocateSpace"},
	}
	for _, v := range vnets {
		g[v] = []string{"SDN.Use"}
	}
	if why, err := Uncloneable(ctx, g, cfg, plan.Templates[0], "01"); err != nil || why != "" {
		t.Fatalf("with SDN.Use on every known-zone NIC: %q, %v", why, err)
	}
	for _, v := range vnets {
		delete(g, v)
		if why, err := Uncloneable(ctx, g, cfg, plan.Templates[0], "01"); err != nil || why != "you don't have SDN.Use on "+v {
			t.Errorf("without SDN.Use on %s: %q, %v", v, why, err)
		}
		g[v] = []string{"SDN.Use"}
	}
}

// sdnUse follows check_vnet_access for a tag and for a plain vnet, and
// leaves trunk VLANs to Proxmox: a NIC asks at most one path, however many
// VLANs its trunks hold.
func TestSDNUseAsksAtMostOnePathPerNIC(t *testing.T) {
	cfg := envConfig()
	z := "/sdn/zones/" + cfg.Environments.SDNZone + "/devint01"
	for nic, want := range map[string][]string{
		"virtio,bridge=devint01":                       {z},
		"virtio,bridge=devint01,tag=5":                 {z + "/5"},
		"virtio,bridge=devint01,trunks=1-4094":         nil,
		"virtio,bridge=devint01,trunks=10-5":           nil,
		"virtio,bridge=devint01,tag=5,trunks=1-4094":   {z + "/5"},
		"virtio,bridge=vmbr0,tag=5,trunks=7":           nil,
		"virtio,bridge=other-zone-vnet,trunks=100-101": nil,
	} {
		var got []string
		for _, n := range sdnUse(cfg, nic) {
			if len(n) != 1 || n[0].priv != "SDN.Use" {
				t.Fatalf("%s: need %v", nic, n)
			}
			got = append(got, n[0].path)
		}
		if !slices.Equal(got, want) {
			t.Errorf("sdnUse(%q) = %v, want %v", nic, got, want)
		}
	}
}

// Building a template clones its master, so it asks SDN.Use on the master's
// bridges as a pod clone does on the template's.
func TestTemplateBuildNeedsSDNUseOnMasterBridges(t *testing.T) {
	cfg := envConfig()
	vnet := "/sdn/zones/" + cfg.Environments.SDNZone + "/devint01"
	plan := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{{Name: "teak.x.tpl", VMID: 9021, MasterVMID: 121, Interfaces: 1,
		AsRead: map[string]string{"net0": "virtio,bridge=devint01"}}}}
	g := grants{
		"/vms/121":                       {"VM.Clone"},
		"/pool/" + cfg.Images.Pool:       {"VM.Allocate"},
		"/storage/" + cfg.Deploy.Storage: {"Datastore.AllocateSpace"},
	}
	if err := BlockUnpermitted(context.Background(), plan, g, cfg); err != nil || plan.Templates[0].Blocked != "you don't have SDN.Use on "+vnet {
		t.Errorf("without SDN.Use on devint01: blocked %q, %v", plan.Templates[0].Blocked, err)
	}
}

// An environment host's NICs need SDN.Use only on known-zone vnets, on the
// values the network step writes (the tag dropped) and those they replace;
// trunk VLANs are left to Proxmox.
func TestNICsNeedSDNUseOnlyOnKnownZoneVnets(t *testing.T) {
	cfg := envConfig()
	vnet := "/sdn/zones/" + cfg.Environments.SDNZone + "/devint01"
	g := grants{"/vms/2101": {"VM.Config.Network"}, vnet: {"SDN.Use"}}
	was := map[string]string{"net0": "virtio=BC:24:11:00:21:01,bridge=vmbr5,tag=9", "net1": "virtio=BC:24:11:00:21:02,bridge=vmbr5"}
	plan := func() *Plan {
		return &Plan{Kind: KindEnvAdd, Items: []Item{{Name: "oak.kilo.alpha", VMID: 2101,
			NICs: map[string]string{"net0": "devint01", "net1": "vmbr0"}, AsRead: was, Steps: []Step{StepNetwork}}}}
	}
	p := plan()
	if err := BlockUnpermitted(context.Background(), p, g, cfg); err != nil || p.Items[0].Blocked != "" {
		t.Errorf("with SDN.Use on devint01: blocked %q, %v", p.Items[0].Blocked, err)
	}
	delete(g, vnet)
	p = plan()
	if err := BlockUnpermitted(context.Background(), p, g, cfg); err != nil || p.Items[0].Blocked != "you don't have SDN.Use on "+vnet {
		t.Errorf("without SDN.Use on devint01: blocked %q, %v", p.Items[0].Blocked, err)
	}
	was["net0"] += ",trunks=30"
	p = plan()
	if err := BlockUnpermitted(context.Background(), p, g, cfg); err != nil || p.Items[0].Blocked != "" {
		t.Errorf("trunked, without SDN.Use on devint01: blocked %q, %v", p.Items[0].Blocked, err)
	}
}

// Reconfiguring an existing VM's network doesn't touch its template's
// bridges, so it needs no SDN.Use there.
func TestExistingVMNeedsNoSDNUseOnTemplateBridges(t *testing.T) {
	tpl := TemplateSpec{Name: "teak.x.tpl", VMID: 9021, MasterVMID: 121, Exists: true, Interfaces: 1,
		AsRead: map[string]string{"net0": "virtio,bridge=ext01"}}
	plan := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{tpl},
		Items: []Item{{Team: "01", Name: "team01-teak", VMID: 10121, Template: tpl.Name, Steps: []Step{StepNetwork},
			AsRead: map[string]string{"net0": "virtio,bridge=vmbr0"}}}}
	acc := grants{"/vms/10121": {"VM.Config.Network"}, "/sdn/zones/teams/int01": {"SDN.Use"}}
	if err := BlockUnpermitted(context.Background(), plan, acc, config.Default()); err != nil || plan.Items[0].Blocked != "" {
		t.Errorf("blocked %q, %v", plan.Items[0].Blocked, err)
	}
	delete(acc, "/sdn/zones/teams/int01")
	plan.Items[0].Blocked = ""
	if err := BlockUnpermitted(context.Background(), plan, acc, config.Default()); err != nil ||
		plan.Items[0].Blocked != "you don't have SDN.Use on /sdn/zones/teams/int01" {
		t.Errorf("without SDN.Use on int01: blocked %q, %v", plan.Items[0].Blocked, err)
	}
}

// The team network step keeps a NIC's trunks and drops its tag; the trunk
// VLANs on the team vnet are left to Proxmox.
func TestTeamNetworkLeavesTrunkVLANsToProxmox(t *testing.T) {
	tpl := TemplateSpec{Name: "teak.x.tpl", VMID: 9021, MasterVMID: 121, Exists: true, Interfaces: 1}
	plan := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{tpl},
		Items: []Item{{Team: "01", Name: "team01-teak", VMID: 10121, Template: tpl.Name, Steps: []Step{StepNetwork},
			AsRead: map[string]string{"net0": "virtio,bridge=vmbr0,tag=4,trunks=40;41"}}}}
	acc := grants{"/vms/10121": {"VM.Config.Network"}}
	if err := BlockUnpermitted(context.Background(), plan, acc, config.Default()); err != nil || plan.Items[0].Blocked != "" {
		t.Errorf("blocked %q, %v", plan.Items[0].Blocked, err)
	}
}

// The guest step's reboot of a Windows host needs VM.PowerMgmt; a Linux
// host's doesn't reboot.
func TestGuestStepPowerNeedsWindows(t *testing.T) {
	for os, want := range map[string]string{ImageWindows: "you don't have VM.PowerMgmt on /vms/2101", ImageLinux: ""} {
		plan := &Plan{Kind: KindEnvAdd, Items: []Item{{Name: "oak.kilo.alpha", VMID: 2101, Guest: &Guest{OS: os}, Steps: []Step{StepGuest}}}}
		if err := BlockUnpermitted(context.Background(), plan, grants{"/vms/2101": {"VM.GuestAgent.Unrestricted"}}, envConfig()); err != nil {
			t.Fatal(err)
		}
		if got := plan.Items[0].Blocked; got != want {
			t.Errorf("%s: blocked %q, want %q", os, got, want)
		}
	}
}

// Every template and item is checked, whatever is before it: an item
// blocked already, one blocked with its template, or a clone without one.
// Unpermitted marks just the items blocked for privileges here.
func TestBlockUnpermittedChecksEveryTemplateAndItem(t *testing.T) {
	plan := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{
		{Name: "elm.x.tpl", VMID: 9023, MasterVMID: 123, Blocked: "no master"},
		{Name: "teak.x.tpl", VMID: 9021, MasterVMID: 121, Interfaces: 1},
	}, Items: []Item{
		{Team: "01", Name: "team01-dc", VMID: 10101, Steps: []Step{StepPower}, Blocked: "no snapshot"},
		{Team: "01", Name: "team01-elm", VMID: 10123, Template: "elm.x.tpl", Steps: []Step{StepClone}},
		{Team: "01", Name: "team01-ash", VMID: 10124, Template: "ash.x.tpl", Steps: []Step{StepClone}},
		{Team: "01", Name: "team01-web", VMID: 10102, Steps: []Step{StepPower}},
		{Team: "01", Name: "team01-db", VMID: 10103, Steps: []Step{StepPower}},
	}}
	if err := BlockUnpermitted(context.Background(), plan, grants{"/vms/10103": {"VM.PowerMgmt"}}, config.Default()); err != nil {
		t.Fatal(err)
	}
	if got := plan.Templates[1].Blocked; !strings.HasPrefix(got, "you don't have VM.Clone on /vms/121") {
		t.Errorf("teak.x.tpl blocked %q", got)
	}
	want := []struct {
		blocked     string
		unpermitted bool
	}{
		{"no snapshot", false},
		{"template elm.x.tpl: no master", false},
		{"", false},
		{"you don't have VM.PowerMgmt on /vms/10102", true},
		{"", false},
	}
	for i, it := range plan.Items {
		if it.Blocked != want[i].blocked || it.Unpermitted != want[i].unpermitted {
			t.Errorf("%s: blocked %q, unpermitted %v; want %q, %v", it.Name, it.Blocked, it.Unpermitted, want[i].blocked, want[i].unpermitted)
		}
	}
}

// failingAt is grants whose reads of path fail.
type failingAt struct {
	grants
	path string
}

func (f failingAt) Privileges(ctx context.Context, path string) (map[string]bool, error) {
	if path == f.path {
		return nil, errors.New("proxmox: 500")
	}
	return f.grants.Privileges(ctx, path)
}

// A privilege read that fails fails the check, but a need held on its pool
// isn't then read on the VM's path.
func TestBlockUnpermittedReadFailures(t *testing.T) {
	cfg := config.Default()
	if err := BlockUnpermitted(context.Background(), powerPlan(), failingAt{grants{}, "/vms/10102"}, cfg); err == nil {
		t.Error("a failed read was ignored")
	}
	acc := failingAt{grants{
		"/vms/8002":                      {"VM.Clone"},
		"/pool/pool-01":                  {"VM.Allocate"},
		"/storage/" + cfg.Deploy.Storage: {"Datastore.AllocateSpace"},
	}, "/vms/10121"}
	plan := &Plan{Kind: KindDeploy, Items: []Item{{Team: "01", Name: "team01-teak", VMID: 10121, Source: &Source{VMID: 8002}, Steps: []Step{StepClone}}}}
	if err := BlockUnpermitted(context.Background(), plan, acc, cfg); err != nil || plan.Items[0].Blocked != "" {
		t.Errorf("held on the pool: blocked %q, %v", plan.Items[0].Blocked, err)
	}
}

// noTagAccess is grants that can't read tag access.
type noTagAccess struct{ grants }

func (noTagAccess) TagAccess(context.Context) (proxmox.TagAccess, error) {
	return proxmox.TagAccess{}, errors.New("proxmox: 500")
}

// Tag access is read only for a set tag an unblocked item writes.
func TestBlockUnpermittedReadsTagAccessOnlyForSetTags(t *testing.T) {
	plan := &Plan{Kind: KindDeploy, Items: []Item{
		{Team: "01", Name: "team01-teak", VMID: 10121, Set: "kilo.alpha", Steps: []Step{StepSetTag}, Blocked: "no template"},
		{Team: "01", Name: "team01-dc", VMID: 10101, Steps: []Step{StepSetTag}},
	}}
	if err := BlockUnpermitted(context.Background(), plan, noTagAccess{grants{"/vms/10101": {"VM.Config.Options"}}}, config.Default()); err != nil {
		t.Fatalf("tag access read: %v", err)
	}
	if b := plan.Items[1].Blocked; b != "" {
		t.Errorf("blocked %q", b)
	}
}

// Retiring the version a register supersedes edits that version's tags;
// a first version retires nothing.
func TestRetireNeedsOptionsOnTheSupersededVersion(t *testing.T) {
	plan := &Plan{Kind: KindImageRegister, Items: []Item{
		{Name: "debian-12-v2", VMID: 8002, Supersedes: &Source{VMID: 8001}, Steps: []Step{StepRetire}},
		{Name: "debian-13-v1", VMID: 8003, Steps: []Step{StepRetire}},
	}}
	if err := BlockUnpermitted(context.Background(), plan, grants{}, envConfig()); err != nil {
		t.Fatal(err)
	}
	if got := blocked(plan); len(got) != 1 || got["debian-12-v2"] != "you don't have VM.Config.Options on /vms/8001" {
		t.Errorf("blocked = %v", got)
	}
}

// Retiring a version adds the deprecated tag, so where user-tag-access
// refuses that tag Proxmox asks Sys.Modify on / as well.
func TestRetireNeedsSysModifyWhereDeprecatedIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags proxmox.TagAccess
		want bool
	}{
		{"free", proxmox.TagAccess{Mode: "free"}, false},
		{"listed", proxmox.TagAccess{Mode: "list", Allowed: []string{DeprecatedTag}}, false},
		{"not listed", proxmox.TagAccess{Mode: "list", Allowed: []string{"base"}}, true},
	} {
		acc := NewAccess(&fakePerms{listed: proxmox.Permissions{"/vms/8001": {"VM.Config.Options": false}}, tags: tc.tags})
		plan := &Plan{Kind: KindImageRegister, Items: []Item{{Name: "debian-12-v2", VMID: 8002, Supersedes: &Source{VMID: 8001}, Steps: []Step{StepRetire}}}}
		if err := BlockUnpermitted(context.Background(), plan, acc, envConfig()); err != nil {
			t.Fatal(err)
		}
		if got := plan.Items[0].Blocked; (got == "you don't have Sys.Modify on /") != tc.want || (!tc.want && got != "") {
			t.Errorf("%s: blocked %q, want Sys.Modify asked: %t", tc.name, got, tc.want)
		}
	}
}

// Registering an approved master copies its tags, registered approval tag
// included, and the label step replaces them, so it asks Sys.Modify on /.
func TestImageRegisterFromApprovedMasterNeedsSysModify(t *testing.T) {
	f := imageFake()
	f.Add(proxmox.VM{VMID: 152, Name: "teak.tango.delta", Node: "birch", Tags: "dev;approved"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:01:52,bridge=vmbr0"})
	cfg := envConfig()
	plan, err := NewPlanner(f, cfg).ImageRegister(context.Background(), ImageRequest{VMID: 152, Family: "teak", ImageKind: ImageLinux})
	if err != nil || plan.Items[0].Blocked != "" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if got := labelEdit(plan.Items[0], cfg); !slices.Equal(got, []string{"approved", "base", "dev", "linux"}) {
		t.Errorf("tags changed = %v", got)
	}
	for _, sysModify := range []bool{false, true} {
		listed := proxmox.Permissions{}
		if sysModify {
			listed["/"] = map[string]bool{"Sys.Modify": false}
		}
		acc := NewAccess(&fakePerms{listed: listed, tags: proxmox.TagAccess{Registered: []string{"approved"}, CaseSensitive: true}})
		p := &Plan{Kind: plan.Kind, Items: slices.Clone(plan.Items)}
		if err := BlockUnpermitted(context.Background(), p, acc, cfg); err != nil {
			t.Fatal(err)
		}
		if asked := strings.Contains(p.Items[0].Blocked, "Sys.Modify on /"); asked == sysModify {
			t.Errorf("with Sys.Modify %t: blocked %q", sysModify, p.Items[0].Blocked)
		}
	}
}

// The tag-set step changes only set tags: none on a pod VM that already
// records its set, so nothing beyond VM.Config.Options is asked; another
// set's tag it removes as well as the one it adds.
func TestSetTagChangesAreTheTagsAsRead(t *testing.T) {
	cfg := config.Default()
	n := NewNaming(cfg.Naming)
	for _, tc := range []struct {
		vmTags string
		want   []string
	}{
		{"dev;set-tango.delta", nil},
		{"dev;set-other.set", []string{"set-other.set", "set-tango.delta"}},
		{"dev", []string{"set-tango.delta"}},
	} {
		f := podstest.New("cedar")
		nic := func() map[string]string { return map[string]string{"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0"} }
		f.Add(proxmox.VM{VMID: 121, Name: "teak.tango.delta", Node: "cedar", Tags: "dev"}, nic())
		f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true, Tags: "dev"}, nic())
		f.Add(proxmox.VM{VMID: n.CloneVMID("01", 9021), Name: n.VMName("01", "teak"), Node: "cedar", Pool: n.Pool("01"), Tags: tc.vmTags}, nic())
		plan, err := NewPlanner(f, cfg).Deploy(context.Background(), DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01"}})
		if err != nil {
			t.Fatal(err)
		}
		it := plan.Items[0]
		got := labelEdit(it, cfg)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%q: tags changed = %v, want %v", tc.vmTags, got, tc.want)
		}
		if asked := tagNeeds(got, proxmox.TagAccess{Mode: "none"}, cfg) != nil; asked != (tc.want != nil) {
			t.Errorf("%q: Sys.Modify asked %t", tc.vmTags, asked)
		}
	}
}

// Tag edits are compared as typed, as Proxmox's assert_tag_permissions
// counts them: case matters, order doesn't, a repeat does.
func TestTagChangesComparesAsTyped(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want []string
	}{
		{"dev;Approved", "dev", []string{"Approved"}},
		{"Dev", "dev", []string{"Dev", "dev"}},
		{"a;b", "b, a", nil},
		{"a;a", "a", []string{"a"}},
		{"", "", nil},
	} {
		if got := tagChanges(tc.a, tc.b); !slices.Equal(got, tc.want) {
			t.Errorf("tagChanges(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if same := sameTags(tc.a, tc.b); same != (tc.want == nil) {
			t.Errorf("sameTags(%q, %q) = %t", tc.a, tc.b, same)
		}
	}
	// A mixed-case registered tag's removal is refused without Sys.Modify.
	if tagNeeds(tagChanges("dev;Keep", "dev"), proxmox.TagAccess{Registered: []string{"Keep"}, CaseSensitive: true}, config.Config{}) == nil {
		t.Error("removing registered tag Keep asks nothing")
	}
}

// labelEdit is the tags an item's label or tag-set step adds or removes
// (stepEdits).
func labelEdit(it Item, cfg config.Config) []string {
	for i, e := range stepEdits(it, nil, cfg) {
		if st := it.Steps[i]; st == StepLabel || st == StepSetTag {
			return e.tags
		}
	}
	return nil
}

// deploy.approved_tag counts as registered only while the reader is shown
// no registered tags; a cluster that lists its registered tags without it
// lets the tag change without Sys.Modify.
func TestApprovedTagCountsRegisteredOnlyWhenNoneShown(t *testing.T) {
	cfg := config.Default()
	cfg.Deploy.ApprovedTag = "approved"
	if tagNeeds([]string{"approved"}, proxmox.TagAccess{Mode: "free", Registered: []string{"other"}}, cfg) != nil {
		t.Error("approved, not among the registered tags shown, asks Sys.Modify")
	}
	if tagNeeds([]string{"approved"}, proxmox.TagAccess{Mode: "free"}, cfg) == nil {
		t.Error("approved, with no registered tags shown, asks nothing")
	}
}

// A resumed create re-spells master_tag as the VM stores it, lower-cased,
// so its label step changes no tag and asks no Sys.Modify.
func TestResumedEnvCreateKeepsTheVMsTagSpelling(t *testing.T) {
	f := envFake()
	f.Add(proxmox.VM{VMID: 2200, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-02", Tags: "dev;kilo.alpha;router"},
		map[string]string{"description": "battleship: base=gw-v1 dns=router"})
	cfg := envConfig()
	cfg.Deploy.MasterTag = "Dev"
	plan, err := NewPlanner(f, cfg).EnvCreate(context.Background(), EnvRequest{Domain: "kilo.alpha", Slot: 2, Router: HostSpec{Host: "gw", Family: "gw"}})
	if err != nil {
		t.Fatal(err)
	}
	gw := plan.Items[0]
	if gw.Blocked != "" || slices.Contains(gw.Steps, StepClone) || gw.Tags != "dev;kilo.alpha;router" || labelEdit(gw, cfg) != nil {
		t.Fatalf("router = %+v, tags changed %v", gw, labelEdit(gw, cfg))
	}
	acc := NewAccess(&fakePerms{listed: proxmox.Permissions{"/vms/2200": {"VM.Config.Options": false, "VM.Config.Network": false, "VM.PowerMgmt": false}},
		tags: proxmox.TagAccess{Mode: "none"}})
	if err := BlockUnpermitted(context.Background(), plan, acc, cfg); err != nil {
		t.Fatal(err)
	}
	if got := plan.Items[0].Blocked; got != "" {
		t.Errorf("blocked = %q", got)
	}
}
