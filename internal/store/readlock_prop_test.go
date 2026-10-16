package store_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// conflicts is the lock rule, written from its definition, over keys
// spelled "read:K" for a read of K: two keys clash when equal, unless both
// are reads; "read:K" also clashes with K.
func conflicts(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			xr, yr := strings.HasPrefix(x, "read:"), strings.HasPrefix(y, "read:")
			switch {
			case xr && yr:
			case strings.TrimPrefix(x, "read:") == strings.TrimPrefix(y, "read:"):
				return true
			}
		}
	}
	return false
}

// Claim starts the oldest pending job that conflicts with no running job and
// no older pending one; reads of one VM share it, a write waits for them.
func TestPropClaimHonoursReadLocks(t *testing.T) {
	s := storetest.New(t)
	round := 0
	rapid.Check(t, func(t *rapid.T) {
		round++
		ns := fmt.Sprintf("r%d-", round) // keys unique to this check
		keyPool := []string{"vmid:1", "vmid:2", "read:vmid:1", "read:vmid:2", "team:01"}
		type job struct {
			id      int64
			keys    []string
			running bool
			done    bool
		}
		var jobs []*job
		for i := range rapid.IntRange(1, 6).Draw(t, "jobs") {
			keys := rapid.SliceOfNDistinct(rapid.SampledFrom(keyPool), 1, 3, rapid.ID).Draw(t, fmt.Sprintf("keys %d", i))
			nj := newJob(fmt.Sprintf("%sown:%d", ns, i)) // every job changes something
			for _, k := range keys {
				if r, ok := strings.CutPrefix(k, "read:"); ok {
					nj.ReadKeys = append(nj.ReadKeys, ns+r)
				} else {
					nj.LockKeys = append(nj.LockKeys, ns+k)
				}
			}
			id, err := s.CreateJob(ctx, nj)
			if err != nil {
				t.Fatal(err)
			}
			jobs = append(jobs, &job{id: id, keys: keys})
		}
		for step := 0; ; step++ {
			// The model's pick: the oldest pending job nothing blocks.
			var want *job
			for i, j := range jobs {
				if j.running || j.done {
					continue
				}
				blocked := false
				for _, o := range jobs[:i] {
					blocked = blocked || !o.done && conflicts(o.keys, j.keys)
				}
				for _, o := range jobs[i+1:] {
					blocked = blocked || o.running && conflicts(o.keys, j.keys)
				}
				if !blocked {
					want = j
					break
				}
			}
			got, err := s.Claim(ctx, "w", 0)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case want == nil && got != nil:
				t.Fatalf("step %d: claimed job %d, want none", step, got.ID)
			case want != nil && (got == nil || got.ID != want.id):
				t.Fatalf("step %d: claimed %+v, want job %d %v", step, got, want.id, want.keys)
			case want != nil:
				want.running = true
				continue
			}
			// Nothing claimable: finish a running job, or stop.
			var running []*job
			for _, j := range jobs {
				if j.running {
					running = append(running, j)
				}
			}
			if len(running) == 0 {
				break
			}
			j := running[rapid.IntRange(0, len(running)-1).Draw(t, fmt.Sprintf("finish at %d", step))]
			if err := s.Finish(ctx, j.id, "w", store.Outcome{Status: store.StatusSucceeded}); err != nil {
				t.Fatal(err)
			}
			j.running, j.done = false, true
		}
		if slices.ContainsFunc(jobs, func(j *job) bool { return !j.done }) {
			t.Fatalf("jobs left undone: %+v", jobs)
		}
	})
}
