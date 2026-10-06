package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// VM is one entry from /cluster/resources.
type VM struct {
	VMID     int
	Name     string
	Node     string
	Status   string // running, stopped, ...
	Template bool
	Tags     string // semicolon-separated
	Pool     string
	MaxMem   int64 // configured memory, bytes
	MaxDisk  int64 // boot disk size, bytes
	// Lock is the config's lock, e.g. "clone", or "destroyed" for a VM a
	// failed delete left half-deleted.
	Lock string
}

// NodeResource is a node entry from /cluster/resources.
type NodeResource struct {
	Node   string
	Status string // online, offline, unknown
	Mem    int64  // used memory, bytes
	MaxMem int64  // total memory, bytes
}

// StorageResource is a storage entry from /cluster/resources: one per node
// the storage is on, shared or not.
type StorageResource struct {
	Storage string
	Node    string
	Shared  bool
	Disk    int64 // used, bytes
	MaxDisk int64 // size, bytes
	Status  string
}

// Resources is the cluster's VMs, nodes and storage from one call.
type Resources struct {
	VMs     []VM
	Nodes   []NodeResource
	Storage []StorageResource
}

// resource is one /cluster/resources entry of any type.
type resource struct {
	Type     string      `json:"type"`
	VMID     json.Number `json:"vmid"`
	Name     string      `json:"name"`
	Node     string      `json:"node"`
	Status   string      `json:"status"`
	Template json.Number `json:"template"`
	Tags     string      `json:"tags"`
	Pool     string      `json:"pool"`
	Storage  string      `json:"storage"`
	Shared   json.Number `json:"shared"`
	Mem      json.Number `json:"mem"`
	MaxMem   json.Number `json:"maxmem"`
	Disk     json.Number `json:"disk"`
	MaxDisk  json.Number `json:"maxdisk"`
	Lock     string      `json:"lock"`
}

func (r resource) vm() (VM, error) {
	id, err := r.VMID.Int64()
	if err != nil {
		return VM{}, fmt.Errorf("bad vmid %q: %w", r.VMID, err)
	}
	return VM{
		VMID: int(id), Name: r.Name, Node: r.Node, Status: r.Status,
		Template: r.Template.String() == "1", Tags: r.Tags, Pool: r.Pool,
		MaxMem: int64Of(r.MaxMem), MaxDisk: int64Of(r.MaxDisk), Lock: r.Lock,
	}, nil
}

// int64Of reads a number (a size or a time) Proxmox sent as an integer or a
// float; missing or malformed is 0.
func int64Of(n json.Number) int64 {
	if i, err := n.Int64(); err == nil {
		return i
	}
	if f, err := n.Float64(); err == nil {
		return int64(f)
	}
	return 0
}

// ClusterVMs lists the cluster's VMs, named (see nameNewVMs), from one
// read of /cluster/resources.
func (c *Client) ClusterVMs(ctx context.Context) ([]VM, error) {
	res, err := c.ClusterResources(ctx)
	if err != nil {
		return nil, err
	}
	down := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Status != "" && n.Status != "online" {
			down[n.Node] = true
		}
	}
	return c.nameNewVMs(ctx, res.VMs, down)
}

// nameNewVMs fills in the name and template flag of VMs /cluster/resources
// listed without them. Proxmox takes both from pvestatd's reports, every
// 10s, so a new VM is nameless until the next one; without this, an
// operation planned right after a deploy would skip the VMs it just made.
// A VM deleted since the listing is dropped. One on a node listed as not
// online (down) isn't asked about, and one whose read fails transiently,
// keeps no name; any other failure fails the listing rather than leave a
// VM out of a plan.
func (c *Client) nameNewVMs(ctx context.Context, vms []VM, down map[string]bool) ([]VM, error) {
	out := vms[:0]
	for _, vm := range vms {
		if vm.Name == "" && !down[vm.Node] {
			cfg, err := c.VMConfig(ctx, vm.Node, vm.VMID)
			switch m := (Classifier{}).Classify(err); {
			case err == nil:
				vm.Name = cfg["name"]
				vm.Template = cfg["template"] == "1"
			case m == NotFound:
				continue
			case m != Transient:
				return nil, fmt.Errorf("VM %d on %s is too new to be listed with its name, and its config can't be read: %w", vm.VMID, vm.Node, err)
			}
		}
		out = append(out, vm)
	}
	return out, nil
}

// ClusterResources reads the cluster's VMs, nodes and storage in one call.
func (c *Client) ClusterResources(ctx context.Context) (Resources, error) {
	var raw []resource
	if err := c.do(ctx, http.MethodGet, "/cluster/resources", nil, &raw); err != nil {
		return Resources{}, err
	}
	var res Resources
	for _, r := range raw {
		switch r.Type {
		case "qemu":
			vm, err := r.vm()
			if err != nil {
				return Resources{}, err
			}
			res.VMs = append(res.VMs, vm)
		case "node":
			res.Nodes = append(res.Nodes, NodeResource{Node: r.Node, Status: r.Status, Mem: int64Of(r.Mem), MaxMem: int64Of(r.MaxMem)})
		case "storage":
			res.Storage = append(res.Storage, StorageResource{
				Storage: r.Storage, Node: r.Node, Shared: r.Shared.String() == "1",
				Disk: int64Of(r.Disk), MaxDisk: int64Of(r.MaxDisk), Status: r.Status,
			})
		}
	}
	return res, nil
}

