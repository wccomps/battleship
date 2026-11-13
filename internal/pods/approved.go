package pods

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// ErrNotApproved: a set is not approved for self-service sessions.
var ErrNotApproved = errors.New("not approved for self-service sessions")

// approvalTag is deploy.approved_tag, or ErrNotApproved when it can approve
// nothing: it is empty, or not one of Proxmox's registered tags, which only
// Sys.Modify on / may put on a VM. Any other tag could be added by whoever
// may edit a master's options.
func approvalTag(cfg config.Deploy, tags proxmox.TagAccess) (string, error) {
	tag := strings.TrimSpace(cfg.ApprovedTag)
	switch {
	case tag == "":
		return "", fmt.Errorf("every set is %w: deploy.approved_tag is empty", ErrNotApproved)
	case !tags.Registers(tag):
		return "", fmt.Errorf("every set is %w: deploy.approved_tag %q is not one of Proxmox's registered tags (Datacenter, Options), "+
			"so anyone who may edit a master's options could add it", ErrNotApproved, tag)
	}
	return tag, nil
}

// requireApproved is ErrNotApproved unless every master carries tag.
func requireApproved(set Set, masters []proxmox.VM, tag string) error {
	for _, m := range masters {
		if !hasTag(m.Tags, tag) {
			return fmt.Errorf("%s is %w: %s lacks tag %q", set, ErrNotApproved, m.Name, tag)
		}
	}
	return nil
}

// mastersBySet groups the masters tagged tag by their set (SplitMaster), each
// group sorted by name. A master with no host or no set is in none.
func mastersBySet(vms []proxmox.VM, n Naming, tag string) map[Set][]proxmox.VM {
	masters, _, _ := FindMasters(vms, n, "*", tag)
	out := map[Set][]proxmox.VM{}
	for _, m := range masters {
		if host, set := n.SplitMaster(m.Name); host != "" && set != "" {
			out[set] = append(out[set], m)
		}
	}
	return out
}

// ApprovedSet is a set approved for self-service sessions.
type ApprovedSet struct {
	Name    Set
	Members []SetMember // one per master name, sorted by it
}

// SetMember is one master of a set and the template a deploy would clone for
// it (usableTemplate), or nil when a deploy would have to build one or is
// blocked.
type SetMember struct {
	Host     string
	Master   proxmox.VM
	Template *proxmox.VM
}

// ApprovedSets is the sets whose masters all carry the approval tag, sorted
// by name. It is ErrNotApproved when the tag can approve nothing
// (approvalTag).
func ApprovedSets(vms []proxmox.VM, n Naming, cfg config.Deploy, tags proxmox.TagAccess) ([]ApprovedSet, error) {
	tag, err := approvalTag(cfg, tags)
	if err != nil {
		return nil, err
	}
	byName := map[string][]proxmox.VM{}
	for _, vm := range vms {
		byName[vm.Name] = append(byName[vm.Name], vm)
	}
	var out []ApprovedSet
	for set, masters := range mastersBySet(vms, n, cfg.MasterTag) {
		if requireApproved(set, masters, tag) != nil {
			continue
		}
		approved := ApprovedSet{Name: set}
		for i, m := range masters {
			if i > 0 && masters[i-1].Name == m.Name {
				continue // a second master of the name: a deploy blocks the host
			}
			host, _ := n.SplitMaster(m.Name)
			member := SetMember{Host: host, Master: m}
			if tpl, ok := usableTemplate(byName[n.TemplateName(m.Name)]); ok {
				member.Template = &tpl
			}
			approved.Members = append(approved.Members, member)
		}
		out = append(out, approved)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
