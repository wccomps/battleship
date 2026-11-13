package pods

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

var ErrNotApproved = errors.New("not approved for self-service sessions")

// approvalTag is deploy.approved_tag, or ErrNotApproved when it is empty or
// not, in that exact spelling, one of the registered tags Proxmox shows the
// reader. Only Sys.Modify on / may put a registered tag on a VM, and only
// Sys.Audit on / may read which tags are registered.
func approvalTag(cfg config.Deploy, tags proxmox.TagAccess) (string, error) {
	tag := strings.TrimSpace(cfg.ApprovedTag)
	switch {
	case tag == "":
		return "", fmt.Errorf("every set is %w: deploy.approved_tag is empty", ErrNotApproved)
	case !slices.Contains(tags.Registered, tag):
		return "", fmt.Errorf("every set is %w: deploy.approved_tag %q is not, spelled exactly so, one of the registered tags Proxmox shows this user "+
			"(Datacenter, Options; reading them needs Sys.Audit on /)", ErrNotApproved, tag)
	}
	return tag, nil
}

// requireApproved is ErrNotApproved unless set has masters and each carries
// tag in that exact spelling: Proxmox reserves a registered tag as written,
// so a variant in another case is one anybody may set.
func requireApproved(set Set, masters []proxmox.VM, tag string) error {
	if len(masters) == 0 {
		return fmt.Errorf("%s is %w: it has no masters", set, ErrNotApproved)
	}
	for _, m := range masters {
		if !slices.Contains(strings.FieldsFunc(m.Tags, isTagSeparator), tag) {
			return fmt.Errorf("%s is %w: %s lacks tag %q", set, ErrNotApproved, m.Name, tag)
		}
	}
	return nil
}

// ApprovedSets is the sets whose masters all carry the approval tag
// (requireApproved), sorted by name. It is ErrNotApproved when the tag can
// approve nothing (approvalTag).
func ApprovedSets(vms []proxmox.VM, n Naming, cfg config.Deploy, tags proxmox.TagAccess) ([]Set, error) {
	tag, err := approvalTag(cfg, tags)
	if err != nil {
		return nil, err
	}
	bySet, _ := mastersBySet(vms, n, cfg.MasterTag)
	sets := slices.DeleteFunc(slices.Sorted(maps.Keys(bySet)), func(set Set) bool {
		return requireApproved(set, bySet[set], tag) != nil
	})
	return sets, nil
}
