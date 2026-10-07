package status

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
)

// fakeListener stands in for store.Notifications: each call waits for the
// test to connect it (a channel the test feeds and closes) or fail it.
type fakeListener struct {
	next chan listenResult
}

type listenResult struct {
	ch  chan store.Notice
	err error
}

func newFakeListener() *fakeListener { return &fakeListener{next: make(chan listenResult)} }

func (l *fakeListener) listen(ctx context.Context) (<-chan store.Notice, error) {
	select {
	case r := <-l.next:
		if r.err != nil {
			return nil, r.err
		}
		return r.ch, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// connect answers the hub's next listen call with a live connection.
func (l *fakeListener) connect() chan store.Notice {
	ch := make(chan store.Notice)
	l.next <- listenResult{ch: ch}
	return ch
}

func (l *fakeListener) fail(err error) { l.next <- listenResult{err: err} }

// startHub runs a hub until the test ends and returns it and a function that
// stops it and waits for Run to return.
func startHub(t *testing.T, l *fakeListener, clock *fakeClock) (*Hub, func()) {
	t.Helper()
	h := NewHub(l.listen, HubOptions{Clock: clock, Logf: t.Logf})
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(rctx); close(done) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return h, stop
}

func TestJobTopic(t *testing.T) {
	if got := JobTopic(42); got != "job:42" {
		t.Errorf("JobTopic(42) = %q, want job:42", got)
	}
	if got := CancelTopic(42); got != "cancel:42" {
		t.Errorf("CancelTopic(42) = %q, want cancel:42", got)
	}
}

// A cancel request also reaches the job's CancelTopic, which the worker
// running it subscribes to; other notices don't.
func TestHubPublishesCancels(t *testing.T) {
	l := newFakeListener()
	h, _ := startHub(t, l, newFakeClock())
	cancels, stop := h.SubscribeTopics(CancelTopic(7))
	defer stop()
	job7, stop7 := h.SubscribeTopics(JobTopic(7))
	defer stop7()
	conn := l.connect()
	for _, ch := range []<-chan Msg{cancels, job7} {
		if m := recv(t, ch); !m.Resync {
			t.Errorf("got %+v on the first connection, want a resync", m)
		}
	}
	conn <- store.Notice{JobID: 7}
	conn <- store.Notice{JobID: 7, Log: true}
	conn <- store.Notice{JobID: 8, Cancel: true}
	conn <- store.Notice{JobID: 7, Cancel: true}
	if m := recv(t, cancels); m != (Msg{Topic: CancelTopic(7)}) {
		t.Errorf("cancel:7 got %+v", m)
	}
	none(t, cancels)
}

// Wake never blocks the hub, holds at most one wake waiting, and closes
// when the subscription stops.
func TestHubWake(t *testing.T) {
	l := newFakeListener()
	h, _ := startHub(t, l, newFakeClock())
	wake, stop := h.Wake(CancelTopic(7))
	conn := l.connect()
	took := func(what string) {
		t.Helper()
		select {
		case _, ok := <-wake:
			if !ok {
				t.Fatalf("%s: wake closed", what)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no wake", what)
		}
	}
	took("the first connection's resync")
	// Many cancels while nobody takes the wake: one waits, none blocks.
	for range 10 {
		conn <- store.Notice{JobID: 7, Cancel: true}
	}
	h.Publish(Msg{Topic: TopicGrid}) // after the cancels: once it lands, they all have
	took("the cancels")
	if n := len(wake); n > 1 {
		t.Errorf("%d wakes waiting, want at most one", n)
	}
	stop()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-wake:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("wake not closed after stop")
		}
	}
}

func TestHubFansOutNotifications(t *testing.T) {
	l := newFakeListener()
	h, _ := startHub(t, l, newFakeClock())
	var all []<-chan Msg
	for i := 0; i < 50; i++ {
		ch, cancel := h.SubscribeTopics(TopicJobs)
		defer cancel()
		all = append(all, ch)
	}
	job7, cancel7 := h.SubscribeTopics(JobTopic(7))
	defer cancel7()
	job8, cancel8 := h.SubscribeTopics(JobTopic(8))
	defer cancel8()
	grid, cancelGrid := h.SubscribeTopics(TopicGrid)
	defer cancelGrid()

	conn := l.connect()
	// Subscribers from before the first connection may have missed changes.
	for i, ch := range append(slices.Clone(all), job7, job8, grid) {
		if m := recv(t, ch); !m.Resync {
			t.Errorf("subscriber %d got %+v on the first connection, want a resync", i, m)
		}
	}
	conn <- store.Notice{JobID: 7}
	for i, ch := range all {
		if m := recv(t, ch); m != (Msg{Topic: TopicJobs}) {
			t.Errorf("jobs subscriber %d got %+v", i, m)
		}
	}
	// The hub publishes a job's own topic after "jobs", so once job:7 has its
	// message the notification is fully delivered.
	if m := recv(t, job7); m != (Msg{Topic: JobTopic(7)}) {
		t.Errorf("job:7 got %+v", m)
	}
	none(t, job8)
	none(t, grid)
	for _, ch := range all {
		none(t, ch)
	}

	// A log line reaches only its job's own topic.
	conn <- store.Notice{JobID: 7, Log: true}
	if m := recv(t, job7); m != (Msg{Topic: JobTopic(7)}) {
		t.Errorf("job:7 got %+v for a log line", m)
	}
	for _, ch := range all {
		none(t, ch) // the hub publishes "jobs" first, so it would be here
	}
	none(t, job8)
	none(t, grid)

	h.Publish(Msg{Topic: TopicGrid})
	if m := recv(t, grid); m != (Msg{Topic: TopicGrid}) {
		t.Errorf("grid got %+v", m)
	}
	none(t, all[0])
}

// A non-reading subscriber blocks nothing and isn't dropped; its unread
// messages merge into one.
func TestHubCoalescesForSlowSubscriber(t *testing.T) {
	var logs logLines
	h := NewHub(nil, HubOptions{Logf: logs.Logf})
	slow, cancelSlow := h.SubscribeTopics(TopicJobs)
	fast, cancelFast := h.SubscribeTopics(TopicJobs)
	defer cancelFast()

	for id := int64(1); id <= 500; id++ {
		published := make(chan struct{})
		m := Msg{Topic: TopicJobs, Resync: id == 7}
		go func() {
			h.Publish(m)
			close(published)
		}()
		select {
		case <-published:
		case <-time.After(10 * time.Second):
			t.Fatal("Publish blocked on a slow subscriber")
		}
		if got := recv(t, fast); got != m {
			t.Errorf("fast subscriber got %+v, want %+v", got, m)
		}
	}

	if m := recvSeq(t, slow); m != (Msg{Topic: TopicJobs, Resync: true, Seq: 500}) {
		t.Errorf("slow subscriber got %+v, want the 500 merged: the latest Seq, and a resync", m)
	}
	none(t, slow)
	// Still subscribed: it gets the next message as it comes.
	h.Publish(Msg{Topic: TopicJobs})
	if m := recv(t, slow); m != (Msg{Topic: TopicJobs}) {
		t.Errorf("slow subscriber then got %+v, want the next message", m)
	}
	cancelSlow()
	if _, ok := <-slow; ok {
		t.Error("channel open after cancel")
	}
	if l := logs.String(); l != "" {
		t.Errorf("publishing logged:\n%s", l)
	}
}

// Publishers racing a reader for the slot: the last message always arrives
// and nothing blocks.
func TestHubCoalesceRacesReader(t *testing.T) {
	h := NewHub(nil, HubOptions{Logf: t.Logf})
	ch, cancel := h.SubscribeTopics(TopicJobs)
	defer cancel()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 1000 {
				h.Publish(Msg{Topic: TopicJobs})
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-ch:
			continue
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("publishers blocked")
		}
		break
	}
	h.Publish(Msg{Topic: TopicJobs})
	want := h.Seq()
	var last Msg
	for last.Seq != want {
		last = recvSeq(t, ch)
	}
	none(t, ch)
}

