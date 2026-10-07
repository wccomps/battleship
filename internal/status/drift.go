package status

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// rules builds grids and judges drift with the engine's rules.
type rules struct {
	cfg    config.Config
	naming pods.Naming
}

// inputs is what a grid is built from.
type inputs struct {
	vms     []proxmox.VM                // the last good poll
	history map[string]store.ItemResult // by VM name, from the last good poll
	scan    scanResults
}

// scanResult is what a deep scan (or a cell detail read) found on one VM.
type scanResult struct {
	vmid   int
	readAt time.Time // when the read started, in the database's clock
	drift  []Drift
}

// scanResults are the latest scan results, by VM name.
type scanResults map[string]scanResult

// put records r unless a later read is already there (scan, task follower
// and cell details race).
func (s scanResults) put(name string, r scanResult) {
	if old, ok := s[name]; !ok || !old.readAt.After(r.readAt) {
		s[name] = r
	}
}

// shownVMs picks, per name, the VM a cell shows (lowest VMID), in order of
// first appearance.
func shownVMs(vms []proxmox.VM) []proxmox.VM {
	var out []proxmox.VM
	index := map[string]int{}
	for _, vm := range vms {
		if i, ok := index[vm.Name]; ok {
			if vm.VMID < out[i].VMID {
				out[i] = vm
			}
			continue
		}
		index[vm.Name] = len(out)
		out = append(out, vm)
	}
	return out
}

