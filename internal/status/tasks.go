package status

import (
	"context"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// Proxmox has no VM change stream, and config edits, cloud-init
// regeneration and pool changes make no task, so the full scan is what finds
// changes. As a speed-up, each poll reads task lists of nodes the viewer has
// Sys.Audit on (without it Proxmox lists only the viewer's own tasks) and
// re-reads the VMs those tasks touched.

// auditRecheck is how long a node's Sys.Audit answer is trusted.
const auditRecheck = 5 * time.Minute

// taskFollow is where the poller's reading of task lists has got to.
type taskFollow struct {
	since map[string]time.Time // per node: ask for tasks started at or after this
	// seen, per node, is each task's state at its VM's last re-read (true:
	// running), so a task triggers re-reads only at start and end.
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

// touchedByTasks re-reads team VMs touched by tasks on auditable nodes.
// Failures are skipped; the full scan still finds the changes.
func (p *Poller) touchedByTasks(ctx context.Context, teamVMs []pods.NamedVM, offset time.Duration) scanResults {
	now := p.clock.Now()
	// Only the VM a cell shows is re-read, as the scan's results are by name.
	byVMID := map[int]pods.NamedVM{}
	nodes := map[string]bool{}
	for _, vm := range shownVMs(teamVMs) {
		byVMID[vm.VMID] = vm
		nodes[vm.Node] = true
	}
	touched := map[int]bool{}
	for node := range nodes {
		if !p.mayAudit(ctx, node, now) {
			continue
		}
		since, seen := p.follow.since[node]
		if !seen {
			since = now // nothing from before the view started
		}
		list, err := p.api.NodeTasks(ctx, node, since)
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

// mayAudit reports whether the viewer has Sys.Audit on node, cached for
// auditRecheck.
func (p *Poller) mayAudit(ctx context.Context, node string, now time.Time) bool {
	if a, ok := p.follow.audit[node]; ok && now.Sub(a.at) < auditRecheck {
		return a.ok
	}
	privs, err := p.api.PermissionsAt(ctx, "/nodes/"+node)
	if err != nil {
		return false // not remembered: a failed read says nothing about the privilege
	}
	_, ok := privs["Sys.Audit"]
	p.follow.audit[node] = auditAnswer{ok: ok, at: now}
	return ok
}
