package jobs

import (
	"context"
	"slices"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// envCluster is a base image and an environment whose hosts are deploy
// masters (they carry the dev tag).
func envCluster() *podstest.Fake {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v1", Node: "alder", Template: true, Tags: "base;linux"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:80:02,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=devext01", "net1": "virtio=BC:24:11:00:00:02,bridge=devint01",
			"description": "battleship: base=gw-v1 dns=router"})
	f.Add(proxmox.VM{VMID: 2101, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:00:03,bridge=devint01",
			"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=router"})
	return f
}

func shareKey(a, b *pods.Plan) bool {
	ka := LockKeys(a)
	return slices.ContainsFunc(LockKeys(b), func(k string) bool { return slices.Contains(ka, k) })
}

// Jobs that touch the same VM serialize, whatever their kinds: a deploy
// copying an environment host waits for Set DNS on it, and an environment
// built from an image waits for that image's delete.
func TestJobsTouchingOneVMShareALock(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	p := pods.NewPlanner(envCluster(), cfg)
	deploy, err := p.Deploy(ctx, pods.DeployRequest{Pattern: "teak.kilo.alpha", Teams: []string{"01"}})
	if err != nil {
		t.Fatal(err)
	}
	dns, err := p.EnvDNS(ctx, pods.EnvRequest{Domain: "kilo.alpha", DNS: "teak"})
	if err != nil {
		t.Fatal(err)
	}
	if !shareKey(deploy, dns) {
		t.Errorf("deploy of teak %v and Set DNS %v share no lock", LockKeys(deploy), LockKeys(dns))
	}
	del, err := p.ImageDelete(ctx, pods.ImageRequest{Image: "debian-12-v1"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := pods.ParseHostSpec("oak=debian-12")
	if err != nil {
		t.Fatal(err)
	}
	add, err := p.EnvAdd(ctx, pods.EnvRequest{Domain: "kilo.alpha", Hosts: []pods.HostSpec{host}})
	if err != nil {
		t.Fatal(err)
	}
	if !shareKey(add, del) {
		t.Errorf("adding a host from debian-12-v1 %v and deleting it %v share no lock", LockKeys(add), LockKeys(del))
	}
	// A deploy of another environment's hosts doesn't wait for this one.
	other, err := p.Deploy(ctx, pods.DeployRequest{Pattern: "gw.kilo.alpha", Teams: []string{"02"}})
	if err == nil && shareKey(other, deploy) {
		t.Errorf("deploys of different masters to different teams share a lock: %v, %v", LockKeys(other), LockKeys(deploy))
	}
}