// logLines collects log lines.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func TestHubCancelUnsubscribes(t *testing.T) {
	h := NewHub(nil, HubOptions{Logf: t.Logf})
	ch, cancel := h.SubscribeTopics(TopicJobs)
	other, cancelOther := h.SubscribeTopics(TopicJobs)
	defer cancelOther()
	cancel()
	cancel()
	h.Publish(Msg{Topic: TopicJobs})
	if m, ok := <-ch; ok {
		t.Errorf("cancelled subscriber got %+v, want a closed channel", m)
	}
	if m := recv(t, other); m.Topic != TopicJobs {
		t.Errorf("other subscriber got %+v", m)
	}
}

func TestHubResyncsAfterReconnect(t *testing.T) {
	l := newFakeListener()
	clock := newFakeClock()
	h, _ := startHub(t, l, clock)
	jobs, c1 := h.SubscribeTopics(TopicJobs)
	defer c1()
	job3, c2 := h.SubscribeTopics(JobTopic(3))
	defer c2()
	grid, c3 := h.SubscribeTopics(TopicGrid)
	defer c3()

	conn := l.connect()
	for _, ch := range []<-chan Msg{jobs, job3, grid} {
		recv(t, ch) // the first connection's resync
	}
	conn <- store.Notice{JobID: 1}
	recv(t, jobs)
	close(conn) // the LISTEN connection is lost

	// Reconnect with backoff: 1s, then 2s after a failed attempt.
	clock.BlockUntil(t, 1)
	if w := clock.Waits(); !slices.Equal(w, []time.Duration{time.Second}) {
		t.Fatalf("waits after the loss = %v, want [1s]", w)
	}
	clock.Advance(time.Second)
	l.fail(errors.New("connection refused"))
	clock.BlockUntil(t, 1)
	if w := clock.Waits(); !slices.Equal(w, []time.Duration{2 * time.Second}) {
		t.Fatalf("waits after a failed reconnect = %v, want [2s]", w)
	}
	for _, ch := range []<-chan Msg{jobs, job3, grid} {
		none(t, ch) // nothing until the connection is back
	}
	clock.Advance(2 * time.Second)
	conn = l.connect()

	for name, ch := range map[string]<-chan Msg{TopicJobs: jobs, JobTopic(3): job3, TopicGrid: grid} {
		if m := recv(t, ch); m != (Msg{Topic: name, Resync: true}) {
			t.Errorf("%s after the reconnect got %+v, want a resync", name, m)
		}
	}
	// They stay subscribed and get live notifications again.
	conn <- store.Notice{JobID: 3}
	if m := recv(t, jobs); m != (Msg{Topic: TopicJobs}) {
		t.Errorf("jobs got %+v, want job 3's notice", m)
	}
	if m := recv(t, job3); m != (Msg{Topic: JobTopic(3)}) {
		t.Errorf("job:3 got %+v, want job 3's notice", m)
	}

	// A connection that stayed up 30s resets the backoff to 1s.
	clock.Advance(30 * time.Second)
	close(conn)
	clock.BlockUntil(t, 1)
	if w := clock.Waits(); !slices.Equal(w, []time.Duration{time.Second}) {
		t.Fatalf("waits after losing a good connection = %v, want [1s]", w)
	}
	clock.Advance(time.Second)
	l.connect()
	if m := recv(t, jobs); !m.Resync {
		t.Errorf("jobs got %+v, want a resync", m)
	}
}

