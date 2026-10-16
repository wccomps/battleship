package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// ErrNothingToRun: the plan has no runnable VMs.
var ErrNothingToRun = errors.New("nothing to run: the plan has no runnable VMs")

// Submitter is who submits a job, as the job records it.
type Submitter struct {
	User string // who: a login name from the CLI, an email address from the web
	// Preview, from the web app, is the confirmed preview; the job consumes
	// it (see store.NewJob.Preview).
	Preview *store.PreviewClaim
	// Credential is the submitter's; the job runs with it.
	Credential proxmox.Credential
	Seal       Credentials
}

// Submit stores a job for a confirmed plan built from in; the worker runs it
// only if a replan has the same fingerprint.
func Submit(ctx context.Context, st *store.Store, in Inputs, plan *pods.Plan, by Submitter) (int64, error) {
	if err := in.Validate(); err != nil {
		return 0, err
	}
	if len(plan.Runnable()) == 0 {
		return 0, ErrNothingToRun
	}
	if !by.Credential.Usable() {
		return 0, ErrNoCredential
	}
	inputs, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return 0, err
	}
	items := make([]store.NewItem, len(plan.Items))
	for i, it := range plan.Items {
		items[i] = store.NewItem{Name: it.Name, Team: it.Team, VMID: it.VMID, Blocked: it.Blocked}
	}
	writes, reads := plan.LockKeys()
	return st.CreateJob(ctx, store.NewJob{
		Kind:        string(in.Kind),
		Inputs:      inputs,
		Plan:        planJSON,
		Fingerprint: Fingerprint(plan),
		LockKeys:    storedLockKeys(writes, reads),
		CreatedBy:   by.User,
		CreatedAs:   by.Credential.User,
		Items:       items,
		Preview:     by.Preview,
		Credential:  by.Seal.Seal(by.Credential),
	})
}

// storedLockKeys is a plan's lock keys as the store keeps them: its writes,
// and "read:"+K for each read (see store's blocksSQL and migration 013).
func storedLockKeys(writes, reads []string) []string {
	keys := slices.Clone(writes)
	for _, k := range reads {
		keys = append(keys, "read:"+k)
	}
	return keys
}