// ClusterNode is a node entry from /cluster/status.
type ClusterNode struct {
	Name   string
	IP     string
	Online bool
}

// ClusterNodes lists the cluster's nodes, online or not, sorted by name.
func (c *Client) ClusterNodes(ctx context.Context) ([]ClusterNode, error) {
	var raw []struct {
		Type   string      `json:"type"`
		Name   string      `json:"name"`
		IP     string      `json:"ip"`
		Online json.Number `json:"online"`
	}
	if err := c.do(ctx, http.MethodGet, "/cluster/status", nil, &raw); err != nil {
		return nil, err
	}
	var nodes []ClusterNode
	for _, r := range raw {
		if r.Type == "node" {
			nodes = append(nodes, ClusterNode{Name: r.Name, IP: r.IP, Online: r.Online.String() == "1"})
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}

// OnlineNodes returns online node names sorted alphabetically.
func (c *Client) OnlineNodes(ctx context.Context) ([]string, error) {
	var raw []struct {
		Node   string `json:"node"`
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, "/nodes", nil, &raw); err != nil {
		return nil, err
	}
	var nodes []string
	for _, n := range raw {
		if n.Status == "online" {
			nodes = append(nodes, n.Node)
		}
	}
	sort.Strings(nodes)
	return nodes, nil
}

// VMConfig returns the VM's current config with every value as a string.
func (c *Client) VMConfig(ctx context.Context, node string, vmid int) (map[string]string, error) {
	var raw map[string]any
	if err := c.do(ctx, http.MethodGet, vmPath(node, vmid)+"/config", nil, &raw); err != nil {
		return nil, err
	}
	cfg := make(map[string]string, len(raw))
	for k, v := range raw {
		cfg[k] = fmt.Sprint(v)
	}
	return cfg, nil
}

func (c *Client) SetVMConfig(ctx context.Context, node string, vmid int, changes map[string]string) error {
	params := url.Values{}
	for k, v := range changes {
		params.Set(k, v)
	}
	return c.do(ctx, http.MethodPut, vmPath(node, vmid)+"/config", params, nil)
}

type CloneRequest struct {
	SourceNode string
	SourceVMID int
	NewVMID    int
	Name       string
	TargetNode string
	Pool       string
	Full       bool
	Storage    string // full clones only
}

// Clone starts a clone and returns its task UPID.
func (c *Client) Clone(ctx context.Context, r CloneRequest) (string, error) {
	params := url.Values{
		"newid": {strconv.Itoa(r.NewVMID)},
		"name":  {r.Name},
		"full":  {boolParam(r.Full)},
	}
	if r.TargetNode != "" {
		params.Set("target", r.TargetNode)
	}
	if r.Pool != "" {
		params.Set("pool", r.Pool)
	}
	if r.Full && r.Storage != "" {
		params.Set("storage", r.Storage)
	}
	var upid string
	err := c.do(ctx, http.MethodPost, vmPath(r.SourceNode, r.SourceVMID)+"/clone", params, &upid)
	return requireUPID(upid, err, "clone")
}

// ConvertToTemplate returns a task UPID, or "" if Proxmox finished synchronously.
func (c *Client) ConvertToTemplate(ctx context.Context, node string, vmid int) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodPost, vmPath(node, vmid)+"/template", nil, &upid)
	return upid, err
}

// RegenerateCloudInit rebuilds the cloud-init drive from the current config.
func (c *Client) RegenerateCloudInit(ctx context.Context, node string, vmid int) error {
	return c.do(ctx, http.MethodPut, vmPath(node, vmid)+"/cloudinit", nil, nil)
}

// Snapshot is one of a VM's snapshots.
type Snapshot struct {
	Name string
	Time int64 // snaptime, Unix seconds; 0 when Proxmox gave none
	// State is snapstate: "" for a finished snapshot, else what Proxmox is
	// doing to it ("prepare" while it is taken, "delete" while it is
	// removed), or was doing when its task died.
	State   string
	VMState bool // it holds the VM's RAM
}

// Snapshots returns a VM's snapshots in Proxmox's order, excluding the
// "current" pseudo-snapshot.
func (c *Client) Snapshots(ctx context.Context, node string, vmid int) ([]Snapshot, error) {
	var raw []struct {
		Name      string      `json:"name"`
		SnapTime  json.Number `json:"snaptime"`
		SnapState string      `json:"snapstate"`
		VMState   json.Number `json:"vmstate"`
	}
	if err := c.do(ctx, http.MethodGet, vmPath(node, vmid)+"/snapshot", nil, &raw); err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, s := range raw {
		if s.Name != "current" {
			out = append(out, Snapshot{Name: s.Name, Time: int64Of(s.SnapTime), State: s.SnapState, VMState: int64Of(s.VMState) != 0})
		}
	}
	return out, nil
}

