package store_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// For any history of jobs of any kind and status, each over some VMs,
// LastJobOf a VM is the newest job with an item for it, with that item,
// and ErrNotFound for a VM no job had.
func TestPropLastJobOf(t *testing.T) {
	s := storetest.New(t)
	round := 0
	rapid.Check(t, func(t *rapid.T) {
		round++
		// Each round has VMs of its own: the store is shared.
		var pool []string
		for _, h := range []string{"dc", "web", "ftp", "db"} {
			pool = append(pool, fmt.Sprintf("team%02d-%s", round%100, h)+fmt.Sprintf("-r%d", round))
		}
		newest := map[string]int64{}
		kinds := map[int64]string{}
		for i := range rapid.IntRange(0, 6).Draw(t, "jobs") {
			names := rapid.SliceOfNDistinct(rapid.SampledFrom(pool), 1, len(pool), rapid.ID).Draw(t, fmt.Sprintf("job %d VMs", i))
			nj := newJob(fmt.Sprintf("round:%d:%d", round, i)) // its own lock: every job can start
			nj.Kind = rapid.SampledFrom([]string{"deploy", "reset", "power", "teardown"}).Draw(t, "kind")
			nj.Items = nil
			for k, n := range names {
				nj.Items = append(nj.Items, store.NewItem{Name: n, Team: "01", VMID: 10100 + k})
			}
			id, err := s.CreateJob(ctx, nj)
			if err != nil {
				t.Fatal(err)
			}
			kinds[id] = nj.Kind
			for _, n := range names {
				newest[n] = max(newest[n], id)
			}
			switch st := rapid.SampledFrom([]string{store.StatusPending, store.StatusRunning, store.StatusSucceeded,
				store.StatusCompletedWithFailures, store.StatusFailed, store.StatusCancelled, store.StatusInterrupted, store.StatusStale}).Draw(t, "status"); st {
			case store.StatusPending:
			default:
				j, err := s.ClaimJob(ctx, id, "w")
				if err != nil || j == nil {
					t.Fatalf("claiming job %d: %v, %v", id, j, err)
				}
				if st == store.StatusRunning {
					break
				}
				out := map[string]store.ItemOutcome{}
				for _, n := range names {
					if o := rapid.SampledFrom([]string{"", store.ItemDone, store.ItemFailed, store.ItemBlocked}).Draw(t, "outcome"); o != "" {
						out[n] = store.ItemOutcome{Status: o, Error: "e"}
					}
				}
				if err := s.Finish(ctx, id, "w", store.Outcome{Status: st, Items: out}); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, name := range append(slices.Clone(pool), "nosuch-"+pool[0]) {
			j, it, err := s.LastJobOf(ctx, name)
			want, ok := newest[name]
			if !ok {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("LastJobOf(%s) = job %d, %v; no job has it", name, j.ID, err)
				}
				continue
			}
			if err != nil || j.ID != want || j.Kind != kinds[want] || it.JobID != want || it.Name != name {
				t.Fatalf("LastJobOf(%s) = job %d (%s), item %+v, %v; want job %d", name, j.ID, j.Kind, it, err, want)
			}
			all, err := s.Items(ctx, want)
			if err != nil {
				t.Fatal(err)
			}
			idx := slices.IndexFunc(all, func(x store.Item) bool { return x.Name == name })
			if idx < 0 || all[idx].Status != it.Status || all[idx].Idx != it.Idx || all[idx].Error != it.Error {
				t.Fatalf("LastJobOf(%s) item %+v; the job's items say %+v", name, it, all)
			}
		}
	})
}
