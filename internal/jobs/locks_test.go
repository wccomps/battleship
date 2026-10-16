package jobs

import (
	"context"
	"fmt"
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

// clash is whether two plans' jobs wait for each other, by the lock rule:
// one changes what the other changes or reads.
func clash(a, b *pods.Plan) bool {
	meet := func(x, y []string) bool {
		return slices.ContainsFunc(x, func(k string) bool { return slices.Contains(y, k) })
	}
	return meet(writeKeys(a), writeKeys(b)) || meet(writeKeys(a), readKeys(b)) || meet(readKeys(a), writeKeys(b))
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
	if !clash(deploy, dns) {
		t.Errorf("deploy of teak %v and Set DNS %v share no lock", writeKeys(deploy), writeKeys(dns))
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
	if !clash(add, del) {
		t.Errorf("adding a host from debian-12-v1 %v and deleting it %v share no lock", writeKeys(add), writeKeys(del))
	}
	// A deploy of another environment's hosts doesn't wait for this one.
	other, err := p.Deploy(ctx, pods.DeployRequest{Pattern: "gw.kilo.alpha", Teams: []string{"02"}})
	if err == nil && clash(other, deploy) {
		t.Errorf("deploys of different masters to different teams share a lock: %v, %v", writeKeys(other), writeKeys(deploy))
	}
}

// Only the image's own changes wait: clones of an image share it, and its
// delete, or a register that retires it, waits for them.
func TestCloningAnImageIsShared(t *testing.T) {
	src := func(vmid int) *pods.Plan {
		return &pods.Plan{Kind: pods.KindEnvAdd, Items: []pods.Item{{Name: fmt.Sprintf("h%d.x", vmid), VMID: vmid, Source: &pods.Source{VMID: 8001}}}}
	}
	del := &pods.Plan{Kind: pods.KindImageDelete, Items: []pods.Item{{Name: "debian-12-v1", VMID: 8001}}}
	retire := &pods.Plan{Kind: pods.KindImageRegister, Items: []pods.Item{{Name: "debian-12-v2", VMID: 8002, Source: &pods.Source{VMID: 150}, Supersedes: &pods.Source{VMID: 8001}}}}
	switch {
	case clash(src(2101), src(2201)):
		t.Errorf("two clones of one image wait for each other: %v, %v", readKeys(src(2101)), readKeys(src(2201)))
	case !clash(src(2101), del):
		t.Errorf("a clone %v and the image's delete %v don't wait for each other", writeKeys(src(2101)), writeKeys(del))
	case !clash(src(2101), retire):
		t.Errorf("a clone %v and a register retiring the image %v don't wait for each other", writeKeys(src(2101)), writeKeys(retire))
	}
}

// A deploy locks a master only when it builds the master's template.
func TestDeployLocksTheMasterOnlyWhenBuilding(t *testing.T) {
	for _, c := range []struct {
		exists, rebuild, locked bool
	}{{false, false, true}, {true, true, true}, {true, false, false}} {
		plan := &pods.Plan{Kind: pods.KindDeploy, Teams: []string{"01"},
			Templates: []pods.TemplateSpec{{Name: "teak.kilo.alpha.tpl", VMID: 9001, MasterVMID: 2101, Exists: c.exists, Rebuild: c.rebuild}}}
		if got := slices.Contains(writeKeys(plan), "vmid:2101"); got != c.locked {
			t.Errorf("template exists %v, rebuild %v: master locked %v, want %v (%v)", c.exists, c.rebuild, got, c.locked, writeKeys(plan))
		}
	}
}
