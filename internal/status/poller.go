package status

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// History reports how the last finished job left each VM, and the database
// clock that job finish times use. *store.Store is one.
type History interface {
	LastItemResults(ctx context.Context, names, kinds []string) (map[string]store.ItemResult, error)
	Now(ctx context.Context) (time.Time, error)
}

var _ History = (*store.Store)(nil)

// Options tune a Poller. Zero values take the defaults.
type Options struct {
	Clock Clock                            // default SystemClock
	Hub   *Hub                             // grid changes are published here; nil for none
	Logf  func(format string, args ...any) // default log.Printf
	// Manual (tests): views never poll or stop on their own; the test drives
	// them via Views.Each.
	Manual bool
}

// WaitingForPoll is Grid.Err before the first poll finishes.
const WaitingForPoll = "waiting for the first poll of the cluster"

// Poller keeps the status grid: it polls the cluster every web.status_poll
// and when a job starts or ends, deep-scans team VMs every web.drift_scan,
// and publishes grid changes to the hub. Methods are concurrency-safe except
// Poll, which must not run concurrently with itself (follow is unlocked).
type Poller struct {
	api   pods.API
	hist  History
	lim   *apply.Limits
	rules rules
	clock Clock
	hub   *Hub
	logf  func(format string, args ...any)
	// scanWorkers is half of concurrency.config_calls (rounded up), leaving
	// the other config-call slots to jobs.
	scanWorkers int
	pollEvery   time.Duration
	scanEvery   time.Duration

	firstOK   chan struct{} // closed by the first good poll
	firstOnce sync.Once

	mu        sync.Mutex
	in        inputs
	polledAt  time.Time // when the last good poll finished; zero before one
	err       string    // why the grid is stale: WaitingForPoll, or the last poll's failure
	scannedAt time.Time
	scanErr   string
	grid      Grid
	// dbOffset is the database clock minus p.clock, from the last scan. Scan
	// times are kept in database time so they compare with job finish times
	// despite host clock skew.
	dbOffset time.Duration
	// dbClockTimeout is dbClockReadTimeout; tests shorten it.
	dbClockTimeout time.Duration

	// follow is task-list read progress (tasks.go); only Poll uses it.
	follow taskFollow
}

// NewPoller makes a poller. lim must be the Limits the job executors share,
// so scan reads count against the same cap.
func NewPoller(api pods.API, hist History, lim *apply.Limits, cfg config.Config, opts Options) (*Poller, error) {
	if api == nil || hist == nil || lim == nil {
		return nil, errors.New("status: a Proxmox API, a job history and limits are required")
	}
	if cfg.Web.StatusPoll <= 0 || cfg.Web.DriftScan <= 0 {
		return nil, errors.New("status: web.status_poll and web.drift_scan must be positive")
	}
	p := &Poller{
		api:         api,
		hist:        hist,
		lim:         lim,
		rules:       rules{cfg: cfg, naming: pods.NewNaming(cfg.Naming)},
		clock:       opts.Clock,
		hub:         opts.Hub,
		logf:        opts.Logf,
		scanWorkers: max(1, (cfg.Concurrency.ConfigCalls+1)/2),
		pollEvery:   cfg.Web.StatusPoll,
		scanEvery:   cfg.Web.DriftScan,
		firstOK:     make(chan struct{}),

		dbClockTimeout: dbClockReadTimeout,
		follow:         taskFollow{since: map[string]time.Time{}, seen: map[string]map[string]bool{}, audit: map[string]auditAnswer{}},
		err:            WaitingForPoll,
	}
	p.in.scan = scanResults{}
	if p.clock == nil {
		p.clock = SystemClock
	}
	if p.logf == nil {
		p.logf = log.Printf
	}
	p.grid = p.compose()
	return p, nil
}

// Grid returns a copy of the current grid.
func (p *Poller) Grid() Grid {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.grid.clone()
}

// Teams is the current grid's rows: teams with VMs in the last good poll,
// sorted.
func (p *Poller) Teams() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.grid.Teams)
}

// Version is the current grid's version, without copying the grid.
func (p *Poller) Version() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.grid.Version
}

