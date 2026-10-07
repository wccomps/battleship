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
	// OnEvent, if set, also receives each progress event as it is recorded,
	// e.g. to print it. It is called from many goroutines.
	OnEvent func(apply.Event)
	// OnStart, if set, is called when a claimed job starts; OnFinish after
	// its outcome is recorded (not for a job whose claim was reaped).
	OnStart  func(job *store.Job)
	OnFinish func(job *store.Job, out store.Outcome)
	// Cancels, if set, subscribes to cancel requests: wake fires when one may
	// have come. Nil: cancels wait for the next heartbeat.
	Cancels func(jobID int64) (wake <-chan struct{}, stop func())

	// beat sends a heartbeat; tests replace it. Nil means Store.Heartbeat.
	beat func(ctx context.Context, jobID int64, worker string) (bool, error)
	// now reads the wall clock for the heartbeat deadline; tests replace it.
	now func() time.Time

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
	})
}

// Run claims and runs jobs until ctx is done, first reaping jobs whose
// workers went silent each pass.
func (w *Worker) Run(ctx context.Context) error {
	w.init()
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
		claimCtx, stopClaim := context.WithTimeout(ctx, DBCallTimeout)
		job, err := w.Store.Claim(claimCtx, w.ID)
		stopClaim()
		if err != nil {
			if ctx.Err() == nil {
				w.Logf("claiming a job: %v", err)
			}
		}
		if job != nil {
			w.RunJob(ctx, job)
			continue
		}
		if err := proxmox.SleepContext(ctx, w.Cfg.Jobs.Poll); err != nil {
			break
		}
	}
	return nil
}

// RunJob runs a job this worker has claimed and records how it ended. A
// shutdown or cancel stops it gracefully (cleanup included) and records it
// interrupted or cancelled; a stop after every runnable VM finished changes
// nothing.
func (w *Worker) RunJob(ctx context.Context, job *store.Job) {
	w.init()
	if w.OnStart != nil {
		w.OnStart(job)
	}
	// Record the outcome even while shutting down.
	bg := context.WithoutCancel(ctx)
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	hbDone := make(chan struct{})
	var wake <-chan struct{}
	if w.Cancels != nil {
		var unsubscribe func()
		wake, unsubscribe = w.Cancels(job.ID)
		defer unsubscribe()
	}
	go w.heartbeat(bg, job.ID, cancel, wake, hbDone)
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
		outcome, finished = w.execute(runCtx, bg, ctx.Done(), job, api)
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
		// Stopped before another worker may reap it. Cleanup still runs, so
		// a partition outlasting StaleAfter can overlap a new job on the
		// team (accepted risk).
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
	switch err := w.Store.Finish(finCtx, job.ID, w.ID, outcome); {
	case errors.Is(err, store.ErrLostClaim):
		w.Logf("job %d: %v; not recording its outcome", job.ID, errLostClaim)
		return
	case err != nil:
		w.Logf("job %d: recording outcome: %v", job.ID, err)
		return
	}
	if w.OnFinish != nil {
		w.OnFinish(job, outcome)
	}
}

// heartbeat beats every Jobs.Heartbeat (and at once on wake) until done is
// closed, cancelling the run on a cancel request or a lost claim.
//
// No successful beat for StaleAfter/2 cancels with errNoHeartbeat; a timer
// enforces this so a beat hung on a dead connection can't defer it. Beating
// continues afterwards to keep the job claimed during cleanup.
func (w *Worker) heartbeat(ctx context.Context, jobID int64, cancel context.CancelCauseFunc, wake <-chan struct{}, done <-chan struct{}) {
	t := time.NewTicker(w.Cfg.Jobs.Heartbeat)
	defer t.Stop()
	threshold := w.Cfg.Jobs.StaleAfter / 2
	// The claim itself counts as a heartbeat.
	deadline := time.AfterFunc(threshold, func() { cancel(errNoHeartbeat) })
	defer deadline.Stop()
	// The timer's monotonic clock stops while the machine sleeps, so also
	// check the wall clock. Round(0) strips the monotonic reading.
	lastOKWall := w.now().Round(0)
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
		if w.now().Round(0).Sub(lastOKWall) >= threshold {
			cancel(errNoHeartbeat)
		}
		start := time.Now()
		startWall := w.now().Round(0)
		beatCtx, stopBeat := context.WithTimeout(ctx, w.Cfg.Jobs.Heartbeat)
		stop, err := w.beat(beatCtx, jobID, w.ID)
		stopBeat()
		switch {
		case errors.Is(err, store.ErrLostClaim):
			cancel(errLostClaim)
			return
		case err != nil:
			w.Logf("job %d: heartbeat: %v", jobID, err)
		default:
			lastOKWall = startWall
			deadline.Reset(threshold - time.Since(start))
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
func (w *Worker) execute(ctx, bg context.Context, halt <-chan struct{}, job *store.Job, api pods.API) (out store.Outcome, finished bool) {
	var in Inputs
	if err := json.Unmarshal(job.Inputs, &in); err != nil {
		return store.Outcome{Status: store.StatusFailed, Error: "reading job inputs: " + err.Error()}, false
	}
	plan, err := BuildPlan(ctx, pods.NewPlanner(api, w.Cfg), in)
	if err != nil {
		if ctx.Err() != nil {
			// Stopped, not failed: RunJob records why.
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
			if w.OnEvent != nil {
				w.OnEvent(ev)
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
		return "battleship's config for what this job does to VMs changed since the preview (a config change was rolled out); nothing was changed. Preview it again."
	}
	if privilegesChanged(then, now) {
		return "your Proxmox privileges changed since the preview, so it would now do something else; nothing was changed. Preview it again."
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
