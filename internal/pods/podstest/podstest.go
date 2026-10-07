// Package podstest is an in-memory Proxmox cluster implementing pods.API,
// for tests of the code that drives it: VMs with configs, disks on a
// shared storage, snapshots and power state, tasks that finish at once,
// queued failures (FailOn), and hooks that hold or fail calls. Tests wrap
// it in their package for whatever else they model (a credential checked
// per call, privileges, task lists).
//
// It doesn't import pods, so pods' own tests can use it.
package podstest

import (
	"context"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
)

// VM is a VM in the fake cluster.
type VM struct {
	proxmox.VM
	Config    map[string]string
	Snapshots []string
	SnapTimes map[string]int64 // snaptime by snapshot name; missing is 0
	// SnapStates is snapstate by snapshot name; missing is "" (finished).
	SnapStates map[string]string
}

// Fake is the cluster. Tasks complete instantly. FailOn queues errors for
// calls keyed like "clone:10105" or "cloudinit:10105"; each call pops one
// error. Mu guards every field: a test that reads or changes them while
// calls may run holds it.
type Fake struct {
	Mu    sync.Mutex
	Nodes []string // OnlineNodes
	VMs   map[int]*VM
	fail  map[string][]error
	// Calls are the keys of the calls made, in order (see record).
	Calls []string
	// Deleted are the VMIDs DeleteVM took off the cluster, in order.
	Deleted []int
	// Gate, if set, runs at the start of every call, without Mu held, with
	// the call's key ("cluster", "nodes", "status:<vmid>" or the key the
	// call records). A non-nil error fails the call before it reaches the
	// cluster; done, if non-nil, runs when the call returns.
	Gate func(ctx context.Context, key string) (done func(), err error)
	// CloneThenFail maps a new VMID to an error returned after the clone was
	// actually created, like a POST that times out after Proxmox accepted it.
	CloneThenFail map[int]error
	// LockConfig maps a VMID to how many VMConfig reads still report
	// lock=clone.
	LockConfig map[int]int
	// LockName is the lock value reported while LockConfig is positive
	// ("clone" if empty). Writes that arrive while locked are recorded as
	// "locked-write:<vmid>" but still succeed.
	LockName string
	// LockHidesName drops "name" from configs read while LockConfig is
	// positive, like a VM whose clone has not written its config yet.
	LockHidesName bool
	// PowerHook, if set, runs on every Power call before it takes effect.
	PowerHook func(vmid int, action string)
	// OnRecord, if set, runs on every recorded call, with Mu held.
	OnRecord func(key string)
	// WaitHook, if set, runs on every WaitTask after it is recorded; a non-nil
	// result fails the wait.
	WaitHook func(ctx context.Context, upid string) error
	// StopHook, if set, runs on every StopTask after it is recorded; its
	// result is StopTask's.
	StopHook func(upid string) error
	// WaitGate, if set, runs on every WaitTask after WaitHook, without Mu
	// held, so it can block until the test lets the task finish.
	WaitGate func(ctx context.Context, upid string) error
	// HalfDestroy makes a DeleteVM of these VMIDs act like a qmdestroy task
	// that deleted the disks and then died: the config is left holding only
	// lock=destroyed, and the listing shows the VM as "VM <vmid>". The test
	// queues the task's error on its wait.
	HalfDestroy map[int]bool
	// DeleteLeaves makes a DeleteVM of these VMIDs start a task that does
	// nothing: the VM is still there afterwards.
	DeleteLeaves map[int]bool
	// Unseen lists VMIDs held by VMs ClusterVMs doesn't show, as for a
	// user without VM.Audit on them.
	Unseen map[int]bool
	// StuckShutdown lists VMIDs whose shutdown task leaves them running.
	StuckShutdown map[int]bool
	// ConvertLeavesDisk makes a ConvertToTemplate of these VMIDs act like a
	// qmtemplate task that set template: 1 and then timed out on the storage
	// lock before renaming the disks to base-*. The test queues the task's
	// error, if any, on its wait.
	ConvertLeavesDisk map[int]bool
	// Vols is the shared storage's content: each volume's volid (as a disk
	// key's value names it, "storage:name") and the VMID that owns it. Add
	// and Clone create a VM's volumes; deletes free them.
	Vols map[string]int
	// DeleteKeepsDisks makes a DeleteVM of these VMIDs act like a qmdestroy
	// task that timed out on the storage lock while freeing the disks and
	// still ended OK: the VM is gone but its volumes stay on the storage.
	DeleteKeepsDisks map[int]bool
	// ConvertThenFail maps a VMID to an error a ConvertToTemplate returns
	// after converting it, like a request whose answer was lost.
	ConvertThenFail map[int]error
	// DeleteThenFail makes a DeleteVM of these VMIDs start a destroy and then
	// fail with the error, like a request that timed out after Proxmox
	// accepted it. The destroy holds lock=destroyed until it finishes,
	// which takes DestroyReads more config reads of the VM; destroying
	// counts down those reads by VMID.
	DeleteThenFail map[int]error
	DestroyReads   int
	destroying     map[int]int
	// SnapshotThenFail maps a VMID to an error a CreateSnapshot returns
	// after taking the snapshot, like a request whose answer was lost.
	SnapshotThenFail map[int]error
	// SnapshotNoop makes a CreateSnapshot of these VMIDs start a task that
	// ends OK without taking the snapshot.
	SnapshotNoop map[int]bool
	// SnapshotLeaves maps a VMID to the snapstate a snapshot taken of it
	// is left in, like a task that died part way.
	SnapshotLeaves map[int]string
	// SnapshotReqs are the snapshot requests made, in order.
	SnapshotReqs []proxmox.SnapshotRequest
}

