package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
)

func TestGridEvents(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	c := h.openSSE(&op, "/events/grid")
	if c.resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events/grid = %d", c.resp.StatusCode)
	}
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := c.resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// The stream starts with the whole grid, so a page that missed changes
	// between rendering and connecting catches up.
	ev := c.next()
	if ev.Event != "grid" {
		t.Fatalf("first event = %+v, want grid", ev)
	}
	contains(t, "first grid event", ev.Data,
		`<div class="pop" id="grid-status"`, `id="cell-01-dc"><label class="cell is-running"`, `id="grid-table"`, `<ul class="legend" id="grid-counts"`)
	if strings.Contains(ev.Data, "<html") || strings.Contains(ev.Data, "<nav") {
		t.Errorf("grid event is a whole page, want the fragment:\n%s", ev.Data)
	}

	// A change in the cluster reaches the stream at the next poll, as a
	// patch of just what changed: the cell and the totals.
	h.api.setStatus(10101, "stopped")
	h.poll()
	ev = c.next()
	if ev.Event != "patch" {
		t.Fatalf("event after the change = %+v, want patch", ev)
	}
	contains(t, "patch after the change", ev.Data,
		`id="cell-01-dc"><label class="cell is-stopped"`, `<ul class="legend" id="grid-counts"`, "<b>5</b>running", "<b>1</b>stopped")
	lacks(t, "patch after the change", ev.Data, "cell-01-web", "cell-02-dc", `id="grid-table"`, `id="grid-status"`)

	// The grid going stale: the whole grid again, with the live dot's
	// pop-over.
	h.api.setListErr(&proxmox.APIError{Status: 503, Message: "proxy timeout"})
	_ = h.poller.Poll(t.Context())
	ev = c.next()
	if ev.Event != "grid" {
		t.Fatalf("event when stale = %+v, want grid", ev)
	}
	contains(t, "grid event when stale", ev.Data, "No answer from Proxmox", "proxy timeout", `data-live="stale"`, `id="grid-table"`)
}

func TestGridEventsSkipUnchanged(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/grid")
	c.next()
	h.clock.BlockUntil(t, 2)

	// A notice that changes nothing shown sends nothing: the next thing on
	// the stream is the heartbeat.
	h.hub.Publish(status.Msg{Topic: status.TopicGrid})
	h.clock.Advance(heartbeatEvery)
	if ev := c.next(); ev.Comment != "heartbeat" {
		t.Fatalf("got %+v, want the heartbeat", ev)
	}

	// A resync sends the whole grid.
	h.hub.Publish(status.Msg{Topic: status.TopicGrid, Resync: true})
	if ev := c.next(); ev.Event != "grid" || !strings.Contains(ev.Data, `id="grid-table"`) {
		t.Fatalf("after a resync: %+v, want the whole grid", ev)
	}
}

func TestGridEventsSameAsPage(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	page := h.get(&op, "/").Body.String()
	ev := h.openSSE(&op, "/events/grid").next()
	ps := pieces(t, ev.Data)
	if len(ps) != 3 {
		t.Fatalf("the grid event has %d pieces, want the live grid, the pop-over and the legend:\n%s", len(ps), ev.Data)
	}
	for _, p := range ps {
		if !strings.Contains(page, p) {
			t.Errorf("the page doesn't contain the event's piece verbatim; the script can't diff them\npiece:\n%s", p)
		}
	}
}

func TestStreamHeartbeat(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/grid")
	c.next() // the grid

	h.clock.BlockUntil(t, 2) // the heartbeat and the stream's end
	h.clock.Advance(heartbeatEvery - time.Second)
	h.clock.Advance(time.Second)
	if ev := c.next(); ev.Comment != "heartbeat" {
		t.Fatalf("after 15s: %+v, want a heartbeat comment", ev)
	}
	h.clock.BlockUntil(t, 2)
	h.clock.Advance(heartbeatEvery)
	if ev := c.next(); ev.Comment != "heartbeat" {
		t.Fatalf("after 30s: %+v, want another heartbeat", ev)
	}
}

