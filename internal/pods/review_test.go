package pods

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// When Proxmox won't list the user's privileges, the access says so once
// and holds nothing, rather than re-asking on every grid cell.
func TestAccessRemembersAFailedListing(t *testing.T) {
	f := &fakePerms{err: &proxmox.APIError{Status: 500, Message: "down"}}
	acc := NewAccess(f)
	for range 10 {
		if p, _ := acc.Approx(context.Background(), "/vms/10101"); len(p) != 0 {
			t.Fatalf("Approx = %v", p)
		}
		if ok, _ := acc.Anywhere(context.Background(), "VM.PowerMgmt"); ok {
			t.Fatal("Anywhere = true")
		}
		if _, err := acc.Privileges(context.Background(), "/vms/10101"); err == nil {
			t.Fatal("Privileges hid the failure")
		}
	}
	if len(f.calls) != 1 {
		t.Fatalf("asked Proxmox %d times: %v", len(f.calls), f.calls)
	}
}

// Rewiring a NIC needs SDN.Use on the vnet it leaves as well as the one it
// joins: a new clone's template NICs are checked on a vnet of a zone
// Battleship knows, and a plain bridge is left to Proxmox, as an existing
// VM's NICs are.
func TestNetworkNeedsSDNUseOnTheBridgeLeft(t *testing.T) {
	cfg := config.Default()
	tpl := TemplateSpec{Name: "teak.x.tpl", VMID: 9021, Exists: true, Interfaces: 1, AsRead: map[string]string{"net0": "virtio,bridge=int07"}}
	item := Item{Team: "01", Name: "team01-teak", VMID: 10121, Template: tpl.Name, Steps: []Step{StepClone, StepNetwork}}
	acc := grants{"/vms/9021": {"VM.Clone"}, "/pool/pool-01": {"VM.Allocate", "VM.Config.Network"},
		"/storage/competitions": {"Datastore.AllocateSpace"}, "/sdn/zones/teams/int01": {"SDN.Use"}}
	plan := &Plan{Kind: KindDeploy, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
	if err := BlockUnpermitted(context.Background(), plan, acc, cfg); err != nil {
		t.Fatal(err)
	}
	if b := plan.Items[0].Blocked; b != "you don't have SDN.Use on /sdn/zones/teams/int07" {
		t.Fatalf("blocked %q", b)
	}
	tpl.AsRead = map[string]string{"net0": "virtio,bridge=vmbr0"}
	plan = &Plan{Kind: KindDeploy, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
	_ = BlockUnpermitted(context.Background(), plan, acc, cfg)
	if b := plan.Items[0].Blocked; b != "" {
		t.Fatalf("blocked %q for vmbr0", b)
	}
}

func TestTemplateSpecReadsNICs(t *testing.T) {
	f := newCluster()
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Templates[0].AsRead; len(got) != 4 || nicBridge(got["net0"]) != "vmbr0" || nicBridge(got["net1"]) != "vmbr1" || got["tags"] == "" || !HasCloudInit(got) {
		t.Fatalf("template as read = %v", got)
	}
	if got := plan.Items[0].AsRead; got != nil { // a clone's steps start from its template's
		t.Fatalf("item as read = %v", got)
	}
}

// A pod VM a deploy keeps starts its steps from its own NICs and tags, not
// its template's, so the network step asks SDN.Use on the team vnet the VM
// leaves.
func TestDeployKeepsAPodVMsOwnNICs(t *testing.T) {
	cfg := config.Default()
	n := NewNaming(cfg.Naming)
	f := podstest.New("cedar")
	nic := func() map[string]string { return map[string]string{"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0"} }
	f.Add(proxmox.VM{VMID: 121, Name: "teak.tango.delta", Node: "cedar", Tags: "dev"}, nic())
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true, Tags: "dev"}, nic())
	vmid := n.CloneVMID("01", 9021)
	f.Add(proxmox.VM{VMID: vmid, Name: n.VMName("01", "teak"), Node: "cedar", Pool: n.Pool("01"), Tags: "set-tango.delta"},
		map[string]string{"net0": "virtio=BC:24:11:00:01:22,bridge=ext02"})
	plan, err := NewPlanner(f, cfg).Deploy(context.Background(), DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	it := plan.Items[0]
	if it.Blocked != "" || slices.Contains(it.Steps, StepClone) || nicBridge(it.AsRead["net0"]) != "ext02" || it.AsRead["tags"] != "set-tango.delta" {
		t.Fatalf("item = %+v", it)
	}
	acc := grants{VMPath(vmid): {"VM.Config.Network", "VM.Config.Disk", "VM.Config.CDROM", "VM.Config.Options", "VM.PowerMgmt"},
		"/sdn/zones/teams/int01": {"SDN.Use"}}
	if err := BlockUnpermitted(context.Background(), plan, acc, cfg); err != nil {
		t.Fatal(err)
	}
	if b := plan.Items[0].Blocked; b != "you don't have SDN.Use on /sdn/zones/teams/ext02" {
		t.Errorf("blocked %q", b)
	}
}

// keptPodCluster is a one-NIC master and template of set tango.delta, and
// a pod VM of the set in each of teams' pods, all on int<team>.
func keptPodCluster(n Naming, tplCfg, podCfg map[string]string, teams ...string) *podstest.Fake {
	f := podstest.New("cedar")
	f.Add(proxmox.VM{VMID: 121, Name: "teak.tango.delta", Node: "cedar", Tags: "dev"}, map[string]string{"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 9021, Name: "teak.tango.delta.tpl", Node: "cedar", Template: true, Tags: "dev"}, tplCfg)
	for _, team := range teams {
		cfg := maps.Clone(podCfg)
		cfg["net0"] = "virtio=BC:24:11:00:01:22,bridge=int" + team
		f.Add(proxmox.VM{VMID: n.CloneVMID(team, 9021), Name: n.VMName(team, "teak"), Node: "cedar", Pool: n.Pool(team), Tags: "set-tango.delta"}, cfg)
	}
	return f
}

// A kept pod VM whose config can't be read blocks only its own item, as
// does one gone since the listing; the other pods still plan.
func TestDeployUnreadablePodVMBlocksOnlyItsItem(t *testing.T) {
	cfg := config.Default()
	n := NewNaming(cfg.Naming)
	f := keptPodCluster(n, map[string]string{"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0"}, map[string]string{}, "01", "02", "03")
	f.FailOn(fmt.Sprintf("config:%d", n.CloneVMID("01", 9021)), &proxmox.APIError{Status: 500, Message: "got timeout"})
	f.FailOn(fmt.Sprintf("config:%d", n.CloneVMID("02", 9021)),
		&proxmox.APIError{Status: 500, Message: fmt.Sprintf("Configuration file 'nodes/cedar/qemu-server/%d.conf' does not exist", n.CloneVMID("02", 9021))})
	plan, err := NewPlanner(f, cfg).Deploy(context.Background(), DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01", "02", "03"}})
	if err != nil {
		t.Fatal(err)
	}
	if b := plan.Items[0].Blocked; !strings.HasPrefix(b, "can't read its config: ") || !strings.Contains(b, "got timeout") {
		t.Errorf("team 01 blocked %q", b)
	}
	if b := plan.Items[1].Blocked; b != "is gone since the listing" {
		t.Errorf("team 02 blocked %q", b)
	}
	if it := plan.Items[2]; it.Blocked != "" || nicBridge(it.AsRead["net0"]) != "int03" {
		t.Errorf("team 03 = %+v", it)
	}
}

// A kept pod VM's network step asks VM.Config.Cloudinit when that VM has a
// cloud-init drive, whatever its template has.
func TestDeployKeptPodVMsOwnCloudInit(t *testing.T) {
	cfg := config.Default()
	n := NewNaming(cfg.Naming)
	ci := func() map[string]string {
		return map[string]string{"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0", "ide2": "local:vm-9021-cloudinit,media=cdrom"}
	}
	for _, tc := range []struct {
		name           string
		tplCfg, podCfg map[string]string
		want           string
	}{
		{"template's drive only", ci(), map[string]string{}, ""},
		{"pod VM's drive only", map[string]string{"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0"}, map[string]string{"ide2": "local:vm-10121-cloudinit,media=cdrom"},
			"you don't have VM.Config.Cloudinit on " + VMPath(n.CloneVMID("01", 9021))},
	} {
		f := keptPodCluster(n, tc.tplCfg, tc.podCfg, "01")
		plan, err := NewPlanner(f, cfg).Deploy(context.Background(), DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01"}})
		if err != nil {
			t.Fatal(err)
		}
		if it := plan.Items[0]; it.Blocked != "" || slices.Contains(it.Steps, StepClone) {
			t.Fatalf("%s: item = %+v", tc.name, it)
		}
		acc := grants{VMPath(n.CloneVMID("01", 9021)): {"VM.Config.Network", "VM.Config.Disk", "VM.Config.CDROM", "VM.Config.Options", "VM.PowerMgmt"}}
		if err := BlockUnpermitted(context.Background(), plan, acc, cfg); err != nil {
			t.Fatal(err)
		}
		if b := plan.Items[0].Blocked; b != tc.want {
			t.Errorf("%s: blocked %q, want %q", tc.name, b, tc.want)
		}
	}
}

// A deploy cancelled while it reads the kept pod VMs fails with the
// cancellation instead of blocking every kept item on it.
func TestDeployCancelledReadingKeptVMs(t *testing.T) {
	cfg := config.Default()
	n := NewNaming(cfg.Naming)
	f := keptPodCluster(n, map[string]string{"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0"}, map[string]string{}, "01")
	ctx, cancel := context.WithCancel(context.Background())
	f.Gate = func(_ context.Context, key string) (func(), error) {
		if key == fmt.Sprintf("config:%d", n.CloneVMID("01", 9021)) {
			cancel()
			return nil, context.Canceled
		}
		return nil, nil
	}
	plan, err := NewPlanner(f, cfg).Deploy(ctx, DeployRequest{Pattern: "*.tango.delta", Teams: []string{"01"}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, plan = %+v; want context.Canceled", err, plan)
	}
}