// New returns an empty cluster whose online nodes are nodes.
func New(nodes ...string) *Fake {
	return &Fake{Nodes: nodes, VMs: map[int]*VM{}, fail: map[string][]error{}}
}

// Add puts a VM on the cluster, stopped unless vm says otherwise, with its
// config's disks on the storage. It doesn't take Mu.
func (f *Fake) Add(vm proxmox.VM, cfg map[string]string, snaps ...string) {
	if cfg == nil {
		cfg = map[string]string{}
	}
	if vm.Status == "" {
		vm.Status = "stopped"
	}
	f.VMs[vm.VMID] = &VM{VM: vm, Config: cfg, Snapshots: snaps}
	f.addVolumes(vm.VMID, cfg)
}

// addVolumes puts the disks named in cfg on the storage, owned by vmid.
func (f *Fake) addVolumes(vmid int, cfg map[string]string) {
	if f.Vols == nil {
		f.Vols = map[string]int{}
	}
	for _, vol := range diskVolumes(cfg) {
		f.Vols[vol] = vmid
	}
}

// freeVolumes frees every volume vmid owns.
func (f *Fake) freeVolumes(vmid int) {
	for vol, owner := range f.Vols {
		if owner == vmid {
			delete(f.Vols, vol)
		}
	}
}

// VolumesOf lists the volumes vmid owns, sorted.
func (f *Fake) VolumesOf(vmid int) []string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var out []string
	for vol, owner := range f.Vols {
		if owner == vmid {
			out = append(out, vol)
		}
	}
	sort.Strings(out)
	return out
}

// FailOn queues errors for the calls recorded as key. It doesn't take Mu.
func (f *Fake) FailOn(key string, errs ...error) { f.fail[key] = append(f.fail[key], errs...) }

// Called counts the recorded calls whose key starts with prefix.
func (f *Fake) Called(prefix string) int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// DeletedIDs is Deleted, copied under Mu.
func (f *Fake) DeletedIDs() []int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return slices.Clone(f.Deleted)
}

// SetSnapTime gives one of a VM's snapshots a snaptime.
func (f *Fake) SetSnapTime(vmid int, name string, t int64) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	vm := f.VMs[vmid]
	if vm.SnapTimes == nil {
		vm.SnapTimes = map[string]int64{}
	}
	vm.SnapTimes[name] = t
}

// enter runs Gate, if set, for a call with this key. The call goes ahead
// only on a nil error, and calls done when it returns.
func (f *Fake) enter(ctx context.Context, key string) (done func(), err error) {
	f.Mu.Lock()
	gate := f.Gate
	f.Mu.Unlock()
	if gate != nil {
		done, err = gate(ctx, key)
	}
	if done == nil {
		done = func() {}
	}
	return done, err
}

// noteLockedWrite records a write that reached a VM which still reports a lock.
func (f *Fake) noteLockedWrite(vmid int) {
	if f.LockConfig[vmid] > 0 {
		f.Calls = append(f.Calls, fmt.Sprintf("locked-write:%d", vmid))
	}
}