// A connection that drops at once doesn't reset the backoff, so a flapping
// server isn't hammered every second.
func TestHubBackoffResetsOnlyAfterStableConnection(t *testing.T) {
	l := newFakeListener()
	clock := newFakeClock()
	h, _ := startHub(t, l, clock)
	jobs, cancel := h.SubscribeTopics(TopicJobs)
	defer cancel()
	wait := func(want time.Duration) {
		t.Helper()
		clock.BlockUntil(t, 1)
		if w := clock.Waits(); !slices.Equal(w, []time.Duration{want}) {
			t.Fatalf("waits = %v, want [%s]", w, want)
		}
	}
	// The hub takes the notice only after reading the connection's start
	// time, so the test advances the clock after that.
	connect := func() chan store.Notice {
		t.Helper()
		conn := l.connect()
		conn <- store.Notice{JobID: 1}
		recv(t, jobs)
		return conn
	}
	close(connect()) // drops at once
	wait(time.Second)
	clock.Advance(time.Second)
	conn := connect()
	clock.Advance(29 * time.Second) // up for 29s only
	close(conn)
	wait(2 * time.Second)
	clock.Advance(2 * time.Second)
	close(connect())
	wait(4 * time.Second)
	clock.Advance(4 * time.Second)
	conn = connect()
	clock.Advance(30 * time.Second)
	close(conn)
	wait(time.Second)
}

