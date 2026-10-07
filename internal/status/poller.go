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

// History tells how the last finished deploy, reset or teardown that touched each VM
// left it, and what time the database's clock says, which job finish times
// are in. *store.Store is one.
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
	// Manual, for Views in tests: views don't poll by themselves and never
	// stop; the test drives them (Views.Each).
	Manual bool
}

// WaitingForPoll is Grid.Err before the first poll finishes.
const WaitingForPoll = "waiting for the first poll of the cluster"

// Poller keeps the status grid: it polls the cluster every web.status_poll
// and whenever a job starts or ends, deep-scans every team VM every
// web.drift_scan, and publishes each change of the grid to the hub. Its methods are safe for concurrent use, except
// that Poll must not run beside itself (it keeps its task follow outside
// the lock); Run calls it from one goroutine.
type Poller struct {
	api   pods.API
	hist  History
	lim   *apply.Limits
	rules rules
	clock Clock
	hub   *Hub
	logf  func(format string, args ...any)
	// scanWorkers is how many VMs the deep scan reads at once, each holding
	// at most one of the shared config-call slots at a time: half of
	// concurrency.config_calls rounded up, leaving the rest to jobs.
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
	// dbOffset is the database's clock minus p.clock, as the last scan
	// measured it. Scan read times are kept in the database's clock, so they
	// compare with job finish times whatever the skew between the hosts.
	dbOffset time.Duration
	// dbClockTimeout bounds a scan's database clock read: dbClockReadTimeout,
	// which tests shorten.
	dbClockTimeout time.Duration

	// follow is where reading task lists has got to (tasks.go); only Poll
	// uses it, one at a time.
	follow taskFollow
}

// NewPoller makes a poller over the teams that have team VMs. lim must be
// the Limits the process's job executors share, so the deep scan's reads count
// against the same cap as theirs.
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

// Teams is the current grid's rows: the teams that had team VMs in the
// last good poll, sorted: every team that exists, as far as this viewer
// can see.
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
	// A job that starts or ends has just changed VMs, or is about to: poll
	// then rather than up to pollEvery later, so the grid shows what it
	// left as its busy marks clear.
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

// Poll reads the cluster and the job history once and updates the grid. If
// either read fails, the grid keeps the last good poll's cells and is marked
// stale with the error, which Poll also returns. A poll cancelled by ctx
// changes nothing; one that passes ctx's deadline counts as failed.
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

// publish tells the hub that the grid changed. It is called without p.mu;
// subscribers re-read Grid, which is always the latest.
func (p *Poller) publish(changed bool) {
	if changed && p.hub != nil {
		p.hub.Publish(Msg{Topic: TopicGrid})
	}
}
