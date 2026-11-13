package pods

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// registered is a tag policy under which "approved" approves: it is a
// registered tag and tags are case-sensitive.
var registered = proxmox.TagAccess{Registered: []string{"approved"}, CaseSensitive: true}

// approvedCluster has set tango.delta's masters teak and oak, each with a
// built template unless its host is in unbuilt, under the registered policy.
func approvedCluster(teakTags, oakTags string, unbuilt ...string) *podstest.Fake {
	f := podstest.New("cedar")
	f.Tags = registered
	for i, m := range []struct{ host, tags string }{{"teak", teakTags}, {"oak", oakTags}} {
		name := m.host + ".tango.delta"
		f.Add(proxmox.VM{VMID: 121 + i, Name: name, Node: "cedar", Tags: m.tags}, map[string]string{"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0"})
		if !slices.Contains(unbuilt, m.host) {
			f.Add(proxmox.VM{VMID: 9021 + i, Name: name + ".tpl", Node: "cedar", Template: true}, map[string]string{"net0": "virtio=BC:24:11:00:01:21,bridge=vmbr0"})
		}
	}
	return f
}

func approvedDeploy(f *podstest.Fake, cfg config.Config, hosts ...string) (*Plan, error) {
	return NewPlanner(f, cfg).Deploy(context.Background(), DeployRequest{ApprovedSet: "tango.delta", Teams: []string{"51"}, Hosts: hosts})
}

// A deploy of an approved set plans only while every master of the set
// carries deploy.approved_tag, spelled as Proxmox registers it; a host filter
// can't leave the unapproved master out.
func TestDeployApprovedSet(t *testing.T) {
	for _, tc := range []struct {
		name        string
		teak, oak   string
		approvedTag string
		policy      proxmox.TagAccess
		hosts       []string
		refused     string // "" plans
	}{
		{name: "every master approved", teak: "dev;approved", oak: "approved;dev", approvedTag: "approved", policy: registered},
		{name: "one master not approved", teak: "dev;approved", oak: "dev", approvedTag: "approved", policy: registered, refused: "oak.tango.delta lacks tag"},
		{name: "the unapproved host filtered out", teak: "dev;approved", oak: "dev", approvedTag: "approved", policy: registered, hosts: []string{"teak"}, refused: "oak.tango.delta lacks tag"},
		{name: "a tag that only contains it", teak: "dev;approved", oak: "dev;approved-once", approvedTag: "approved", policy: registered, refused: "oak.tango.delta lacks tag"},
		{name: "the tag in another case on a master", teak: "dev;approved", oak: "dev;Approved", approvedTag: "approved", policy: registered, refused: "oak.tango.delta lacks tag"},
		{name: "the tag registered in another case", teak: "dev;approved", oak: "dev;approved", approvedTag: "approved",
			policy: proxmox.TagAccess{Registered: []string{"Approved"}}, refused: "registered tags"},
		{name: "no approved tag configured", teak: "dev;approved", oak: "dev;approved", approvedTag: "", policy: registered, refused: "deploy.approved_tag is empty"},
		{name: "the tag anyone may set", teak: "dev;approved", oak: "dev;approved", approvedTag: "approved", policy: proxmox.TagAccess{}, refused: "registered tags"},
		{name: "the tag only allowed, not registered", teak: "dev;approved", oak: "dev;approved", approvedTag: "approved",
			policy: proxmox.TagAccess{Mode: "list", Allowed: []string{"approved"}}, refused: "registered tags"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Deploy.ApprovedTag = tc.approvedTag
			f := approvedCluster(tc.teak, tc.oak)
			f.Tags = tc.policy
			plan, err := approvedDeploy(f, cfg, tc.hosts...)
			if tc.refused == "" {
				if err != nil || len(plan.Runnable()) != 2 {
					t.Fatalf("Deploy = %+v, %v; want both VMs runnable", plan, err)
				}
				return
			}
			if !errors.Is(err, ErrNotApproved) || !strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("Deploy = %v, want ErrNotApproved naming %q", err, tc.refused)
			}
			// By pattern the same deploy plans: volunteers' deploys need no approval.
			if _, err := NewPlanner(f, cfg).Deploy(context.Background(), DeployRequest{Pattern: "*.tango.delta", Teams: []string{"51"}}); err != nil {
				t.Errorf("a volunteer's deploy of the same set is refused: %v", err)
			}
		})
	}
}

