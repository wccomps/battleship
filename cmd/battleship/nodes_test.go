package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// nodesAPI answers only /cluster/status; anything else panics through the
// nil embedded API, which proves the command only reads it.
type nodesAPI struct {
	pods.API
	nodes []proxmox.ClusterNode
	err   error
}

func (f *nodesAPI) ClusterNodes(context.Context) ([]proxmox.ClusterNode, error) {
	return f.nodes, f.err
}

func TestNodesListsNodesAndPrintsURLs(t *testing.T) {
	e := newEnv(t, false, "")
	api := &nodesAPI{nodes: []proxmox.ClusterNode{
		{Name: "alder", IP: "192.0.2.121", Online: true},
		{Name: "cedar", IP: "192.0.2.123", Online: true},
		{Name: "spruce", IP: "192.0.2.126", Online: false},
		{Name: "v6", IP: "fd00::7", Online: true},
		{Name: "noip", Online: true},
	}}
	e.d.newAPI = func(config.Proxmox) pods.API { return api }
	if code := e.run("nodes"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, e.stderr.String())
	}
	out := e.stdout.String()
	for _, want := range []string{
		"alder", "online", "192.0.2.121",
		"spruce", "offline", "192.0.2.126",
		`urls = ["https://192.0.2.121:8006", "https://192.0.2.123:8006", "https://192.0.2.126:8006", "https://[fd00::7]:8006"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "noip") || strings.Contains(out, "https://:8006") {
		t.Errorf("a node without an IP should be listed but left out of urls:\n%s", out)
	}
}

func TestNodesReportsErrors(t *testing.T) {
	e := newEnv(t, false, "")
	e.d.newAPI = func(config.Proxmox) pods.API {
		return &nodesAPI{err: errors.New("GET /cluster/status: 403 Permission check failed")}
	}
	if code := e.run("nodes"); code != 1 || !strings.Contains(e.stderr.String(), "403") {
		t.Errorf("exit %d, stderr %q", code, e.stderr.String())
	}
	if code := e.run("nodes", "extra"); code != 2 {
		t.Errorf("extra argument: exit %d, want 2", code)
	}
}

func TestNewProxmoxAPIUsesEveryURL(t *testing.T) {
	api := newProxmoxAPI(config.Proxmox{URLs: []string{"https://192.0.2.121:8006", "https://192.0.2.123:8006"}})
	c, ok := api.(*proxmox.Client)
	if !ok {
		t.Fatalf("newProxmoxAPI returned %T", api)
	}
	if got := c.Endpoints(); len(got) != 2 || got[1] != "192.0.2.123:8006" {
		t.Errorf("Endpoints = %v", got)
	}
}
