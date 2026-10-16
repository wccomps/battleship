package pods

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// BaselineSnapshot is what a reset naming no snapshot rolls back to:
// deploy.snapshot_name if present, else the newest match of
// deploy.baseline_patterns (latest snaptime, then greatest name, so
// timestamped names sort right without snaptimes). Unfinished snapshots
// don't count. ok is false if none.
func BaselineSnapshot(d config.Deploy, snaps []proxmox.Snapshot) (name string, ok bool) {
	var best *proxmox.Snapshot
	for i := range snaps {
		s := &snaps[i]
		if s.State != "" {
			continue
		}
		if s.Name == d.SnapshotName {
			return s.Name, true
		}
		if !matchesBaseline(d, s.Name) {
			continue
		}
		if best == nil || s.Time > best.Time || (s.Time == best.Time && s.Name > best.Name) {
			best = s
		}
	}
	if best == nil {
		return "", false
	}
	return best.Name, true
}

// HasBaseline reports whether a VM with these snapshots has a baseline.
func HasBaseline(d config.Deploy, snaps []proxmox.Snapshot) bool {
	_, ok := BaselineSnapshot(d, snaps)
	return ok
}

func matchesBaseline(d config.Deploy, name string) bool {
	for _, p := range d.BaselinePatterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// BaselineText describes what counts as the baseline, e.g.
// `initial, else the newest fresh_clone_*`.
func BaselineText(d config.Deploy) string {
	if len(d.BaselinePatterns) == 0 {
		return d.SnapshotName
	}
	return d.SnapshotName + ", else the newest " + strings.Join(d.BaselinePatterns, " or ")
}

// NoBaseline says a VM lacks a baseline, e.g.
// `no "initial" or fresh_clone_* baseline snapshot`.
func NoBaseline(d config.Deploy) string {
	names := append([]string{fmt.Sprintf("%q", d.SnapshotName)}, d.BaselinePatterns...)
	return "no " + strings.Join(names, " or ") + " baseline snapshot"
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

// SnapshotLabel names the snapshot a reset item rolls back to, e.g.
// "baseline (fresh_clone_20261002034615)" or "before-scoring".
func SnapshotLabel(it Item) string {
	if it.Baseline {
		if it.Snapshot == "" {
			return "baseline"
		}
		return "baseline (" + it.Snapshot + ")"
	}
	return it.Snapshot
}
