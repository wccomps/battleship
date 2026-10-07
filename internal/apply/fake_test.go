package apply

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

type fakeVM struct {
	proxmox.VM
	Config    map[string]string
	Snapshots []string
	SnapTimes map[string]int64 // snaptime by snapshot name; missing is 0
	// SnapStates is snapstate by snapshot name; missing is "" (finished).
	SnapStates map[string]string
}

// fakeAPI is an in-memory Proxmox. Tasks complete instantly. fail queues
// errors for calls keyed like "clone:10105" or "cloudinit:10105"; each call
// pops one error.
type fakeAPI struct {
	mu    sync.Mutex
	nodes []string
	vms   map[int]*fakeVM
	fail  map[string][]error
	calls []string
	// cloneThenFail maps a new VMID to an error returned after the clone was
	// actually created, like a POST that times out after Proxmox accepted it.
	cloneThenFail map[int]error
	// lockConfig maps a VMID to how many VMConfig reads still report
	// lock=clone.
	lockConfig map[int]int
	// lockName is the lock value reported while lockConfig is positive
	// ("clone" if empty). Writes that arrive while locked are recorded as
	// "locked-write:<vmid>" but still succeed.
	lockName string
	// lockHidesName drops "name" from configs read while lockConfig is
	// positive, like a VM whose clone has not written its config yet.
	lockHidesName bool
	// powerHook, if set, runs on every Power call before it takes effect.
	powerHook func(vmid int, action string)
	// onRecord, if set, runs on every recorded call, with f.mu held.
	onRecord func(key string)
	// waitHook, if set, runs on every WaitTask after it is recorded; a non-nil
	// result fails the wait.
	waitHook func(ctx context.Context, upid string) error
	// stopHook, if set, runs on every StopTask after it is recorded; its
	// result is StopTask's.
	stopHook func(upid string) error
	// waitGate, if set, runs on every WaitTask after waitHook, without f.mu
	// held, so it can block until the test lets the task finish.
	waitGate func(ctx context.Context, upid string) error
	// halfDestroy makes a DeleteVM of these VMIDs act like a qmdestroy task
	// that deleted the disks and then died: the config is left holding only
	// lock=destroyed, and the listing shows the VM as "VM <vmid>". The test
	// queues the task's error on its wait.
	halfDestroy map[int]bool
	// deleteLeaves makes a DeleteVM of these VMIDs start a task that does
	// nothing: the VM is still there afterwards.
	deleteLeaves map[int]bool
	// unseen lists VMIDs held by VMs ClusterVMs doesn't show, as for a
	// user without VM.Audit on them.
	unseen map[int]bool
	// stuckShutdown lists VMIDs whose shutdown task leaves them running.
	stuckShutdown map[int]bool
	// convertLeavesDisk makes a ConvertToTemplate of these VMIDs act like a
	// qmtemplate task that set template: 1 and then timed out on the storage
	// lock before renaming the disks to base-*. The test queues the task's
	// error, if any, on its wait.
	convertLeavesDisk map[int]bool
	// vols is the shared storage's content: each volume's volid (as a disk
	// key's value names it, "storage:name") and the VMID that owns it. add
	// and Clone create a VM's volumes; deletes free them.
	vols map[string]int
	// deleteKeepsDisks makes a DeleteVM of these VMIDs act like a qmdestroy
	// task that timed out on the storage lock while freeing the disks and
	// still ended OK: the VM is gone but its volumes stay on the storage.
	deleteKeepsDisks map[int]bool
	// convertThenFail maps a VMID to an error a ConvertToTemplate returns
	// after converting it, like a request whose answer was lost.
	convertThenFail map[int]error
	// deleteThenFail makes a DeleteVM of these VMIDs start a destroy and then
	// fail with the error, like a request that timed out after Proxmox
	// accepted it. The destroy holds lock=destroyed until it finishes,
	// which takes destroyReads more config reads of the VM; destroying
	// counts down those reads by VMID.
	deleteThenFail map[int]error
	destroyReads   int
	destroying     map[int]int
	// snapshotThenFail maps a VMID to an error a CreateSnapshot returns
	// after taking the snapshot, like a request whose answer was lost.
	snapshotThenFail map[int]error
	// snapshotNoop makes a CreateSnapshot of these VMIDs start a task that
	// ends OK without taking the snapshot.
	snapshotNoop map[int]bool
	// snapshotLeaves maps a VMID to the snapstate a snapshot taken of it
	// is left in, like a task that died part way.
	snapshotLeaves map[int]string
	// snapshotReqs are the snapshot requests made, in order.
	snapshotReqs []proxmox.SnapshotRequest
}

