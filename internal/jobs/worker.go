package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

var (
	errLostClaim   = errors.New("job was taken away from this worker")
	errNoHeartbeat = errors.New("lost contact with the database")
	// errAuthLapsed: the submitter's Proxmox credential can't be used any
	// more (Proxmox refused it, it expired, or the renewer dropped it).
	errAuthLapsed = errors.New("authorization lapsed")
)

// DBCallTimeout bounds each worker claim/reap and each CLI database call, so a
// dead connection can't stall either.
const DBCallTimeout = 30 * time.Second

// eventTimeoutFloor keeps a very short heartbeat interval (tests use 20ms) from
// making event writes time out under load; real intervals are at least 1s.
const eventTimeoutFloor = 2 * time.Second

// Worker claims and runs pending jobs one at a time. Workers can share a
// store; lock keys keep overlapping jobs apart.
type Worker struct {
	Store *store.Store
	Cfg   config.Config
	// Bind gives the API a job runs with. Each call carries cred()'s current
	// value (the submitter's credential) and calls refused on a 401, so the
	// job stops at once. The worker has no credential of its own.
	Bind func(cred func() proxmox.Credential, refused func()) pods.API
	// Credentials opens the jobs' sealed credentials.
	Credentials Credentials
	// ID names this worker in claims; it must be unique among running workers.
	ID string
	// Limits is shared by every job this worker runs, and should be shared
	// with other workers in the same process. Nil means one per Worker.
	Limits *apply.Limits
	// Logf reports problems that don't fail a job, e.g. a lost database
	// connection. Nil discards them.
	Logf func(format string, args ...any)
	// OnStart, if set, is called when a claimed job starts; OnFinish after
	// its outcome is recorded (not for a job whose claim was reaped).
	OnStart  func(job *store.Job)
	OnFinish func(job *store.Job, out store.Outcome)
	// Hub, if set, wakes the worker when jobs change and when its job's
	// cancel is requested. Nil: an idle worker looks for jobs every
	// Jobs.Poll, and cancels wait for the next heartbeat.
	Hub *status.Hub

	// onEvent, if set, also receives each progress event as it is recorded,
	// from many goroutines; tests use it to act at a point in a run.
	onEvent func(apply.Event)
	// beat sends a heartbeat; tests replace it. Nil means Store.Heartbeat.
	beat func(ctx context.Context, jobID int64, worker string) (bool, error)
	// now and monoNow read the wall and the monotonic clock for a claim's
	// lease; tests replace them.
	now, monoNow func() time.Time

	once sync.Once
}

func (w *Worker) init() {
	w.once.Do(func() {
		if w.Limits == nil {
			w.Limits = apply.NewClusterLimits(w.Cfg.Concurrency, w.Store)
		}
		if w.Logf == nil {
			w.Logf = func(string, ...any) {}
		}
		if w.beat == nil {
			w.beat = w.Store.Heartbeat
		}
		if w.now == nil {
			w.now = time.Now
		}
		if w.monoNow == nil {
			w.monoNow = time.Now
		}
	})
}

// Run claims and runs jobs until ctx is done, first reaping jobs whose
// workers went silent each pass.
func (w *Worker) Run(ctx context.Context) error {
	w.init()
	idle := w.Cfg.Jobs.Poll
	var changed <-chan struct{}
	if w.Hub != nil {
		var stop func()
		changed, stop = w.Hub.Wake(status.TopicJobs)
		defer stop()
		// The hub announces every change that can free a job, and resyncs
		// after reconnecting, so the timer is only for reaping.
		idle = w.Cfg.Jobs.StaleAfter
	}
	for ctx.Err() == nil {
		reapCtx, stopReap := context.WithTimeout(ctx, DBCallTimeout)
		ids, err := w.Store.ReapStale(reapCtx, w.Cfg.Jobs.StaleAfter)
		stopReap()
		switch {
		case err != nil && ctx.Err() == nil: // not just the worker stopping
			w.Logf("reaping stale jobs: %v", err)
		case len(ids) > 0:
			w.Logf("marked jobs %v interrupted: their workers stopped sending heartbeats", ids)
		}
		job, claim, err := w.claimNext(ctx)
		if err != nil {
			if ctx.Err() == nil {
				w.Logf("claiming a job: %v", err)
			}
		}
		if job != nil {
			w.runClaimed(ctx, job, claim)
			continue
		}
		t := time.NewTimer(idle)
		select {
		case <-ctx.Done():
		case <-t.C:
		case _, ok := <-changed:
			if !ok { // the hub stopped: poll instead
				changed, idle = nil, w.Cfg.Jobs.Poll
			}
		}
		t.Stop()
	}
	return nil
}

