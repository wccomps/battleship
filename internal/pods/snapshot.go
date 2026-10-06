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

// CheckSnapshotName reports whether Proxmox would take name for a new
// snapshot: its pve-configid format (a letter, then letters, digits, _ or
// -, at least 2 characters in all), at most MaxSnapshotName characters,
// and not "current" (exactly) or "pending" (in any case), the names its API
// reserves.
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

// Snapshot takes a new snapshot of team VMs. It refuses a name Proxmox
// wouldn't take, and one that is, or could pass for, the deploy baseline,
// which a reset that names no snapshot would then roll back to. A VM that
// already has a snapshot of the name is blocked: a snapshot is never
// replaced.
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

// notBaseline refuses a new snapshot's name that is the deploy baseline's,
// or matches baseline_patterns.
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

// takeSnapshot takes the item's snapshot. It never replaces one: a VM
// that has a snapshot of the name fails, unless this run asked for it
// before (a request whose answer was lost, or an earlier round's; see
// createSnapshot). The snapshot is then checked: present and finished.
func (e *Executor) takeSnapshot(ctx context.Context, it *Item) (bool, error) {
	find := func() (snap proxmox.Snapshot, found bool, err error) {
		var snaps []proxmox.Snapshot
		if err := e.call(ctx, func() (err error) { snaps, err = e.api.Snapshots(ctx, it.Node, it.VMID); return }); err != nil {
			return proxmox.Snapshot{}, false, err
		}
		i := slices.IndexFunc(snaps, func(s proxmox.Snapshot) bool { return s.Name == it.Snapshot })
		if i < 0 {
			return proxmox.Snapshot{}, false, nil
		}
		return snaps[i], true, nil
	}
	snap, found, err := find()
	if err != nil {
		return false, err
	}
	if found && !e.askedFor(it.VMID, it.Snapshot) {
		return false, fmt.Errorf("already has a snapshot named %q, taken since the preview; battleship never replaces a snapshot", it.Snapshot)
	}
	if !found {
		err := e.createSnapshot(ctx, it.Node, it.VMID, proxmox.SnapshotRequest{
			Name: it.Snapshot, Description: it.Description, VMState: it.VMState,
		})
		if err != nil {
			return false, err
		}
		if snap, found, err = find(); err != nil {
			return false, err
		}
	}
	switch {
	case !found:
		return false, fmt.Errorf("the snapshot task ended, but %s has no snapshot %q", it.Name, it.Snapshot)
	case snap.State != "":
		return false, fmt.Errorf("snapshot %q of %s was left unfinished (snapstate %s); delete it in Proxmox, then retry", it.Snapshot, it.Name, snap.State)
	}
	return true, nil
}

// createSnapshot takes a snapshot through task. Each request after this
// run's first for the name checks first whether an earlier one took it, so
// a request whose answer was lost isn't sent again (and refused as "already
// used").
func (e *Executor) createSnapshot(ctx context.Context, node string, vmid int, req proxmox.SnapshotRequest) error {
	return e.task(ctx, func() (string, error) {
		if e.askedFor(vmid, req.Name) {
			snaps, err := e.api.Snapshots(ctx, node, vmid)
			if err != nil {
				return "", err
			}
			if slices.Contains(SnapshotNames(snaps), req.Name) {
				return "", nil // an earlier request took it
			}
		}
		e.askFor(vmid, req.Name)
		return e.api.CreateSnapshot(ctx, node, vmid, req)
	})
}

// askFor records that this run sent a request for a VM's snapshot.
func (e *Executor) askFor(vmid int, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.asked[vmid] = append(e.asked[vmid], name)
}

// askedFor reports whether this run sent a request for a VM's snapshot.
func (e *Executor) askedFor(vmid int, name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Contains(e.asked[vmid], name)
}
