package status

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/store"
)

// Hub topics.
const (
	TopicGrid = "grid" // the status grid changed
	TopicJobs = "jobs" // some job's status or items changed other than by a log line
)

// JobTopic is the topic for changes to one job: its status, items or log.
func JobTopic(id int64) string { return "job:" + strconv.FormatInt(id, 10) }

// CancelTopic is the topic for cancel requests for one job; its worker
// subscribes to it.
func CancelTopic(id int64) string { return "cancel:" + strconv.FormatInt(id, 10) }

// Msg says only that its topic changed; the subscriber re-reads. Unread
// messages coalesce (see Hub), so one Msg may stand for several changes and
// is a Resync if any was. Resync means changes may have been missed: re-read
// everything shown.
type Msg struct {
	Topic  string
	Resync bool
	// Seq is set by Publish; see Hub.Seq.
	Seq uint64
}

// ListenFunc delivers a notice per changed job, closing the channel when ctx
// is done or the connection fails. store.Notifications is one.
type ListenFunc func(ctx context.Context) (<-chan store.Notice, error)

// HubOptions tune a Hub. Zero values take the defaults.
type HubOptions struct {
	Clock Clock                            // default SystemClock
	Logf  func(format string, args ...any) // default log.Printf
}

// stableConnection is how long a LISTEN connection must last to reset the
// reconnect backoff.
const stableConnection = 30 * time.Second

// Hub fans change notifications out to subscribers (e.g. SSE streams). Run
// owns the process's only LISTEN connection to Postgres.
//
// Publishing never blocks or drops a subscriber: each holds at most one
// unread message, and later ones merge into it. A stalled reader costs one
// message of memory; its owner decides when to give up on it.
//
// After a lost connection Run reconnects with backoff and sends everyone a
// Resync.
type Hub struct {
	listen ListenFunc
	clock  Clock
	logf   func(format string, args ...any)
	minB   time.Duration // first wait before reconnecting
	maxB   time.Duration // longest wait between reconnects

	mu      sync.Mutex
	subs    map[string]map[*subscriber]struct{}
	stopped bool
	seq     uint64 // the last message's Seq
}

type subscriber struct {
	topics []string
	ch     chan Msg
	closed bool
}

// NewHub makes a hub that listens with listen. listen may be nil for a hub
// that only carries Publish calls; Run then just waits for ctx.
func NewHub(listen ListenFunc, opts HubOptions) *Hub {
	h := &Hub{
		listen: listen,
		clock:  opts.Clock,
		logf:   opts.Logf,
		minB:   time.Second,
		maxB:   30 * time.Second,
		subs:   map[string]map[*subscriber]struct{}{},
	}
	if h.clock == nil {
		h.clock = SystemClock
	}
	if h.logf == nil {
		h.logf = log.Printf
	}
	return h
}

