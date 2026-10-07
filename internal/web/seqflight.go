package web

import (
	"context"
	"sync"
)

// seqFlight shares store reads among this replica's event streams, so a crowd
// watching one thing costs a read per change, not per browser.
//
// Reads are ordered by hub sequence numbers (status.Hub.Seq); an entry records
// the number when its read started. A stream needing change N may use any
// entry started at N or later: changes committed before N's notice have
// notices numbered at most N, so the read sees them; a change it missed
// commits after the read started and its higher-numbered notice triggers a
// new read.
//
// A held key's entry is dropped when its last holder releases it; unheld
// entries are kept.
type seqFlight[K comparable, V any] struct {
	mu    sync.Mutex
	m     map[K]*seqEntry[V]
	holds map[K]int
}

type seqEntry[V any] struct {
	seq  uint64        // the hub's sequence number when the read started
	done chan struct{} // closed when v and err are set
	v    V
	err  error
}

// get returns key's value as read at or after sequence need, joining a recent
// enough read already made or under way, or else calling read with seq() as
// its number. Failed reads aren't kept. read should not depend on ctx, since
// others may wait for it.
func (c *seqFlight[K, V]) get(ctx context.Context, key K, need uint64, seq func() uint64, read func() (V, error)) (V, error) {
	c.mu.Lock()
	if e := c.m[key]; e != nil && e.seq >= need {
		c.mu.Unlock()
		select {
		case <-e.done:
			return e.v, e.err
		case <-ctx.Done():
			var zero V
			return zero, ctx.Err()
		}
	}
	e := &seqEntry[V]{seq: seq(), done: make(chan struct{})}
	if c.m == nil {
		c.m = map[K]*seqEntry[V]{}
	}
	c.m[key] = e
	c.mu.Unlock()

	e.v, e.err = read()
	close(e.done)
	if e.err != nil {
		c.mu.Lock()
		if c.m[key] == e {
			delete(c.m, key)
		}
		c.mu.Unlock()
	}
	return e.v, e.err
}

// hold keeps key's entry until this and every other holder releases it.
func (c *seqFlight[K, V]) hold(key K) (release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.holds == nil {
		c.holds = map[K]int{}
	}
	c.holds[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.holds[key]--; c.holds[key] == 0 {
				delete(c.holds, key)
				delete(c.m, key)
			}
		})
	}
}
