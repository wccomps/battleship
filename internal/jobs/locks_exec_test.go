package jobs

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// changeLog is an API that records the VMID of every change sent through it.
type changeLog struct {
	pods.API
	mu      sync.Mutex
	changed []int
}

func (c *changeLog) saw(vmid int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changed = append(c.changed, vmid)
}

func (c *changeLog) take() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.changed
	c.changed = nil
	return out
}

func (c *changeLog) SetVMConfig(ctx context.Context, node string, vmid int, ch map[string]string) error {
	c.saw(vmid)
	return c.API.SetVMConfig(ctx, node, vmid, ch)
}
func (c *changeLog) AgentExec(ctx context.Context, node string, vmid int, cmd []string, in string) (int, error) {
	c.saw(vmid)
	return c.API.AgentExec(ctx, node, vmid, cmd, in)
}
func (c *changeLog) Clone(ctx context.Context, r proxmox.CloneRequest) (string, error) {
	c.saw(r.NewVMID)
	return c.API.Clone(ctx, r)
}
func (c *changeLog) ConvertToTemplate(ctx context.Context, node string, vmid int) (string, error) {
	c.saw(vmid)
	return c.API.ConvertToTemplate(ctx, node, vmid)
}
func (c *changeLog) RegenerateCloudInit(ctx context.Context, node string, vmid int) error {
	c.saw(vmid)
	return c.API.RegenerateCloudInit(ctx, node, vmid)
}
func (c *changeLog) CreateSnapshot(ctx context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error) {
	c.saw(vmid)
	return c.API.CreateSnapshot(ctx, node, vmid, r)
}
func (c *changeLog) Rollback(ctx context.Context, node string, vmid int, snap string) (string, error) {
	c.saw(vmid)
	return c.API.Rollback(ctx, node, vmid, snap)
}
func (c *changeLog) Power(ctx context.Context, node string, vmid int, action string) (string, error) {
	c.saw(vmid)
	return c.API.Power(ctx, node, vmid, action)
}
func (c *changeLog) Shutdown(ctx context.Context, node string, vmid int, timeout time.Duration, force bool) (string, error) {
	c.saw(vmid)
	return c.API.Shutdown(ctx, node, vmid, timeout, force)
}
func (c *changeLog) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	c.saw(vmid)
	return c.API.DeleteVM(ctx, node, vmid)
}

var volumeVMID = regexp.MustCompile(`(?:^|[:/])(?:vm|base)-(\d+)-`)

func (c *changeLog) DeleteVolume(ctx context.Context, node, storage, volid string) (string, error) {
	if m := volumeVMID.FindStringSubmatch(volid); m != nil {
		id, _ := strconv.Atoi(m[1])
		c.saw(id)
	}
	return c.API.DeleteVolume(ctx, node, storage, volid)
}

// Every VM a job changes is one of its lock keys, so jobs that change it
// wait for each other: the keys follow what the executor really does.
func TestJobsLockEveryVMTheyChange(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	f := envCluster()
	f.Add(proxmox.VM{VMID: 150, Name: "deb-fresh", Node: "alder", Status: "stopped"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:01:50,bridge=vmbr0"})
	api := &changeLog{API: f}
	p := pods.NewPlanner(f, cfg)
	exec := &apply.Executor{API: api, Cfg: cfg, Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
	host, err := pods.ParseHostSpec("oak=debian-12")
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name string
		plan func() (*pods.Plan, error)
	}{
		{"deploy", func() (*pods.Plan, error) {
			return p.Deploy(ctx, pods.DeployRequest{Pattern: "teak.kilo.alpha", Teams: []string{"01"}, Snapshot: true})
		}},
		{"snapshot", func() (*pods.Plan, error) {
			return p.Snapshot(ctx, pods.SnapshotRequest{Teams: []string{"01"}, Name: "round2"})
		}},
		{"reset", func() (*pods.Plan, error) { return p.Reset(ctx, []string{"01"}, nil, "round2") }},
		{"power", func() (*pods.Plan, error) { return p.Power(ctx, []string{"01"}, nil, "stop") }},
		{"rebuild", func() (*pods.Plan, error) {
			return p.Deploy(ctx, pods.DeployRequest{Pattern: "teak.kilo.alpha", Teams: []string{"02"}, Rebuild: true})
		}},
		{"teardown", func() (*pods.Plan, error) { return p.Teardown(ctx, []string{"01", "02"}, nil) }},
		{"env add", func() (*pods.Plan, error) {
			return p.EnvAdd(ctx, pods.EnvRequest{Domain: "kilo.alpha", Hosts: []pods.HostSpec{host}})
		}},
		{"env dns", func() (*pods.Plan, error) { return p.EnvDNS(ctx, pods.EnvRequest{Domain: "kilo.alpha", DNS: "router"}) }},
		{"image register", func() (*pods.Plan, error) {
			return p.ImageRegister(ctx, pods.ImageRequest{VMID: 150, Family: "debian-12"})
		}},
		{"image edit", func() (*pods.Plan, error) { return p.ImageEdit(ctx, pods.ImageRequest{Family: "debian-12"}) }},
		{"image delete", func() (*pods.Plan, error) { return p.ImageDelete(ctx, pods.ImageRequest{Image: "debian-12-v1"}) }},
	}
	for _, s := range steps {
		plan, err := s.plan()
		if err != nil {
			t.Fatalf("%s: planning: %v", s.name, err)
		}
		writes, _ := LockKeys(plan)
		exec.Run(ctx, plan)
		changed := api.take()
		if len(changed) == 0 && len(plan.Runnable()) > 0 {
			t.Errorf("%s: ran %d items and changed nothing; the step list is stale", s.name, len(plan.Runnable()))
		}
		for _, id := range changed {
			if !slices.Contains(writes, "vmid:"+strconv.Itoa(id)) {
				t.Errorf("%s changed VMID %d, which isn't among its lock keys %v", s.name, id, writes)
			}
		}
	}
}
