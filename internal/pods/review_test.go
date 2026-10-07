package pods

import (
	"context"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// Item 17: when Proxmox won't list the user's privileges, the access says
// so once and is taken as holding nothing, rather than asking again for
// every cell of every grid render.
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

// Item 21: rewiring a NIC needs SDN.Use on the vnet it leaves as well as
// the one it joins. A new clone's NICs start on its template's bridges;
// when one is a team vnet of the configured zone, the preview checks it.
// Other bridges (and an existing VM's, unknown at plan time) are left to
// Proxmox.
func TestNetworkNeedsSDNUseOnTheBridgeLeft(t *testing.T) {
	cfg := config.Default()
	tpl := TemplateSpec{Name: "teak.x.tpl", VMID: 9021, Exists: true, Interfaces: 1, Bridges: []string{"int07"}}
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
	// A bridge outside the zone's team vnets isn't checked here.
	tpl.Bridges = []string{"vmbr0"}
	plan = &Plan{Kind: KindDeploy, Templates: []TemplateSpec{tpl}, Items: []Item{item}}
	_ = BlockUnpermitted(context.Background(), plan, acc, cfg)
	if b := plan.Items[0].Blocked; b != "" {
		t.Fatalf("blocked %q for vmbr0", b)
	}
}

func TestTemplateSpecReadsBridges(t *testing.T) {
	f := newCluster()
	plan, err := testPlanner(f).Deploy(context.Background(), DeployRequest{Pattern: "teak.*", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Templates[0].Bridges; len(got) != 2 || got[0] != "vmbr0" || got[1] != "vmbr1" {
		t.Fatalf("bridges = %v", got)
	}
}