// claimNext claims the next runnable job, if any, with its lease. The
// claim's heartbeat stamp is taken during Claim, so the lease, started
// before it, never outlasts the claim.
func (w *Worker) claimNext(ctx context.Context) (*store.Job, *lease, error) {
	claim := w.newLease()
	claimCtx, stop := context.WithTimeout(ctx, DBCallTimeout)
	defer stop()
	job, err := w.Store.Claim(claimCtx, w.ID, claim.stale)
	return job, claim, err
}

// runClaimed runs a job this worker has claimed under lease claim (see
// claimNext) and records how it ended. A shutdown or cancel stops it
// gracefully (cleanup included) and records it interrupted or cancelled; a
// stop after every runnable VM finished changes nothing.
func (w *Worker) runClaimed(ctx context.Context, job *store.Job, claim *lease) {
	w.init()
	var wake <-chan struct{}
	if w.Hub != nil {
		var unsubscribe func()
		wake, unsubscribe = w.Hub.Wake(status.CancelTopic(job.ID))
		defer unsubscribe()
	}
	if w.OnStart != nil {
		w.OnStart(job)
	}
	// Record the outcome even while shutting down.
	bg := context.WithoutCancel(ctx)
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	hbDone := make(chan struct{})
	go w.heartbeat(bg, job.ID, claim, cancel, wake, hbDone)
	stopHeartbeat := func() { close(hbDone) }

	var outcome store.Outcome
	var finished bool
	cred, err := w.jobCredential(bg, job.ID)
	switch {
	case errors.Is(err, errAuthLapsed):
		w.Logf("job %d: %v", job.ID, err)
		outcome = store.Outcome{Status: store.StatusStale, Error: lapsedMessage}
	case err != nil:
		outcome = store.Outcome{Status: store.StatusFailed, Error: err.Error() + "; nothing was changed. Re-run it."}
	default:
		held := &heldCredential{}
		held.set(cred)
		go w.watchCredential(bg, job.ID, held, cancel, hbDone)
		api := w.Bind(held.get, func() { cancel(errAuthLapsed) })
		outcome, finished = w.execute(runCtx, bg, ctx.Done(), job, api, claim.held)
	}
	// A lost claim (errLostClaim) needs no case: Finish refuses it below.
	switch cause := context.Cause(runCtx); {
	case errors.Is(cause, errAuthLapsed) && !(finished && outcome.Status == store.StatusSucceeded):
		// Before finished: a 401 in the last round interrupts no VM but
		// still stopped the job. No Summary means it lapsed while planning.
		if outcome.Summary == nil {
			outcome = store.Outcome{Status: store.StatusStale, Error: lapsedMessage}
			break
		}
		outcome.Status = store.StatusInterrupted
		outcome.Error = "authorization lapsed: Proxmox no longer accepts the submitter's login, so the job stopped; whoever re-runs it acts as themselves"
	case finished:
	case errors.Is(cause, apply.ErrCancelRequested):
		outcome.Status = store.StatusCancelled
	case errors.Is(cause, errNoHeartbeat):
		// Stopped before another worker may reap it. Cleanup runs, but
		// sends a change only while the claim is sure (see lease).
		if store.JobStatus(outcome.Status).RanNothing() {
			break
		}
		outcome.Status = store.StatusInterrupted
		outcome.Error = "lost contact with the database, so the job stopped before another worker could take over; re-run it to finish"
	case ctx.Err() != nil && !store.JobStatus(outcome.Status).RanNothing():
		outcome.Status = store.StatusInterrupted
		outcome.Error = "the worker shut down during the job; re-run it to finish"
	}
	stopHeartbeat()
	// Finish refuses a job that is no longer this worker's.
	finCtx, stopFin := context.WithTimeout(bg, 30*time.Second)
	defer stopFin()
	// An outcome the database won't take is logged whole, so what cleanup
	// left (masters stopped, VMs to remove by hand) isn't lost with it.
	switch err := w.Store.Finish(finCtx, job.ID, w.ID, outcome); {
	case errors.Is(err, store.ErrLostClaim):
		w.Logf("job %d: %v; not recording its outcome: %s", job.ID, errLostClaim, describeOutcome(outcome))
		return
	case err != nil:
		w.Logf("job %d: recording outcome: %v; the outcome was: %s", job.ID, err, describeOutcome(outcome))
		return
	}
	if w.OnFinish != nil {
		w.OnFinish(job, outcome)
	}
}