func TestStreamEndsAtSessionRefresh(t *testing.T) {
	// A stream ends every web.session_refresh, so the browser reconnects
	// through the login check: someone who lost access stops getting
	// updates, and the session's refresh happens as usual.
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/grid")
	c.next()
	h.clock.BlockUntil(t, 2)
	beats := int(h.cfg.Web.SessionRefresh / heartbeatEvery)
	for range beats - 1 {
		h.clock.Advance(heartbeatEvery)
		if ev := c.next(); ev.Comment != "heartbeat" {
			t.Fatalf("got %+v, want a heartbeat", ev)
		}
		h.clock.BlockUntil(t, 2)
	}
	// The last heartbeat falls due with the end; the end wins.
	h.clock.Advance(heartbeatEvery)
	c.ended()
}

func TestStreamEndsWhenHubStops(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/grid")
	c.next()
	h.stop() // the hub stops, closing every subscription
	c.ended()
}

func TestCloseStreams(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/grid")
	c.next()
	h.srv.CloseStreams()
	c.ended()
	h.srv.CloseStreams() // idempotent

	// New streams are refused, and told to wait before retrying.
	c = h.openSSE(&op, "/events/grid")
	if c.resp.StatusCode != http.StatusServiceUnavailable || c.resp.Header.Get("Retry-After") == "" {
		t.Errorf("stream after CloseStreams = %d, Retry-After %q; want 503 with Retry-After", c.resp.StatusCode, c.resp.Header.Get("Retry-After"))
	}
}

