package status

import (
	"context"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// Proxmox has no event stream for VM changes, so the grid polls: the
// cluster's VMs every web.status_poll, and every team VM's config and
// snapshots every web.drift_scan (the full scan, which is what finds
// changes: config edits, cloud-init regeneration and pool changes make no
// task). As a speed-up only, each poll also reads the task list of each
// node the viewer has Sys.Audit on, and reads again at once the VMs those
// tasks touched. Without Sys.Audit on a node, Proxmox would list only the
// viewer's own tasks there, so it isn't asked.

// TaskReader lists a node's tasks; *proxmox.Client is one.
type TaskReader interface {
	NodeTasks(ctx context.Context, node string, since time.Time) ([]proxmox.Task, error)
}

// auditRecheck is how long a node's Sys.Audit answer is trusted.
const auditRecheck = 5 * time.Minute

// taskFollow is where the poller's reading of task lists has got to.
type taskFollow struct {
	since map[string]time.Time // per node: ask for tasks started at or after this
	// seen, per node, is each listed task's state when its VM was last
	// re-read (true: running), so a task causes a re-read when it starts
	// and when it ends, not on every poll while it runs.
	seen  map[string]map[string]bool
	audit map[string]auditAnswer
}

type auditAnswer struct {
	ok bool
	at time.Time
}

// vmTasks are the task types that change what the grid shows of a VM.
var vmTasks = map[string]bool{
	"qmstart": true, "qmstop": true, "qmshutdown": true, "qmreboot": true, "qmreset": true,
	"qmsuspend": true, "qmresume": true, "qmrollback": true, "qmsnapshot": true, "qmdelsnapshot": true,
	"qmclone": true, "qmdestroy": true, "qmconfig": true, "qmtemplate": true, "qmmove": true,
	"qmigrate": true, "hastart": true, "hastop": true,
}

// touchedByTasks reads the task lists of the nodes team VMs are on that
// the viewer may audit, and re-reads the team VMs those tasks touched.
// Failures are only logged: the full scan still finds the changes.
func (p *Poller) touchedByTasks(ctx context.Context, teamVMs []proxmox.VM, offset time.Duration) scanResults {
	tasks, ok1 := p.api.(TaskReader)
	perms, ok2 := p.api.(pods.PermissionReader)
	if !ok1 || !ok2 {
		return nil
	}
	now := p.clock.Now()
	// Only the VM a cell shows is re-read, as the scan's results are by name.
	byVMID := map[int]proxmox.VM{}
	nodes := map[string]bool{}
	for _, vm := range shownVMs(teamVMs) {
		byVMID[vm.VMID] = vm
		nodes[vm.Node] = true
	}
	touched := map[int]bool{}
	for node := range nodes {
		if !p.mayAudit(ctx, perms, node, now) {
			continue
		}
		since, seen := p.follow.since[node]
		if !seen {
			since = now // nothing from before the view started
		}
		list, err := tasks.NodeTasks(ctx, node, since)
		if err != nil {
			p.logf("status: reading the tasks of %s: %v", node, proxmox.Describe(err))
			continue
		}
		next := since
		var running *time.Time
		states := map[string]bool{}
		for _, t := range list {
			if t.Start.Add(time.Second).After(next) {
				next = t.Start.Add(time.Second)
			}
			if !vmTasks[t.Type] {
				continue // a console or a backup changes nothing the grid shows
			}
			if was, ok := p.follow.seen[node][t.UPID]; !ok || was != t.Running {
				touched[t.VMID] = true
			}
			states[t.UPID] = t.Running
			if t.Running && (running == nil || t.Start.Before(*running)) {
				start := t.Start
				running = &start
			}
		}
		if running != nil {
			next = *running // until it ends, so its end is seen
		}
		p.follow.since[node] = next
		p.follow.seen[node] = states
	}
	out := scanResults{}
	for vmid := range touched {
		vm, ok := byVMID[vmid]
		if !ok {
			continue
		}
		r, err := p.scanVM(ctx, vm, offset)
		if err != nil {
			continue
		}
		out[vm.Name] = r
	}
	return out
}

// mayAudit reports whether the viewer has Sys.Audit on node, asking
// Proxmox at most every auditRecheck.
func (p *Poller) mayAudit(ctx context.Context, perms pods.PermissionReader, node string, now time.Time) bool {
	if a, ok := p.follow.audit[node]; ok && now.Sub(a.at) < auditRecheck {
		return a.ok
	}
	privs, err := perms.PermissionsAt(ctx, "/nodes/"+node)
	if err != nil {
		return false // not remembered: a failed read says nothing about the privilege
	}
	_, ok := privs["Sys.Audit"]
	p.follow.audit[node] = auditAnswer{ok: ok, at: now}
	return ok
}
