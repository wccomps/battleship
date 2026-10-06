package web

import (
	"context"
	"net/url"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

const gib = int64(1) << 30

// resourcesAPI adds /cluster/resources to the fake cluster.
type resourcesAPI struct {
	*fakeAPI
	res *proxmox.Resources
}

func (r resourcesAPI) ClusterResources(context.Context) (proxmox.Resources, error) {
	return *r.res, nil
}

// team 01's dc (8 GiB) and web (4 GiB) are stopped on n1, which has
// free GiB of memory out of 64.
func capacityResources(free int64) *proxmox.Resources {
	return &proxmox.Resources{
		VMs: []proxmox.VM{
			{VMID: 10101, Node: "n1", Status: "stopped", MaxMem: 8 * gib},
			{VMID: 10102, Node: "n1", Status: "stopped", MaxMem: 4 * gib},
		},
		Nodes: []proxmox.NodeResource{{Node: "n1", Status: "online", Mem: (64 - free) * gib, MaxMem: 64 * gib}},
	}
}

// Over capacity, the preview warns with the numbers and shows a bar per
// node, but can still be confirmed; the capacity never changes the
// fingerprint, so a preview isn't made stale by memory moving.
func TestPreviewWarnsAboutCapacity(t *testing.T) {
	h := newHarness(t)
	h.poll()
	res := capacityResources(4)
	h.srv.as = func(proxmox.Credential) pods.API { return resourcesAPI{h.api, res} }
	lead := h.login(asLead)

	action, form, body := h.preview(&lead, "/power", url.Values{"teams": {"1"}, "action": {"start"}})
	contains(t, "over capacity", body,
		`<h2 class="sub">Capacity</h2>`,
		"n1: needs 12 GiB of memory, 4 GiB free.",
		"You can still confirm if the overcommit is deliberate.",
		`<li class="cap-over">`,
		`<span class="bar" aria-hidden="true"><span class="u w95"></span><span class="n w5"></span></span>`,
		"60 GiB used of 64 GiB, needs 12 GiB, 4 GiB free",
	)
	lacks(t, "over capacity", body, ` style=`)
	if action != "/power/confirm" {
		t.Fatalf("no confirm form over capacity: %q", action)
	}
	overFP := form.Get("fingerprint")

	*res = *capacityResources(60)
	_, form, body = h.preview(&lead, "/power", url.Values{"teams": {"1"}, "action": {"start"}})
	contains(t, "plenty of room", body, `<h2 class="sub">Capacity</h2>`, `<li class="cap-ok">`, "4 GiB used of 64 GiB, needs 12 GiB, 60 GiB free")
	lacks(t, "plenty of room", body, "needs 12 GiB of memory", "overcommit")
	if fp := form.Get("fingerprint"); fp != overFP {
		t.Errorf("fingerprint changed with capacity: %s vs %s", fp, overFP)
	}

	// Near full is a softer notice, outside the warnings.
	*res = *capacityResources(16)
	_, _, body = h.preview(&lead, "/power", url.Values{"teams": {"1"}, "action": {"start"}})
	contains(t, "near full", body, `<li class="cap-near">`, `<p class="meta">n1: memory would be 94% used (needs 12 GiB, 16 GiB free)</p>`)
	lacks(t, "near full", body, "overcommit")

	// Stopping needs nothing, so there is no capacity section.
	_, _, body = h.preview(&lead, "/power", url.Values{"teams": {"1"}, "action": {"stop"}})
	lacks(t, "stop", body, `<h2 class="sub">Capacity</h2>`)
}

// Without /cluster/resources (the default fake), previews look as before.
func TestPreviewWithoutResourcesHasNoCapacity(t *testing.T) {
	h := newHarness(t)
	h.poll()
	lead := h.login(asLead)
	_, _, body := h.preview(&lead, "/power", url.Values{"teams": {"1"}, "action": {"start"}})
	lacks(t, "no resources", body, `<h2 class="sub">Capacity</h2>`)
}
