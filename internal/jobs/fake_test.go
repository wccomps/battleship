package jobs

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
)

// fakeAPI is a minimal in-memory Proxmox for teardown, power and reset
// plans. Methods the tests don't need panic through the nil embedded API.
type fakeAPI struct {
	pods.API
	mu  sync.Mutex
	vms map[int]*proxmox.VM
	// gate, if set, holds every task wait until it is closed or ctx ends.
	gate chan struct{}
	// waiting, if set, gets a value each time a task wait starts at the gate.
	waiting chan struct{}
	deleted []int
	// listGate, if set, holds every ClusterVMs call (planning's first read)
	// until ctx ends, first sending on listing.
	listGate chan struct{}
	listing  chan struct{}
	// deleteErr, if set, fails every DeleteVM of these VMIDs.
	deleteErr map[int]error
	// taken are the snapshots taken, by VMID, after the "initial" every
	// VM has.
	taken map[int][]string
	// accept, if set, decides which credentials Proxmox still takes; the
	// others get 401. used records the credential of every call made
	// through as.
	accept func(proxmox.Credential) bool
	used   []proxmox.Credential
	// pve, if set, answers the privileges of whoever calls (see as).
	pve *pvetest.Server
}

func newFake(vms ...proxmox.VM) *fakeAPI {
	f := &fakeAPI{vms: map[int]*proxmox.VM{}}
	for i := range vms {
		vm := vms[i]
		f.vms[vm.VMID] = &vm
	}
	return f
}

