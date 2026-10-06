package status

import (
	"context"
	"errors"
	"fmt"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// ErrUnknownCell means the grid has no such team and host.
var ErrUnknownCell = errors.New("no such team VM in the status grid")

// Detail is a live look at one grid cell, for the cell page and the reset
// snapshot picker.
type Detail struct {
	// Cell is the grid cell, with the drift judged from this read.
	Cell      Cell
	Config    map[string]string // the VM's config; nil when it is missing
	Snapshots []string          // its snapshots, in Proxmox's order; nil when it is missing
	// Baseline is the snapshot a reset that names none rolls this VM back
	// to (pods.BaselineSnapshot); empty when it has none or is missing.
	Baseline string
}

// Detail reads the cell's VM config and snapshots now, one shared call slot
// at a time, and judges its drift from them. The read counts as a scan of
// that VM, so the grid shows its result at once; its read time is compared
// with job finish times using the last scan's database clock offset. For a missing VM it reads
// nothing. On a read error it returns the grid's cell with the error.
func (p *Poller) Detail(ctx context.Context, team, host string) (Detail, error) {
	p.mu.Lock()
	c, ok := p.grid.cellCopy(team, host)
	p.mu.Unlock()
	d := Detail{Cell: c}
	if !ok {
		return Detail{}, fmt.Errorf("team %q host %q: %w", team, host, ErrUnknownCell)
	}
	if c.State == StateMissing {
		return d, nil
	}
	readAt := p.clock.Now()
	cfg, snaps, err := p.readVM(ctx, c.Node, c.VMID)
	if err != nil {
		return d, fmt.Errorf("reading %s (VMID %d on %s): %s", c.Name, c.VMID, c.Node, proxmox.Describe(err))
	}
	d.Config, d.Snapshots = cfg, pods.SnapshotNames(snaps)
	d.Baseline, _ = pods.BaselineSnapshot(p.rules.cfg.Deploy, snaps)

	p.mu.Lock()
	// Scan results are kept in the database's clock, by the last scan's
	// measure of its offset.
	dbReadAt := readAt.Add(p.dbOffset)
	p.in.scan.put(c.Name, scanResult{vmid: c.VMID, readAt: dbReadAt, drift: p.rules.configDrift(team, cfg, d.Snapshots)})
	changed := p.update()
	if now, ok := p.grid.cellCopy(team, host); ok && now.VMID == c.VMID {
		d.Cell = now
	}
	p.mu.Unlock()
	p.publish(changed)
	return d, nil
}
