package pods

import (
	"context"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
)

// API is the subset of *proxmox.Client the planner and executor use.
// Tests substitute an in-memory fake.
type API interface {
	Reads
	Changes
}

// Reads are the API calls still allowed once a run must send no more
// changes: reads, task waits, and StopTask, which only stops a task the run
// started.
type Reads interface {
	ClusterVMs(ctx context.Context) ([]proxmox.VM, error)
	OnlineNodes(ctx context.Context) ([]string, error)
	AgentPing(ctx context.Context, node string, vmid int) error
	AgentRead(ctx context.Context, node string, vmid int, command []string) (int, error)
	AgentExecStatus(ctx context.Context, node string, vmid, pid int, limit int64) (proxmox.ExecStatus, error)
	AgentInterfaces(ctx context.Context, node string, vmid int) ([]proxmox.GuestNIC, error)
	AgentHostname(ctx context.Context, node string, vmid int) (string, error)
	VMConfig(ctx context.Context, node string, vmid int) (map[string]string, error)
	Snapshots(ctx context.Context, node string, vmid int) ([]proxmox.Snapshot, error)
	CurrentStatus(ctx context.Context, node string, vmid int) (string, error)
	StorageContent(ctx context.Context, node, storage string, vmid int) ([]string, error)
	VMIDHeld(ctx context.Context, vmid int) (bool, error)
	WaitTask(ctx context.Context, upid string, poll time.Duration) error
	StopTask(ctx context.Context, upid string) error
}

// Changes are the API calls that change the cluster. A wrapper that must
// see every change (apply's guard, test recorders) embeds Reads only, so
// a new method here doesn't compile until it handles it.
type Changes interface {
	AgentExec(ctx context.Context, node string, vmid int, command []string, input string) (int, error)
	SetVMConfig(ctx context.Context, node string, vmid int, changes map[string]string) error
	Clone(ctx context.Context, r proxmox.CloneRequest) (string, error)
	ConvertToTemplate(ctx context.Context, node string, vmid int) (string, error)
	RegenerateCloudInit(ctx context.Context, node string, vmid int) error
	CreateSnapshot(ctx context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error)
	Rollback(ctx context.Context, node string, vmid int, snapshot string) (string, error)
	Power(ctx context.Context, node string, vmid int, action string) (string, error)
	Shutdown(ctx context.Context, node string, vmid int, timeout time.Duration, forceStop bool) (string, error)
	DeleteVM(ctx context.Context, node string, vmid int) (string, error)
	DeleteVolume(ctx context.Context, node, storage, volid string) (string, error)
}

var _ API = (*proxmox.Client)(nil)