func TestStreamRetryHint(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	srv := httptest.NewServer(h.h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/events/grid", nil)
	op.Apply(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, len("retry: 2000\n\n"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "retry: 2000\n\n" {
		t.Errorf("stream starts %q (%v), want the retry hint", buf, err)
	}
}

func TestStreamHead(t *testing.T) {
	// The script checks the page with HEAD; a HEAD of a stream must answer
	// at once rather than stream.
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	rec := h.do(&op, http.MethodHead, "/events/grid", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Body.Len() != 0 {
		t.Errorf("HEAD /events/grid = %d %q with %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
}

// TestStreamCoalesces drives the stream helper directly: messages that
// arrive while a send is held back by the gap become one send, and a Resync
// among them is passed on.
func TestStreamCoalesces(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var sends []bool
	sent := make(chan struct{}, 10)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.srv.stream(w, r, []string{"test"}, time.Second, func(sw *sseWriter, resync bool, _ uint64) error {
			mu.Lock()
			sends = append(sends, resync)
			n := len(sends)
			mu.Unlock()
			defer func() { sent <- struct{}{} }()
			return sw.event("n", "", fmt.Sprint(n))
		})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	wait := func() {
		t.Helper()
		select {
		case <-sent:
		case <-time.After(10 * time.Second):
			t.Fatal("no send after 10s")
		}
	}
	wait()                   // the first send, a resync
	h.clock.BlockUntil(t, 2) // heartbeat and stream end
	h.hub.Publish(status.Msg{Topic: "test"})
	h.clock.BlockUntil(t, 3) // held back by the gap
	h.hub.Publish(status.Msg{Topic: "test"})
	h.hub.Publish(status.Msg{Topic: "test", Resync: true})
	h.hub.Publish(status.Msg{Topic: "test"})
	h.clock.Advance(time.Second)
	wait()
	select {
	case <-sent:
		t.Fatal("a second send, want the messages coalesced into one")
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sends) != 2 || !sends[0] || !sends[1] {
		t.Errorf("sends (resync flags) = %v, want [true true]", sends)
	}
}

func TestSSEWriterSplitsLines(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &sseWriter{w: rec, rc: http.NewResponseController(rec)}
	if err := sw.event("grid", "7", "<p>\r\nhi\n</p>"); err != nil {
		t.Fatal(err)
	}
	if err := sw.comment("heartbeat"); err != nil {
		t.Fatal(err)
	}
	want := "event: grid\nid: 7\ndata: <p>\ndata: hi\ndata: </p>\n\n: heartbeat\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
	if err := sw.event("bad\nname", "", "x"); err == nil {
		t.Error("an event name with a newline was accepted")
	}
}

// deadlineRecorder records the write deadlines an sseWriter sets.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

// Each write gets its own deadline, so a client that stops reading can't
// hold a stream's goroutine for ever.
func TestSSEWriteDeadlinePerEvent(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	check := func(what string, write func() error) {
		t.Helper()
		before := time.Now()
		n := len(rec.deadlines)
		if err := write(); err != nil {
			t.Fatal(err)
		}
		after := time.Now()
		if len(rec.deadlines) <= n {
			t.Fatalf("%s set no write deadline", what)
		}
		d := rec.deadlines[len(rec.deadlines)-1]
		if d.Before(before.Add(sseWriteTimeout)) || d.After(after.Add(sseWriteTimeout)) {
			t.Errorf("%s deadline %v, want %s from now", what, d, sseWriteTimeout)
		}
	}
	var sw *sseWriter
	check("start", func() (err error) { sw, err = startSSE(rec); return })
	check("event", func() error { return sw.event("grid", "", "x") })
	check("comment", func() error { return sw.comment("heartbeat") })
	if sseWriteTimeout != 30*time.Second {
		t.Errorf("sseWriteTimeout = %s, want 30s", sseWriteTimeout)
	}
}

// One viewer's grid streams (their tabs) share one rendering per grid
// version.
func TestGridEventsShareRenders(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	a := h.openSSE(&op, "/events/grid")
	ea := a.next()
	b := h.openSSE(&op, "/events/grid")
	eb := b.next()
	if ea != eb {
		t.Errorf("two tabs got different grids:\n%+v\n%+v", ea, eb)
	}
	if n := h.srv.gridRenders.Load(); n != 1 {
		t.Errorf("two streams of one grid version rendered it %d times, want 1", n)
	}
	h.api.setStatus(10101, "stopped")
	h.poll()
	pa, pb := a.next(), b.next()
	if pa.Event != "patch" || pa != pb {
		t.Errorf("after a change: %+v and %+v, want the same patch", pa, pb)
	}
	if n := h.srv.gridRenders.Load(); n != 2 {
		t.Errorf("after one change the grid was rendered %d times, want 2", n)
	}
}

// The grid says when things happened by the clock, never "3 min ago",
// which would freeze between events.
func TestGridHasNoRelativeTimes(t *testing.T) {
	h := newHarness(t)
	h.poll()
	h.api.setListErr(errors.New("proxy timeout"))
	h.clock.Advance(2 * time.Minute)
	_ = h.poller.Poll(t.Context())
	op := h.login(asOperator)
	ev := h.openSSE(&op, "/events/grid").next()
	contains(t, "stale grid", ev.Data, "last update 09:00:00 UTC")
	lacks(t, "stale grid", ev.Data, "ago", "just now")
}

// A job notice that changes no busy mark costs a grid stream its busy key
// and the grid's version, not a copy of the grid and a view of it.
func TestGridEventsCheapWhenNothingChanged(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/grid")
	c.next()
	views := h.srv.gridViews.Load()
	for range 20 {
		h.hub.Publish(status.Msg{Topic: status.TopicJobs})
	}
	h.clock.BlockUntil(t, 2)
	h.clock.Advance(heartbeatEvery)
	if ev := c.next(); ev.Comment != "heartbeat" {
		t.Fatalf("got %+v, want the heartbeat", ev)
	}
	if n := h.srv.gridViews.Load() - views; n != 0 {
		t.Errorf("20 job notices that changed nothing built %d grid views, want 0", n)
	}
}

// Streams end a little after web.session_refresh, by up to a tenth more, so
// the browsers of a crowd that loaded the page together don't all
// reconnect at once.
func TestStreamEndJitter(t *testing.T) {
	h := newHarness(t)
	h.poll()
	h.srv.jitter = func() float64 { return 0.5 }
	op := h.login(asOperator)
	c := h.openSSE(&op, "/events/grid")
	c.next()
	h.clock.BlockUntil(t, 2)
	refresh := h.cfg.Web.SessionRefresh
	if w := h.clock.Waits(); len(w) != 2 || w[1] != refresh+refresh/20 {
		t.Errorf("waits = %v, want a heartbeat and the end at %s", w, refresh+refresh/20)
	}

	// The real jitter stays within a tenth.
	s, err := New(Deps{Config: h.cfg, Store: h.st, As: h.as, Credentials: h.creds, Auth: h.srv.auth, Views: h.views, Hub: h.hub})
	if err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		if d := s.streamLifetime(time.Time{}); d < refresh || d > refresh+refresh/10 {
			t.Fatalf("streamLifetime = %s, want %s to %s", d, refresh, refresh+refresh/10)
		}
	}
}

// A burst bigger than any per-subscriber queue, published while one send is
// held back by the gap, becomes a single send: the hub coalesces rather
// than drops, and the stream drains its messages while it waits. The
// stream stays open afterwards, and nothing is logged as dropped.
func TestStreamBurstDuringGapIsOneSend(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var sends []bool
	sent := make(chan struct{}, 10)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.srv.stream(w, r, []string{"test"}, time.Second, func(sw *sseWriter, resync bool, _ uint64) error {
			mu.Lock()
			sends = append(sends, resync)
			n := len(sends)
			mu.Unlock()
			defer func() { sent <- struct{}{} }()
			return sw.event("n", "", fmt.Sprint(n))
		})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	wait := func() {
		t.Helper()
		select {
		case <-sent:
		case <-time.After(10 * time.Second):
			t.Fatal("no send after 10s")
		}
	}
	wait()                   // the first send
	h.clock.BlockUntil(t, 2) // heartbeat and stream end
	h.hub.Publish(status.Msg{Topic: "test"})
	h.clock.BlockUntil(t, 3) // held back by the gap
	for range 499 {
		h.hub.Publish(status.Msg{Topic: "test"})
	}
	h.clock.Advance(time.Second)
	wait()
	select {
	case <-sent:
		t.Fatal("a second send, want the burst coalesced into one")
	default:
	}

	// Still subscribed: the next change is sent too, after the gap.
	h.clock.BlockUntil(t, 2)
	h.hub.Publish(status.Msg{Topic: "test"})
	h.clock.BlockUntil(t, 3)
	h.clock.Advance(time.Second)
	wait()
	mu.Lock()
	got := fmt.Sprint(sends)
	mu.Unlock()
	if got != "[true false false]" {
		t.Errorf("sends (resync flags) = %s, want [true false false]", got)
	}
}

// A stream ends when its session's next group check falls due, not a whole
// web.session_refresh after it opened: a page opened just before the check
// would otherwise keep a user who lost access updated for nearly two
// periods.
func TestStreamEndsAtSessionsNextRefresh(t *testing.T) {
	h := newHarness(t)
	h.poll()
	h.srv.jitter = func() float64 { return 0.5 }
	refresh := h.cfg.Web.SessionRefresh
	op := h.login(asOperator) // refreshed (logged in) now
	h.clock.Advance(refresh - 2*time.Minute)
	c := h.openSSE(&op, "/events/grid")
	c.next()
	h.clock.BlockUntil(t, 2)
	want := 2*time.Minute + refresh/20 // the refresh is due in 2m, plus the jitter
	if w := h.clock.Waits(); len(w) != 2 || w[1] != want {
		t.Errorf("waits = %v, want a heartbeat and the end at %s", w, want)
	}
}

func TestStreamEnd(t *testing.T) {
	start := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	const refresh = 5 * time.Minute
	for _, tc := range []struct {
		name        string
		refreshedAt time.Time
		extra, want time.Duration
	}{
		{"no session", time.Time{}, 10 * time.Second, refresh + 10*time.Second},
		{"just refreshed", start, 10 * time.Second, refresh + 10*time.Second},
		{"refresh due in 1m", start.Add(-4 * time.Minute), 10 * time.Second, time.Minute + 10*time.Second},
		// The refresh is overdue: it is being retried, or the identity
		// provider is unreachable and it backs off. Don't end the stream at
		// once, or the browser would reconnect every 2s until it's done.
		{"refresh overdue", start.Add(-20 * time.Minute), 10 * time.Second, minStreamLifetime},
		{"refresh due in 10s", start.Add(-refresh + 10*time.Second), 0, minStreamLifetime},
		// A refresh time in the future (clock skew) never extends a stream.
		{"refreshed in the future", start.Add(time.Hour), 10 * time.Second, refresh + 10*time.Second},
	} {
		if got := streamEnd(start, tc.refreshedAt, refresh, tc.extra); got != tc.want {
			t.Errorf("%s: streamEnd = %s, want %s", tc.name, got, tc.want)
		}
	}
	if minStreamLifetime != time.Minute {
		t.Errorf("minStreamLifetime = %s, want 1m (the refresh backoff)", minStreamLifetime)
	}
}