// SubscribeTopics returns one channel for all topics and a cancel that
// closes it (safe to call repeatedly or after the hub stopped). A stopped hub
// returns a closed channel. Merged messages carry the latest Topic; a Resync
// carries the first topic.
func (h *Hub) SubscribeTopics(topics ...string) (<-chan Msg, func()) {
	// The one slot holds the unread message that later ones merge into.
	s := &subscriber{topics: topics, ch: make(chan Msg, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped || len(topics) == 0 {
		close(s.ch)
		return s.ch, func() {}
	}
	for _, topic := range topics {
		if h.subs[topic] == nil {
			h.subs[topic] = map[*subscriber]struct{}{}
		}
		h.subs[topic][s] = struct{}{}
	}
	return s.ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.remove(s)
	}
}

// Wake is SubscribeTopics for a subscriber that only needs a "may have
// changed" signal. wake buffers one signal and closes on stop or hub stop.
func (h *Hub) Wake(topics ...string) (wake <-chan struct{}, stop func()) {
	msgs, stop := h.SubscribeTopics(topics...)
	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		for range msgs {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}()
	return ch, stop
}

// Seq is the last published Msg.Seq; connecting and disconnecting take one
// too. Notices publish after commit, so a read started once Seq reached a
// message's Seq sees its change.
func (h *Hub) Seq() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// Publish sends m to the subscribers of m.Topic without blocking, stamped
// with the next sequence number (m.Seq is ignored).
func (h *Hub) Publish(m Msg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	m.Seq = h.seq
	for s := range h.subs[m.Topic] {
		h.deliver(s, m)
	}
}

// deliver puts m in s's slot, merging with any unread message. Only the hub
// sends on s.ch, always under h.mu, so once the slot is empty the send can't
// block.
func (h *Hub) deliver(s *subscriber, m Msg) {
	select {
	case s.ch <- m:
		return
	default:
	}
	select {
	case old := <-s.ch:
		m = merge(old, m)
	default: // the reader took it meanwhile
	}
	s.ch <- m
}

// merge combines old then m: m's topic and Seq, Resync if either was.
func merge(old, m Msg) Msg {
	m.Resync = m.Resync || old.Resync
	return m
}

// remove closes s's channel and forgets it. h.mu must be held.
func (h *Hub) remove(s *subscriber) {
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
	for _, topic := range s.topics {
		delete(h.subs[topic], s)
		if len(h.subs[topic]) == 0 {
			delete(h.subs, topic)
		}
	}
}

// bumpSeq takes a sequence number without a message, so reads started
// before a lost connection can't serve streams started after it.
func (h *Hub) bumpSeq() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
}

// resyncAll tells every subscriber to re-read what it shows.
func (h *Hub) resyncAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	for _, subs := range h.subs {
		for s := range subs {
			// Multi-topic subscribers get one per topic; they merge.
			h.deliver(s, Msg{Topic: s.topics[0], Resync: true, Seq: h.seq})
		}
	}
}

// stop closes every subscription; later ones start closed.
func (h *Hub) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	for _, subs := range h.subs {
		for s := range subs {
			h.remove(s)
		}
	}
}

// Run listens for job changes until ctx is done, publishing each to
// TopicJobs (except log lines), JobTopic and, for cancels, CancelTopic.
// Every connection, the first included, resyncs all subscribers. Failures
// retry with exponential backoff. When Run returns every subscription is
// closed. Call it once.
func (h *Hub) Run(ctx context.Context) {
	defer h.stop()
	if h.listen == nil {
		<-ctx.Done()
		return
	}
	backoff := h.minB
	for attempt := 0; ; attempt++ {
		ch, err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			h.logf("status: listening for job changes: %v; retrying in %s", err, backoff)
		} else {
			if attempt > 0 {
				h.logf("status: listening for job changes again; asking pages to reload")
			}
			// Even on the first connection: streams may have opened before it.
			h.resyncAll() // takes a sequence number
			up := h.clock.Now()
			if !h.forward(ctx, ch) {
				return
			}
			// A server that accepts then drops shouldn't be retried every minB.
			if h.clock.Now().Sub(up) >= stableConnection {
				backoff = h.minB
			}
			// Changes until the reconnect send no notice.
			h.bumpSeq()
			h.logf("status: lost the connection that listens for job changes; reconnecting in %s", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-h.clock.After(backoff):
		}
		backoff = min(backoff*2, h.maxB)
	}
}

// forward publishes each notification until ch closes, returning true, or
// ctx is done, returning false.
func (h *Hub) forward(ctx context.Context, ch <-chan store.Notice) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case n, ok := <-ch:
			if !ok {
				return ctx.Err() == nil
			}
			if !n.Log {
				h.Publish(Msg{Topic: TopicJobs})
			}
			h.Publish(Msg{Topic: JobTopic(n.JobID)})
			if n.Cancel {
				h.Publish(Msg{Topic: CancelTopic(n.JobID)})
			}
		}
	}
}