func newFakeAPI(nodes ...string) *fakeAPI {
	return &fakeAPI{nodes: nodes, vms: map[int]*fakeVM{}, fail: map[string][]error{}}
}

func (f *fakeAPI) add(vm proxmox.VM, cfg map[string]string, snaps ...string) {
	if cfg == nil {
		cfg = map[string]string{}
	}
	if vm.Status == "" {
		vm.Status = "stopped"
	}
	f.vms[vm.VMID] = &fakeVM{VM: vm, Config: cfg, Snapshots: snaps}
	f.addVolumes(vm.VMID, cfg)
}

// addVolumes puts the disks named in cfg on the storage, owned by vmid.
func (f *fakeAPI) addVolumes(vmid int, cfg map[string]string) {
	if f.vols == nil {
		f.vols = map[string]int{}
	}
	for _, vol := range pods.DiskVolumes(cfg) {
		f.vols[vol] = vmid
	}
}

// freeVolumes frees every volume vmid owns.
func (f *fakeAPI) freeVolumes(vmid int) {
	for vol, owner := range f.vols {
		if owner == vmid {
			delete(f.vols, vol)
		}
	}
}

// volumesOf lists the volumes vmid owns, sorted.
func (f *fakeAPI) volumesOf(vmid int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for vol, owner := range f.vols {
		if owner == vmid {
			out = append(out, vol)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeAPI) failOn(key string, errs ...error) { f.fail[key] = append(f.fail[key], errs...) }

func (f *fakeAPI) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// noteLockedWrite records a write that reached a VM which still reports a lock.
func (f *fakeAPI) noteLockedWrite(vmid int) {
	if f.lockConfig[vmid] > 0 {
		f.calls = append(f.calls, fmt.Sprintf("locked-write:%d", vmid))
	}
}

// record logs a call and returns a queued error, if any.
func (f *fakeAPI) record(key string) error {
	f.calls = append(f.calls, key)
	if f.onRecord != nil {
		f.onRecord(key)
	}
	if errs := f.fail[key]; len(errs) > 0 {
		f.fail[key] = errs[1:]
		return errs[0]
	}
	return nil
}

// get finds a VM on a node, like a per-VM call to real Proxmox: asking the
// wrong node fails the same way as a missing VM.
func (f *fakeAPI) get(node string, vmid int) (*fakeVM, error) {
	vm, ok := f.vms[vmid]
	if !ok || vm.Node != node {
		return nil, &proxmox.APIError{Status: 500, Message: fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", node, vmid)}
	}
	return vm, nil
}

func upid(node, kind string, vmid int) string {
	return fmt.Sprintf("UPID:%s:0:0:0:%s:%d:battleship@pve!app:", node, kind, vmid)
}

func (f *fakeAPI) ClusterVMs(context.Context) ([]proxmox.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proxmox.VM
	for _, vm := range f.vms {
		v := vm.VM
		v.Lock = vm.Config["lock"] // /cluster/resources lists the config's lock
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out, f.record("cluster")
}

func (f *fakeAPI) OnlineNodes(context.Context) ([]string, error) { return f.nodes, nil }

func (f *fakeAPI) VMConfig(_ context.Context, node string, vmid int) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("config:%d", vmid)); err != nil {
		return nil, err
	}
	if n, ok := f.destroying[vmid]; ok {
		if n <= 0 {
			delete(f.destroying, vmid)
			f.freeVolumes(vmid)
			delete(f.vms, vmid)
		} else {
			f.destroying[vmid] = n - 1
		}
	}
	vm, err := f.get(node, vmid)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range vm.Config {
		out[k] = v
	}
	if vm.Template {
		out["template"] = "1"
	}
	if n := f.lockConfig[vmid]; n > 0 {
		f.lockConfig[vmid] = n - 1
		if f.lockHidesName {
			delete(out, "name")
		}
		out["lock"] = f.lockName
		if out["lock"] == "" {
			out["lock"] = "clone"
		}
	}
	return out, nil
}

func (f *fakeAPI) SetVMConfig(_ context.Context, node string, vmid int, changes map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("setconfig:%d", vmid)); err != nil {
		return err
	}
	f.noteLockedWrite(vmid)
	vm, err := f.get(node, vmid)
	if err != nil {
		return err
	}
	for k, v := range changes {
		vm.Config[k] = v
	}
	return nil
}

func (f *fakeAPI) Clone(_ context.Context, r proxmox.CloneRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("clone:%d", r.NewVMID)); err != nil {
		return "", err
	}
	src, err := f.get(r.SourceNode, r.SourceVMID)
	if err != nil {
		return "", err
	}
	if !r.Full && !src.Template {
		return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("linked clone source %d is not a template", r.SourceVMID)}
	}
	if _, taken := f.vms[r.NewVMID]; taken {
		return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("VM %d already exists", r.NewVMID)}
	}
	cfg := map[string]string{}
	for k, v := range src.Config {
		cfg[k] = v
	}
	cfg["name"] = r.Name
	// Like Proxmox, a clone gets its own disks: a full clone a copy named
	// for the new VMID, a linked clone an overlay on the template's base-*
	// volume.
	disks := pods.DiskVolumes(src.Config)
	for i, k := range slices.Sorted(maps.Keys(disks)) {
		storage, name, _ := strings.Cut(disks[k], ":")
		ext := path.Ext(pods.VolumeFile(disks[k]))
		if ext == "" {
			ext = ".qcow2"
		}
		vol := fmt.Sprintf("%s:%s/%d/vm-%d-disk-%d%s", storage, name, r.NewVMID, r.NewVMID, i, ext)
		if r.Full {
			if r.Storage != "" {
				storage = r.Storage
			}
			vol = fmt.Sprintf("%s:%d/vm-%d-disk-%d%s", storage, r.NewVMID, r.NewVMID, i, ext)
		}
		cfg[k] = vol + strings.TrimPrefix(cfg[k], disks[k])
	}
	f.vms[r.NewVMID] = &fakeVM{VM: proxmox.VM{VMID: r.NewVMID, Name: r.Name, Node: r.TargetNode, Status: "stopped", Pool: r.Pool}, Config: cfg}
	f.addVolumes(r.NewVMID, cfg)
	if err := f.cloneThenFail[r.NewVMID]; err != nil {
		delete(f.cloneThenFail, r.NewVMID)
		return "", err
	}
	return upid(r.SourceNode, "qmclone", r.SourceVMID), nil
}

