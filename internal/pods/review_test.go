package pods

import (
	"context"
	"maps"
	"slices"
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
	item := Item{Team: "01", Name: "team01-teak", VMID: 10121, Template: tpl.Name, Steps: []Step{StepClone, StepNetwork}, AsRead: tpl.AsRead}
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
	item.AsRead = tpl.AsRead
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
	if got := plan.Templates[0].AsRead; len(got) != 3 || nicBridge(got["net0"]) != "vmbr0" || nicBridge(got["net1"]) != "vmbr1" || got["tags"] == "" {
		t.Fatalf("template as read = %v", got)
	}
	if got := plan.Items[0].AsRead; !maps.Equal(got, plan.Templates[0].AsRead) {
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
