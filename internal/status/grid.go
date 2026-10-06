// Package status keeps the live status grid of team VMs for the web app, and
// the hub that fans change notifications out to its pages.
//
// A Poller reads the cluster every web.status_poll and builds a Grid, teams ×
// hosts. It marks a VM drifted when its pool is wrong, when the last finished
// deploy, reset or teardown that touched it failed on it or was interrupted while working
// on it, or when the deep scan, which reads every team VM's config and
// snapshots every web.drift_scan, finds it isn't wired the way a deploy
// leaves it. The drift rules are the engine's own.
package status

import (
	"slices"
	"sort"
	"time"

	"github.com/wccomps/battleship/internal/pods"
)

// State is what a grid cell shows.
type State string

const (
	StateRunning State = "running"
	StateStopped State = "stopped" // any Proxmox status but running; Cell.Power has it
	StateMissing State = "missing" // no team VM of that name
	StateDrifted State = "drifted" // the VM exists but isn't as a deploy leaves it; see Cell.Drift
)

// DriftKind says which rule found a drift.
type DriftKind string

const (
	DriftPool       DriftKind = "pool"        // not in the team's pool
	DriftDuplicate  DriftKind = "duplicate"   // several VMs have the name
	DriftJob        DriftKind = "job"         // the last finished deploy, reset or teardown that touched it failed or was cut off on it
	DriftNetwork    DriftKind = "network"     // the scan found NICs, IP settings or cloud-init snippet not as a deploy sets them
	DriftDiskLimits DriftKind = "disk-limits" // the scan found disks without the configured limits
	DriftSnapshot   DriftKind = "snapshot"    // the scan found no baseline snapshot
)

// Drift is one reason a VM is drifted.
type Drift struct {
	Kind   DriftKind
	Reason string // a sentence for volunteers
	JobID  int64  // DriftJob: the job to look at
}

// Cell is one team VM in the grid.
type Cell struct {
	Team  string
	Host  string
	Name  string // the team VM's name, also when it is missing
	State State
	Power string // Proxmox status, e.g. running or stopped; "" when missing
	VMID  int    // 0 when missing; the lowest VMID when several share the name
	Node  string
	Pool  string
	Drift []Drift // nil unless drifted
}

// Row is one team's cells, in the order of Grid.Hosts.
type Row struct {
	Team  string
	Cells []Cell
}

// Grid is the status of every team VM. The Poller hands out copies, so a
// Grid may be read and changed freely.
type Grid struct {
	// Version goes up by one whenever anything but PolledAt changes, and is
	// what the poller publishes on TopicGrid.
	Version uint64
	Teams   []string // the rows: the teams with team VMs, sorted
	Hosts   []string // the columns, sorted
	Rows    []Row
	// PolledAt is when the cluster was last read successfully; zero before
	// the first good poll.
	PolledAt time.Time
	// Stale is set while polling fails, or before the first good poll. The
	// cells then show the last good poll, and Err says what went wrong.
	Stale bool
	Err   string
	// ScannedAt is when the last deep scan finished; zero before the first.
	// ScanErr says which VMs it couldn't read; they keep the drift an
	// earlier scan found.
	ScannedAt time.Time
	ScanErr   string
	// Sets are the template sets on the cluster, from the masters tagged
	// deploy.master_tag in the last good poll, and OtherMasters the names
	// of tagged masters in no set: what the empty grid offers to deploy.
	// Both are nil before the first good poll.
	Sets         []pods.MasterSet
	OtherMasters []string
}

// Cell returns the cell of team (two digits, e.g. "01") and host.
func (g Grid) Cell(team, host string) (Cell, bool) {
	i := sort.SearchStrings(g.Teams, team)
	j := sort.SearchStrings(g.Hosts, host)
	if i >= len(g.Rows) || g.Teams[i] != team || j >= len(g.Hosts) || g.Hosts[j] != host {
		return Cell{}, false
	}
	return g.Rows[i].Cells[j], true
}

func (g Grid) clone() Grid {
	out := g
	out.Teams = slices.Clone(g.Teams)
	out.Hosts = slices.Clone(g.Hosts)
	out.Sets = slices.Clone(g.Sets)
	for i := range out.Sets {
		out.Sets[i].Hosts = slices.Clone(g.Sets[i].Hosts)
	}
	out.OtherMasters = slices.Clone(g.OtherMasters)
	out.Rows = make([]Row, len(g.Rows))
	for i, row := range g.Rows {
		cells := make([]Cell, len(row.Cells))
		for j, c := range row.Cells {
			c.Drift = slices.Clone(c.Drift)
			cells[j] = c
		}
		out.Rows[i] = Row{Team: row.Team, Cells: cells}
	}
	return out
}

// sameContent reports whether a and b show the same thing: everything but
// Version and PolledAt.
func sameContent(a, b Grid) bool {
	if a.Stale != b.Stale || a.Err != b.Err || !a.ScannedAt.Equal(b.ScannedAt) || a.ScanErr != b.ScanErr ||
		!slices.Equal(a.Teams, b.Teams) || !slices.Equal(a.Hosts, b.Hosts) || len(a.Rows) != len(b.Rows) ||
		!slices.EqualFunc(a.Sets, b.Sets, sameSet) || !slices.Equal(a.OtherMasters, b.OtherMasters) {
		return false
	}
	for i := range a.Rows {
		ra, rb := a.Rows[i], b.Rows[i]
		if ra.Team != rb.Team || len(ra.Cells) != len(rb.Cells) {
			return false
		}
		for j := range ra.Cells {
			if !sameCell(ra.Cells[j], rb.Cells[j]) {
				return false
			}
		}
	}
	return true
}

func sameCell(a, b Cell) bool {
	return a.Team == b.Team && a.Host == b.Host && a.Name == b.Name && a.State == b.State && a.Power == b.Power &&
		a.VMID == b.VMID && a.Node == b.Node && a.Pool == b.Pool && slices.Equal(a.Drift, b.Drift)
}

func sameSet(a, b pods.MasterSet) bool {
	return a.Name == b.Name && a.Masters == b.Masters && a.Running == b.Running && slices.Equal(a.Hosts, b.Hosts)
}

// cellCopy is Cell with a Drift slice of its own.
func (g Grid) cellCopy(team, host string) (Cell, bool) {
	c, ok := g.Cell(team, host)
	c.Drift = slices.Clone(c.Drift)
	return c, ok
}
