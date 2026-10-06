package store_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// BusyVMs names the VMs active jobs still have to work on, each with the
// oldest such job: what the grid marks busy.
func TestBusyVMs(t *testing.T) {
	s := storetest.New(t)

	// A finished job's items are never busy.
	runJob(t, s, "reset", items("team01-web"), nil, store.StatusSucceeded,
		map[string]store.ItemOutcome{"team01-web": {Status: store.ItemDone}})

	// Pending job: its runnable item is busy, its blocked one isn't.
	pending := create(t, s, "team:01") // team01-teak, team01-dc (blocked)

	// Running job: its items are busy, started or not.
	nj := newJob("team:02")
	nj.Items = []store.NewItem{{Name: "team02-dc", Team: "02", VMID: 10201}, {Name: "team02-web", Team: "02", VMID: 10202}}
	running, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(ctx, running, "w"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEvent(ctx, running, store.Event{At: time.Now(), Item: "team02-dc", Step: "stop", Status: "started"}); err != nil {
		t.Fatal(err)
	}

	// A later pending job on a VM the first one also has: the older job wins.
	again := newJob("team:01")
	again.Items = []store.NewItem{{Name: "team01-teak", Team: "01", VMID: 10121}}
	if _, err := s.CreateJob(ctx, again); err != nil {
		t.Fatal(err)
	}

	got, err := s.BusyVMs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"team01-teak": pending, "team02-dc": running, "team02-web": running}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BusyVMs = %v, want %v", got, want)
	}

	// The read is bounded.
	got, err = s.BusyVMs(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("BusyVMs(limit 1) = %v, want one VM", got)
	}
}
