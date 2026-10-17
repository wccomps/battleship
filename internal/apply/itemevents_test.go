package apply

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/proxmox"
)

// statuses are item's event statuses, in order, but info lines.
func statuses(rec *recorder, item string) []EventStatus {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []EventStatus
	for _, e := range rec.events {
		if e.Item == item && e.Status != EventInfo {
			out = append(out, e.Status)
		}
	}
	return out
}

// Every attempt at an item starts with a started event, a gone VM's in a
// retry round too, and an item that ran its last step ends with succeeded.
func TestEveryAttemptStartsAndSuccessEnds(t *testing.T) {
	f := newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, map[string]string{"name": "team01-teak"})
	plan := teardownTeam01(t, f)
	f.OnRecord = func(key string) {
		if strings.HasPrefix(key, "shutdown:10121:") {
			delete(f.VMs, 10121) // deleted by hand while the job stopped it
		}
	}
	rec := &recorder{}
	ex := testExecutor(f, rec)
	ex.Cfg.Retry.Rounds = 1
	res := ex.Run(context.Background(), plan)
	got := statuses(rec, "team01-teak")
	want := []EventStatus{EventStarted, EventFailed, EventStarted, EventSkipped, EventSucceeded}
	if !slices.Equal(got, want) || len(res.Succeeded) != 1 {
		t.Errorf("events %v, want %v; result %+v", got, want, res)
	}
	if ev := rec.find("team01-teak", EventStarted); len(ev) != 2 || ev[1].Step != "delete" {
		t.Errorf("started events %+v, want the second at delete", ev)
	}

	// An item whose steps all ran ends with succeeded, once.
	f = newCluster()
	rec = &recorder{}
	testExecutor(f, rec).Run(context.Background(), deployTeak(t, f, "01"))
	if got := statuses(rec, "team01-teak"); len(got) < 3 || got[0] != EventStarted || got[len(got)-1] != EventSucceeded ||
		len(rec.find("team01-teak", EventSucceeded)) != 1 {
		t.Errorf("deploy: events %v, want started first and succeeded last, once", got)
	}

	// A VM gone from a plan that doesn't delete it fails after its start.
	f = newCluster()
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "cedar", Status: "running"}, map[string]string{"name": "team01-teak"})
	plan, err := testPlanner(f).Power(context.Background(), []string{"01"}, []string{"teak"}, "stop")
	if err != nil {
		t.Fatal(err)
	}
	delete(f.VMs, 10121)
	rec = &recorder{}
	ex = testExecutor(f, rec)
	ex.Cfg.Retry.Rounds = 1
	ex.Run(context.Background(), plan)
	if got, want := statuses(rec, "team01-teak"), []EventStatus{EventStarted, EventFailed, EventStarted, EventFailed}; !slices.Equal(got, want) {
		t.Errorf("gone VM: events %v, want %v", got, want)
	}

	// So does a clone whose template couldn't be built.
	f = newCluster()
	plan = deployTeak(t, f, "01")
	f.FailOn("clone:9021", &proxmox.APIError{Status: 400, Message: "clone: bad request"})
	rec = &recorder{}
	ex = testExecutor(f, rec)
	ex.Cfg.Retry.Rounds = 0
	ex.Run(context.Background(), plan)
	if got, want := statuses(rec, "team01-teak"), []EventStatus{EventStarted, EventFailed}; !slices.Equal(got, want) {
		t.Errorf("unbuilt template: events %v, want %v", got, want)
	}
}
