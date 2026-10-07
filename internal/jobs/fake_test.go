package jobs

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
)

// fakeAPI is the shared fake cluster on one node, n1, with what the job
// tests hold or refuse. Its fields are guarded by the fake's Mu.
type fakeAPI struct {
	*podstest.Fake
	// gate, if set, holds every task wait until it is closed or ctx ends.
	gate chan struct{}
	// waiting, if set, gets a value each time a task wait starts at the gate.
	waiting chan struct{}
	// listGate, if set, holds every ClusterVMs call (planning's first read)
	// until ctx ends, first sending on listing.
	listGate chan struct{}
	listing  chan struct{}
	// deleteErr, if set, fails every DeleteVM of these VMIDs.
	deleteErr map[int]error
	// accept, if set, decides which credentials Proxmox still takes; the
	// others get 401. used records the credential of every call made
	// through as.
	accept func(proxmox.Credential) bool
	used   []proxmox.Credential
	// pve, if set, answers the privileges of whoever calls (see as).
	pve *pvetest.Server
}

func newFake(vms ...proxmox.VM) *fakeAPI {
	f := &fakeAPI{Fake: podstest.New("n1")}
	for _, vm := range vms {
		f.addVM(vm)
	}
	f.Gate = f.hold
	return f
}

// addVM puts a VM on the cluster with a one-NIC config, enough for masters
// and templates in deploy plans, and the "initial" snapshot every VM has.
// It doesn't take Mu.
func (f *fakeAPI) addVM(vm proxmox.VM) {
	f.Add(vm, map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0"}, "initial")
}

// taken lists the snapshots taken of a VM after its "initial".
func (f *fakeAPI) taken(vmid int) []string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]string(nil), f.VMs[vmid].Snapshots[1:]...)
}

// hold is the fake's Gate: it holds listings at listGate and task waits at
// gate, and fails deletes in deleteErr.
func (f *fakeAPI) hold(ctx context.Context, key string) (func(), error) {
	f.Mu.Lock()
	gate, waiting, listGate, listing := f.gate, f.waiting, f.listGate, f.listing
	var deleteErr error
	if id, ok := strings.CutPrefix(key, "delete:"); ok {
		vmid, _ := strconv.Atoi(id)
		deleteErr = f.deleteErr[vmid]
	}
	f.Mu.Unlock()
	switch {
	case key == "cluster" && listGate != nil:
		listing <- struct{}{}
		select {
		case <-listGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case strings.HasPrefix(key, "wait:") && gate != nil:
		if waiting != nil {
			waiting <- struct{}{}
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, deleteErr
}

// as is the fake seen through a credential, as Worker.Bind makes it: each
// call records whose credential it carried, and fails with 401 unless
// accept (if set) takes it.
func (f *fakeAPI) as(src func() proxmox.Credential, refused func()) pods.API {
	if f.pve != nil {
		return &permsFake{boundFake: &boundFake{Fake: f.Fake, f: f, src: src, refused: refused}}
	}
	return &boundFake{Fake: f.Fake, f: f, src: src, refused: refused}
}

// permsFake is boundFake that also reads the caller's privileges, from
// the fake's pvetest, with the credential of the call.
type permsFake struct{ *boundFake }

// view is the pvetest client as the call's credential, calling refused on
// a 401 as Worker.Bind's views do.
func (p *permsFake) view() *proxmox.Client {
	v := p.f.pve.Client().AsSource(p.src)
	if p.refused != nil {
		v = v.WhenRefused(p.refused)
	}
	return v
}

func (p *permsFake) Permissions(ctx context.Context) (proxmox.Permissions, error) {
	return p.view().Permissions(ctx)
}

func (p *permsFake) PermissionsAt(ctx context.Context, path string) (map[string]bool, error) {
	return p.view().PermissionsAt(ctx, path)
}

// boundFake checks the credential of the calls below; the others go
// straight to the fake.
type boundFake struct {
	*podstest.Fake
	f       *fakeAPI
	src     func() proxmox.Credential
	refused func() // called on each 401, as Worker.Bind's views do
}

// usedCreds lists the credentials calls carried, in order.
func (f *fakeAPI) usedCreds() []proxmox.Credential {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]proxmox.Credential(nil), f.used...)
}

func (b *boundFake) check() error {
	c := b.src()
	b.f.Mu.Lock()
	b.f.used = append(b.f.used, c)
	accept := b.f.accept
	b.f.Mu.Unlock()
	if !c.Usable() {
		return proxmox.ErrNoCredential
	}
	if accept != nil && !accept(c) {
		if b.refused != nil {
			b.refused()
		}
		return &proxmox.APIError{Method: "GET", Path: "/x", Status: 401, Message: "authentication failure"}
	}
	return nil
}

func (b *boundFake) ClusterVMs(ctx context.Context) ([]proxmox.VM, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	return b.Fake.ClusterVMs(ctx)
}
func (b *boundFake) OnlineNodes(ctx context.Context) ([]string, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	return b.Fake.OnlineNodes(ctx)
}
func (b *boundFake) CurrentStatus(ctx context.Context, node string, vmid int) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.Fake.CurrentStatus(ctx, node, vmid)
}
func (b *boundFake) Power(ctx context.Context, node string, vmid int, action string) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.Fake.Power(ctx, node, vmid, action)
}
func (b *boundFake) Shutdown(ctx context.Context, node string, vmid int, d time.Duration, force bool) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.Fake.Shutdown(ctx, node, vmid, d, force)
}
func (b *boundFake) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.Fake.DeleteVM(ctx, node, vmid)
}
func (b *boundFake) Snapshots(ctx context.Context, node string, vmid int) ([]proxmox.Snapshot, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	return b.Fake.Snapshots(ctx, node, vmid)
}
func (b *boundFake) CreateSnapshot(ctx context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.Fake.CreateSnapshot(ctx, node, vmid, r)
}
func (b *boundFake) WaitTask(ctx context.Context, upid string, poll time.Duration) error {
	if err := b.check(); err != nil {
		return err
	}
	return b.Fake.WaitTask(ctx, upid, poll)
}
func (b *boundFake) VMConfig(ctx context.Context, node string, vmid int) (map[string]string, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	return b.Fake.VMConfig(ctx, node, vmid)
}
func (b *boundFake) StorageContent(ctx context.Context, node, storage string, vmid int) ([]string, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	return b.Fake.StorageContent(ctx, node, storage, vmid)
}
func (b *boundFake) Rollback(ctx context.Context, node string, vmid int, snap string) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.Fake.Rollback(ctx, node, vmid, snap)
}
func (b *boundFake) VMIDHeld(ctx context.Context, vmid int) (bool, error) {
	if err := b.check(); err != nil {
		return false, err
	}
	return b.Fake.VMIDHeld(ctx, vmid)
}
