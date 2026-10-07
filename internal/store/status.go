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
// first event makes it running. When its job ends, Finish sets it from the
// worker's outcome (done, failed, interrupted, removed), then endItems marks
// an item that never reached a step not run and, for a stopped job, any
// other unfinished one interrupted. Events and outcomes leave blocked and
// interrupted items be; nothing changes a not run one.
const (
	ItemPending     = "pending"
	ItemRunning     = "running"
	ItemDone        = "done"
	ItemFailed      = "failed"
	ItemBlocked     = "blocked"
	ItemRemoved     = "removed"     // created, didn't finish, and cleaned up
	ItemInterrupted = "interrupted" // its job was interrupted or cancelled after it reached a step
	// ItemNotRun: its job ended (or was cancelled) before it reached a
	// step, so it never touched its VM.
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
	StatusInterrupted:           {stopped: true, retry: true},
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

// StartAgain reports whether the job's form is offered again: nothing ran,
// or it was cancelled.
func (s JobStatus) StartAgain() bool { r, _ := s.rule(); return r.startAgain }

// CancelTooLate reports whether a cancel requested on a job that ended so
// stopped nothing: the job ended as its work did.
func (s JobStatus) CancelTooLate() bool { r, _ := s.rule(); return r.cancelTooLate }

// ItemStatus is a job item's status, with the rules that hang on it.
type ItemStatus string

type itemRule struct {
	unfinished    bool // pending or running: its job still has to work on it
	notRunNoStep  bool // becomes not run if its job ends before it reaches a step
	retry         bool // a retry of its finished job runs it again
	touched       bool // its job did something to its VM, so it is the VM's last result
	keptOnEvent   bool // an event doesn't make it running again
	keptOnOutcome bool // the worker's outcome doesn't overwrite it
}

// itemRules is every item status's rules; an unknown status has none.
var itemRules = map[ItemStatus]itemRule{
	ItemPending:     {unfinished: true, notRunNoStep: true},
	ItemRunning:     {unfinished: true, notRunNoStep: true},
	ItemDone:        {touched: true},
	ItemFailed:      {retry: true, touched: true},
	ItemBlocked:     {retry: true, keptOnEvent: true, keptOnOutcome: true},
	ItemInterrupted: {notRunNoStep: true, retry: true, touched: true, keptOnEvent: true, keptOnOutcome: true},
	ItemRemoved:     {retry: true, touched: true},
	ItemNotRun:      {retry: true, keptOnEvent: true},
}

func (s ItemStatus) rule() itemRule { return itemRules[s] }

// Unfinished reports whether the item is pending or running.
func (s ItemStatus) Unfinished() bool { return s.rule().unfinished }

// Retryable reports whether an item of a finished job didn't finish, so a
// retry should run it again: it failed, was blocked, was interrupted, was
// removed after failing half-built, or never ran.
func (s ItemStatus) Retryable() bool { return s.rule().retry }

// TouchedVM reports whether the item's job reached an outcome on its VM.
func (s ItemStatus) TouchedVM() bool { return s.rule().touched }

// SQL lists of the sets above, for queries; TestStatusSQLLists keeps them
// in step with the tables.
const (
	activeJobsSQL      = `('pending', 'running')`
	unfinishedItemsSQL = `('pending', 'running')`
	notRunNoStepSQL    = `('pending', 'running', 'interrupted')`
	touchedItemsSQL    = `('done', 'failed', 'interrupted', 'removed')`
	keptOnEventSQL     = `('blocked', 'interrupted', 'not run')`
	keptOnOutcomeSQL   = `('blocked', 'interrupted')`
)
