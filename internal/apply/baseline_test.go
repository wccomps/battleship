package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// A reset that names no snapshot rolls each VM back to deploy.snapshot_name.
func TestResetDefaultsToBaseline(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial", "pre-inject")
	f.Add(proxmox.VM{VMID: 10130, Name: "team01-notes", Node: "cedar"}, nil, "initial")
	f.Add(proxmox.VM{VMID: 10131, Name: "team01-mail", Node: "cedar"}, nil, "before-scoring")

	rs, err := testPlanner(f).Reset(context.Background(), []string{"01"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]pods.Item{}
	for _, it := range rs.Items {
		got[it.Name] = it
	}
	for _, name := range []string{"team01-teak", "team01-notes"} {
		it := got[name]
		if it.Snapshot != "initial" || it.Blocked != "" {
			t.Errorf("%s = snapshot %q blocked %q; want initial", name, it.Snapshot, it.Blocked)
		}
	}
	if it := got["team01-mail"]; it.Blocked == "" || !strings.HasPrefix(it.Blocked, `no snapshot "initial"`) || !strings.Contains(it.Blocked, "before-scoring") {
		t.Errorf("team01-mail = %+v, want blocked with no initial", it)
	}

	res := testExecutor(f, &recorder{}).Run(context.Background(), rs)
	if len(res.Failed) != 0 || f.Called("rollback:10121:initial") != 1 || f.Called("rollback:10130:initial") != 1 {
		t.Errorf("result %+v, calls %v", res, f.Calls)
	}
}

// An unfinished snapshot is no baseline: a deploy takes snapshot_name.
func TestDeployIgnoresUnfinishedBaseline(t *testing.T) {
	f := newCluster()
	testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	vm := f.VMs[10121]
	vm.Snapshots = []string{"initial"}
	vm.SnapStates = map[string]string{"initial": "delete"}
	vm.Status = "stopped"
	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	if len(res.Failed) != 0 || f.Called("snapshot:10121") != 2 {
		t.Errorf("result %+v, calls %v", res, f.Calls)
	}
}