func describeOutcome(o store.Outcome) string {
	msg := string(o.Status)
	if o.Error != "" {
		msg += ": " + o.Error
	}
	if len(o.Summary) > 0 {
		msg += "; summary " + string(o.Summary)
	}
	return msg
}

// heartbeat beats every Jobs.Heartbeat (and at once on wake) until done is
// closed, renewing claim on each successful beat and cancelling the run on
// a cancel request, a lost claim, or (errNoHeartbeat) once the claim is
// unsure (see lease.watch). Beating continues afterwards to keep the job
// claimed during cleanup.
func (w *Worker) heartbeat(ctx context.Context, jobID int64, claim *lease, cancel context.CancelCauseFunc, wake <-chan struct{}, done <-chan struct{}) {
	t := time.NewTicker(w.Cfg.Jobs.Heartbeat)
	defer t.Stop()
	stopWatch := claim.watch(func() { cancel(errNoHeartbeat) })
	defer stopWatch()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		case _, ok := <-wake:
			if !ok { // the subscription ended: rely on the ticker
				wake = nil
				continue
			}
		}
		claim.check()
		start := w.monoNow()
		startWall := w.now().Round(0)
		beatCtx, stopBeat := context.WithTimeout(ctx, w.Cfg.Jobs.Heartbeat)
		stop, err := w.beat(beatCtx, jobID, w.ID)
		stopBeat()
		switch {
		case errors.Is(err, store.ErrLostClaim):
			claim.lose()
			cancel(errLostClaim)
			return
		case err != nil:
			w.Logf("job %d: heartbeat: %v", jobID, err)
		default:
			claim.renew(start, startWall)
			if stop {
				cancel(apply.ErrCancelRequested)
			}
		}
	}
}

// execute replans the job, checks it against the confirmed preview, and runs
// it. bg outlives ctx and records progress; halt closes on shutdown
// (apply.Executor.Halt). finished means no VM was interrupted and no retry
// round was skipped.
func (w *Worker) execute(ctx, bg context.Context, halt <-chan struct{}, job *store.Job, api pods.API, held func() (time.Time, error)) (out store.Outcome, finished bool) {
	var in Inputs
	if err := json.Unmarshal(job.Inputs, &in); err != nil {
		return store.Outcome{Status: store.StatusFailed, Error: "reading job inputs: " + err.Error()}, false
	}
	plan, err := BuildPlan(ctx, pods.NewPlanner(api, w.Cfg), in)
	if err != nil {
		if ctx.Err() != nil {
			// Stopped, not failed: runJob records why.
			return store.Outcome{Status: store.StatusInterrupted, Error: "stopped before anything ran; nothing was changed"}, false
		}
		return store.Outcome{Status: store.StatusFailed, Error: "planning: " + proxmox.Describe(err)}, false
	}
	var then pods.Plan
	_ = json.Unmarshal(job.Plan, &then)
	if fp := Fingerprint(plan); fp != job.Fingerprint {
		return store.Outcome{Status: store.StatusStale, Error: staleMessage(&then, plan)}, false
	}
	exec := &apply.Executor{
		API:    api,
		Cfg:    w.Cfg,
		Limits: w.Limits,
		Halt:   halt,
		Held:   held,
		OnEvent: func(ev apply.Event) {
			evCtx, stopEv := context.WithTimeout(bg, max(w.Cfg.Jobs.Heartbeat, eventTimeoutFloor))
			defer stopEv()
			err := w.Store.AddEvent(evCtx, job.ID, store.Event{
				At: ev.Time, Item: ev.Item, Step: string(ev.Step), Status: string(ev.Status), Message: ev.Message,
			})
			switch {
			case errors.Is(err, store.ErrNotActive):
				w.Logf("job %d: not recorded, the job already finished: item=%q step=%q status=%s: %s",
					job.ID, ev.Item, ev.Step, ev.Status, ev.Message)
			case err != nil:
				w.Logf("job %d: recording event: %v", job.ID, err)
			}
			if w.onEvent != nil {
				w.onEvent(ev)
			}
		},
	}
	res := exec.Run(ctx, plan)
	// Skipped retry rounds mean the job didn't finish.
	return outcomeOf(res), len(res.Interrupted) == 0 && !res.RoundsSkipped
}

