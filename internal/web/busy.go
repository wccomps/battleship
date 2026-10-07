package web

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// busyLimit caps busy VMs per read: above any competition's VM count, but
// keeps a backlog of queued jobs from making the answer unbounded.
const busyLimit = 5000

// busyTimeout bounds a shared read of the busy VMs.
const busyTimeout = 30 * time.Second

// busySet maps the VMs active jobs still have to work on to the job's ID,
// with a key that changes whenever the set does.
type busySet struct {
	jobs map[string]int64
	key  string
}

// busyCache shares the busy-VM read among this replica's grid pages and
// their event streams.
type busyCache struct {
	flight seqFlight[struct{}, busySet]
	mu     sync.Mutex
	last   busySet // the last good read, shown while a read fails
}

// sharedBusy returns the busy VMs as read at or after hub sequence need, on
// the server's lifetime since others may wait. On failure it logs and returns
// the last good set: the marks are a hint and the grid works without them.
func (s *Server) sharedBusy(ctx context.Context, need uint64) busySet {
	c := &s.busy
	set, err := c.flight.get(ctx, struct{}{}, need, s.hub.Seq, func() (busySet, error) {
		s.busyReads.Add(1)
		rctx, cancel := context.WithTimeout(s.life, busyTimeout)
		defer cancel()
		jobs, err := s.st.BusyVMs(rctx, busyLimit)
		if err != nil {
			s.logf("web: reading which VMs jobs are working on: %v", err)
			return busySet{}, err
		}
		set := busySet{jobs: jobs, key: busyKey(jobs)}
		c.mu.Lock()
		c.last = set
		c.mu.Unlock()
		return set, nil
	})
	if err != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.last
	}
	return set
}

// busyKey writes a busy set in a fixed order, for the grid cache's key.
func busyKey(jobs map[string]int64) string {
	var b strings.Builder
	for _, n := range slices.Sorted(maps.Keys(jobs)) {
		fmt.Fprintf(&b, "%s=%d,", n, jobs[n])
	}
	return b.String()
}
