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
		change func(f *fakeAPI) // between submission and the run
		status string
		clones bool
	}{
		{name: "still approved", change: func(*fakeAPI) {}, status: store.StatusSucceeded, clones: true},
		{name: "the tag removed from a master", change: func(f *fakeAPI) { f.VMs[5021].Config["tags"] = "dev" }, status: store.StatusFailed},
		{name: "the tag no longer registered", change: func(f *fakeAPI) { f.Tags = proxmox.TagAccess{CaseSensitive: true} }, status: store.StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := storetest.New(t)
			f := twoTeams()
			f.Tags = proxmox.TagAccess{Registered: []string{"approved"}, CaseSensitive: true}
			// VMID 9021 gives pod 01's clone VMID 10121, which is free.
			f.addVM(proxmox.VM{VMID: 5021, Name: "teak.kilo.alpha", Node: "n1", Status: "stopped", Tags: "dev;approved"})
			f.addVM(proxmox.VM{VMID: 9021, Name: "teak.kilo.alpha.tpl", Node: "n1", Template: true})
			id := submit(t, st, f, Inputs{Kind: pods.KindDeploy, Teams: "1", Pattern: "*.kilo.alpha", ApprovedOnly: true})
			f.Mu.Lock()
			tc.change(f)
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

// approved_only belongs to a deploy of one set: Normalize drops it elsewhere,
// and Validate refuses it with a pattern that names no set.
func TestApprovedOnlyInputs(t *testing.T) {
	if in := (Inputs{Kind: pods.KindDeploy, Teams: "51", Pattern: "*.kilo.alpha", ApprovedOnly: true}).Normalize(); !in.ApprovedOnly || in.Validate() != nil {
		t.Errorf("a deploy of one set: approved_only %t, Validate %v", in.ApprovedOnly, in.Validate())
	}
	if in := (Inputs{Kind: pods.KindTeardown, Teams: "51", ApprovedOnly: true}).Normalize(); in.ApprovedOnly {
		t.Error("Normalize kept approved_only on a teardown")
	}
	for _, pattern := range []string{"*", "dc.*", "*.kilo.*", "kilo.alpha"} {
		in := Inputs{Kind: pods.KindDeploy, Teams: "51", Pattern: pattern, ApprovedOnly: true}
		if err := in.Validate(); err == nil || !strings.Contains(err.Error(), "names one set") {
			t.Errorf("approved_only with pattern %q: Validate = %v", pattern, err)
		}
		in.ApprovedOnly = false
		if err := in.Validate(); err != nil {
			t.Errorf("a volunteer's deploy of %q is refused: %v", pattern, err)
		}
	}
}