// jobCredential opens the job's credential. errAuthLapsed means it is gone
// or lapsed; any other error is the database's or the seal key's.
func (w *Worker) jobCredential(ctx context.Context, jobID int64) (proxmox.Credential, error) {
	callCtx, stop := context.WithTimeout(ctx, DBCallTimeout)
	defer stop()
	jc, err := w.Store.JobCredential(callCtx, jobID)
	if errors.Is(err, store.ErrNoCredential) {
		return proxmox.Credential{}, fmt.Errorf("%w: its Proxmox credential is gone", errAuthLapsed)
	}
	if err != nil {
		return proxmox.Credential{}, fmt.Errorf("reading its Proxmox credential: %w", err)
	}
	if w.Credentials.RowLapsed(jc, w.now()) {
		return proxmox.Credential{}, fmt.Errorf("%w: its Proxmox ticket of %s expired or is past proxmox.ticket_max_age", errAuthLapsed, jc.User)
	}
	cred, err := w.Credentials.Open(jc)
	if err != nil {
		return proxmox.Credential{}, err // the seal key, not the credential: not a lapse
	}
	return cred, nil
}

var errClaimUnsure = errors.New("not sent to Proxmox: battleship lost contact with its database, so the job may have been reaped and another job may be changing these VMs")

// lease is whether this worker is sure it still holds a job's claim, and
// how long it waits for a change's answer.
//
// A job is reaped once its heartbeat stamp is StaleAfter old (the
// claimer's StaleAfter, see store.Claim; a reaper older than migration 014
// uses its own), and a stamp is never earlier than the start of the beat
// (or claim) that wrote it. The claim is sure while that start is under
// StaleAfter/2 ago; no change is sent once it isn't. The wait for a
// change's answer ends StaleAfter - margin after that start.
//
// Elapsed time is the larger of the monotonic clock's (which stops while
// the machine sleeps) and the wall clock's, assuming NTP never steps the
// wall clock back by more than the margin while the machine sleeps. The
// wait is a context deadline, so a sleep delays it too; the next change is
// checked against both clocks. Giving up waiting doesn't undo a request
// Proxmox already received, and tasks a change started run on, under
// Proxmox's own VM locks.
type lease struct {
	mu      sync.Mutex
	mono    time.Time // start of the last successful beat, with a monotonic reading
	wall    time.Time // the same, by the wall clock
	lost    bool
	stale   time.Duration
	wallNow func() time.Time
	monoNow func() time.Time
	// onUnsure, set by watch, runs when the claim is unsure (perhaps more
	// than once); timer fires it when that is due by the monotonic clock,
	// check by the wall clock.
	onUnsure func()
	timer    *time.Timer
}

// newLease starts a lease now with the worker's clocks: call it before the
// claim it is for.
func (w *Worker) newLease() *lease {
	w.init()
	l := &lease{stale: w.Cfg.Jobs.StaleAfter, wallNow: w.now, monoNow: w.monoNow}
	l.renew(l.monoNow(), l.wallNow().Round(0))
	return l
}