func TestHubResyncsWhenFirstConnectFails(t *testing.T) {
	l := newFakeListener()
	clock := newFakeClock()
	h, _ := startHub(t, l, clock)
	jobs, cancel := h.SubscribeTopics(TopicJobs)
	defer cancel()
	l.fail(errors.New("connection refused"))
	clock.BlockUntil(t, 1)
	clock.Advance(time.Second)
	l.connect()
	if m := recv(t, jobs); !m.Resync {
		t.Errorf("got %+v, want a resync: changes before the first connection were missed", m)
	}
}

// Streams opened before the first LISTEN missed changes until it took hold,
// so the first connect resyncs.
func TestHubResyncsOnFirstConnect(t *testing.T) {
	l := newFakeListener()
	h, _ := startHub(t, l, newFakeClock())
	jobs, cancel := h.SubscribeTopics(TopicJobs)
	defer cancel()
	l.connect()
	if m := recv(t, jobs); !m.Resync {
		t.Errorf("got %+v, want a resync: changes before the first connection were missed", m)
	}
}

// A reconnect's Resync merges into a message the subscriber hasn't read:
// it stays subscribed and learns to re-read everything.
func TestHubReconnectResyncMergesIntoUnread(t *testing.T) {
	l := newFakeListener()
	clock := newFakeClock()
	h, _ := startHub(t, l, clock)
	unread, cancel := h.SubscribeTopics(TopicJobs)
	defer cancel()
	probe, cancelProbe := h.SubscribeTopics(TopicGrid)
	defer cancelProbe()
	conn := l.connect()
	recv(t, probe) // the first connection's resync round is over
	recv(t, unread)
	conn <- store.Notice{JobID: 1}
	conn <- store.Notice{JobID: 2}
	close(conn)
	clock.BlockUntil(t, 1)
	clock.Advance(time.Second)
	l.connect()
	// Wait for the resync round to finish before reading: the probe's
	// resync is sent in that round.
	recv(t, probe)
	if m := recv(t, unread); m != (Msg{Topic: TopicJobs, Resync: true}) {
		t.Errorf("unread subscriber got %+v, want job 2's notice merged with the resync", m)
	}
	none(t, unread)
	h.Publish(Msg{Topic: TopicJobs})
	if m := recv(t, unread); m != (Msg{Topic: TopicJobs}) {
		t.Errorf("then got %+v, want job 3's notice: still subscribed", m)
	}
}

func TestHubStopClosesSubscribers(t *testing.T) {
	l := newFakeListener()
	h, stop := startHub(t, l, newFakeClock())
	ch, cancel := h.SubscribeTopics(TopicJobs)
	l.connect()
	recv(t, ch) // the first connection's resync
	stop()
	if m, ok := <-ch; ok {
		t.Errorf("got %+v after the hub stopped, want a closed channel", m)
	}
	cancel()
	late, cancelLate := h.SubscribeTopics(TopicGrid)
	defer cancelLate()
	if _, ok := <-late; ok {
		t.Error("a subscription after the hub stopped is open, want closed")
	}
	h.Publish(Msg{Topic: TopicGrid}) // no panic
}