func (f *fakeAPI) ConvertToTemplate(_ context.Context, node string, vmid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("template:%d", vmid)); err != nil {
		return "", err
	}
	f.noteLockedWrite(vmid)
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	if vm.Template {
		return "", &proxmox.APIError{Status: 500, Message: "you can't convert a template to a template"}
	}
	vm.Template = true
	if !f.convertLeavesDisk[vmid] {
		// Converting renames each disk's file from vm-* to base-*.
		for k, vol := range pods.DiskVolumes(vm.Config) {
			file := pods.VolumeFile(vol)
			if strings.HasPrefix(file, "vm-") {
				base := strings.TrimSuffix(vol, file) + "base-" + strings.TrimPrefix(file, "vm-")
				vm.Config[k] = base + strings.TrimPrefix(vm.Config[k], vol)
				if owner, ok := f.vols[vol]; ok {
					delete(f.vols, vol)
					f.vols[base] = owner
				}
			}
		}
	}
	if err := f.convertThenFail[vmid]; err != nil {
		delete(f.convertThenFail, vmid)
		return "", err
	}
	return upid(node, "qmtemplate", vmid), nil
}

func (f *fakeAPI) RegenerateCloudInit(_ context.Context, node string, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("cloudinit:%d", vmid)); err != nil {
		return err
	}
	f.noteLockedWrite(vmid)
	_, err := f.get(node, vmid)
	return err
}