// watch runs onUnsure when the claim is unsure without a renewal (a timer,
// so a beat hung on a dead connection can't delay it; see heartbeat), until
// the returned stop is called. onUnsure must be safe to run more than once.
func (l *lease) watch(onUnsure func()) (stop func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onUnsure = onUnsure
	elapsed, _ := l.elapsed()
	l.timer = time.AfterFunc(l.stale/2-elapsed, onUnsure)
	return func() { l.timer.Stop() }
}

// check runs onUnsure if the claim is unsure by the wall clock, which the
// timer misses after the machine sleeps.
func (l *lease) check() {
	if _, err := l.held(); errors.Is(err, errClaimUnsure) && l.onUnsure != nil {
		l.onUnsure()
	}
}

// margin is how long before the earliest reap the wait for an answer ends.
func (l *lease) margin() time.Duration { return min(5*time.Second, l.stale/4) }

func (l *lease) renew(mono, wall time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mono, l.wall = mono, wall
	if l.timer != nil {
		elapsed, _ := l.elapsed()
		l.timer.Reset(l.stale/2 - elapsed)
	}
}

func (l *lease) lose() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lost = true
}

// elapsed is how long ago the last successful beat began, and the
// monotonic now. The caller holds l.mu.
func (l *lease) elapsed() (time.Duration, time.Time) {
	monoNow := l.monoNow()
	return max(monoNow.Sub(l.mono), l.wallNow().Round(0).Sub(l.wall)), monoNow
}

// held is when to stop waiting for a change sent now, or why no change may
// be sent: it is apply.Executor.Held.
func (l *lease) held() (time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	elapsed, monoNow := l.elapsed()
	switch {
	case l.lost:
		return time.Time{}, fmt.Errorf("not sent to Proxmox: the %w", errLostClaim)
	case elapsed >= l.stale/2:
		return time.Time{}, errClaimUnsure
	}
	return monoNow.Add(l.stale - l.margin() - elapsed), nil
}

// stillOurs reports whether this worker still runs job jobID, as far as
// the database says; a read error counts as yes.
func (w *Worker) stillOurs(ctx context.Context, jobID int64) bool {
	callCtx, stop := context.WithTimeout(ctx, DBCallTimeout)
	defer stop()
	j, err := w.Store.Job(callCtx, jobID)
	return err != nil || j.Status == store.StatusRunning && j.ClaimedBy == w.ID
}

// heldCredential is the credential a running job's calls carry; the
// watcher replaces it when the renewer renews it.
type heldCredential struct {
	mu   sync.Mutex
	cred proxmox.Credential
}

func (h *heldCredential) get() proxmox.Credential {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cred
}

func (h *heldCredential) set(c proxmox.Credential) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cred = c
}

// watchCredential reloads the job's credential every heartbeat so calls
// pick up renewed tickets, and stops the run if it lapsed. A database error
// keeps the current one; the heartbeat handles lost contact.
func (w *Worker) watchCredential(ctx context.Context, jobID int64, held *heldCredential, cancel context.CancelCauseFunc, done <-chan struct{}) {
	t := time.NewTicker(w.Cfg.Jobs.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		cred, err := w.jobCredential(ctx, jobID)
		if errors.Is(err, errAuthLapsed) && !w.stillOurs(ctx, jobID) {
			// Reaped elsewhere (which deletes the credential); the
			// heartbeat will find the claim lost.
			return
		}
		switch {
		case errors.Is(err, errAuthLapsed):
			w.Logf("job %d: %v; stopping it", jobID, err)
			cancel(errAuthLapsed)
			return
		case err != nil:
			w.Logf("job %d: reloading its credential: %v", jobID, err)
		default:
			held.set(cred)
		}
	}
}

// staleMessage explains why the replan no longer matches the confirmed
// preview.
func staleMessage(then, now *pods.Plan) string {
	if then.Config != now.Config {
		return "battleship's config for what this job does to VMs changed since the preview (a config change was rolled out, or a CLI run used a battleship.toml unlike serve's); nothing was changed. Preview it again."
	}
	if privilegesChanged(then, now) {
		return "your Proxmox privileges changed since the preview, so it would now do something else; nothing was changed. Preview it again."
	}
	if len(then.Items) == len(now.Items) && len(then.Runnable()) == len(now.Runnable()) {
		// Something the counts don't show: a VM, step or setting, or a
		// battleship upgrade that plans the same job differently.
		return "the plan no longer matches the preview (the cluster changed, or battleship was upgraded, since); nothing was changed. Preview it again."
	}
	return fmt.Sprintf("the cluster changed since the preview (it had %d VMs, %d runnable; now %d, %d runnable); nothing was changed. Preview it again.",
		len(then.Items), len(then.Runnable()), len(now.Items), len(now.Runnable()))
}

