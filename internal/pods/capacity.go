package pods

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// ResourceReader is the optional part of an API that reads the cluster's
// VMs, nodes and storage in one call (*proxmox.Client does). Without it,
// previews have no capacity check.
type ResourceReader interface {
	ClusterResources(ctx context.Context) (proxmox.Resources, error)
}

// UsageKind is what a Usage measures.
type UsageKind string

const (
	UsageMemory  UsageKind = "memory"
	UsageStorage UsageKind = "storage"
)

// Usage is one node's memory, or one storage's space, as the cluster
// reports it now and as much more as a plan needs. Sizes are bytes.
type Usage struct {
	Kind    UsageKind `json:"kind"`
	Node    string    `json:"node"`              // "" for shared storage
	Storage string    `json:"storage,omitempty"` // storage only
	Used    int64     `json:"used"`
	Total   int64     `json:"total"`
	Needed  int64     `json:"needed"`
}

// Label names what u measures: "cedar", "competitions on cedar", or a
// shared storage's name.
func (u Usage) Label() string {
	switch {
	case u.Kind == UsageMemory:
		return u.Node
	case u.Node == "":
		return u.Storage
	}
	return u.Storage + " on " + u.Node
}

// Free is what is left before the plan runs.
func (u Usage) Free() int64 { return max(u.Total-u.Used, 0) }

// Over reports that the plan needs more than is free.
func (u Usage) Over() bool { return u.Needed > u.Free() }

// Near reports that the plan would leave it more than 90% used.
func (u Usage) Near() bool { return u.Total > 0 && (u.Used+u.Needed)*10 > u.Total*9 }

// Capacity is how a plan's needs compare with what the cluster has free.
// It is advice for the preview only: it is not part of the plan, so it
// never changes the plan's fingerprint, and it never blocks anything, since
// overcommit may be deliberate.
type Capacity struct {
	// Usage lists the nodes' memory and the clone storage the plan uses,
	// memory first, each sorted by node.
	Usage []Usage `json:"usage"`
	// Notes are what the numbers can't say, e.g. that linked clones grow.
	Notes []string `json:"notes"`
}

// CapacityWarning is one line of the preview's capacity warnings: Over is
// more than is free; otherwise it would be more than 90% used.
type CapacityWarning struct {
	Over bool
	Text string
}

// Warnings lists the usages that are over, then those near full, e.g.
// "cedar: needs 96 GiB of memory, 40 GiB free".
func (c *Capacity) Warnings() []CapacityWarning {
	if c == nil {
		return nil
	}
	var over, near []CapacityWarning
	for _, u := range c.Usage {
		what := "of memory"
		word := "memory"
		if u.Kind == UsageStorage {
			what, word = "of disk", "disk"
		}
		switch {
		case u.Over():
			over = append(over, CapacityWarning{Over: true,
				Text: fmt.Sprintf("%s: needs %s %s, %s free", u.Label(), GiB(u.Needed), what, GiB(u.Free()))})
		case u.Near():
			pct := float64(u.Used+u.Needed) * 100 / float64(u.Total)
			near = append(near, CapacityWarning{
				Text: fmt.Sprintf("%s: %s would be %.0f%% used (needs %s, %s free)", u.Label(), word, pct, GiB(u.Needed), GiB(u.Free()))})
		}
	}
	return append(over, near...)
}

