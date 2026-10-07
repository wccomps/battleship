package jobs

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// stepPrivileges is the privilege table, written out again on its own: a
// step may run if the user holds any of these on the VM.
var stepPrivileges = map[pods.Step][]string{
	pods.StepPower:    {"VM.PowerMgmt"},
	pods.StepStop:     {"VM.PowerMgmt"},
	pods.StepStart:    {"VM.PowerMgmt"},
	pods.StepDelete:   {"VM.Allocate"},
	pods.StepSnapshot: {"VM.Snapshot"},
	pods.StepRollback: {"VM.Snapshot", "VM.Snapshot.Rollback"},
}

// For random privileges: the preview leaves runnable exactly the VMs the
// user may fully act on, and the job touches only those, with its own ticket.
func TestPropertyPreviewOffersOnlyWhatTheUserMayDo(t *testing.T) {
	st := storetest.New(t)
	privs := []string{"VM.PowerMgmt", "VM.Allocate", "VM.Snapshot", "VM.Snapshot.Rollback"}
	n := 0
	rapid.Check(t, func(rt *rapid.T) {
		n++
		f := teamVMs()
		f.pve = pvetest.New(t)
		user := fmt.Sprintf("u%d@auth.example.org", n)
		f.pve.AddUser(user, nil)
		f.pve.Grant(user, "/vms", "VM.Audit")
		for _, vmid := range []int{10105, 10121} {
			held := rapid.SliceOfDistinct(rapid.SampledFrom(privs), rapid.ID[string]).Draw(rt, fmt.Sprintf("privs %d", vmid))
			f.pve.Grant(user, fmt.Sprintf("/vms/%d", vmid), append(held, "VM.Audit")...)
		}
		in := rapid.SampledFrom([]Inputs{
			{Kind: pods.KindPower, Teams: "1", Action: "stop"},
			{Kind: pods.KindTeardown, Teams: "1"},
			{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "round2"},
			{Kind: pods.KindReset, Teams: "1"},
		}).Draw(rt, "op")
		cred := f.pve.Ticket(user)
		cfg := testCfg()
		plan, err := BuildPlan(bg, pods.NewPlanner(f.as(func() proxmox.Credential { return cred }, nil), cfg), in)
		if err != nil {
			rt.Fatal(err)
		}
		mayRun := map[string]bool{}
		for _, it := range plan.Items {
			ok := true
			for _, step := range it.Steps {
				any := false
				for _, p := range stepPrivileges[step] {
					any = any || f.pve.Allowed(user, fmt.Sprintf("/vms/%d", it.VMID), p)
				}
				ok = ok && any
			}
			mayRun[it.Name] = ok
			if ok != (it.Blocked == "") {
				rt.Fatalf("%s %s: may run %v, but blocked %q", in.Kind, it.Name, ok, it.Blocked)
			}
		}
		if len(plan.Runnable()) == 0 {
			return
		}
		id, err := Submit(bg, st, in, plan, Submitter{User: "u", Credential: cred, Seal: mustCreds()})
		if err != nil {
			rt.Fatal(err)
		}
		before := len(f.usedCreds())
		j, err := st.ClaimJob(bg, id, "w1")
		if err != nil || j == nil {
			rt.Fatalf("claim: %v %v", j, err)
		}
		newWorker(st, f).RunJob(bg, j)
		if got := job(t, st, id); got.Status == store.StatusStale || got.Status == store.StatusFailed {
			rt.Fatalf("job %s: %s", got.Status, got.Error)
		}
		for _, c := range f.usedCreds()[before:] {
			if c.User != user || c.Ticket != cred.Ticket {
				rt.Fatalf("a call carried %v, not the job's ticket", c)
			}
		}
		items, _ := st.Items(bg, id)
		for _, it := range items {
			if it.Step != "" && !mayRun[it.Name] {
				rt.Fatalf("the job reached step %q of %s, which the user may not do", it.Step, it.Name)
			}
		}
	})
}
