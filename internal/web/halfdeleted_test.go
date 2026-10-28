package web

import (
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/proxmox"
)

func TestEnvironmentsPageShowsHalfDeletedVMs(t *testing.T) {
	h := newHarness(t)
	addCatalog(h)
	addEnvs(h)
	h.api.Add(proxmox.VM{VMID: 2104, Name: "VM 2104", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "stopped"},
		map[string]string{"lock": "destroyed"})
	h.api.Add(proxmox.VM{VMID: 2350, Name: "VM 2350", Node: "n1", Status: "stopped"}, map[string]string{"lock": "destroyed"})
	sess := h.login(asLead)
	body := h.get(&sess, "/environments").Body.String()
	if got := strings.Join(hostNames(envSection(t, body, "kilo.alpha")), " "); got != "gw yew teak VM 2104 elm" {
		t.Errorf("kilo.alpha hosts %q", got)
	}
	contains(t, "dev-03", envSection(t, body, "slot-03"), `<span class="hname">VM 2350</span>`)
	lacks(t, "Create form", h.get(&sess, "/environments/new").Body.String(), `value="03"`)
}
