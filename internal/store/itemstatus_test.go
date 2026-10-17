package store_test

import (
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// An item's status follows its events while the job runs: succeeded makes
// it done and failed failed, which only a retry round's started undoes;
// the job's outcome has the last word, and leaves each item's step time.
func TestItemStatusFollowsItsEvents(t *testing.T) {
	s := storetest.New(t)
	nj := newJob("team:01")
	nj.Items = []store.NewItem{{Name: "team01-teak", Team: "01", VMID: 10121}, {Name: "team01-oak", Team: "01", VMID: 10122}}
	id, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	mustClaim(t, s, "w")
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	items := func() map[string]store.Item {
		t.Helper()
		got, err := s.Items(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]store.Item{}
		for _, it := range got {
			out[it.Name] = it
		}
		return out
	}
	for i, c := range []struct {
		item, step, status string
		want               string
	}{
		{"team01-teak", "stop", "started", store.ItemRunning},
		{"team01-teak", "stop", "done", store.ItemRunning},
		{"team01-teak", "delete", "info", store.ItemRunning},
		{"team01-teak", "delete", "done", store.ItemRunning},
		{"team01-teak", "", "succeeded", store.ItemDone},
		{"team01-teak", "delete", "info", store.ItemDone},
		{"team01-oak", "stop", "started", store.ItemRunning},
		{"team01-oak", "stop", "failed", store.ItemFailed},
		{"team01-oak", "stop", "info", store.ItemFailed},
		{"team01-oak", "", "interrupted", store.ItemFailed},
	} {
		ev := store.Event{At: at.Add(time.Duration(i) * time.Minute), Item: c.item, Step: c.step, Status: c.status}
		if err := s.AddEvent(ctx, id, ev); err != nil {
			t.Fatal(err)
		}
		if got := items()[c.item].Status; got != c.want {
			t.Fatalf("after %s %s %s: status %q, want %q", c.item, c.status, c.step, got, c.want)
		}
	}
	busy, err := s.BusyVMs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := busy["team01-teak"]; ok || busy["team01-oak"] != id {
		t.Errorf("busy = %v, want team01-oak (a later round may retry it) and not team01-teak", busy)
	}

	// A retry round starts it again.
	if err := s.AddEvent(ctx, id, store.Event{At: at.Add(time.Hour), Item: "team01-oak", Step: "stop", Status: "started"}); err != nil {
		t.Fatal(err)
	}
	if oak := items()["team01-oak"]; oak.Status != store.ItemRunning || len(oak.Steps) != 0 {
		t.Errorf("after the retry round's start: %+v", oak)
	}
	stepAt := items()["team01-teak"].StepAt
	err = s.Finish(ctx, id, "w", store.Outcome{Status: store.StatusCompletedWithFailures, Items: map[string]store.ItemOutcome{
		"team01-teak": {Status: store.ItemRemoved}, "team01-oak": {Status: store.ItemFailed, Error: "boom"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := items()
	if got["team01-teak"].Status != store.ItemRemoved || got["team01-oak"].Status != store.ItemFailed {
		t.Errorf("after Finish: %+v", got)
	}
	if !got["team01-teak"].StepAt.Equal(stepAt) || !got["team01-oak"].StepAt.Equal(at.Add(time.Hour)) {
		t.Errorf("Finish moved the step times: %v, %v", got["team01-teak"].StepAt, got["team01-oak"].StepAt)
	}
}

// A retry round's attempt starts afresh: an item that failed in round 1 and
// succeeded in round 2 is done with no error, before the job's outcome.
func TestRetryRoundClearsTheEarlierError(t *testing.T) {
	s := storetest.New(t)
	nj := newJob("team:01")
	nj.Items = []store.NewItem{{Name: "team01-oak", Team: "01", VMID: 10122}}
	id, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	mustClaim(t, s, "w")
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	for i, ev := range []store.Event{
		{Item: "team01-oak", Step: "stop", Status: "started"},
		{Item: "team01-oak", Step: "stop", Status: "failed", Message: "round 1 boom"},
		{Item: "team01-oak", Step: "stop", Status: "started"},
		{Item: "team01-oak", Step: "stop", Status: "done"},
		{Item: "team01-oak", Status: "succeeded"},
	} {
		ev.At = at.Add(time.Duration(i) * time.Minute)
		if err := s.AddEvent(ctx, id, ev); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Items(ctx, id)
	if err != nil || len(got) != 1 {
		t.Fatalf("items = %+v, %v", got, err)
	}
	if oak := got[0]; oak.Status != store.ItemDone || oak.Error != "" || oak.Steps["stop"] != "done" {
		t.Errorf("after round 2 succeeded: status %q, error %q, steps %v", oak.Status, oak.Error, oak.Steps)
	}
}

// A stopped job's items keep their last event's time too.
func TestEndItemsLeavesStepTime(t *testing.T) {
	s := storetest.New(t)
	id := create(t, s, "team:01")
	mustClaim(t, s, "w")
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	if err := s.AddEvent(ctx, id, store.Event{At: at, Item: "team01-teak", Step: "stop", Status: "started"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, id, "w", store.Outcome{Status: store.StatusCancelled}); err != nil {
		t.Fatal(err)
	}
	its, err := s.Items(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range its {
		if it.Name == "team01-teak" && (it.Status != store.ItemInterrupted || !it.StepAt.Equal(at)) {
			t.Errorf("teak = %+v, want interrupted at %v", it, at)
		}
	}
}