// record logs a call and returns a queued error, if any.
func (f *Fake) record(key string) error {
	f.Calls = append(f.Calls, key)
	if f.OnRecord != nil {
		f.OnRecord(key)
	}
	if errs := f.fail[key]; len(errs) > 0 {
		f.fail[key] = errs[1:]
		return errs[0]
	}
	return nil
}

// get finds a VM on a node, like a per-VM call to real Proxmox: asking the
// wrong node fails the same way as a missing VM.
func (f *Fake) get(node string, vmid int) (*VM, error) {
	vm, ok := f.VMs[vmid]
	if !ok || vm.Node != node {
		return nil, NotExist(node, vmid)
	}
	return vm, nil
}

// NotExist is the error Proxmox answers a per-VM call with when the VM
// isn't on the node.
func NotExist(node string, vmid int) error {
	return &proxmox.APIError{Status: 500, Message: fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", node, vmid)}
}

// UPID is the task ID the fake gives a task of this kind on a VM.
func UPID(node, kind string, vmid int) string {
	return fmt.Sprintf("UPID:%s:0:0:0:%s:%d:battleship@pve!app:", node, kind, vmid)
}

func (f *Fake) ClusterVMs(ctx context.Context) ([]proxmox.VM, error) {
	done, err := f.enter(ctx, "cluster")
	defer done()
	if err != nil {
		return nil, err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var out []proxmox.VM
	for _, vm := range f.VMs {
		v := vm.VM
		v.Lock = vm.Config["lock"] // /cluster/resources lists the config's lock
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out, f.record("cluster")
}

func (f *Fake) OnlineNodes(ctx context.Context) ([]string, error) {
	done, err := f.enter(ctx, "nodes")
	defer done()
	if err != nil {
		return nil, err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return slices.Clone(f.Nodes), nil
}

func (f *Fake) VMConfig(ctx context.Context, node string, vmid int) (map[string]string, error) {
	key := fmt.Sprintf("config:%d", vmid)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return nil, err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return nil, err
	}
	if n, ok := f.destroying[vmid]; ok {
		if n <= 0 {
			delete(f.destroying, vmid)
			f.freeVolumes(vmid)
			delete(f.VMs, vmid)
		} else {
			f.destroying[vmid] = n - 1
		}
	}
	vm, err := f.get(node, vmid)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	maps.Copy(out, vm.Config)
	if vm.Template {
		out["template"] = "1"
	}
	if n := f.LockConfig[vmid]; n > 0 {
		f.LockConfig[vmid] = n - 1
		if f.LockHidesName {
			delete(out, "name")
		}
		out["lock"] = f.LockName
		if out["lock"] == "" {
			out["lock"] = "clone"
		}
	}
	return out, nil
}

func (f *Fake) SetVMConfig(ctx context.Context, node string, vmid int, changes map[string]string) error {
	key := fmt.Sprintf("setconfig:%d", vmid)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return err
	}
	f.noteLockedWrite(vmid)
	vm, err := f.get(node, vmid)
	if err != nil {
		return err
	}
	maps.Copy(vm.Config, changes)
	return nil
}

func (f *Fake) Clone(ctx context.Context, r proxmox.CloneRequest) (string, error) {
	key := fmt.Sprintf("clone:%d", r.NewVMID)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return "", err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return "", err
	}
	src, err := f.get(r.SourceNode, r.SourceVMID)
	if err != nil {
		return "", err
	}
	if !r.Full && !src.Template {
		return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("linked clone source %d is not a template", r.SourceVMID)}
	}
	if _, taken := f.VMs[r.NewVMID]; taken {
		return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("VM %d already exists", r.NewVMID)}
	}
	cfg := map[string]string{}
	maps.Copy(cfg, src.Config)
	cfg["name"] = r.Name
	// Like Proxmox, a clone gets its own disks: a full clone a copy named
	// for the new VMID, a linked clone an overlay on the template's base-*
	// volume.
	disks := diskVolumes(src.Config)
	for i, k := range slices.Sorted(maps.Keys(disks)) {
		storage, name, _ := strings.Cut(disks[k], ":")
		ext := path.Ext(volumeFile(disks[k]))
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
	f.VMs[r.NewVMID] = &VM{VM: proxmox.VM{VMID: r.NewVMID, Name: r.Name, Node: r.TargetNode, Status: "stopped", Pool: r.Pool}, Config: cfg}
	f.addVolumes(r.NewVMID, cfg)
	if err := f.CloneThenFail[r.NewVMID]; err != nil {
		delete(f.CloneThenFail, r.NewVMID)
		return "", err
	}
	return UPID(r.SourceNode, "qmclone", r.SourceVMID), nil
}

