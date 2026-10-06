package main

import (
	"context"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

const gib = int64(1) << 30

// resourcesAPI adds /cluster/resources to the CLI fake.
type resourcesAPI struct {
	*fakeAPI
	res proxmox.Resources
}

func (r *resourcesAPI) ClusterResources(context.Context) (proxmox.Resources, error) {
	return r.res, nil
}

// The CLI preview prints the same capacity warnings as the web preview,
// and they don't stop the plan.
func TestPowerStartPreviewPrintsCapacity(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"), vm(10702, "team07-web"))
	api := &resourcesAPI{fakeAPI: e.api, res: proxmox.Resources{
		VMs: []proxmox.VM{
			{VMID: 10701, Node: "n1", Status: "stopped", MaxMem: 8 * gib},
			{VMID: 10702, Node: "n1", Status: "stopped", MaxMem: 4 * gib},
		},
		Nodes: []proxmox.NodeResource{{Node: "n1", Status: "online", Mem: 60 * gib, MaxMem: 64 * gib}},
	}}
	e.d.newAPI = func(config.Proxmox) pods.API { return api }
	if code := e.run("power", "-teams", "7", "-action", "start"); code != 0 {
		t.Fatalf("exit %d: %s", code, e.stderr.String())
	}
	out := e.stdout.String()
	for _, want := range []string{
		"Capacity:",
		"n1 memory", "60 GiB used of 64 GiB, needs 12 GiB, 4 GiB free",
		"WARNING: n1: needs 12 GiB of memory, 4 GiB free",
		"Nothing changed. Re-run with -yes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Stopping needs no capacity, so nothing is printed.
func TestPowerStopPreviewHasNoCapacity(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	api := &resourcesAPI{fakeAPI: e.api}
	e.d.newAPI = func(config.Proxmox) pods.API { return api }
	if code := e.run("power", "-teams", "7", "-action", "stop"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(e.stdout.String(), "Capacity") {
		t.Errorf("stop printed capacity:\n%s", e.stdout.String())
	}
}