func (f *fakeAPI) Snapshots(_ context.Context, node string, vmid int) ([]proxmox.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("snapshots:%d", vmid)); err != nil {
		return nil, err
	}
	vm, err := f.get(node, vmid)
	if err != nil {
		return nil, err
	}
	var out []proxmox.Snapshot
	for _, n := range vm.Snapshots {
		out = append(out, proxmox.Snapshot{Name: n, Time: vm.SnapTimes[n], State: vm.SnapStates[n]})
	}
	return out, nil
}

// setSnapTime gives one of a VM's snapshots a snaptime.
func (f *fakeAPI) setSnapTime(vmid int, name string, t int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm := f.vms[vmid]
	if vm.SnapTimes == nil {
		vm.SnapTimes = map[string]int64{}
	}
	vm.SnapTimes[name] = t
}

func (f *fakeAPI) CreateSnapshot(_ context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("snapshot:%d", vmid)); err != nil {
		return "", err
	}
	f.noteLockedWrite(vmid)
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	f.snapshotReqs = append(f.snapshotReqs, r)
	if slices.Contains(vm.Snapshots, r.Name) {
		return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("snapshot name '%s' already used", r.Name)}
	}
	if !f.snapshotNoop[vmid] {
		vm.Snapshots = append(vm.Snapshots, r.Name)
		if st := f.snapshotLeaves[vmid]; st != "" {
			if vm.SnapStates == nil {
				vm.SnapStates = map[string]string{}
			}
			vm.SnapStates[r.Name] = st
		}
	}
	if err := f.snapshotThenFail[vmid]; err != nil {
		delete(f.snapshotThenFail, vmid)
		return "", err
	}
	return upid(node, "qmsnapshot", vmid), nil
}

func (f *fakeAPI) Rollback(_ context.Context, node string, vmid int, snapshot string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("rollback:%d:%s", vmid, snapshot)); err != nil {
		return "", err
	}
	if _, err := f.get(node, vmid); err != nil {
		return "", err
	}
	return upid(node, "qmrollback", vmid), nil
}

func (f *fakeAPI) Power(_ context.Context, node string, vmid int, action string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("power:%d:%s", vmid, action)); err != nil {
		return "", err
	}
	f.noteLockedWrite(vmid)
	if f.powerHook != nil {
		f.powerHook(vmid, action)
	}
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	switch action {
	case "start", "reboot":
		vm.Status = "running"
	case "stop", "shutdown":
		vm.Status = "stopped"
	}
	return upid(node, "qm"+action, vmid), nil
}

func (f *fakeAPI) Shutdown(_ context.Context, node string, vmid int, timeout time.Duration, forceStop bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("shutdown:%d:%s:%t", vmid, timeout, forceStop)); err != nil {
		return "", err
	}
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	if !f.stuckShutdown[vmid] {
		vm.Status = "stopped"
	}
	return upid(node, "qmshutdown", vmid), nil
}

func (f *fakeAPI) CurrentStatus(_ context.Context, node string, vmid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	return vm.Status, nil
}

func (f *fakeAPI) DeleteVM(_ context.Context, node string, vmid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("delete:%d", vmid)); err != nil {
		return "", err
	}
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	if lock := vm.Config["lock"]; lock != "" {
		return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("VM is locked (%s)", lock)}
	}
	if err := f.deleteThenFail[vmid]; err != nil {
		delete(f.deleteThenFail, vmid)
		vm.Config["lock"] = "destroyed"
		if f.destroying == nil {
			f.destroying = map[int]int{}
		}
		f.destroying[vmid] = f.destroyReads
		return "", err
	}
	switch {
	case f.halfDestroy[vmid]:
		f.freeVolumes(vmid)
		vm.Config = map[string]string{"lock": "destroyed"}
		vm.Name = fmt.Sprintf("VM %d", vmid)
	case f.deleteLeaves[vmid]:
	case f.deleteKeepsDisks[vmid]:
		delete(f.vms, vmid)
	default:
		f.freeVolumes(vmid)
		delete(f.vms, vmid)
	}
	return upid(node, "qmdestroy", vmid), nil
}