func (f *Fake) ConvertToTemplate(ctx context.Context, node string, vmid int) (string, error) {
	key := fmt.Sprintf("template:%d", vmid)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return "", err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
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
	if !f.ConvertLeavesDisk[vmid] {
		// Converting renames each disk's file from vm-* to base-*.
		for k, vol := range diskVolumes(vm.Config) {
			file := volumeFile(vol)
			if strings.HasPrefix(file, "vm-") {
				base := strings.TrimSuffix(vol, file) + "base-" + strings.TrimPrefix(file, "vm-")
				vm.Config[k] = base + strings.TrimPrefix(vm.Config[k], vol)
				if owner, ok := f.Vols[vol]; ok {
					delete(f.Vols, vol)
					f.Vols[base] = owner
				}
			}
		}
	}
	if err := f.ConvertThenFail[vmid]; err != nil {
		delete(f.ConvertThenFail, vmid)
		return "", err
	}
	return UPID(node, "qmtemplate", vmid), nil
}

func (f *Fake) RegenerateCloudInit(ctx context.Context, node string, vmid int) error {
	key := fmt.Sprintf("cloudinit:%d", vmid)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return err
	}
	f.noteLockedWrite(vmid)
	_, err = f.get(node, vmid)
	return err
}

func (f *Fake) Snapshots(ctx context.Context, node string, vmid int) ([]proxmox.Snapshot, error) {
	key := fmt.Sprintf("snapshots:%d", vmid)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return nil, err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
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

func (f *Fake) CreateSnapshot(ctx context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error) {
	key := fmt.Sprintf("snapshot:%d", vmid)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return "", err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return "", err
	}
	f.noteLockedWrite(vmid)
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	f.SnapshotReqs = append(f.SnapshotReqs, r)
	if slices.Contains(vm.Snapshots, r.Name) {
		return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("snapshot name '%s' already used", r.Name)}
	}
	if !f.SnapshotNoop[vmid] {
		vm.Snapshots = append(vm.Snapshots, r.Name)
		if st := f.SnapshotLeaves[vmid]; st != "" {
			if vm.SnapStates == nil {
				vm.SnapStates = map[string]string{}
			}
			vm.SnapStates[r.Name] = st
		}
	}
	if err := f.SnapshotThenFail[vmid]; err != nil {
		delete(f.SnapshotThenFail, vmid)
		return "", err
	}
	return UPID(node, "qmsnapshot", vmid), nil
}

// Rollback rolls a VM back to a snapshot. The VM is left stopped, as
// Proxmox leaves it after rolling back a snapshot without RAM.
func (f *Fake) Rollback(ctx context.Context, node string, vmid int, snapshot string) (string, error) {
	key := fmt.Sprintf("rollback:%d:%s", vmid, snapshot)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return "", err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return "", err
	}
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	vm.Status = "stopped"
	return UPID(node, "qmrollback", vmid), nil
}

func (f *Fake) Power(ctx context.Context, node string, vmid int, action string) (string, error) {
	key := fmt.Sprintf("power:%d:%s", vmid, action)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return "", err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return "", err
	}
	f.noteLockedWrite(vmid)
	if f.PowerHook != nil {
		f.PowerHook(vmid, action)
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
	return UPID(node, "qm"+action, vmid), nil
}

func (f *Fake) Shutdown(ctx context.Context, node string, vmid int, timeout time.Duration, forceStop bool) (string, error) {
	key := fmt.Sprintf("shutdown:%d:%s:%t", vmid, timeout, forceStop)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return "", err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return "", err
	}
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	if !f.StuckShutdown[vmid] {
		vm.Status = "stopped"
	}
	return UPID(node, "qmshutdown", vmid), nil
}