// SnapshotRequest is a snapshot to take.
type SnapshotRequest struct {
	Name        string
	Description string
	VMState     bool // also save the RAM of a running VM
}

// CreateSnapshot starts taking a snapshot and returns its task's UPID.
func (c *Client) CreateSnapshot(ctx context.Context, node string, vmid int, r SnapshotRequest) (string, error) {
	params := url.Values{"snapname": {r.Name}, "description": {r.Description}}
	if r.VMState {
		params.Set("vmstate", "1")
	}
	var upid string
	err := c.do(ctx, http.MethodPost, vmPath(node, vmid)+"/snapshot", params, &upid)
	return requireUPID(upid, err, "snapshot")
}

func (c *Client) Rollback(ctx context.Context, node string, vmid int, snapshot string) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodPost, vmPath(node, vmid)+"/snapshot/"+url.PathEscape(snapshot)+"/rollback", nil, &upid)
	return requireUPID(upid, err, "rollback")
}

// Power runs start, stop (hard), shutdown (ACPI) or reboot.
func (c *Client) Power(ctx context.Context, node string, vmid int, action string) (string, error) {
	switch action {
	case "start", "stop", "shutdown", "reboot":
	default:
		return "", fmt.Errorf("unknown power action %q", action)
	}
	var upid string
	err := c.do(ctx, http.MethodPost, vmPath(node, vmid)+"/status/"+action, nil, &upid)
	return requireUPID(upid, err, "power "+action)
}

// Shutdown asks the guest to shut down cleanly (ACPI, or the guest agent).
// Proxmox waits up to timeout (whole seconds) for it; with forceStop it then
// hard-stops the VM, so the task ends with the VM stopped either way.
func (c *Client) Shutdown(ctx context.Context, node string, vmid int, timeout time.Duration, forceStop bool) (string, error) {
	params := url.Values{"timeout": {strconv.Itoa(int(timeout / time.Second))}}
	if forceStop {
		params.Set("forceStop", "1")
	}
	var upid string
	err := c.do(ctx, http.MethodPost, vmPath(node, vmid)+"/status/shutdown", params, &upid)
	return requireUPID(upid, err, "shutdown")
}

func (c *Client) CurrentStatus(ctx context.Context, node string, vmid int) (string, error) {
	var raw struct {
		Status string `json:"status"`
	}
	err := c.do(ctx, http.MethodGet, vmPath(node, vmid)+"/status/current", nil, &raw)
	return raw.Status, err
}

// DeleteVM destroys the VM, removes it from Proxmox's backup and
// replication jobs and from pools, and deletes unreferenced disks.
func (c *Client) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodDelete, vmPath(node, vmid),
		url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}, &upid)
	return requireUPID(upid, err, "delete")
}

// StorageContent lists the volids on storage that Proxmox reports for vmid
// (every volume when vmid is 0), as node sees it, e.g.
// "competitions:9008/base-9008-disk-0.qcow2".
// Proxmox doesn't filter import/ files by vmid, so those come back for any
// vmid; callers match the names they expect.
func (c *Client) StorageContent(ctx context.Context, node, storage string, vmid int) ([]string, error) {
	var raw []struct {
		VolID string `json:"volid"`
	}
	path := fmt.Sprintf("/nodes/%s/storage/%s/content", url.PathEscape(node), url.PathEscape(storage))
	params := url.Values{}
	if vmid > 0 {
		params.Set("vmid", strconv.Itoa(vmid))
	}
	if err := c.do(ctx, http.MethodGet, path, params, &raw); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.VolID)
	}
	return out, nil
}

// VMIDHeld reports whether some VM or container in the cluster has vmid,
// whether or not the user may see it: Proxmox answers this for any user,
// from the cluster's own list.
func (c *Client) VMIDHeld(ctx context.Context, vmid int) (bool, error) {
	params := url.Values{"vmid": {strconv.Itoa(vmid)}}
	err := c.do(ctx, http.MethodGet, "/cluster/nextid", params, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == 400 && strings.Contains(apiErr.Message, "already exists") {
		return true, nil
	}
	return false, err
}

// DeleteVolume frees a storage volume such as
// "competitions:10105/vm-10105-disk-0.qcow2". It returns the task's UPID, or
// "" if Proxmox freed it synchronously. Proxmox refuses to free a base volume
// that linked clones still use.
func (c *Client) DeleteVolume(ctx context.Context, node, storage, volid string) (string, error) {
	path := fmt.Sprintf("/nodes/%s/storage/%s/content/%s", url.PathEscape(node), url.PathEscape(storage), url.PathEscape(volid))
	var upid string
	err := c.do(ctx, http.MethodDelete, path, nil, &upid)
	return upid, err
}

// requireUPID rejects a successful response that carried no task ID, which
// would otherwise look like a finished task.
func requireUPID(upid string, err error, what string) (string, error) {
	if err == nil && upid == "" {
		return "", fmt.Errorf("%s: Proxmox returned no task ID", what)
	}
	return upid, err
}

func boolParam(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
