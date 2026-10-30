package pods

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/wccomps/battleship/internal/config"
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
// reject or the deploy baseline (deploy.snapshot_name). VMs already holding
// the name are blocked: snapshots are never replaced.
func (p Planner) Snapshot(ctx context.Context, req SnapshotRequest) (*Plan, error) {
	if err := CheckSnapshotName(req.Name); err != nil {
		return nil, err
	}
	if req.Name == p.Cfg.Deploy.SnapshotName {
		return nil, fmt.Errorf("%q is the baseline snapshot a deploy takes (deploy.snapshot_name); choose another name", req.Name)
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
		if it.Blocked != "" {
			continue
		}
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

// alreadyHas blocks an item whose VM has a snapshot of the name.
func alreadyHas(name string) string {
	return fmt.Sprintf("already has a snapshot named %q; choose another name", name)
}

// HasBaseline reports whether a VM with these snapshots has the finished
// deploy baseline (deploy.snapshot_name).
func HasBaseline(d config.Deploy, snaps []proxmox.Snapshot) bool {
	return slices.Contains(SnapshotNames(Finished(snaps)), d.SnapshotName)
}

// NoBaseline says a VM lacks the baseline snapshot, e.g.
// `no "initial" baseline snapshot`.
func NoBaseline(d config.Deploy) string {
	return fmt.Sprintf("no %q baseline snapshot", d.SnapshotName)
}

// Finished drops snapshots still being taken or deleted, or whose task died
// (snapstate set): Proxmox refuses to roll back to them.
func Finished(snaps []proxmox.Snapshot) []proxmox.Snapshot {
	return slices.DeleteFunc(slices.Clone(snaps), func(s proxmox.Snapshot) bool { return s.State != "" })
}

func SnapshotNames(snaps []proxmox.Snapshot) []string {
	if snaps == nil {
		return nil
	}
	out := make([]string, len(snaps))
	for i, s := range snaps {
		out[i] = s.Name
	}
	return out
}