// Run polls and scans until ctx is done. The first scan starts after the
// first good poll, and each waits its interval after the previous one ends.
func (p *Poller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.scanLoop(ctx)
	}()
	// Poll when a job starts or ends, so the grid shows what it left as its
	// busy marks clear.
	var jobs <-chan Msg
	if p.hub != nil {
		var unsubscribe func()
		jobs, unsubscribe = p.hub.SubscribeTopics(TopicJobs)
		defer unsubscribe()
	}
	for {
		// A hung poll must not keep the grid from going stale for long.
		pctx, cancel := context.WithTimeout(ctx, max(3*p.pollEvery, 10*time.Second))
		p.Poll(pctx) //nolint:errcheck // recorded in the grid and logged
		cancel()
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-p.clock.After(p.pollEvery):
		case _, ok := <-jobs:
			if !ok { // the hub stopped
				jobs = nil
			}
		}
	}
}

func (p *Poller) scanLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-p.firstOK:
	}
	for {
		if err := p.Scan(ctx); err != nil && ctx.Err() == nil {
			p.logf("status: drift scan: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.clock.After(p.scanEvery):
		}
	}
}

// Poll reads the cluster and job history once and updates the grid. On
// failure the grid keeps the last good cells and is marked stale. A
// cancelled poll changes nothing; a timed-out one counts as failed.
func (p *Poller) Poll(ctx context.Context) error {
	vms, err := p.api.ClusterVMs(ctx)
	if err != nil {
		return p.pollFailed(ctx, fmt.Errorf("reading the cluster from Proxmox: %s", proxmox.Describe(err)))
	}
	var names []string
	teamVMs := pods.AllTeamVMs(vms, p.rules.naming)
	for _, vm := range teamVMs {
		names = append(names, vm.Name)
	}
	history, err := p.hist.LastItemResults(ctx, names, driftKinds)
	if err != nil {
		return p.pollFailed(ctx, fmt.Errorf("reading the job history: %w", err))
	}
	p.mu.Lock()
	offset := p.dbOffset
	p.mu.Unlock()
	rescanned := p.touchedByTasks(ctx, teamVMs, offset)

	p.mu.Lock()
	recovered := p.failing()
	p.in.vms, p.in.history = vms, history
	for name, r := range rescanned {
		p.in.scan.put(name, r)
	}
	p.polledAt = p.clock.Now()
	p.err = ""
	changed := p.update()
	p.mu.Unlock()

	if recovered {
		p.logf("status: polling the cluster works again")
	}
	p.publish(changed)
	p.firstOnce.Do(func() { close(p.firstOK) })
	return nil
}

func (p *Poller) pollFailed(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return err // shutting down, not a failure worth showing
	}
	p.mu.Lock()
	first := !p.failing()
	p.err = err.Error()
	lastOK := p.polledAt
	changed := p.update()
	p.mu.Unlock()

	if first {
		since := "no good poll yet"
		if !lastOK.IsZero() {
			since = "showing the poll from " + lastOK.Format(time.RFC3339)
		}
		p.logf("status: polling failed: %v; the grid is stale (%s)", err, since)
	}
	p.publish(changed)
	return err
}

// failing reports whether the last poll failed (not just that none has
// finished yet). p.mu must be held.
func (p *Poller) failing() bool { return p.err != "" && p.err != WaitingForPoll }

// compose builds the grid from the current inputs. p.mu must be held.
func (p *Poller) compose() Grid {
	teams, hosts, rows := p.rules.grid(p.in)
	sets, others := pods.MasterSets(p.in.vms, p.rules.naming, p.rules.cfg.Deploy.MasterTag)
	return Grid{
		Sets:         sets,
		OtherMasters: others,
		Teams:        teams,
		Hosts:        hosts,
		Rows:         rows,
		PolledAt:     p.polledAt,
		Stale:        p.err != "",
		Err:          p.err,
		ScannedAt:    p.scannedAt,
		ScanErr:      p.scanErr,
	}
}

// update rebuilds the grid and bumps its version if anything but PolledAt
// changed, reporting whether it did. p.mu must be held.
func (p *Poller) update() bool {
	g := p.compose()
	if sameContent(g, p.grid) {
		p.grid.PolledAt = g.PolledAt
		return false
	}
	g.Version = p.grid.Version + 1
	p.grid = g
	return true
}

// publish tells the hub the grid changed. Call without p.mu.
func (p *Poller) publish(changed bool) {
	if changed && p.hub != nil {
		p.hub.Publish(Msg{Topic: TopicGrid})
	}
}