// privilegesChanged reports whether then and now differ only in which
// items the user's privileges block.
func privilegesChanged(then, now *pods.Plan) bool {
	if len(then.Items) != len(now.Items) {
		return false
	}
	differs := false
	for i := range now.Items {
		a, b := then.Items[i], now.Items[i]
		if a.Name != b.Name || a.VMID != b.VMID {
			return false
		}
		if (a.Blocked == "") != (b.Blocked == "") {
			if !a.Unpermitted && !b.Unpermitted {
				return false
			}
			differs = true
		}
	}
	return differs
}

// Summary is stored with a finished job.
type Summary struct {
	Succeeded []string          `json:"succeeded"`
	Failed    map[string]string `json:"failed"`
	// Interrupted lists VMs a stop cut off; they are not in Failed,
	// matching their items' interrupted status.
	Interrupted   []string          `json:"interrupted"`
	Blocked       []string          `json:"blocked"`
	Completed     []string          `json:"completed"`
	Removed       []string          `json:"removed"`
	AlreadyGone   []string          `json:"already_gone"`
	CleanupFailed map[string]string `json:"cleanup_failed"`
}

// Summarize is how a run ended, for people. Cleanup failures carry their
// advice (apply.CleanupAdvice).
func Summarize(res apply.Result) Summary {
	sum := Summary{
		Succeeded: res.Succeeded, Blocked: res.Blocked, Completed: res.Completed,
		Removed: res.Removed, AlreadyGone: res.AlreadyGone,
		Failed: map[string]string{}, Interrupted: []string{}, CleanupFailed: map[string]string{},
	}
	for name, err := range res.Failed {
		sum.Failed[name] = proxmox.Describe(err)
	}
	for name := range res.Interrupted {
		sum.Interrupted = append(sum.Interrupted, name)
	}
	for name, err := range res.CleanupFailed {
		sum.CleanupFailed[name] = apply.CleanupAdvice(name, err)
	}
	for _, list := range [][]string{sum.Succeeded, sum.Interrupted, sum.Blocked, sum.Completed, sum.Removed, sum.AlreadyGone} {
		sort.Strings(list)
	}
	return sum
}

// leftConfig is how the store records the executor's verdict on how an
// interrupted item left its VM's config.
func leftConfig(l apply.Left) string {
	switch l {
	case apply.LeftUntouched:
		return store.LeftUntouched
	case apply.LeftConverged:
		return store.LeftConverged
	}
	return ""
}

func outcomeOf(res apply.Result) store.Outcome {
	sum := Summarize(res)
	items := map[string]store.ItemOutcome{}
	for _, name := range res.Succeeded {
		items[name] = store.ItemOutcome{Status: store.ItemDone}
	}
	for name, err := range res.Failed {
		items[name] = store.ItemOutcome{Status: store.ItemFailed, Error: proxmox.Describe(err)}
	}
	for name, err := range res.Interrupted {
		items[name] = store.ItemOutcome{Status: store.ItemInterrupted, Error: proxmox.Describe(err), LeftConfig: leftConfig(apply.LeftBy(err))}
	}
	for _, name := range res.Removed {
		if it, ok := items[name]; ok {
			it.Status = store.ItemRemoved
			items[name] = it
		}
	}

	status := store.StatusSucceeded
	if len(res.Failed) > 0 || len(res.Interrupted) > 0 || len(res.Blocked) > 0 || len(res.CleanupFailed) > 0 {
		status = store.StatusCompletedWithFailures
	}
	b, _ := json.Marshal(sum) // strings and maps of strings can't fail
	return store.Outcome{Status: status, Summary: b, Items: items}
}
