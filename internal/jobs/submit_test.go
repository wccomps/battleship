package jobs

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func TestSubmitStoresPlan(t *testing.T) {
	st := storetest.New(t)
	f := newFake(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "n1", Status: "running"})
	cfg := testCfg()
	in := Inputs{Kind: pods.KindTeardown, Teams: "1"}
	p, err := BuildPlan(bg, pods.NewPlanner(f, cfg), in)
	if err != nil {
		t.Fatal(err)
	}
	id, err := Submit(bg, st, in, p, Submitter{User: "alice", Credential: aliceTicket(), Seal: mustCreds()})
	if err != nil {
		t.Fatal(err)
	}
	j, err := st.Job(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != store.StatusPending || j.Fingerprint != Fingerprint(p) || j.CreatedBy != "alice" || len(j.LockKeys) != 1 {
		t.Errorf("job = %+v", j)
	}
}

func TestSubmitRejectsNothingToRun(t *testing.T) {
	st := storetest.New(t)
	f := newFake(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "n1", Status: "running"})
	cfg := testCfg()
	p, err := BuildPlan(bg, pods.NewPlanner(f, cfg), Inputs{Kind: pods.KindTeardown, Teams: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Submit(bg, st, Inputs{Kind: pods.KindTeardown, Teams: "7"}, p, Submitter{User: "alice", Credential: aliceTicket(), Seal: mustCreds()}); !errors.Is(err, ErrNothingToRun) {
		t.Errorf("err = %v, want ErrNothingToRun", err)
	}
}

func TestSubmitRecordsRoleAndConsumesPreview(t *testing.T) {
	st := storetest.New(t)
	sess := store.Session{
		ID: "sess-1", Subject: "sub", Groups: []string{}, RefreshToken: "r", CSRF: "c",
		CreatedAt: time.Now(), LastSeen: time.Now(), RefreshedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := st.CreateSession(bg, sess); err != nil {
		t.Fatal(err)
	}
	f := newFake(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "n1", Status: "running"})
	cfg := testCfg()
	in := Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}
	p, err := BuildPlan(bg, pods.NewPlanner(f, cfg), in)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	if err := st.CreatePreview(bg, store.Preview{
		ID: "p1", SessionID: "sess-1", Kind: "power", Inputs: raw, Fingerprint: Fingerprint(p),
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	by := Submitter{User: "alice@example.org", Preview: &store.PreviewClaim{SessionID: "sess-1", ID: "p1"}, Credential: aliceTicket(), Seal: mustCreds()}
	id, err := Submit(bg, st, in, p, by)
	if err != nil {
		t.Fatal(err)
	}
	j, err := st.Job(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.CreatedBy != "alice@example.org" || j.CreatedAs != "alice@auth.example.org" || j.Fingerprint != Fingerprint(p) {
		t.Errorf("job = %+v", j)
	}
	if _, err := Submit(bg, st, in, p, by); !errors.Is(err, store.ErrPreviewUsed) {
		t.Errorf("second submit: err = %v, want ErrPreviewUsed", err)
	}
}