// An approved set is exactly its own masters: another set whose name ends
// with it ("*.delta" matches tango.delta's masters) neither joins the deploy
// nor decides its approval, and a set with no masters is not approved.
func TestDeployApprovedSetIsExact(t *testing.T) {
	f := approvedCluster("dev", "dev") // tango.delta: unapproved
	f.Add(proxmox.VM{VMID: 131, Name: "elm.delta", Node: "cedar", Tags: "dev;approved"}, map[string]string{"net0": "virtio=BC:24:11:00:01:31,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 9031, Name: "elm.delta.tpl", Node: "cedar", Template: true}, map[string]string{"net0": "virtio=BC:24:11:00:01:31,bridge=vmbr0"})
	p := NewPlanner(f, config.Default())
	plan, err := p.Deploy(context.Background(), DeployRequest{ApprovedSet: "delta", Teams: []string{"51"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Name != "team51-elm" || plan.Items[0].Blocked != "" {
		t.Errorf("items = %+v, want team51-elm alone", plan.Items)
	}
	if _, err := p.Deploy(context.Background(), DeployRequest{ApprovedSet: "no.such", Teams: []string{"51"}}); !errors.Is(err, ErrNotApproved) {
		t.Errorf("a set with no masters: %v, want ErrNotApproved", err)
	}
}

// A session deploy clones built templates and builds none, so a master whose
// template is missing blocks its VM rather than being copied as it stands.
func TestDeployApprovedSetBuildsNoTemplate(t *testing.T) {
	plan, err := approvedDeploy(approvedCluster("dev;approved", "dev;approved", "oak"), config.Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plan.Items {
		blocked := it.Blocked != ""
		if want := it.Name == "team51-oak"; blocked != want {
			t.Errorf("%s blocked = %q, want blocked %t", it.Name, it.Blocked, want)
		}
	}
	for _, tpl := range plan.Templates {
		if tpl.Host == "oak" && (!strings.Contains(tpl.Blocked, "builds none") || tpl.WillStopMaster) {
			t.Errorf("oak's template = %+v, want blocked and its master left running", tpl)
		}
	}
}

// ApprovedSets lists a set only when every one of its masters carries the
// registered approval tag.
func TestApprovedSets(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	cfg := config.Default().Deploy
	vms := []proxmox.VM{
		{VMID: 1, Name: "dc.all.set", Tags: "dev;approved"},
		{VMID: 2, Name: "web.all.set", Tags: "approved;dev"},
		{VMID: 3, Name: "mail.all.set", Tags: "approved"}, // no master tag: not a master
		{VMID: 4, Name: "dc.some.set", Tags: "dev;approved"},
		{VMID: 5, Name: "web.some.set", Tags: "dev"},
		{VMID: 6, Name: "dc.none.set", Tags: "dev;approved-soon"},
		{VMID: 7, Name: "dc.twice.set", Tags: "dev;approved"},
		{VMID: 8, Name: "dc.twice.set", Tags: "dev"}, // a second VM of the name, unapproved
		{VMID: 9, Name: "dc.case.set", Tags: "dev;Approved"},
		{VMID: 10, Name: "dc.set", Tags: "dev;approved"}, // "*.set" matches every master above
	}
	sets, err := ApprovedSets(vms, n, cfg, registered)
	if want := []Set{"all.set", "set"}; err != nil || !slices.Equal(sets, want) {
		t.Errorf("approved sets = %v, %v; want %v", sets, err, want)
	}

	for name, policy := range map[string]proxmox.TagAccess{
		"unregistered":       {},
		"allowed only":       {Mode: "list", Allowed: []string{"approved"}},
		"registered as Case": {Registered: []string{"Approved"}},
	} {
		if sets, err := ApprovedSets(vms, n, cfg, policy); !errors.Is(err, ErrNotApproved) || sets != nil {
			t.Errorf("%s tag: sets %v, err %v; want none and ErrNotApproved", name, sets, err)
		}
	}
	cfg.ApprovedTag = ""
	if _, err := ApprovedSets(vms, n, cfg, registered); !errors.Is(err, ErrNotApproved) {
		t.Errorf("no approval tag configured: err %v, want ErrNotApproved", err)
	}
}

// The tag approves only while a user without Sys.Modify can't set a case
// variant of it, which Proxmox would store as the tag.
func TestApprovalTagCaseVariant(t *testing.T) {
	reg := []string{"approved", "1337"}
	for _, tc := range []struct {
		name   string
		tag    string
		policy proxmox.TagAccess
		ok     bool
	}{
		{"free, case-insensitive", "approved", proxmox.TagAccess{Registered: reg}, false},
		{"free named, case-insensitive", "approved", proxmox.TagAccess{Mode: "free", Registered: reg}, false},
		{"free, case-sensitive", "approved", proxmox.TagAccess{Registered: reg, CaseSensitive: true}, true},
		{"list with a variant", "approved", proxmox.TagAccess{Mode: "list", Allowed: []string{"dev", "Approved"}, Registered: reg}, false},
		{"list without a variant", "approved", proxmox.TagAccess{Mode: "list", Allowed: []string{"dev", "approved"}, Registered: reg}, true},
		{"none", "approved", proxmox.TagAccess{Mode: "none", Registered: reg}, true},
		{"existing with a variant", "approved", proxmox.TagAccess{Mode: "existing", Allowed: []string{"Approved"}, Registered: reg}, false},
		{"existing without a variant", "approved", proxmox.TagAccess{Mode: "existing", Allowed: []string{"dev"}, Registered: reg}, true},
		{"list with a registered variant", "approved", proxmox.TagAccess{Mode: "list", Allowed: []string{"Approved"}, Registered: append(slices.Clone(reg), "Approved")}, true},
		{"no letters, free", "1337", proxmox.TagAccess{Registered: reg}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := approvalTag(config.Deploy{ApprovedTag: tc.tag}, tc.policy)
			if tc.ok {
				if err != nil || got != tc.tag {
					t.Fatalf("approvalTag = %q, %v; want %q", got, err, tc.tag)
				}
				return
			}
			if !errors.Is(err, ErrNotApproved) || !strings.Contains(err.Error(), "case variant") {
				t.Fatalf("approvalTag = %q, %v; want ErrNotApproved naming the case variant", got, err)
			}
		})
	}
}

// The digest follows what makes a template the machine it is and ignores
// what changes around it.
func TestConfigDigest(t *testing.T) {
	base := map[string]string{"scsi0": "competitions:base-9001-disk-0,size=32G", "smbios1": "uuid=1", "digest": "aaa", "tags": "dev", "description": "notes"}
	same := map[string]string{"scsi0": "competitions:base-9001-disk-0,size=32G", "smbios1": "uuid=1", "digest": "bbb", "tags": "dev;approved", "lock": "backup"}
	if ConfigDigest(base) != ConfigDigest(same) {
		t.Error("the digest changed with Proxmox's digest, tags, notes or a lock")
	}
	for k, v := range map[string]string{"smbios1": "uuid=2", "scsi0": "competitions:base-9001-disk-0,size=64G", "net1": "virtio=BC:24:11:00:00:02,bridge=vmbr1", "hookscript": "local:snippets/x.sh"} {
		changed := map[string]string{}
		for bk, bv := range base {
			changed[bk] = bv
		}
		changed[k] = v
		if ConfigDigest(changed) == ConfigDigest(base) {
			t.Errorf("the digest ignores a change to %s", k)
		}
	}
	// Keys and values can't run together.
	if ConfigDigest(map[string]string{"a": "b=c"}) == ConfigDigest(map[string]string{"a=b": "c"}) {
		t.Error("two different configs share a digest")
	}
}
