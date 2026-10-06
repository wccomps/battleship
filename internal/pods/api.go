package pods

import (
	"context"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
)

// API is the subset of *proxmox.Client the planner and executor use.
// Tests substitute an in-memory fake.
type API interface {
	ClusterVMs(ctx context.Context) ([]proxmox.VM, error)
	OnlineNodes(ctx context.Context) ([]string, error)
	VMConfig(ctx context.Context, node string, vmid int) (map[string]string, error)
	SetVMConfig(ctx context.Context, node string, vmid int, changes map[string]string) error
	Clone(ctx context.Context, r proxmox.CloneRequest) (string, error)
	ConvertToTemplate(ctx context.Context, node string, vmid int) (string, error)
	RegenerateCloudInit(ctx context.Context, node string, vmid int) error
	Snapshots(ctx context.Context, node string, vmid int) ([]proxmox.Snapshot, error)
	CreateSnapshot(ctx context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error)
	Rollback(ctx context.Context, node string, vmid int, snapshot string) (string, error)
	Power(ctx context.Context, node string, vmid int, action string) (string, error)
	Shutdown(ctx context.Context, node string, vmid int, timeout time.Duration, forceStop bool) (string, error)
	CurrentStatus(ctx context.Context, node string, vmid int) (string, error)
	DeleteVM(ctx context.Context, node string, vmid int) (string, error)
	StorageContent(ctx context.Context, node, storage string, vmid int) ([]string, error)
	DeleteVolume(ctx context.Context, node, storage, volid string) (string, error)
	VMIDHeld(ctx context.Context, vmid int) (bool, error)
	WaitTask(ctx context.Context, upid string, poll time.Duration) error
	StopTask(ctx context.Context, upid string) error
}

var _ API = (*proxmox.Client)(nil)
