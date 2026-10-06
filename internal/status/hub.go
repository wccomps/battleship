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

// CancelTopic is the topic for requests to cancel one job, which the worker
// running it subscribes to.
func CancelTopic(id int64) string { return "cancel:" + strconv.FormatInt(id, 10) }

// Msg is one message to a subscriber. It only says that its topic changed;
// the subscriber re-reads the grid or the store. Messages a subscriber
// hasn't read yet are coalesced into one (see Hub), so a Msg may stand for
// several changes: it is a Resync if any of them was. A Resync means
// changes may have been missed, so the subscriber must re-read everything
// it shows. A subscriber's channel closes when it cancels or the hub stops.
type Msg struct {
	Topic  string
	Resync bool
	// Seq is the hub's sequence number for the (latest) message, set by
	// Publish; see Hub.Seq.
	Seq uint64
}

// ListenFunc delivers a notice for each changed job until ctx is done or
// the connection fails, when it closes the channel. store.Notifications is
// one.
type ListenFunc func(ctx context.Context) (<-chan store.Notice, error)

// HubOptions tune a Hub. Zero values take the defaults.
type HubOptions struct {
	Clock Clock                            // default SystemClock
	Logf  func(format string, args ...any) // default log.Printf
}

// stableConnection is how long a LISTEN connection must stay up for its loss
// to restart the reconnect backoff at minB.
const stableConnection = 30 * time.Second

// Hub fans change notifications out to subscribers, such as the web app's
// server-sent event streams. Run owns the process's only LISTEN connection
// to Postgres; nothing else in the process listens.
//
// Publishing never blocks and never drops a subscriber: each subscriber
// holds at most one unread message, and a message published while one is
// unread is merged into it (see Msg). So a burst of changes reaches a
// subscriber that is busy (say, waiting out its send gap) as one message,
// and a subscriber whose reader has stalled costs one message of memory; its
// owner, not the hub, decides when to give up on it (event streams do so
// when a write passes its deadline).
//
// When the LISTEN connection is lost, Run reconnects with backoff and sends
// every subscriber a Resync, since changes in between were missed.
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

// SubscribeTopics returns a channel of messages for topics and a function
// that ends the subscription and closes the channel. cancel may be called
// more than once, and after the hub stopped. Once the hub has stopped, it
// returns a closed channel. The topics' messages share the one channel and
// merge like any other (the merged message's Topic is the latest one's). A
// Resync carries the first topic.
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

// Wake is SubscribeTopics for a subscriber that only needs to know a topic
// may have changed, such as a worker woken by a cancel. wake holds at most
// one signal waiting, so the hub never blocks on it, and closes once stop
// is called or the hub stops.
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

// Seq is the sequence number of the last message published (Msg.Seq);
// gaining or losing the LISTEN connection takes one too. Notices are
// published after their change commits, so a read started once Seq reached
// a message's Seq sees that message's change.
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

// deliver puts m in s's one slot, merging it into the message already there
// if s hasn't read that yet. Only the hub sends on s.ch, and always under
// h.mu, so once the slot is emptied (by the hub taking the unread message
// out, or by the reader taking it meanwhile) the send can't block.
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

// merge is the one message that stands for old followed by m: m's topic
// and Seq, and a Resync if either was one.
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

// bumpSeq takes the next sequence number without a message, so that reads
// started before it can't serve a stream that starts after it. Run calls it
// when the connection is lost.
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
			// A subscriber of several topics gets one Resync per topic;
			// they merge into one message.
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
// TopicJobs (unless it is a log line), then to the job's JobTopic and, for a
// cancel request, its CancelTopic. Every
// connection, the first included, sends all subscribers a Resync, since
// changes made before it sent them no notice. When the connection is lost
// or can't be made, Run retries with exponential backoff, which starts over
// only after a connection stayed up for 30s. When Run returns, every
// subscription is closed. Call it once.
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
			// Only a connection that stayed up a while resets the backoff,
			// so a server that accepts and then drops connections isn't
			// retried every minB.
			if h.clock.Now().Sub(up) >= stableConnection {
				backoff = h.minB
			}
			// Changes made from here on send no notice until the
			// reconnect, so reads made earlier must not satisfy later needs.
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
