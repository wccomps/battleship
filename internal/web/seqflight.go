package web

import (
	"context"
	"sync"
)

// seqFlight shares reads of the store among this replica's event streams,
// so a crowd watching the same thing costs a read per change, not one per
// browser.
//
// Reads are ordered by the hub's sequence numbers (status.Hub.Seq). An
// entry records the sequence number when its read started. A stream that
// must show at least the change the hub numbered N may use any entry that
// started at N or later: every change committed before N was notified has
// its notice numbered at most N, so such a read sees it; and a change the
// read missed was committed after the read started, so its notice comes
// later with a higher number and makes the stream read again.
//
// A held key's entry is dropped when its last holder lets go; entries of
// keys nobody holds are kept.
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

// get returns key's value as read at or after sequence number need,
// joining a read already made or under way if it is recent enough, or else
// calling read, after taking seq() as the new read's sequence number. A
// failed read isn't kept. read should not depend on ctx, the caller's, as
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

// hold keeps key's entry until the returned release is called, and every
// other holder's too.
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
