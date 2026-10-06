package proxmox

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestClusterResourcesReadsVMsNodesAndStorageInOneCall(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /cluster/resources"] = jsonData(`[
		{"type":"qemu","vmid":9005,"name":"teak.x.tpl","node":"cedar","status":"stopped","template":1,"maxmem":8589934592,"maxdisk":34359738368},
		{"type":"qemu","vmid":10105,"name":"team01-teak","node":"cedar","status":"running","maxmem":4294967296,"maxdisk":1.073741824e+10},
		{"type":"lxc","vmid":300,"name":"ct","node":"cedar"},
		{"type":"node","node":"cedar","status":"online","mem":68719476736,"maxmem":274877906944},
		{"type":"node","node":"spruce","status":"offline"},
		{"type":"storage","storage":"competitions","node":"cedar","shared":0,"disk":1000,"maxdisk":5000,"status":"available"},
		{"type":"storage","storage":"cephfs","node":"spruce","shared":1,"disk":10,"maxdisk":20},
		{"type":"pool","pool":"pool-01"},
		{"type":"sdn","sdn":"localnetwork","node":"cedar"}]`)

	res, err := c.ClusterResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests); n != 1 || f.requests[0] != "GET /cluster/resources?" {
		t.Errorf("requests = %v, want one unfiltered /cluster/resources", f.requests)
	}
	if len(res.VMs) != 2 || res.VMs[0].MaxMem != 8<<30 || res.VMs[0].MaxDisk != 32<<30 || res.VMs[1].MaxDisk != 10<<30 || !res.VMs[0].Template {
		t.Errorf("VMs = %+v", res.VMs)
	}
	wantNodes := []NodeResource{{Node: "cedar", Status: "online", Mem: 64 << 30, MaxMem: 256 << 30}, {Node: "spruce", Status: "offline"}}
	if len(res.Nodes) != 2 || res.Nodes[0] != wantNodes[0] || res.Nodes[1] != wantNodes[1] {
		t.Errorf("Nodes = %+v", res.Nodes)
	}
	wantStorage := []StorageResource{
		{Storage: "competitions", Node: "cedar", Disk: 1000, MaxDisk: 5000, Status: "available"},
		{Storage: "cephfs", Node: "spruce", Shared: true, Disk: 10, MaxDisk: 20},
	}
	if len(res.Storage) != 2 || res.Storage[0] != wantStorage[0] || res.Storage[1] != wantStorage[1] {
		t.Errorf("Storage = %+v", res.Storage)
	}
}

func TestClusterNodesReadsClusterStatus(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /cluster/status"] = jsonData(`[
		{"type":"cluster","name":"lab","nodes":3,"quorate":1},
		{"type":"node","name":"spruce","ip":"192.0.2.126","online":0,"nodeid":3},
		{"type":"node","name":"cedar","ip":"192.0.2.123","online":1,"local":1,"nodeid":1}]`)
	nodes, err := c.ClusterNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ClusterNode{{Name: "cedar", IP: "192.0.2.123", Online: true}, {Name: "spruce", IP: "192.0.2.126"}}
	if len(nodes) != 2 || nodes[0] != want[0] || nodes[1] != want[1] {
		t.Errorf("nodes = %+v, want %+v (sorted by name)", nodes, want)
	}
}

func TestSnapshotsReadsNamesAndTimes(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /nodes/n1/qemu/10101/snapshot"] = jsonData(`[
		{"name":"fresh_clone_20261002034615","snaptime":1791000000,"description":""},
		{"name":"no-time"},
		{"name":"current","running":1,"digest":"x"}]`)
	snaps, err := c.Snapshots(context.Background(), "n1", 10101)
	if err != nil {
		t.Fatal(err)
	}
	want := []Snapshot{{Name: "fresh_clone_20261002034615", Time: 1791000000}, {Name: "no-time"}}
	if len(snaps) != 2 || snaps[0] != want[0] || snaps[1] != want[1] {
		t.Errorf("snapshots = %+v, want %+v", snaps, want)
	}
}

func TestSnapshotsReadStateAndVMState(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /nodes/n1/qemu/10101/snapshot"] = jsonData(`[
		{"name":"half","snaptime":5,"snapstate":"prepare"},
		{"name":"with-ram","snaptime":6,"vmstate":1},
		{"name":"current","running":1,"digest":"x"}]`)
	snaps, err := c.Snapshots(context.Background(), "n1", 10101)
	if err != nil {
		t.Fatal(err)
	}
	want := []Snapshot{{Name: "half", Time: 5, State: "prepare"}, {Name: "with-ram", Time: 6, VMState: true}}
	if len(snaps) != 2 || snaps[0] != want[0] || snaps[1] != want[1] {
		t.Errorf("snapshots = %+v, want %+v", snaps, want)
	}
}

func TestCreateSnapshotSendsNameDescriptionAndVMState(t *testing.T) {
	f, c := newFake(t)
	f.routes["POST /nodes/n1/qemu/10101/snapshot"] = jsonData(`"UPID:n1:00001:00002:00003:qmsnapshot:10101:root@pam:"`)
	for _, tc := range []struct {
		req  SnapshotRequest
		want url.Values
	}{
		{SnapshotRequest{Name: "before-scoring", Description: "round 2"}, url.Values{"snapname": {"before-scoring"}, "description": {"round 2"}}},
		{SnapshotRequest{Name: "ram", VMState: true}, url.Values{"snapname": {"ram"}, "description": {""}, "vmstate": {"1"}}},
	} {
		upid, err := c.CreateSnapshot(context.Background(), "n1", 10101, tc.req)
		if err != nil || !strings.Contains(upid, "qmsnapshot") {
			t.Fatalf("CreateSnapshot = %q, %v", upid, err)
		}
		got, err := url.ParseQuery(f.bodies["POST /nodes/n1/qemu/10101/snapshot"])
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: body = %v, want %v", tc.req, got, tc.want)
		}
	}
}
