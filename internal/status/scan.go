package status

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"golang.org/x/sync/errgroup"
)

var errNoPoll = errors.New("no good poll of the cluster yet")

// Scan reads the config and snapshots of every team VM in the last good poll
// and records the drift it finds, replacing the previous scan's. A VM it
// can't read keeps its previous result, and Grid.ScanErr says so. Each read
// holds one of the shared config-call slots and gives it back before the
// next, so jobs keep getting slots while a scan runs. A scan cut off by ctx
// records nothing and returns ctx's error. Each scan first measures the
// database clock's offset from the poller's, and keeps its read times in
// the database's clock; if it can't, it keeps the last offset.
func (p *Poller) Scan(ctx context.Context) error {
	p.mu.Lock()
	if p.polledAt.IsZero() {
		p.mu.Unlock()
		return errNoPoll
	}
	vms := pods.AllTeamVMs(p.in.vms, p.rules.naming)
	p.mu.Unlock()
	offset := p.measureOffset(ctx)

	targets := shownVMs(vms)
	results := make([]scanResult, len(targets))
	errs := make([]error, len(targets))
	var g errgroup.Group
	g.SetLimit(p.scanWorkers)
	for i, vm := range targets {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			if ctx.Err() == nil {
				results[i], errs[i] = p.scanVM(ctx, vm, offset)
			}
			return nil
		})
	}
	_ = g.Wait() // each read's error is in errs
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	prev := p.in.scan
	next := make(scanResults, len(targets))
	failed, firstErr := 0, ""
	for i, vm := range targets {
		// Only a result for the VM this scan read carries over.
		if old, ok := prev[vm.Name]; ok && old.vmid == vm.VMID {
			next[vm.Name] = old
		}
		if errs[i] != nil {
			failed++
			if firstErr == "" {
				firstErr = fmt.Sprintf("%s: %s", vm.Name, proxmox.Describe(errs[i]))
			}
			continue
		}
		// A cell detail may have read the VM after this scan did.
		next.put(vm.Name, results[i])
	}
	p.in.scan = next
	p.scannedAt = p.clock.Now()
	p.scanErr = ""
	if failed > 0 {
		p.scanErr = fmt.Sprintf("%d of %d VMs could not be read (they keep their last result), e.g. %s", failed, len(targets), firstErr)
	}
	changed := p.update()
	p.mu.Unlock()

	if failed > 0 {
		p.logf("status: drift scan: %d of %d VMs could not be read, e.g. %s", failed, len(targets), firstErr)
	}
	p.publish(changed)
	return nil
}

// dbClockReadTimeout bounds a scan's read of the database's clock, so a hung
// database connection can't stall the drift scan.
const dbClockReadTimeout = 10 * time.Second

// measureOffset reads the database's clock and records and returns its
// offset from p.clock, taking the local time halfway through the query. If
// the read fails or takes longer than p.dbClockTimeout it returns the last
// offset.
func (p *Poller) measureOffset(ctx context.Context) time.Duration {
	rctx, cancel := context.WithTimeout(ctx, p.dbClockTimeout)
	defer cancel()
	before := p.clock.Now()
	dbNow, err := p.hist.Now(rctx)
	after := p.clock.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			p.logf("status: drift scan: reading the database clock: %v; keeping the last offset (%s)", err, p.dbOffset)
		}
		return p.dbOffset
	}
	p.dbOffset = dbNow.Sub(before.Add(after.Sub(before) / 2))
	return p.dbOffset
}

// scanVM reads one team VM and judges it. offset is the database clock's
// offset from p.clock.
func (p *Poller) scanVM(ctx context.Context, vm proxmox.VM, offset time.Duration) (scanResult, error) {
	team, _, _ := p.rules.naming.ParseVMName(vm.Name)
	readAt := p.clock.Now().Add(offset)
	cfg, snaps, err := p.readVM(ctx, vm.Node, vm.VMID)
	if err != nil {
		return scanResult{}, err
	}
	return scanResult{vmid: vm.VMID, readAt: readAt, drift: p.rules.configDrift(team, cfg, pods.SnapshotNames(snaps))}, nil
}

// readVM reads a VM's config and snapshots, one shared call slot at a time.
func (p *Poller) readVM(ctx context.Context, node string, vmid int) (map[string]string, []proxmox.Snapshot, error) {
	var cfg map[string]string
	var snaps []proxmox.Snapshot
	if err := p.lim.Call(ctx, func() (err error) { cfg, err = p.api.VMConfig(ctx, node, vmid); return }); err != nil {
		return nil, nil, err
	}
	if err := p.lim.Call(ctx, func() (err error) { snaps, err = p.api.Snapshots(ctx, node, vmid); return }); err != nil {
		return nil, nil, err
	}
	return cfg, snaps, nil
}
