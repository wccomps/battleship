package status

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// fastLoops sets a 5s poll and a 10s scan.
func fastLoops(c *config.Config, _ *Options) {
	c.Web.StatusPoll = 5 * time.Second
	c.Web.DriftScan = 10 * time.Second
}

// startRun runs the poller until the test ends.
func startRun(t *testing.T, h *harness) {
	t.Helper()
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.p.Run(rctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestRunPollsAndScansOnCadence(t *testing.T) {
	h := newHarness(t, fastLoops)
	driftCluster(h)
	sub, cancel := h.hub.SubscribeTopics(TopicGrid)
	defer cancel()
	start := h.clock.Now()
	startRun(t, h)

	// First poll, then first scan; the hub may merge their messages.
	for recv(t, sub); h.p.Grid().ScannedAt.IsZero(); recv(t, sub) {
	}
	h.clock.BlockUntil(t, 2) // both loops wait for their next turn
	if g := h.p.Grid(); !g.ScannedAt.Equal(start) || cell(t, g, "01", "web").State != StateDrifted {
		t.Errorf("after the first scan: scanned %v, 01/web %s", g.ScannedAt, cell(t, g, "01", "web").State)
	}
	reads, lists, _ := h.api.counts()
	if reads != 8 || lists != 1 {
		t.Errorf("reads %d lists %d, want 8 and 1", reads, lists)
	}

	// 5s later only the poll runs.
	h.api.setStatus(10101, "stopped")
	h.clock.Advance(5 * time.Second)
	recv(t, sub)
	h.clock.BlockUntil(t, 2)
	if reads, lists, _ := h.api.counts(); reads != 8 || lists != 2 {
		t.Errorf("after 5s: reads %d lists %d, want 8 and 2", reads, lists)
	}
	if g := h.p.Grid(); cell(t, g, "01", "dc").State != StateStopped || !h.p.Grid().PolledAt.Equal(h.clock.Now()) {
		t.Errorf("after 5s: 01/dc %s, LastOK %v", cell(t, g, "01", "dc").State, h.p.Grid().PolledAt)
	}

	// At 10s both run: the poll and the second scan.
	h.api.setConfig(10102, "net1", "virtio=BC:24:11:00:00:02,bridge=int01")
	h.clock.Advance(5 * time.Second)
	h.clock.BlockUntil(t, 2)
	reads, lists, _ = h.api.counts()
	if reads != 16 || lists != 3 {
		t.Errorf("after 10s: reads %d lists %d, want 16 and 3", reads, lists)
	}
	if g := h.p.Grid(); !g.ScannedAt.Equal(start.Add(10*time.Second)) || cell(t, g, "01", "web").State != StateRunning {
		t.Errorf("after the second scan: scanned %v, 01/web %s", g.ScannedAt, cell(t, g, "01", "web").State)
	}
}

func TestRunScansOnlyAfterAGoodPoll(t *testing.T) {
	h := newHarness(t, fastLoops)
	driftCluster(h)
	h.api.setListErr(&proxmox.APIError{Status: 595, Message: "no route to host"})
	sub, cancel := h.hub.SubscribeTopics(TopicGrid)
	defer cancel()
	startRun(t, h)
	recv(t, sub)             // the failed first poll: stale with the error
	h.clock.BlockUntil(t, 1) // the poll loop; the scan loop waits for a good poll
	if reads, _, _ := h.api.counts(); reads != 0 {
		t.Errorf("%d reads before any good poll, want 0", reads)
	}
	if g := h.p.Grid(); !g.Stale || !h.p.Grid().PolledAt.IsZero() {
		t.Errorf("stale %v LastOK %v, want stale and zero", g.Stale, h.p.Grid().PolledAt)
	}
	h.api.setListErr(nil)
	h.clock.Advance(5 * time.Second)
	h.clock.BlockUntil(t, 2) // the poll and the finished scan
	if reads, _, _ := h.api.counts(); reads != 8 {
		t.Errorf("reads = %d after the first good poll, want 8", reads)
	}
}

func TestGridIsSafeForConcurrentReaders(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.poll(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				g := h.p.Grid()
				for _, row := range g.Rows {
					for j := range row.Cells {
						row.Cells[j].State = StateMissing // readers may change their copy
					}
				}
				_ = h.p.Grid().PolledAt
			}
		}()
	}
	for i := 0; i < 20; i++ {
		h.api.setStatus(10101, []string{"running", "stopped"}[i%2])
		h.poll(t)
		h.scan(t)
		if _, err := h.p.Detail(ctx, "01", "web"); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if c := cell(t, h.p.Grid(), "01", "dc"); c.State != StateStopped {
		t.Errorf("01/dc = %s, want stopped (readers must not change the poller's grid)", c.State)
	}
}

// A job starting or ending polls at once, not at the next turn: the grid
// shows what the job left as its busy marks clear.
func TestRunPollsWhenAJobChanges(t *testing.T) {
	h := newHarness(t, fastLoops)
	driftCluster(h)
	sub, cancel := h.hub.SubscribeTopics(TopicGrid)
	defer cancel()
	startRun(t, h)
	for recv(t, sub); h.p.Grid().ScannedAt.IsZero(); recv(t, sub) {
	}
	h.clock.BlockUntil(t, 2)

	h.api.setStatus(10101, "stopped")
	h.hub.Publish(Msg{Topic: TopicJobs})
	recv(t, sub) // no clock advance
	if g := h.p.Grid(); cell(t, g, "01", "dc").State != StateStopped {
		t.Errorf("after a job change: 01/dc %s, want stopped", cell(t, g, "01", "dc").State)
	}
}
