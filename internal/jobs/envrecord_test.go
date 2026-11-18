package jobs

import (
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// withSlotPools gives f a Proxmox access layer holding the slot pools, the
// first with an admin's comment, where alice holds privs on every pool;
// it returns a ticket of hers.
func withSlotPools(t *testing.T, f *fakeAPI, privs ...string) proxmox.Credential {
	t.Helper()
	f.pve = pvetest.New(t)
	for i, s := range pods.Slots(testCfg().Environments) {
		comment := ""
		if i == 0 {
			comment = "spare"
		}
		f.pve.SetPool(s.Pool, comment)
	}
	f.pve.AddUser("alice@auth.example.org", nil)
	if len(privs) > 0 {
		f.pve.Grant("alice@auth.example.org", "/pool", privs...)
	}
	return f.pve.Ticket("alice@auth.example.org")
}

// A create of no machines records the domain in the first slot pool's
// comment, as the submitter, keeping the rest; Environments then reads that
// slot as the domain's, but only to someone with Pool.Audit on it.
func TestCreateWithoutMachinesRecordsTheDomain(t *testing.T) {
	st := storetest.New(t)
	f := newFake()
	cred := withSlotPools(t, f, "Pool.Audit", "Pool.Allocate")
	pool := pods.Slots(testCfg().Environments)[0].Pool
	id, plan := submitAs(t, st, f, Inputs{Kind: pods.KindEnvCreate, Domain: "lima.alpha"}, cred)
	if len(plan.Items) != 1 || plan.Items[0].Blocked != "" || plan.Items[0].Pool != pool {
		t.Fatalf("items = %+v", plan.Items)
	}
	newWorker(st, f).runJob(bg, claim(t, st))
	if j := job(t, st, id); j.Status != store.StatusSucceeded {
		t.Fatalf("job = %s: %s", j.Status, j.Error)
	}
	if got := f.pve.PoolComment(pool); got != "spare [env lima.alpha]" {
		t.Fatalf("comment = %q", got)
	}
	envs, _, err := pods.NewPlanner(f.as(func() proxmox.Credential { return cred }, nil), testCfg()).Environments(bg)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := pods.EnvByDomain(envs, "lima.alpha"); !ok || e.Slot.Pool != pool || !e.Empty() {
		t.Fatalf("lima.alpha = %+v (found %v)", e, ok)
	}
	f.pve.Revoke("alice@auth.example.org", "/pool")
	envs, _, err = pods.NewPlanner(f.as(func() proxmox.Credential { return cred }, nil), testCfg()).Environments(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pods.EnvByDomain(envs, "lima.alpha"); ok || !envs[0].Free() {
		t.Errorf("without Pool.Audit the record shows: %+v", envs[0])
	}
}

func TestCreateWithoutMachinesNeedsPoolAllocate(t *testing.T) {
	f := newFake()
	cred := withSlotPools(t, f, "Pool.Audit")
	plan, err := BuildPlan(bg, pods.NewPlanner(f.as(func() proxmox.Credential { return cred }, nil), testCfg()), Inputs{Kind: pods.KindEnvCreate, Domain: "lima.alpha"})
	if err != nil {
		t.Fatal(err)
	}
	pool := pods.Slots(testCfg().Environments)[0].Pool
	if len(plan.Items) != 1 || plan.Items[0].Blocked != "you don't have Pool.Allocate on /pool/"+pool {
		t.Fatalf("items = %+v", plan.Items)
	}
}

// A failed create of no machines retries whole: its record item is no VM
// to narrow to.
func TestRetryOfACreateWithoutMachines(t *testing.T) {
	plan := pods.Plan{Kind: pods.KindEnvCreate, Slot: "01", Locks: []string{"slot:01", "domain:lima.alpha"},
		Items: []pods.Item{{Name: "dev-01", Pool: "dev-01", Record: "lima.alpha", Steps: []pods.Step{pods.StepRecord}}}}
	orig := Inputs{Kind: pods.KindEnvCreate, Domain: "lima.alpha"}
	r, err := RetryInputs(finishedJob(t, store.StatusCompletedWithFailures, orig, plan), []store.Item{{Name: "dev-01", Status: store.ItemFailed}})
	if err != nil {
		t.Fatal(err)
	}
	if r.VMs != nil || r.Slot != "01" || r.Validate() != nil {
		t.Errorf("retry inputs = %+v (%v)", r, r.Validate())
	}
}