// Every message gets the next sequence number, a merged message keeps the
// latest one's, and a reconnect's Resync takes one too.
func TestHubSeq(t *testing.T) {
	l := newFakeListener()
	clock := newFakeClock()
	h, _ := startHub(t, l, clock)
	jobs, cancel := h.SubscribeTopics(TopicJobs)
	defer cancel()
	grid, cancelGrid := h.SubscribeTopics(TopicGrid)
	defer cancelGrid()
	if h.Seq() != 0 {
		t.Fatalf("Seq before any message = %d", h.Seq())
	}
	h.Publish(Msg{Topic: TopicJobs, Seq: 99}) // Seq is the hub's to set
	if m := recvSeq(t, jobs); m.Seq != 1 || h.Seq() != 1 {
		t.Errorf("first message Seq %d, hub Seq %d; want 1, 1", m.Seq, h.Seq())
	}
	h.Publish(Msg{Topic: TopicGrid}) // 2
	h.Publish(Msg{Topic: TopicJobs}) // 3
	h.Publish(Msg{Topic: TopicJobs}) // 4, merged with 3
	if m := recvSeq(t, jobs); m.Seq != 4 {
		t.Errorf("merged message = %+v, want Seq 4", m)
	}
	if m := recvSeq(t, grid); m.Seq != 2 {
		t.Errorf("grid message Seq %d, want 2", m.Seq)
	}
	conn := l.connect() // the first connection's Resync is numbered 5
	for _, ch := range []<-chan Msg{jobs, grid} {
		if m := recvSeq(t, ch); !m.Resync || m.Seq != 5 {
			t.Errorf("first-connect resync = %+v, want Seq 5", m)
		}
	}
	close(conn) // lost: Seq 6
	clock.BlockUntil(t, 1)
	clock.Advance(time.Second)
	l.connect() // a reconnect: a Resync numbered 7 for everyone
	if m := recvSeq(t, jobs); !m.Resync || m.Seq != 7 {
		t.Errorf("reconnect resync = %+v, want Seq 7", m)
	}
	if m := recvSeq(t, grid); !m.Resync || m.Seq != 7 {
		t.Errorf("reconnect resync = %+v, want Seq 7", m)
	}
}

// Reads while LISTEN is down may miss changes, so Seq moves on both
// connection edges.
func TestHubSeqAdvancesOnConnectionEdges(t *testing.T) {
	l := newFakeListener()
	clock := newFakeClock()
	h, _ := startHub(t, l, clock)
	waitSeq := func(what string, want func(uint64) bool) uint64 {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if s := h.Seq(); want(s) {
				return s
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("Seq did not advance %s (still %d)", what, h.Seq())
		return 0
	}
	before := h.Seq()
	conn := l.connect()
	afterConnect := waitSeq("on the first connect", func(s uint64) bool { return s > before })
	close(conn)
	afterLoss := waitSeq("when the connection was lost", func(s uint64) bool { return s > afterConnect })
	clock.BlockUntil(t, 1)
	clock.Advance(time.Second)
	l.connect()
	waitSeq("on the reconnect", func(s uint64) bool { return s > afterLoss })
}

// One subscription can cover several topics: their messages share its
// channel and merge as any others do, and cancelling ends all of them.
func TestHubSubscribeTopics(t *testing.T) {
	h := NewHub(nil, HubOptions{})
	ch, cancel := h.SubscribeTopics(TopicGrid, TopicJobs)
	other, cancelOther := h.SubscribeTopics(TopicGrid)
	defer cancelOther()

	h.Publish(Msg{Topic: TopicJobs})
	if m := recv(t, ch); m != (Msg{Topic: TopicJobs}) {
		t.Errorf("got %+v", m)
	}
	none(t, other)

	// Unread messages of both topics merge into one.
	h.Publish(Msg{Topic: TopicGrid})
	h.Publish(Msg{Topic: TopicJobs})
	if m := recv(t, ch); m != (Msg{Topic: TopicJobs}) {
		t.Errorf("merged = %+v", m)
	}
	recv(t, other)

	// A resync reaches it once.
	h.resyncAll()
	if m := recv(t, ch); !m.Resync {
		t.Errorf("after resyncAll: %+v, want a Resync", m)
	}
	none(t, ch)

	cancel()
	if _, ok := <-ch; ok {
		t.Error("channel open after cancel")
	}
	h.mu.Lock()
	n := len(h.subs[TopicJobs])
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("%d jobs subscribers left after cancel", n)
	}
}
