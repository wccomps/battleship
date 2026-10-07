package apply

import (
	"context"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// newCluster returns a fake with two tagged masters (teak: 2 NICs,
// cloud-init; oak: GPU) and one untagged VM.
func newCluster() *podstest.Fake {
	f := podstest.New("cedar", "birch", "spruce")
	f.Add(proxmox.VM{VMID: 121, Name: "teak.tango.delta", Node: "cedar", Tags: "dev;tango.delta"}, map[string]string{
		"net0":  "virtio=BC:24:11:00:01:21,bridge=vmbr0",
		"net1":  "virtio=BC:24:11:00:01:22,bridge=vmbr1",
		"ide2":  "competitions:vm-121-cloudinit,media=cdrom",
		"scsi0": "competitions:121/vm-121-disk-0.qcow2,size=32G",
	})
	f.Add(proxmox.VM{VMID: 125, Name: "oak.tango.delta", Node: "birch", Tags: "dev"}, map[string]string{
		"net0": "virtio=BC:24:11:00:01:25,bridge=vmbr0",
		"vga":  "virtio-gl,memory=256",
	})
	f.Add(proxmox.VM{VMID: 130, Name: "notes.tango.delta", Node: "cedar", Tags: "docs"}, nil)
	return f
}

func testPlanner(f *podstest.Fake) pods.Planner {
	cfg := config.Default()
	return pods.NewPlanner(f, cfg)
}

// resourceAPI is the fake with /cluster/resources, counting calls.
type resourceAPI struct {
	*podstest.Fake
	res   proxmox.Resources
	calls int
}

func (r *resourceAPI) ClusterResources(context.Context) (proxmox.Resources, error) {
	r.calls++
	return r.res, nil
}
