package jobs

import (
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// A session deploy runs only while its set is approved: approval taken away
// after the job was submitted ends it before anything is cloned.
func TestWorkerRefusesDeployOfSetNoLongerApproved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tags   string // the master's tags when the job runs
		status string
		clones bool
	}{
		{name: "still approved", tags: "dev;approved", status: store.StatusSucceeded, clones: true},
		{name: "approval removed", tags: "dev", status: store.StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := storetest.New(t)
			f := twoTeams()
			// VMID 5021 gives pod 01's clone VMID 10121, which is free.
			f.addVM(proxmox.VM{VMID: 5021, Name: "teak.kilo.alpha", Node: "n1", Status: "stopped", Tags: "dev;approved"})
			id := submit(t, st, f, Inputs{Kind: pods.KindDeploy, Teams: "1", Pattern: "*.kilo.alpha", ApprovedOnly: true})
			f.Mu.Lock()
			f.VMs[5021].Config["tags"] = tc.tags // where the fake's listing reads them
			f.Mu.Unlock()

			newWorker(st, f).runJob(bg, claim(t, st))

			j := job(t, st, id)
			if j.Status != tc.status {
				t.Fatalf("job = %s (%s), want %s", j.Status, j.Error, tc.status)
			}
			if cloned := f.Called("clone:") > 0; cloned != tc.clones {
				t.Errorf("cloned = %t, want %t", cloned, tc.clones)
			}
			if !tc.clones && !strings.Contains(j.Error, "not approved for self-service sessions") {
				t.Errorf("the job doesn't say why it didn't run: %q", j.Error)
			}
		})
	}
}

// Only a deploy carries approved_only; Normalize keeps it there and drops it
// elsewhere.
func TestNormalizeKeepsApprovedOnlyOnDeploys(t *testing.T) {
	if in := (Inputs{Kind: pods.KindDeploy, Teams: "51", Pattern: "*.kilo.alpha", ApprovedOnly: true}).Normalize(); !in.ApprovedOnly {
		t.Error("Normalize dropped approved_only from a deploy")
	}
	if in := (Inputs{Kind: pods.KindTeardown, Teams: "51", ApprovedOnly: true}).Normalize(); in.ApprovedOnly {
		t.Error("Normalize kept approved_only on a teardown")
	}
}