// GiB shows a size in GiB: whole when it is 10 or more or a whole number,
// else to one decimal.
func GiB(b int64) string {
	v := float64(b) / float64(1<<30)
	if v >= 10 || v == float64(int64(v)) {
		return strconv.FormatFloat(v, 'f', 0, 64) + " GiB"
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " GiB"
}

// NeedsCapacity reports whether plan's preview checks capacity: a deploy, or
// power start. A reset also starts its VMs; it isn't checked.
func NeedsCapacity(plan *Plan) bool {
	switch plan.Kind {
	case KindDeploy:
		return true
	case KindPower:
		for _, it := range plan.Items {
			if it.Action == "start" {
				return true
			}
		}
	}
	return false
}

// ReadCapacity reads the cluster's resources in one call and compares them
// with plan's needs. It returns nil when the plan starts nothing or api
// can't read resources.
func ReadCapacity(ctx context.Context, api API, plan *Plan, cfg config.Config) (*Capacity, error) {
	rr, ok := api.(ResourceReader)
	if !ok || !NeedsCapacity(plan) {
		return nil, nil
	}
	res, err := rr.ClusterResources(ctx)
	if err != nil {
		return nil, err
	}
	return EstimateCapacity(plan, res, cfg.Deploy), nil
}

// EstimateCapacity compares what plan needs with what res has free.
//
// Memory, per node: a new clone needs its template's memory (the master's
// when the plan builds the template) on the node the planner assigned it;
// an existing VM the plan starts needs its own, unless it already runs.
//
// Storage, deploy.storage per node (or once, if shared): a template build
// copies its master in full, and so does each clone when deploy.linked is
// false. Linked clones need next to nothing at first but grow as they are
// used, which a note says rather than a guessed number.
//
// Sizes come from res (maxmem and maxdisk, the boot disk). Only nodes and
// storage the plan needs something from are listed.
func EstimateCapacity(plan *Plan, res proxmox.Resources, deploy config.Deploy) *Capacity {
	if !NeedsCapacity(plan) {
		return nil
	}
	vms := make(map[int]proxmox.VM, len(res.VMs))
	for _, vm := range res.VMs {
		vms[vm.VMID] = vm
	}
	mem := map[string]int64{}
	disk := map[string]int64{}
	linked := 0

	sources := map[string]proxmox.VM{} // template name -> what its clones copy
	for _, t := range plan.Templates {
		if t.Blocked != "" {
			continue
		}
		if t.Exists && !t.Rebuild {
			sources[t.Name] = vms[t.VMID]
			continue
		}
		master := vms[t.MasterVMID]
		sources[t.Name] = master
		disk[t.MasterNode] += master.MaxDisk // the full copy the template is built from
	}

	for _, it := range plan.Runnable() {
		switch {
		case plan.Kind == KindPower:
			if it.Action == "start" && vms[it.VMID].Status != "running" {
				mem[it.Node] += vms[it.VMID].MaxMem
			}
		case slices.Contains(it.Steps, StepClone):
			src := sources[it.Template]
			mem[it.Node] += src.MaxMem
			if deploy.Linked {
				linked++
			} else {
				disk[it.Node] += src.MaxDisk
			}
		case slices.Contains(it.Steps, StepStart):
			if vm, ok := vms[it.VMID]; ok && vm.Status != "running" {
				mem[it.Node] += vm.MaxMem
			}
		}
	}

	c := &Capacity{}
	// An online node without memory figures is not offline: Proxmox leaves
	// mem and maxmem out of /cluster/resources for a caller without
	// Sys.Audit. One note covers every such node.
	var noAudit, offline []string
	for _, node := range slices.Sorted(maps.Keys(mem)) {
		if mem[node] <= 0 {
			continue
		}
		n, ok := findNode(res.Nodes, node)
		switch {
		case !ok || n.Status != "online":
			offline = append(offline, fmt.Sprintf("%s reports no memory (it may be offline), so its %s can't be checked.", node, GiB(mem[node])))
			continue
		case n.MaxMem <= 0:
			noAudit = append(noAudit, node)
			continue
		}
		c.Usage = append(c.Usage, Usage{Kind: UsageMemory, Node: node, Used: n.Mem, Total: n.MaxMem, Needed: mem[node]})
	}
	if len(noAudit) > 0 {
		c.Notes = append(c.Notes, fmt.Sprintf("Memory on %s is unknown: you don't have Sys.Audit on /nodes there, so Proxmox shows no memory figures. Nothing is blocked for it.",
			strings.Join(noAudit, ", ")))
	}
	c.Notes = append(c.Notes, offline...)
	c.Usage = append(c.Usage, storageUsage(c, res.Storage, deploy.Storage, disk)...)
	if linked > 0 {
		c.Notes = append(c.Notes, fmt.Sprintf("Linked clones (%d) need almost no space on %s at first, but grow as teams use them.", linked, deploy.Storage))
	}
	return c
}

// storageUsage turns the per-node disk needs into usages of storage, one
// per node, or one in all if the storage is shared.
func storageUsage(c *Capacity, all []proxmox.StorageResource, storage string, disk map[string]int64) []Usage {
	var total int64
	for _, b := range disk {
		total += b
	}
	if total <= 0 {
		return nil
	}
	for _, s := range all {
		if s.Storage == storage && s.Shared {
			return []Usage{{Kind: UsageStorage, Storage: storage, Used: s.Disk, Total: s.MaxDisk, Needed: total}}
		}
	}
	var out []Usage
	for _, node := range slices.Sorted(maps.Keys(disk)) {
		if disk[node] <= 0 {
			continue
		}
		found := false
		for _, s := range all {
			if s.Storage == storage && s.Node == node && s.MaxDisk > 0 {
				out = append(out, Usage{Kind: UsageStorage, Node: node, Storage: storage, Used: s.Disk, Total: s.MaxDisk, Needed: disk[node]})
				found = true
				break
			}
		}
		if !found {
			c.Notes = append(c.Notes, fmt.Sprintf("%s reports no storage %s, so the %s copied there can't be checked.", node, storage, GiB(disk[node])))
		}
	}
	return out
}

func findNode(nodes []proxmox.NodeResource, name string) (proxmox.NodeResource, bool) {
	for _, n := range nodes {
		if n.Node == name {
			return n, true
		}
	}
	return proxmox.NodeResource{}, false
}
