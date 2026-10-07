package apply

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

func snapshotPlan(t *testing.T, f *fakeAPI, req pods.SnapshotRequest) *pods.Plan {
	t.Helper()
	if req.Teams == nil {
		req.Teams = []string{"01"}
	}
	plan, err := testPlanner(f).Snapshot(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestTakeSnapshot(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, nil, "initial")
	plan := snapshotPlan(t, f, pods.SnapshotRequest{Name: "before-scoring", Description: "round 2", VMState: true})
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), plan)
	if len(res.Failed) != 0 || !slices.Equal(res.Succeeded, []string{"team01-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	want := []proxmox.SnapshotRequest{{Name: "before-scoring", Description: "round 2", VMState: true}}
	if !slices.Equal(f.snapshotReqs, want) {
		t.Errorf("requests = %+v, want %+v", f.snapshotReqs, want)
	}
	if got := f.vms[10121].Snapshots; !slices.Equal(got, []string{"initial", "before-scoring"}) {
		t.Errorf("snapshots = %v", got)
	}
	if f.called("power:") != 0 {
		t.Errorf("calls = %v: taking a snapshot doesn't change power", f.calls)
	}
	if done := rec.find("team01-teak", EventDone); len(done) != 1 || done[0].Step != pods.StepSnapshot {
		t.Errorf("done events = %+v", done)
	}
}

// A snapshot of the name that appeared after the plan was made (someone
// took one in Proxmox) is never replaced: the item fails and nothing is
// sent.
func TestTakeSnapshotNeverReplacesOne(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil, "initial")
	plan := snapshotPlan(t, f, pods.SnapshotRequest{Name: "before-scoring"})
	f.vms[10121].Snapshots = append(f.vms[10121].Snapshots, "before-scoring")

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	err := res.Failed["team01-teak"]
	if err == nil || !strings.Contains(err.Error(), `already has a snapshot named "before-scoring", taken since the preview; battleship never replaces a snapshot`) {
		t.Errorf("error = %v", err)
	}
	if n := f.called("snapshot:"); n != 0 {
		t.Errorf("snapshot POSTed %d times, want 0", n)
	}
}

// A request whose answer was lost took the snapshot: the retry finds it
// and doesn't send it again, and the item succeeds.
func TestTakeSnapshotLostAnswerIsNotResent(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
	plan := snapshotPlan(t, f, pods.SnapshotRequest{Name: "before-scoring"})
	f.snapshotThenFail = map[int]error{10121: &proxmox.APIError{Status: 500, Message: "Connection timed out"}}
	rec := &recorder{}

	res := testExecutor(f, rec).Run(context.Background(), plan)
	if len(res.Failed) != 0 || !slices.Equal(res.Succeeded, []string{"team01-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("snapshot:"); n != 1 {
		t.Errorf("snapshot POSTed %d times, want 1", n)
	}
	for _, e := range rec.find("team01-teak", EventFailed) {
		t.Errorf("unexpected failure event: %+v", e)
	}
}

// The same holds across retry rounds: a round whose request's answer was
// lost for good fails the item, and the next round finds the snapshot
// this run took and accepts it rather than calling it someone else's.
func TestTakeSnapshotRetryRoundAcceptsItsOwnSnapshot(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
	plan := snapshotPlan(t, f, pods.SnapshotRequest{Name: "before-scoring"})
	f.snapshotThenFail = map[int]error{10121: &proxmox.APIError{Status: 400, Message: "bad gateway, permanently"}}

	res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
	if len(res.Failed) != 0 || !slices.Equal(res.Succeeded, []string{"team01-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("snapshot:"); n != 1 {
		t.Errorf("snapshot POSTed %d times, want 1", n)
	}
}

// A task that ended OK is checked: the snapshot must be there, finished.
func TestTakeSnapshotIsVerified(t *testing.T) {
	f := newCluster()
	f.add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar"}, nil)
	f.add(proxmox.VM{VMID: 10125, Name: "team01-oak", Node: "birch"}, nil)
	plan := snapshotPlan(t, f, pods.SnapshotRequest{Name: "before-scoring"})
	f.snapshotNoop = map[int]bool{10121: true}
	f.snapshotLeaves = map[int]string{10125: "prepare"}

	exec := testExecutor(f, &recorder{})
	exec.Cfg.Retry.Rounds = 0
	res := exec.Run(context.Background(), plan)
	if err := res.Failed["team01-teak"]; err == nil || !strings.Contains(err.Error(), `the snapshot task ended, but team01-teak has no snapshot "before-scoring"`) {
		t.Errorf("teak error = %v", err)
	}
	if err := res.Failed["team01-oak"]; err == nil || !strings.Contains(err.Error(), `snapshot "before-scoring" of team01-oak was left unfinished (snapstate prepare); delete it in Proxmox`) {
		t.Errorf("oak error = %v", err)
	}
}

// Deploy's baseline follows the same rule: a request whose answer was lost
// took the baseline, so the retry finds it rather than POSTing again and
// failing "already used".
func TestBaselineLostAnswerIsNotResent(t *testing.T) {
	f := newCluster()
	plan := deployTeak(t, f, "01")
	f.snapshotThenFail = map[int]error{10121: &proxmox.APIError{Status: 500, Message: "Connection timed out"}}

	exec := testExecutor(f, &recorder{})
	exec.Cfg.Retry.Rounds = 0
	res := exec.Run(context.Background(), plan)
	if len(res.Failed) != 0 || !slices.Equal(res.Succeeded, []string{"team01-teak"}) {
		t.Fatalf("result = %+v", res)
	}
	if n := f.called("snapshot:10121"); n != 1 {
		t.Errorf("baseline POSTed %d times, want 1", n)
	}
}