func (f *fakeAPI) ClusterVMs(ctx context.Context) ([]proxmox.VM, error) {
	if f.listGate != nil {
		f.listing <- struct{}{}
		select {
		case <-f.listGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proxmox.VM
	for _, vm := range f.vms {
		out = append(out, *vm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out, nil
}

func (f *fakeAPI) OnlineNodes(context.Context) ([]string, error) { return []string{"n1"}, nil }

func (f *fakeAPI) get(vmid int) (*proxmox.VM, error) {
	vm, ok := f.vms[vmid]
	if !ok {
		return nil, &proxmox.APIError{Status: 500, Message: fmt.Sprintf("Configuration file 'qemu-server/%d.conf' does not exist", vmid)}
	}
	return vm, nil
}

func (f *fakeAPI) CurrentStatus(_ context.Context, _ string, vmid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, err := f.get(vmid)
	if err != nil {
		return "", err
	}
	return vm.Status, nil
}

func (f *fakeAPI) Power(_ context.Context, node string, vmid int, action string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, err := f.get(vmid)
	if err != nil {
		return "", err
	}
	if action == "start" || action == "reboot" {
		vm.Status = "running"
	} else {
		vm.Status = "stopped"
	}
	return fmt.Sprintf("UPID:%s:0:0:0:qm%s:%d:t:", node, action, vmid), nil
}

func (f *fakeAPI) Shutdown(ctx context.Context, node string, vmid int, _ time.Duration, _ bool) (string, error) {
	return f.Power(ctx, node, vmid, "shutdown")
}

func (f *fakeAPI) DeleteVM(_ context.Context, node string, vmid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.get(vmid); err != nil {
		return "", err
	}
	if err := f.deleteErr[vmid]; err != nil {
		return "", err
	}
	delete(f.vms, vmid)
	f.deleted = append(f.deleted, vmid)
	return fmt.Sprintf("UPID:%s:0:0:0:qmdestroy:%d:t:", node, vmid), nil
}

func (f *fakeAPI) Snapshots(_ context.Context, _ string, vmid int) ([]proxmox.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []proxmox.Snapshot{{Name: "initial"}}
	for _, n := range f.taken[vmid] {
		out = append(out, proxmox.Snapshot{Name: n})
	}
	return out, nil
}

func (f *fakeAPI) CreateSnapshot(_ context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.get(vmid); err != nil {
		return "", err
	}
	if f.taken == nil {
		f.taken = map[int][]string{}
	}
	f.taken[vmid] = append(f.taken[vmid], r.Name)
	return fmt.Sprintf("UPID:%s:0:0:0:qmsnapshot:%d:t:", node, vmid), nil
}

// Rollback rolls a VM back to a snapshot: it stops, as Proxmox leaves it.
func (f *fakeAPI) Rollback(_ context.Context, node string, vmid int, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, err := f.get(vmid)
	if err != nil {
		return "", err
	}
	vm.Status = "stopped"
	return fmt.Sprintf("UPID:%s:0:0:0:qmrollback:%d:t:", node, vmid), nil
}

func (f *fakeAPI) WaitTask(ctx context.Context, _ string, _ time.Duration) error {
	f.mu.Lock()
	gate, waiting := f.gate, f.waiting
	f.mu.Unlock()
	if gate == nil {
		return nil
	}
	if waiting != nil {
		waiting <- struct{}{}
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeAPI) deletedIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.deleted...)
}

// VMConfig answers for masters and templates in deploy plans: a one-NIC
// config for any VM the fake has.
func (f *fakeAPI) VMConfig(_ context.Context, _ string, vmid int) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.get(vmid); err != nil {
		return nil, err
	}
	return map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0"}, nil
}

func (f *fakeAPI) VMIDHeld(_ context.Context, vmid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, held := f.vms[vmid]
	return held, nil
}

// StorageContent reports no volumes: the fake's VMs have no disks, and a
// delete frees whatever they had.
func (f *fakeAPI) StorageContent(context.Context, string, string, int) ([]string, error) {
	return nil, nil
}

// as is the fake seen through a credential, as Worker.Bind makes it: each
// call records whose credential it carried, and fails with 401 unless
// accept (if set) takes it.
func (f *fakeAPI) as(src func() proxmox.Credential, refused func()) pods.API {
	if f.pve != nil {
		return &permsFake{boundFake: &boundFake{f: f, src: src, refused: refused}}
	}
	return &boundFake{f: f, src: src, refused: refused}
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

type boundFake struct {
	f       *fakeAPI
	src     func() proxmox.Credential
	refused func() // called on each 401, as Worker.Bind's views do
}

// creds lists the credentials calls carried, in order.
func (f *fakeAPI) usedCreds() []proxmox.Credential {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proxmox.Credential(nil), f.used...)
}

func (b *boundFake) check() error {
	c := b.src()
	b.f.mu.Lock()
	b.f.used = append(b.f.used, c)
	accept := b.f.accept
	b.f.mu.Unlock()
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
	return b.f.ClusterVMs(ctx)
}
func (b *boundFake) OnlineNodes(ctx context.Context) ([]string, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	return b.f.OnlineNodes(ctx)
}
func (b *boundFake) CurrentStatus(ctx context.Context, node string, vmid int) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.f.CurrentStatus(ctx, node, vmid)
}
func (b *boundFake) Power(ctx context.Context, node string, vmid int, action string) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.f.Power(ctx, node, vmid, action)
}
func (b *boundFake) Shutdown(ctx context.Context, node string, vmid int, d time.Duration, force bool) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.f.Shutdown(ctx, node, vmid, d, force)
}
func (b *boundFake) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.f.DeleteVM(ctx, node, vmid)
}
func (b *boundFake) Snapshots(ctx context.Context, node string, vmid int) ([]proxmox.Snapshot, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	return b.f.Snapshots(ctx, node, vmid)
}
func (b *boundFake) CreateSnapshot(ctx context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.f.CreateSnapshot(ctx, node, vmid, r)
}
func (b *boundFake) WaitTask(ctx context.Context, upid string, poll time.Duration) error {
	if err := b.check(); err != nil {
		return err
	}
	return b.f.WaitTask(ctx, upid, poll)
}
func (b *boundFake) VMConfig(ctx context.Context, node string, vmid int) (map[string]string, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	return b.f.VMConfig(ctx, node, vmid)
}
func (b *boundFake) StorageContent(ctx context.Context, node, storage string, vmid int) ([]string, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	return b.f.StorageContent(ctx, node, storage, vmid)
}

// The calls the fake doesn't implement go to the nil API and panic, as
// they do on the fake itself.
func (b *boundFake) SetVMConfig(ctx context.Context, node string, vmid int, c map[string]string) error {
	return b.f.SetVMConfig(ctx, node, vmid, c)
}
func (b *boundFake) Clone(ctx context.Context, r proxmox.CloneRequest) (string, error) {
	return b.f.Clone(ctx, r)
}
func (b *boundFake) ConvertToTemplate(ctx context.Context, node string, vmid int) (string, error) {
	return b.f.ConvertToTemplate(ctx, node, vmid)
}
func (b *boundFake) RegenerateCloudInit(ctx context.Context, node string, vmid int) error {
	return b.f.RegenerateCloudInit(ctx, node, vmid)
}
func (b *boundFake) Rollback(ctx context.Context, node string, vmid int, snap string) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	return b.f.Rollback(ctx, node, vmid, snap)
}
func (b *boundFake) VMIDHeld(ctx context.Context, vmid int) (bool, error) {
	if err := b.check(); err != nil {
		return false, err
	}
	return b.f.VMIDHeld(ctx, vmid)
}
func (b *boundFake) DeleteVolume(ctx context.Context, node, storage, volid string) (string, error) {
	return b.f.DeleteVolume(ctx, node, storage, volid)
}
func (b *boundFake) StopTask(ctx context.Context, upid string) error { return b.f.StopTask(ctx, upid) }
