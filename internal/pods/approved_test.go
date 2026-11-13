package pods

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// An ApprovedOnly deploy plans only a set whose every master carries
// deploy.approved_tag; a host filter can't leave the unapproved one out.
func TestDeployApprovedOnly(t *testing.T) {
	cluster := func(teakTags, oakTags string) *podstest.Fake {
		f := podstest.New("cedar")
		f.Add(proxmox.VM{VMID: 121, Name: "teak.tango.delta", Node: "cedar", Tags: teakTags}, map[string]string{"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0"})
		f.Add(proxmox.VM{VMID: 125, Name: "oak.tango.delta", Node: "cedar", Tags: oakTags}, map[string]string{"net0": "virtio=BC:24:11:00:01:25,bridge=vmbr0"})
		return f
	}
	for _, tc := range []struct {
		name        string
		teak, oak   string
		approvedTag string
		hosts       []string
		refused     string // "" plans
	}{
		{name: "every master approved", teak: "dev;approved", oak: "approved;dev", approvedTag: "approved"},
		{name: "one master not approved", teak: "dev;approved", oak: "dev", approvedTag: "approved", refused: "oak.tango.delta lacks tag"},
		{name: "the unapproved host filtered out", teak: "dev;approved", oak: "dev", approvedTag: "approved", hosts: []string{"teak"}, refused: "oak.tango.delta lacks tag"},
		{name: "no master approved", teak: "dev", oak: "dev", approvedTag: "approved", refused: "lacks tag"},
		{name: "a tag that only contains it", teak: "dev;approved", oak: "dev;approved-once", approvedTag: "approved", refused: "oak.tango.delta lacks tag"},
		{name: "no approved tag configured", teak: "dev;approved", oak: "dev;approved", approvedTag: "", refused: "deploy.approved_tag is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Deploy.ApprovedTag = tc.approvedTag
			p := NewPlanner(cluster(tc.teak, tc.oak), cfg)
			req := DeployRequest{Pattern: "*.tango.delta", Teams: []string{"51"}, Hosts: tc.hosts, ApprovedOnly: true}
			plan, err := p.Deploy(context.Background(), req)
			if tc.refused == "" {
				if err != nil || len(plan.Items) != 2 {
					t.Fatalf("Deploy = %+v, %v; want both VMs planned", plan, err)
				}
				return
			}
			if !errors.Is(err, ErrNotApproved) || !strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("Deploy = %v, want ErrNotApproved naming %q", err, tc.refused)
			}
			// Without ApprovedOnly the same deploy plans: volunteers' deploys don't need approval.
			req.ApprovedOnly = false
			if _, err := p.Deploy(context.Background(), req); err != nil {
				t.Errorf("a volunteer's deploy of the same set is refused: %v", err)
			}
		})
	}
}
