package pods

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/wccomps/battleship/internal/proxmox"
)

// MaxSnapshotName is the longest snapshot name Proxmox takes.
const MaxSnapshotName = 40

// CheckSnapshotName reports whether Proxmox would accept name for a new
// snapshot: pve-configid format (a letter, then letters, digits, _ or -; 2+
// characters), at most MaxSnapshotName long, and not the reserved "current"
// (exact) or "pending" (any case).
func CheckSnapshotName(name string) error {
	if name == "" {
		return errors.New("a snapshot needs a name")
	}
	for _, r := range name {
		if !isLetter(r) && !(r >= '0' && r <= '9') && r != '_' && r != '-' {
			return fmt.Errorf("snapshot name %q may contain only letters, digits, _ and -", name)
		}
	}
	switch {
	case !isLetter(rune(name[0])):
		return fmt.Errorf("snapshot name %q must start with a letter", name)
	case len(name) < 2:
		return fmt.Errorf("snapshot name %q is too short: Proxmox needs at least 2 characters", name)
	case len(name) > MaxSnapshotName:
		return fmt.Errorf("snapshot name %q is too long: at most %d characters", name, MaxSnapshotName)
	case name == "current" || strings.EqualFold(name, "pending"):
		return fmt.Errorf("snapshot name %q is reserved by Proxmox", name)
	}
	return nil
}

func isLetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

// SnapshotRequest asks for a new snapshot of team VMs.
type SnapshotRequest struct {
	Teams       []string
	Hosts       []string // optional host filter
	Name        string
	Description string
	VMState     bool // also save the RAM of running VMs
}

// Snapshot takes a new snapshot of team VMs. It refuses names Proxmox would
// reject or that could pass for the deploy baseline (which a bare reset
// would roll back to). VMs already holding the name are blocked: snapshots
// are never replaced.
func (p Planner) Snapshot(ctx context.Context, req SnapshotRequest) (*Plan, error) {
	if err := CheckSnapshotName(req.Name); err != nil {
		return nil, err
	}
	if err := p.notBaseline(req.Name); err != nil {
		return nil, err
	}
	plan, _, err := p.simple(ctx, KindSnapshot, req.Teams, req.Hosts, func(it *Item) {
		it.Steps = []Step{StepSnapshot}
		it.Snapshot, it.Description, it.VMState = req.Name, req.Description, req.VMState
	})
	if err != nil {
		return nil, err
	}
	for i := range plan.Items {
		it := &plan.Items[i]
		snaps, err := p.API.Snapshots(ctx, it.Node, it.VMID)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, err
		case err != nil:
			it.Blocked = "cannot list snapshots: " + proxmox.Describe(err)
		case slices.Contains(SnapshotNames(snaps), req.Name):
			it.Blocked = alreadyHas(req.Name)
		}
	}
	return plan, nil
}

// notBaseline refuses a name that is the deploy baseline's or matches
// baseline_patterns.
func (p Planner) notBaseline(name string) error {
	d := p.Cfg.Deploy
	if name == d.SnapshotName {
		return fmt.Errorf("%q is the baseline snapshot a deploy takes (deploy.snapshot_name); choose another name", name)
	}
	for _, pat := range d.BaselinePatterns {
		if ok, _ := path.Match(pat, name); ok {
			return fmt.Errorf("%q matches %s, so a reset to the baseline could pick it (deploy.baseline_patterns); choose another name", name, pat)
		}
	}
	return nil
}

// alreadyHas blocks an item whose VM has a snapshot of the name.
func alreadyHas(name string) string {
	return fmt.Sprintf("already has a snapshot named %q; choose another name", name)
}
