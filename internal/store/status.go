package store

// Job statuses. A job is created pending; Claim makes it running, or
// RequestCancel ends it cancelled before it starts. A running job ends in one
// final status, through Finish (its worker's outcome) or ReapStale
// (interrupted, when its heartbeats stop). Final statuses never change.
const (
	StatusPending               = "pending"
	StatusRunning               = "running"
	StatusSucceeded             = "succeeded"
	StatusCompletedWithFailures = "completed_with_failures"
	StatusFailed                = "failed"      // could not run at all, e.g. planning failed
	StatusCancelled             = "cancelled"   // someone cancelled it
	StatusInterrupted           = "interrupted" // its worker stopped sending heartbeats
	StatusStale                 = "stale"       // the cluster changed since the preview
)

// Item statuses. An item is created pending, or blocked (never run); its
// first event makes it running. Its succeeded event makes it done and its
// failed event failed, until a retry round's started event makes it running
// again. When its job ends, Finish applies the worker's outcome, then
// endItems marks items that never reached a step not run and, for a stopped
// job, other unfinished ones interrupted. Events and outcomes leave blocked
// and interrupted items be; not run is final.
const (
	ItemPending     = "pending"
	ItemRunning     = "running"
	ItemDone        = "done"
	ItemFailed      = "failed"
	ItemBlocked     = "blocked"
	ItemRemoved     = "removed"     // created, didn't finish, and cleaned up
	ItemInterrupted = "interrupted" // its job was interrupted or cancelled after it reached a step
	// ItemNotRun: its job ended before it reached a step.
	ItemNotRun = "not run"
)

// JobStatus is a job's status, with the rules that hang on it.
type JobStatus string

type jobRule struct {
	active        bool // pending or running: may still change, may be cancelled
	stopped       bool // ended by a cancel or a lost worker: its unfinished items are interrupted
	ranNothing    bool // ended before any VM was touched; a lost worker doesn't change it
	retry         bool // its unfinished items may be retried (jobs.RetryInputs)
	startAgain    bool // its form is offered again, filled in, when retry isn't
	cancelTooLate bool // a cancel requested on a job ending this way stopped nothing
}

// jobRules is every job status's rules; an unknown status has none.
var jobRules = map[JobStatus]jobRule{
	StatusPending:               {active: true},
	StatusRunning:               {active: true},
	StatusSucceeded:             {cancelTooLate: true},
	StatusCompletedWithFailures: {retry: true, cancelTooLate: true},
	StatusFailed:                {ranNothing: true, startAgain: true, cancelTooLate: true},
	StatusCancelled:             {stopped: true, retry: true, startAgain: true},
	StatusInterrupted:           {stopped: true, retry: true, startAgain: true},
	StatusStale:                 {ranNothing: true, startAgain: true},
}

func (s JobStatus) rule() (jobRule, bool) { r, ok := jobRules[s]; return r, ok }

// Active reports whether the job is pending or running.
func (s JobStatus) Active() bool { r, _ := s.rule(); return r.active }

// Final reports whether s is a known status a job ends in.
func (s JobStatus) Final() bool { r, ok := s.rule(); return ok && !r.active }

// Stopped reports whether the job was cut off: cancelled or interrupted.
func (s JobStatus) Stopped() bool { r, _ := s.rule(); return r.stopped }

// RanNothing reports whether the job ended before touching any VM.
func (s JobStatus) RanNothing() bool { r, _ := s.rule(); return r.ranNothing }

// CanRetry reports whether a job with this status may be retried.
func (s JobStatus) CanRetry() bool { r, _ := s.rule(); return r.retry }

// StartAgain reports whether the job's form is offered again (when retry isn't):
// nothing ran, or it failed, was interrupted, or was cancelled.
func (s JobStatus) StartAgain() bool { r, _ := s.rule(); return r.startAgain }

// CancelTooLate reports whether a cancel on a job ending this way stopped
// nothing.
func (s JobStatus) CancelTooLate() bool { r, _ := s.rule(); return r.cancelTooLate }

type ItemStatus string

// Retryable reports whether a retry should run the item again.
func (s ItemStatus) Retryable() bool {
	switch s {
	case ItemFailed, ItemBlocked, ItemInterrupted, ItemRemoved, ItemNotRun:
		return true
	default:
		return false
	}
}

// SQL status lists for queries.
const (
	activeJobsSQL      = `('pending', 'running')`
	unfinishedItemsSQL = `('pending', 'running')`
	notRunNoStepSQL    = `('pending', 'running', 'interrupted')`
	touchedItemsSQL    = `('done', 'failed', 'interrupted', 'removed')`
	keptOnEventSQL     = `('blocked', 'interrupted', 'not run')`
	keptOnOutcomeSQL   = `('blocked', 'interrupted')`
	endedItemsSQL      = `('done', 'failed')`
	busyItemsSQL       = `('pending', 'running', 'failed')`
)