func (f *Fake) CurrentStatus(ctx context.Context, node string, vmid int) (string, error) {
	done, err := f.enter(ctx, fmt.Sprintf("status:%d", vmid))
	defer done()
	if err != nil {
		return "", err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	return vm.Status, nil
}

func (f *Fake) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	key := fmt.Sprintf("delete:%d", vmid)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return "", err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return "", err
	}
	vm, err := f.get(node, vmid)
	if err != nil {
		return "", err
	}
	if lock := vm.Config["lock"]; lock != "" {
		return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("VM is locked (%s)", lock)}
	}
	if err := f.DeleteThenFail[vmid]; err != nil {
		delete(f.DeleteThenFail, vmid)
		vm.Config["lock"] = "destroyed"
		if f.destroying == nil {
			f.destroying = map[int]int{}
		}
		f.destroying[vmid] = f.DestroyReads
		return "", err
	}
	switch {
	case f.HalfDestroy[vmid]:
		f.freeVolumes(vmid)
		vm.Config = map[string]string{"lock": "destroyed"}
		vm.Name = fmt.Sprintf("VM %d", vmid)
	case f.DeleteLeaves[vmid]:
	case f.DeleteKeepsDisks[vmid]:
		delete(f.VMs, vmid)
		f.Deleted = append(f.Deleted, vmid)
	default:
		f.freeVolumes(vmid)
		delete(f.VMs, vmid)
		f.Deleted = append(f.Deleted, vmid)
	}
	return UPID(node, "qmdestroy", vmid), nil
}

func (f *Fake) StorageContent(ctx context.Context, node, storage string, vmid int) ([]string, error) {
	key := fmt.Sprintf("content:%s:%d", storage, vmid)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return nil, err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return nil, err
	}
	var out []string
	for vol, owner := range f.Vols {
		if (vmid == 0 || owner == vmid) && strings.HasPrefix(vol, storage+":") {
			out = append(out, vol)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *Fake) VMIDHeld(ctx context.Context, vmid int) (bool, error) {
	key := fmt.Sprintf("vmid-held:%d", vmid)
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return false, err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return false, err
	}
	_, held := f.VMs[vmid]
	return held || f.Unseen[vmid], nil
}

// DeleteVolume frees a volume, refusing, like Proxmox, a base volume that a
// linked clone's overlay still uses.
func (f *Fake) DeleteVolume(ctx context.Context, node, storage, volid string) (string, error) {
	key := "volume:" + volid
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return "", err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return "", err
	}
	owner, ok := f.Vols[volid]
	if !ok || !strings.HasPrefix(volid, storage+":") {
		return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("no such volume '%s'", volid)}
	}
	for vol := range f.Vols {
		if strings.HasPrefix(vol, volid+"/") {
			return "", &proxmox.APIError{Status: 500, Message: fmt.Sprintf("base volume '%s' is still in use by linked cloned", volid)}
		}
	}
	delete(f.Vols, volid)
	return UPID(node, "imgdel", owner), nil
}

func (f *Fake) WaitTask(ctx context.Context, upid string, _ time.Duration) error {
	key := "wait:" + upid
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return err
	}
	f.Mu.Lock()
	err = f.record(key)
	if err == nil && f.WaitHook != nil {
		err = f.WaitHook(ctx, upid)
	}
	gate := f.WaitGate
	f.Mu.Unlock()
	if err != nil || gate == nil {
		return err
	}
	return gate(ctx, upid)
}

func (f *Fake) StopTask(ctx context.Context, upid string) error {
	key := "stoptask:" + upid
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return err
	}
	f.Mu.Lock()
	err = f.record(key)
	hook := f.StopHook
	f.Mu.Unlock()
	if err == nil && hook != nil {
		err = hook(upid)
	}
	return err
}

// diskKeyRE matches the config keys that hold a VM's disks.
var diskKeyRE = regexp.MustCompile(`^((ide|sata|scsi|virtio)\d+|efidisk0|tpmstate0)$`)

// diskVolumes maps each disk key in cfg to its volume, the part of the
// value before the first comma, leaving out CD-ROMs and empty drives. It is
// the fake's own reading of a config, so a bug in pods' is not hidden.
func diskVolumes(cfg map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range cfg {
		if !diskKeyRE.MatchString(k) {
			continue
		}
		head, opts, _ := strings.Cut(v, ",")
		if head == "" || head == "none" || slices.Contains(strings.Split(opts, ","), "media=cdrom") {
			continue
		}
		out[k] = head
	}
	return out
}

// volumeFile is the last path segment of a volume's name.
func volumeFile(vol string) string {
	_, name, ok := strings.Cut(vol, ":")
	if !ok {
		name = vol
	}
	return name[strings.LastIndex(name, "/")+1:]
}
