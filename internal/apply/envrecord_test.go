package apply

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func recordPlan(kind pods.Kind, step pods.Step, items ...pods.Item) *pods.Plan {
	return &pods.Plan{Kind: kind, Items: append(items, pods.Item{Name: "dev-02", Pool: "dev-02", Record: "kilo.alpha", Steps: []pods.Step{step}, Last: step == pods.StepClearRecord})}
}

// The record step writes the marker into the pool's comment, keeping the
// admins' text; run again, it changes nothing.
func TestRecordWritesTheMarkerKeepingTheComment(t *testing.T) {
	f := podstest.New("alder")
	f.PoolComments = map[string]string{"dev-02": "spare slot"}
	rec := &recorder{}
	res := testExecutor(f, rec).Run(context.Background(), recordPlan(pods.KindEnvCreate, pods.StepRecord))
	if len(res.Succeeded) != 1 || f.PoolComments["dev-02"] != "spare slot [env kilo.alpha]" {
		t.Fatalf("result %+v, comment %q", res, f.PoolComments["dev-02"])
	}
	if len(rec.find("dev-02", EventDone)) != 1 {
		t.Errorf("events = %+v", rec.events)
	}
	res = testExecutor(f, &recorder{}).Run(context.Background(), recordPlan(pods.KindEnvCreate, pods.StepRecord))
	if len(res.Succeeded) != 1 || f.Called("pool-comment:") != 1 {
		t.Errorf("again: result %+v, %d writes", res, f.Called("pool-comment:"))
	}
}

// A pool Proxmox doesn't list (absent, or without Pool.Audit) fails the
// step rather than writing over a comment it couldn't read.
func TestRecordOfAnUnlistedPoolFails(t *testing.T) {
	f := podstest.New("alder")
	res := testExecutor(f, &recorder{}).Run(context.Background(), recordPlan(pods.KindEnvCreate, pods.StepRecord))
	if res.Failed["dev-02"] == nil || f.Called("pool-comment:") != 0 {
		t.Errorf("result %+v, %d writes", res, f.Called("pool-comment:"))
	}
}

func dcItem() pods.Item {
	return pods.Item{Name: "dc.kilo.alpha", Host: "dc", VMID: 2201, Node: "alder", Pool: "dev-02", Steps: []pods.Step{pods.StepStop, pods.StepDelete}}
}

func recordedSlot() *podstest.Fake {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 2201, Name: "dc.kilo.alpha", Node: "alder", Pool: "dev-02"}, nil)
	f.PoolComments = map[string]string{"dev-02": "spare [env kilo.alpha]"}
	return f
}

// A teardown clears the record only after its VMs are deleted.
func TestClearRecordRunsAfterTheDeletes(t *testing.T) {
	f := recordedSlot()
	res := testExecutor(f, &recorder{}).Run(context.Background(), recordPlan(pods.KindEnvTeardown, pods.StepClearRecord, dcItem()))
	del, clr := slices.Index(f.Calls, "delete:2201"), slices.Index(f.Calls, "pool-comment:dev-02")
	if len(res.Succeeded) != 2 || del < 0 || clr < del || f.PoolComments["dev-02"] != "spare" {
		t.Errorf("result %+v, calls %q, comment %q", res, f.Calls, f.PoolComments["dev-02"])
	}
}

// A VM the teardown couldn't delete keeps the record. A permanent failure
// gets no retry round, so the clear ends failed once.
func TestClearRecordWaitsForAFailedDelete(t *testing.T) {
	f := recordedSlot()
	f.FailOn("delete:2201", &proxmox.APIError{Status: 403, Message: "Permission check failed (/vms/2201, VM.Allocate)"})
	rec := &recorder{}
	res := testExecutor(f, rec).Run(context.Background(), recordPlan(pods.KindEnvTeardown, pods.StepClearRecord, dcItem()))
	if !errors.Is(res.Failed["dev-02"], errLastWaits) || f.Called("pool-comment:") != 0 || f.PoolComments["dev-02"] != "spare [env kilo.alpha]" {
		t.Errorf("result %+v, comment %q", res, f.PoolComments["dev-02"])
	}
	if n := len(rec.find("dev-02", EventFailed)); n != 1 {
		t.Errorf("clear failed %d times, want once", n)
	}
}

// A stop that keeps the clear from running interrupts it before it starts
// rather than failing it.
func TestClearRecordStoppedIsInterrupted(t *testing.T) {
	f := recordedSlot()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	f.OnRecord = func(key string) {
		if key == "delete:2201" {
			cancel(ErrCancelRequested)
		}
	}
	rec := &recorder{}
	res := testExecutor(f, rec).Run(ctx, recordPlan(pods.KindEnvTeardown, pods.StepClearRecord, dcItem()))
	if res.Interrupted["dev-02"] == nil || res.Failed["dev-02"] != nil || f.PoolComments["dev-02"] != "spare [env kilo.alpha]" {
		t.Errorf("result %+v, comment %q", res, f.PoolComments["dev-02"])
	}
	if ev := rec.find("dev-02", EventStarted); len(ev) != 0 {
		t.Errorf("started events %+v, want none", ev)
	}
}

// Hosts added to a marker-only environment take over from the marker: the
// add clears it last.
func TestEnvAddClearsTheMarker(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 8000, Name: "gw-v1", Node: "alder", Template: true, Tags: "base;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:80:00,bridge=vmbr0", "net1": "virtio=BC:24:11:00:80:01,bridge=vmbr0"})
	f.PoolComments = map[string]string{"dev-01": "spare [env kilo.alpha]"}
	ex := catalogExec(f)
	ex.Cfg.Environments.Slots = "1-1"
	plan, err := pods.NewPlanner(f, ex.Cfg).EnvAdd(context.Background(),
		pods.EnvRequest{Domain: "kilo.alpha", Hosts: []pods.HostSpec{{Host: "gw", Family: "gw"}}})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(plan.Items); n != 2 || !plan.Items[1].Last || !slices.Equal(plan.Items[1].Steps, []pods.Step{pods.StepClearRecord}) {
		t.Fatalf("items = %+v", plan.Items)
	}
	res := ex.Run(context.Background(), plan)
	if len(res.Succeeded) != 2 || f.PoolComments["dev-01"] != "spare" {
		t.Errorf("result %+v, comment %q", res, f.PoolComments["dev-01"])
	}
}