// grid lays team VMs out as teams × hosts. With web.templates set, hosts of
// matching tagged masters get a column even if no team has them yet.
func (r rules) grid(in inputs) (teams, hosts []string, rows []Row) {
	teams = pods.TeamsWithVMs(in.vms, r.naming)
	byCell := map[[2]string][]proxmox.VM{}
	seen := map[string]bool{}
	for _, vm := range pods.AllTeamVMs(in.vms, r.naming) {
		team, host, _ := r.naming.ParseVMName(vm.Name)
		byCell[[2]string{team, host}] = append(byCell[[2]string{team, host}], vm)
		seen[host] = true
	}
	if pattern := strings.TrimSpace(r.cfg.Web.Templates); pattern != "" {
		masters, _ := pods.FindMasters(in.vms, r.naming, pattern, r.cfg.Deploy.MasterTag)
		for _, m := range masters {
			seen[pods.Hostname(m.Name)] = true
		}
	}
	for h := range seen {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	rows = make([]Row, len(teams))
	for i, team := range teams {
		cells := make([]Cell, len(hosts))
		for j, host := range hosts {
			cells[j] = r.cell(team, host, byCell[[2]string{team, host}], in)
		}
		rows[i] = Row{Team: team, Cells: cells}
	}
	return teams, hosts, rows
}

// cell builds one cell from the VMs with its name.
func (r rules) cell(team, host string, vms []proxmox.VM, in inputs) Cell {
	c := Cell{Team: team, Host: host, Name: r.naming.VMName(team, host), State: StateMissing}
	if len(vms) == 0 {
		return c
	}
	vm := shownVMs(vms)[0]
	c.Power, c.VMID, c.Node, c.Pool = vm.Status, vm.VMID, vm.Node, vm.Pool

	// Proxmox leaves pool out of /cluster/resources when the viewer can't
	// audit pools, so an empty pool is unknown, not drift.
	if want := r.naming.Pool(team); vm.Pool != "" && vm.Pool != want {
		c.Drift = append(c.Drift, Drift{Kind: DriftPool, Reason: fmt.Sprintf("in pool %q, expected %q", vm.Pool, want)})
	}
	if len(vms) > 1 {
		vmids := make([]int, len(vms))
		for i, v := range vms {
			vmids[i] = v.VMID
		}
		slices.Sort(vmids)
		ids := make([]string, len(vmids))
		for i, id := range vmids {
			ids[i] = strconv.Itoa(id)
		}
		c.Drift = append(c.Drift, Drift{Kind: DriftDuplicate,
			Reason: fmt.Sprintf("several VMs are named %s (VMIDs %s); remove the extras", c.Name, strings.Join(ids, ", "))})
	}
	// last is the VM's latest deploy, reset or teardown.
	last, touched := in.history[c.Name]
	if d, ok := jobDrift(last); touched && ok {
		c.Drift = append(c.Drift, d)
	}
	if s, ok := in.scan[c.Name]; ok && s.vmid == vm.VMID && !(touched && supersedes(last, s)) {
		c.Drift = append(c.Drift, s.drift...)
	}
	return withState(c)
}

func withState(c Cell) Cell {
	switch {
	case c.VMID == 0:
		c.State = StateMissing
	case len(c.Drift) > 0:
		c.State = StateDrifted
	case c.Power == "running":
		c.State = StateRunning
	default:
		c.State = StateStopped
	}
	return c
}

// jobDrift is the drift a VM's last driftKinds job left if it failed on it
// or was cut off before its config steps finished (store.ItemOutcome.LeftConfig).
func jobDrift(r store.ItemResult) (Drift, bool) {
	switch r.Status {
	case store.ItemFailed:
		reason := fmt.Sprintf("failed in %s job %d", r.JobKind, r.JobID)
		if r.Error != "" {
			reason += ": " + r.Error
		}
		return Drift{Kind: DriftJob, JobID: r.JobID, Reason: reason}, true
	case store.ItemInterrupted:
		if r.LeftConfig == store.LeftConverged {
			return Drift{}, false // only power steps were left
		}
		return Drift{Kind: DriftJob, JobID: r.JobID,
			Reason: fmt.Sprintf("%s job %d was interrupted; its last step was %s", r.JobKind, r.JobID, r.Step)}, true
	}
	return Drift{}, false
}

// driftKinds are the job kinds that converge what the scan judges (NICs,
// disk limits, pool, baseline). Power and snapshot jobs touch none of it.
var driftKinds = []string{string(pods.KindDeploy), string(pods.KindReset), string(pods.KindTeardown)}

// supersedes reports whether a re-converging job (deploy or reset) finished
// after the scan read the VM. Both times are database time.
func supersedes(r store.ItemResult, s scanResult) bool {
	return (r.JobKind == string(pods.KindDeploy) || r.JobKind == string(pods.KindReset)) && r.FinishedAt.After(s.readAt)
}

// configDrift judges a team VM by the rules a deploy converges it with. The
// NIC count comes from the VM, since a clone has its template's NICs.
func (r rules) configDrift(team string, cfg map[string]string, snaps []string) []Drift {
	var out []Drift
	if net := pods.NetworkChanges(r.cfg.Network, cfg, team, pods.CountInterfaces(cfg)); len(net) > 0 {
		parts := make([]string, 0, len(net))
		for _, k := range slices.Sorted(maps.Keys(net)) {
			parts = append(parts, k+" should be "+net[k])
		}
		out = append(out, Drift{Kind: DriftNetwork,
			Reason: fmt.Sprintf("network differs from team %s's: %s", team, strings.Join(parts, "; "))})
	}
	rd, wr := r.cfg.Deploy.DiskMBpsRead, r.cfg.Deploy.DiskMBpsWrite
	if disk := pods.DiskLimitChanges(cfg, rd, wr); len(disk) > 0 {
		out = append(out, Drift{Kind: DriftDiskLimits,
			Reason: fmt.Sprintf("disk limits differ on %s (want %d MB/s read and %d MB/s write)", strings.Join(slices.Sorted(maps.Keys(disk)), ", "), rd, wr)})
	}
	if !pods.HasBaseline(r.cfg.Deploy, snaps) {
		out = append(out, Drift{Kind: DriftSnapshot, Reason: pods.NoBaseline(r.cfg.Deploy)})
	}
	return out
}