func (f *fakeAPI) StorageContent(_ context.Context, node, storage string, vmid int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("content:%s:%d", storage, vmid)); err != nil {
		return nil, err
	}
	var out []string
	for vol, owner := range f.vols {
		if (vmid == 0 || owner == vmid) && strings.HasPrefix(vol, storage+":") {
			out = append(out, vol)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeAPI) VMIDHeld(_ context.Context, vmid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fmt.Sprintf("vmid-held:%d", vmid)); err != nil {
		return false, err
	}
	_, held := f.vms[vmid]
	return held || f.unseen[vmid], nil
}

// DeleteVolume frees a volume, refusing, like Proxmox, a base volume that a
// linked clone's overlay still uses.
func (f *fakeAPI) DeleteVolume(_ context.Context, node, storage, volid string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("volume:" + volid); err != nil {
		return "", err
	}
	owner, ok := f.vols[volid]
	if !ok || !strings.HasPrefix(volid, storage+":") {
		return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("no such volume '%s'", volid)}
	}
	for vol := range f.vols {
		if strings.HasPrefix(vol, volid+"/") {
			return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("base volume '%s' is still in use by linked cloned", volid)}
		}
	}
	delete(f.vols, volid)
	return upid(node, "imgdel", owner), nil
}

func (f *fakeAPI) WaitTask(ctx context.Context, upid string, _ time.Duration) error {
	f.mu.Lock()
	err := f.record("wait:" + upid)
	if err == nil && f.waitHook != nil {
		err = f.waitHook(ctx, upid)
	}
	gate := f.waitGate
	f.mu.Unlock()
	if err != nil || gate == nil {
		return err
	}
	return gate(ctx, upid)
}

func (f *fakeAPI) StopTask(_ context.Context, upid string) error {
	f.mu.Lock()
	err := f.record("stoptask:" + upid)
	hook := f.stopHook
	f.mu.Unlock()
	if err == nil && hook != nil {
		err = hook(upid)
	}
	return err
}

// newCluster returns a fake with two tagged masters (teak: 2 NICs,
// cloud-init; oak: GPU) and one untagged VM.
func newCluster() *fakeAPI {
	f := newFakeAPI("cedar", "birch", "spruce")
	f.add(proxmox.VM{VMID: 121, Name: "teak.tango.delta", Node: "cedar", Tags: "dev;tango.delta"}, map[string]string{
		"net0":  "virtio=BC:24:11:00:01:21,bridge=vmbr0",
		"net1":  "virtio=BC:24:11:00:01:22,bridge=vmbr1",
		"ide2":  "competitions:vm-121-cloudinit,media=cdrom",
		"scsi0": "competitions:121/vm-121-disk-0.qcow2,size=32G",
	})
	f.add(proxmox.VM{VMID: 125, Name: "oak.tango.delta", Node: "birch", Tags: "dev"}, map[string]string{
		"net0": "virtio=BC:24:11:00:01:25,bridge=vmbr0",
		"vga":  "virtio-gl,memory=256",
	})
	f.add(proxmox.VM{VMID: 130, Name: "notes.tango.delta", Node: "cedar", Tags: "docs"}, nil)
	return f
}

func testPlanner(f *fakeAPI) pods.Planner {
	cfg := config.Default()
	return pods.NewPlanner(f, cfg)
}

// resourceAPI is the fake with /cluster/resources, counting calls.
type resourceAPI struct {
	*fakeAPI
	res   proxmox.Resources
	calls int
}

func (r *resourceAPI) ClusterResources(context.Context) (proxmox.Resources, error) {
	r.calls++
	return r.res, nil
}
