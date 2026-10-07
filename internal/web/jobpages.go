package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

// jobsPerPage is how many jobs a page of the job list shows.
const jobsPerPage = 50

// statusLook is how a job, item or log status is shown: a glyph (icon name,
// or "spin"), a word, a colour class (s-ok …) and, for items, a pip.
type statusLook struct {
	Glyph, Label, Class, Pip string
}

var jobLooks = map[string]statusLook{
	store.StatusPending:               {"wait", "pending", "s-wait", ""},
	store.StatusRunning:               {"spin", "running", "", ""},
	store.StatusSucceeded:             {"done", "succeeded", "s-ok", ""},
	store.StatusCompletedWithFailures: {"warn", "completed with failures", "s-part", ""},
	store.StatusFailed:                {"failed", "failed", "s-fail", ""},
	store.StatusCancelled:             {"blocked", "cancelled", "s-cancel", ""},
	store.StatusInterrupted:           {"paused", "interrupted", "s-int", ""},
	store.StatusStale:                 {"stale", "stale", "s-stale", ""},
}

var itemLooks = map[string]statusLook{
	store.ItemPending:     {"wait", "pending", "s-wait", "wait"},
	store.ItemRunning:     {"spin", "running", "", "run"},
	store.ItemDone:        {"done", "done", "s-ok", "done"},
	store.ItemFailed:      {"close", "failed", "s-fail", "fail"},
	store.ItemBlocked:     {"blocked", "blocked", "s-cancel", "skip"},
	store.ItemInterrupted: {"paused", "interrupted", "s-int", "skip"},
	store.ItemRemoved:     {"teardown", "removed", "s-part", "fail"},
	store.ItemNotRun:      {"dash", "not run", "s-skip", "skip"},
}

// opLabel is how a job's operation is shown: icon, words and a mono detail,
// e.g. Deploy kilo.alpha.
type opLabel struct {
	Icon   string // an operation kind or power action, for opIcon
	Label  string
	Detail string
}

// opLabelOf names the operation of a job of kind with inputs in.
func opLabelOf(kind string, in jobs.Inputs) opLabel {
	switch pods.Kind(kind) {
	case pods.KindPower:
		return opLabel{Icon: in.Action, Label: powerLabel(in.Action)}
	case pods.KindReset:
		return opLabel{Icon: "reset", Label: "Reset to snapshot", Detail: in.Snapshot}
	case pods.KindSnapshot:
		return opLabel{Icon: "snapshot", Label: "Take snapshot", Detail: in.Snapshot}
	case pods.KindDeploy:
		return opLabel{Icon: "deploy", Label: "Deploy", Detail: strings.TrimPrefix(in.Pattern, "*.")}
	case pods.KindTeardown:
		return opLabel{Icon: "teardown", Label: "Teardown"}
	}
	return opLabel{Icon: kind, Label: kind}
}

func lookOf(looks map[string]statusLook, status string) statusLook {
	if l, ok := looks[status]; ok {
		return l
	}
	return statusLook{Glyph: "dash", Label: status, Class: "s-skip", Pip: "skip"}
}

// jobRow is one job in the list.
type jobRow struct {
	ID    int64
	Look  statusLook
	Op    opLabel
	Teams string
	Hosts string // "" for all hosts
	Who   string // the name part of who started it, e.g. "sean"
	By    string // who started it in full, for a title
	Role  string
	When  string // the time of day it was created; no "ago", which a live table can't keep current
	At    string // When in full, for a title
	Took  string // how long a finished job ran, e.g. "22 min"; "" otherwise
	// Note is the line under the status, e.g. "after #423" or "cancel requested".
	Note string
}

// whoShort is the local part of an actor's email: "sean" of "sean@example.org".
func whoShort(actor string) string {
	if i := strings.IndexByte(actor, '@'); i > 0 {
		return actor[:i]
	}
	return actor
}

// took is how long a job ran ("14 s", "1 h 5 min"); "" if it hasn't finished
// or never started.
func took(j store.Job) string {
	if j.FinishedAt == nil || j.StartedAt == nil {
		return ""
	}
	d := j.FinishedAt.Sub(*j.StartedAt).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d%time.Hour < time.Minute:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	}
	return fmt.Sprintf("%d h %d min", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// jobsList is the data of the job list.
type jobsList struct {
	Rows  []jobRow
	Older string // the next page, if any
	Newer bool   // this isn't the first page
	Live  bool   // the first page updates by itself
}

// jobsPage lists jobs newest first, a page at a time; ?before=<id> continues
// after that job.
func (s *Server) jobsPage(w http.ResponseWriter, r *http.Request) {
	var before int64
	if b := r.URL.Query().Get("before"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			s.notFound(w, r)
			return
		}
		before = n
	}
	list, err := s.jobsList(r.Context(), before)
	if err != nil {
		s.serverError(w, r, "listing jobs", err)
		return
	}
	view := s.newView(w, r, "Logs", "logs", list)
	if list.Live {
		view.Live = "live"
	}
	s.render(w, r, http.StatusOK, "jobs", view)
}

func (s *Server) jobsList(ctx context.Context, before int64) (jobsList, error) {
	js, err := s.st.JobsBefore(ctx, before, jobsPerPage+1)
	if err != nil {
		return jobsList{}, err
	}
	list := jobsList{Newer: before > 0, Live: before == 0}
	if len(js) > jobsPerPage {
		js = js[:jobsPerPage]
		list.Older = fmt.Sprintf(logsPath+"?before=%d", js[len(js)-1].ID)
	}
	var pending []int64
	for _, j := range js {
		if j.Status == store.StatusPending {
			pending = append(pending, j.ID)
		}
	}
	waiting, err := s.st.BlockersOf(ctx, pending)
	if err != nil {
		return jobsList{}, err
	}
	now := s.now()
	for _, j := range js {
		in := readInputs(j)
		row := jobRow{
			ID:    j.ID,
			Look:  lookOf(jobLooks, j.Status),
			Op:    opLabelOf(j.Kind, in),
			Teams: teamsText(in.Teams),
			Who:   whoShort(j.CreatedBy),
			By:    j.CreatedBy,
			Role:  actsAs(j),
			When:  minuteTime(now, j.CreatedAt),
			At:    clockTime(now, j.CreatedAt),
			Took:  took(j),
		}
		if len(in.Hosts) > 0 {
			row.Hosts = strings.Join(in.Hosts, ", ")
		}
		switch {
		case j.Active() && j.CancelRequested:
			row.Note = "cancel requested"
		case j.Status == store.StatusPending && len(waiting[j.ID]) > 0:
			row.Note = "after #" + strconv.FormatInt(waiting[j.ID][0], 10)
		case j.Status == store.StatusStale:
			row.Note = "nothing ran"
		}
		list.Rows = append(list.Rows, row)
	}
	return list, nil
}

// readInputs reads a job's stored inputs; unreadable ones read as empty.
func readInputs(j store.Job) jobs.Inputs {
	var in jobs.Inputs
	_ = json.Unmarshal(j.Inputs, &in)
	return in
}

func hostsText(hosts []string) string {
	if len(hosts) == 0 {
		return "all"
	}
	return strings.Join(hosts, ", ")
}

// jobsEvents streams the job list's first page as a "patch" event whenever it
// changes.
func (s *Server) jobsEvents(w http.ResponseWriter, r *http.Request) {
	var last string
	s.stream(w, r, []string{status.TopicJobs}, 0, func(sw *sseWriter, resync bool, _ uint64) error {
		list, err := s.jobsList(r.Context(), 0)
		if err != nil {
			s.logf("web: listing jobs for the event stream: %v", err)
			return err
		}
		html, err := fragment("jobs", "jobs-table", list)
		if err != nil {
			s.logf("web: rendering the job list for its event stream: %v", err)
			return err
		}
		if !resync && html == last {
			return nil
		}
		last = html
		return sw.event("patch", "", piece(html))
	})
}

// jobView is a job's page, or the parts of it the event stream sends.
type jobView struct {
	CSRF  string
	ID    int64
	Op    opLabel
	Look  statusLook
	Who   string // the name part of who started it
	By    string // in full
	Role  string
	Teams string // e.g. "01-03"
	Hosts string // e.g. "dc, web", or "all"
	// Ran is when it ran ("13:12–13:34"), or when it was made if not started;
	// RanAt is the full time, for a title.
	Ran, RanAt string
	Asked      []fact // the inputs beyond teams and hosts, e.g. the snapshot
	Error      string
	Headline   string // a succeeded deploy's or teardown's result (fleetHeadline)
	Summary    *summaryView
	CancelNote string  // who asked to cancel it, and whether it took effect
	Waiting    []int64 // pending: the jobs it waits for
	Items      jobItems
	Live       bool // the job is active: the page follows it
	CanCancel  bool
	AskCancel  bool // the job could be cancelled, but not by this viewer
	CanRetry   bool
	RetryN     int    // how many VMs a retry runs again
	RetryTip   string // what to know before retrying
	RetryLock  string // why this viewer can't retry it: they lack its privilege
	RetryFault string // why the job can't be retried at all, when its stored job can't be read
	StartAgain string // a stale, failed or cancelled job: its form, filled in, if the user may use it
	AgainLock  string // why this viewer can't start it again: they lack its privilege
	Events     []eventView
	EventsHref string
}

// summaryView is what a finished job's summary adds to its items.
type summaryView struct {
	Removed       []string // half-built VMs deleted again
	AlreadyGone   []string
	CleanupAdvice []string // cleanup that failed, with what to do
}

// jobItems is a job's VM table, pips and tally; the same for every viewer, so
// its rendering is shared.
type jobItems struct {
	JobID int64
	Rows  []jobItemView // the ones needing attention first
	Pips  []string      // one class per VM, in plan order
	Tally []tallyPart
	// Fold is how many done rows the panel folds into one line; the job page
	// shows them all.
	Fold int
}

// tallyPart is one count of the tally, e.g. 3 failed.
type tallyPart struct {
	N     int
	Label string
	Bad   bool
}

// TallyText is the tally in words, e.g. "381 done, 3 failed".
func (t jobItems) TallyText() string {
	parts := make([]string, len(t.Tally))
	for i, p := range t.Tally {
		parts[i] = fmt.Sprintf("%d %s", p.N, p.Label)
	}
	return strings.Join(parts, ", ")
}

// jobItemView is one VM of a job.
type jobItemView struct {
	Name  string
	VMID  string
	Step  string     // the last step it reported
	Steps []stepView // its planned steps and how far it got; none if the plan can't be read
	Look  statusLook
	Error string
	Fold  bool // a done row the panel folds away

	status string // for ordering
}

// stepView is one of an item's steps, drawn as a pip.
type stepView struct {
	Name  string
	State string // a pip class: done, skip, fail, int, run or wait
}

// Title is the step and its state in words, e.g. "network: done".
func (v stepView) Title() string { return v.Name + ": " + stepWords[v.State] }

var stepWords = map[string]string{"done": "done", "skip": "skipped", "fail": "failed", "int": "interrupted", "run": "running", "wait": "waiting"}

// StepsText is the row's steps in words, for a screen reader.
func (v jobItemView) StepsText() string {
	parts := make([]string, len(v.Steps))
	for i, st := range v.Steps {
		parts[i] = st.Title()
	}
	return strings.Join(parts, ", ")
}

// itemSteps is an item's planned steps: each one's reported outcome, the next
// running, the rest waiting. A done item's unreported steps weren't needed
// (an already-deleted VM's stop), except in old jobs that kept no outcomes.
func itemSteps(planned []pods.Step, it store.Item) []stepView {
	pips := map[string]string{"done": "done", "skipped": "skip", "failed": "fail", "interrupted": "int"}
	out := make([]stepView, 0, len(planned))
	running := it.Status == store.ItemRunning
	for _, st := range planned {
		v := stepView{Name: string(st), State: "wait"}
		got, ok := it.Steps[string(st)]
		switch {
		case ok:
			v.State = pips[got]
		case running:
			v.State, running = "run", false
		case it.Status == store.ItemDone && len(it.Steps) == 0:
			v.State = "done"
		case it.Status == store.ItemDone:
			v.State = "skip"
		}
		out = append(out, v)
	}
	return out
}

// eventView is one line of a job's log.
type eventView struct {
	ID   int64
	At   string // e.g. "14:05:01"
	Full string // At in full, for a title
	Item string // the VM, or "" for the job itself
	Text string
	Err  bool
}

// eventText is a log line beyond its VM: step, outcome and message.
func eventText(ev store.Event) string {
	var head string
	switch apply.EventStatus(ev.Status) {
	case apply.EventDone:
		head = strings.TrimSpace(ev.Step + " done")
	case apply.EventFailed:
		head = strings.TrimSpace(ev.Step + " failed")
	case apply.EventSkipped:
		head = strings.TrimSpace(ev.Step + " skipped")
	case apply.EventBlocked:
		head = "blocked"
	case apply.EventInterrupted:
		head = strings.TrimSpace(ev.Step + " interrupted")
	default:
		head = ev.Step
	}
	switch {
	case head == "":
		return ev.Message
	case ev.Message == "":
		return head
	}
	return head + ": " + ev.Message
}

// jobPage shows one job: request, per-VM progress, and a log that follows the
// job live while active.
func (s *Server) jobPage(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.jobOf(w, r)
	if !ok {
		return
	}
	id := snap.job.ID
	v := s.jobViewOf(r, snap)
	events, last, err := s.jobEventsAfter(r.Context(), id, 0)
	if err != nil {
		s.serverError(w, r, "reading job "+strconv.FormatInt(id, 10)+"'s log", err)
		return
	}
	v.Events = events
	v.EventsHref = fmt.Sprintf("/events/jobs/%d?after=%d", id, last)
	view := s.newView(w, r, fmt.Sprintf("Log #%d", id), "logs", v)
	if v.Live {
		view.Live = "live"
	}
	s.render(w, r, http.StatusOK, "job", view)
}

// jobID reads the job ID from the path, answering 404 itself if invalid.
func (s *Server) jobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.notFound(w, r)
		return 0, false
	}
	return id, true
}

// jobSnap is a job page's data apart from the log: the job, its items and,
// while pending, the jobs it waits for.
type jobSnap struct {
	job     store.Job
	items   []store.Item
	waiting []int64
	// itemsHTML is the rendered job-items fragment, shared by every viewer; ""
	// until an event stream renders it (see sharedJobSnap).
	itemsHTML string
}

func (s *Server) readJobSnap(ctx context.Context, id int64) (jobSnap, error) {
	j, err := s.st.Job(ctx, id)
	if err != nil {
		return jobSnap{}, err
	}
	items, err := s.st.Items(ctx, id)
	if err != nil {
		return jobSnap{}, err
	}
	snap := jobSnap{job: j, items: items}
	if j.Status == store.StatusPending {
		if snap.waiting, err = s.st.Blockers(ctx, id); err != nil {
			return jobSnap{}, err
		}
	}
	return snap, nil
}

// jobOf reads the job the path names, answering 404 or 500 itself on failure.
func (s *Server) jobOf(w http.ResponseWriter, r *http.Request) (jobSnap, bool) {
	id, ok := s.jobID(w, r)
	if !ok {
		return jobSnap{}, false
	}
	snap, err := s.readJobSnap(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound, "No such job", fmt.Sprintf("There is no job %d.", id), logsPath, "Back to the logs")
		return jobSnap{}, false
	case err != nil:
		s.serverError(w, r, "reading job "+strconv.FormatInt(id, 10), err)
		return jobSnap{}, false
	}
	return snap, true
}

// jobViewOf is the view of snap for the request's user.
func (s *Server) jobViewOf(r *http.Request, snap jobSnap) jobView {
	ctx := r.Context()
	j, items := snap.job, snap.items
	now := s.now()
	in := readInputs(j)
	v := jobView{
		CSRF:       auth.CSRFToken(ctx),
		ID:         j.ID,
		Op:         opLabelOf(j.Kind, in),
		Look:       lookOf(jobLooks, j.Status),
		Who:        whoShort(j.CreatedBy),
		By:         j.CreatedBy,
		Role:       actsAs(j),
		Teams:      teamsText(in.Teams),
		Hosts:      hostsText(in.Hosts),
		Error:      j.Error,
		Live:       j.Active(),
		CancelNote: cancelNote(j),
		Waiting:    snap.waiting,
	}
	if len(in.VMs) > 0 {
		v.Hosts = "all" // the table names the VMs
	}
	v.Asked = askedFacts(in)
	v.Ran, v.RanAt = minuteTime(now, j.CreatedAt), "made "+clockTime(now, j.CreatedAt)
	if j.StartedAt != nil {
		v.Ran, v.RanAt = minuteTime(now, *j.StartedAt), "started "+clockTime(now, *j.StartedAt)
		if j.FinishedAt != nil {
			v.Ran += "–" + minuteTime(now, *j.FinishedAt)
			v.RanAt += ", finished " + clockTime(now, *j.FinishedAt)
		}
	}
	v.Summary = readSummary(j.Summary)
	v.Items = jobItemsOf(j, items)
	v.Headline = fleetHeadline(j.Kind, j.Status, items, v.Summary)

	u, _ := auth.UserFrom(ctx)
	mayOp := s.mayAnywhere(ctx, pods.OfferPrivileges(pods.Kind(j.Kind))...)
	v.CanCancel = j.Active() && !j.CancelRequested && s.mayCancel(ctx, j, items, u)
	v.AskCancel = j.Active() && !j.CancelRequested && !v.CanCancel
	// A stale, failed or cancelled job is restarted from a new preview when
	// retry isn't offered.
	if store.JobStatus(j.Status).StartAgain() {
		if op, ok := operationFor(pods.Kind(j.Kind)); ok {
			if mayOp {
				v.StartAgain = formQuery(op, in)
			} else {
				v.AgainLock = notPermitted(j.Kind)
			}
		}
	}
	if store.JobStatus(j.Status).CanRetry() {
		retry, err := jobs.RetryInputs(j, items)
		switch {
		case err == nil && !mayOp:
			v.RetryLock = notPermitted(j.Kind)
		case errors.Is(err, jobs.ErrNotRetryable), errors.Is(err, jobs.ErrNothingToRetry):
		case err != nil:
			v.RetryFault = sentence(err.Error())
		default:
			v.CanRetry = true
			v.RetryN = len(retry.VMs)
			v.RetryTip = fmt.Sprintf("Runs the job again for %s, after a preview.", countVMs(len(retry.VMs)))
			if n := s.cfg.Retry.Rounds; n > 0 && j.Status == store.StatusCompletedWithFailures {
				rounds := "1 automatic round"
				if n != 1 {
					rounds = fmt.Sprintf("%d automatic rounds", n)
				}
				v.RetryTip = "Battleship already tried them again by itself (" + rounds + "), so fix the cause first. " + v.RetryTip
			}
		}
	}
	if v.CanRetry {
		v.StartAgain = ""
	}
	return v
}

// foldAbove is the VM count above which the panel folds done rows into one line.
const foldAbove = 12

// jobItemsOf is job j's VM table: rows (needing attention first), pips and
// the tally by status. It is the same for every viewer.
func jobItemsOf(j store.Job, items []store.Item) jobItems {
	t := jobItems{JobID: j.ID}
	var plan pods.Plan
	_ = json.Unmarshal(j.Plan, &plan) // unreadable: the rows show no steps
	planned := map[string][]pods.Step{}
	for _, it := range plan.Items {
		planned[it.Name] = it.Steps
	}
	n := map[string]int{}
	var order []string
	for _, it := range items {
		st := it.Status
		if n[st] == 0 {
			order = append(order, st)
		}
		n[st]++
		look := lookOf(itemLooks, st)
		t.Pips = append(t.Pips, look.Pip)
		t.Rows = append(t.Rows, jobItemView{
			Name: it.Name, VMID: orDash(it.VMID), Step: it.Step, Steps: itemSteps(planned[it.Name], it), Look: look, Error: it.Error, status: st,
		})
	}
	sort.SliceStable(t.Rows, func(a, b int) bool { return itemRank(t.Rows[a].status) < itemRank(t.Rows[b].status) })
	sort.SliceStable(order, func(a, b int) bool { return itemRank(order[a]) < itemRank(order[b]) })
	for _, st := range order {
		t.Tally = append(t.Tally, tallyPart{N: n[st], Label: lookOf(itemLooks, st).Label,
			Bad: st == store.ItemFailed || st == store.ItemRemoved})
	}
	if done := n[store.ItemDone]; len(items) > foldAbove && done > 0 {
		t.Fold = done
		for i := range t.Rows {
			t.Rows[i].Fold = t.Rows[i].status == store.ItemDone
		}
	}
	return t
}

// itemRank orders item counts for display, needing attention first.
func itemRank(status string) int {
	for i, s := range []string{store.ItemFailed, store.ItemInterrupted, store.ItemRemoved, store.ItemBlocked, store.ItemNotRun,
		store.ItemRunning, store.ItemPending, store.ItemDone} {
		if s == status {
			return i
		}
	}
	return 99
}

// cancelNote says who asked to cancel, and whether it stopped the job or came
// too late, once known.
func cancelNote(j store.Job) string {
	if !j.CancelRequested {
		return ""
	}
	by := j.CancelledBy
	if by == "" {
		by = "someone"
	}
	switch {
	case j.Status == store.StatusCancelled:
		return "cancelled by " + by
	case j.CancelCameTooLate():
		return "cancel requested by " + by + " came too late to stop anything"
	}
	return "cancel requested by " + by
}

// fleetHeadline is a succeeded deploy's or teardown's one-line result: the VMs
// it made or deleted, not those already gone. Other outcomes keep their status.
func fleetHeadline(kind, status string, items []store.Item, sum *summaryView) string {
	if status != store.StatusSucceeded {
		return ""
	}
	n := 0
	for _, it := range items {
		if it.Status == store.ItemDone {
			n++
		}
	}
	if sum != nil {
		n -= len(sum.AlreadyGone)
	}
	vms := countVMs(n)
	switch {
	case n <= 0:
		return ""
	case pods.Kind(kind) == pods.KindDeploy:
		return "Fleet deployed: " + vms
	case pods.Kind(kind) == pods.KindTeardown:
		return "Sunk: " + vms
	}
	return ""
}

// readSummary reads what a finished job's summary adds beyond its items.
func readSummary(raw json.RawMessage) *summaryView {
	if len(raw) == 0 {
		return nil
	}
	var sum jobs.Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return nil
	}
	v := &summaryView{Removed: sum.Removed, AlreadyGone: sum.AlreadyGone}
	for _, name := range slices.Sorted(maps.Keys(sum.CleanupFailed)) {
		v.CleanupAdvice = append(v.CleanupAdvice, sum.CleanupFailed[name])
	}
	if len(v.Removed) == 0 && len(v.AlreadyGone) == 0 && len(v.CleanupAdvice) == 0 {
		return nil
	}
	return v
}

// eventBatch is how many log lines one read takes.
const eventBatch = 500

// jobEventsAfter reads the job's log after event ID after, and the last ID
// read (after if none). Like the CLI, it skips bare "already done" lines, but
// their IDs still count.
func (s *Server) jobEventsAfter(ctx context.Context, id, after int64) ([]eventView, int64, error) {
	now := s.now()
	var out []eventView
	for {
		evs, err := s.st.Events(ctx, id, after, eventBatch)
		if err != nil {
			return nil, after, err
		}
		for _, ev := range evs {
			after = ev.ID
			if ev.Status == string(apply.EventSkipped) && ev.Message == "" {
				continue
			}
			out = append(out, eventView{
				ID: ev.ID, At: shortTime(now, ev.At), Full: clockTime(now, ev.At), Item: ev.Item,
				Text: eventText(ev), Err: ev.Status == string(apply.EventFailed) || ev.Status == string(apply.EventInterrupted),
			})
		}
		if len(evs) < eventBatch {
			return out, after, nil
		}
	}
}

// errJobOver ends a job's event stream once the job finished and all was sent.
var errJobOver = errors.New("the job finished")

// jobEvents streams a running job's page:
//
//   - "log": new log lines; the event ID is the last line's, so a reconnecting
//     browser (Last-Event-ID) resumes where it was;
//   - "patch": header, buttons and item table when they change (everything
//     after a Resync), wrapped in <template>s (see piece);
//   - "end": the job finished; the stream closes.
//
// Without Last-Event-ID, it starts after ?after=, the page's last event.
func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.jobOf(w, r)
	if !ok {
		return
	}
	j, id := snap.job, snap.job.ID
	topics := []string{status.JobTopic(id)}
	if j.Status == store.StatusPending {
		// The jobs it waits for end on their own topics.
		topics = append(topics, status.TopicJobs)
	}
	after := resumeFrom(r)
	defer s.jobSnaps.hold(id)()
	var shown pieceSet
	s.stream(w, r, topics, 500*time.Millisecond, func(sw *sseWriter, resync bool, seq uint64) error {
		// Read the job first: once finished, all its events are stored, so the read
		// below gets them all. Shared with the job's other streams (sharedJobSnap).
		snap, err := s.sharedJobSnap(r.Context(), id, seq)
		if err != nil {
			s.logf("web: reading job %d for its event stream: %v", id, err)
			return err
		}
		v := s.jobViewOf(r, snap)
		events, last, err := s.jobEventsAfter(r.Context(), id, after)
		if err != nil {
			s.logf("web: reading job %d's log for its event stream: %v", id, err)
			return err
		}
		if last > after {
			html, err := fragment("job", "job-log-lines", events)
			if err != nil {
				return err
			}
			if err := sw.event("log", strconv.FormatInt(last, 10), piece(html)); err != nil {
				return err
			}
			after = last
		}
		var cur pieceSet
		for _, name := range []string{"job-head", "job-actions"} {
			h, err := fragment("job", name, v)
			if err != nil {
				return err
			}
			cur.add(name, h)
		}
		cur.add("job-items", snap.itemsHTML)
		if resync {
			shown = pieceSet{}
		}
		patch := cur.changedSince(shown)
		shown = cur
		if patch != "" {
			if err := sw.event("patch", "", patch); err != nil {
				return err
			}
		}
		if !v.Live {
			if err := sw.event("end", "", "the job finished"); err != nil {
				return err
			}
			return errJobOver
		}
		return nil
	})
}

// resumeFrom is the browser's last log event: Last-Event-ID on reconnect,
// else ?after=.
func resumeFrom(r *http.Request) int64 {
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")} {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

// cancelJob asks a job to stop: a pending one is cancelled at once, a running
// one stops starting new steps.
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	// Fail closed: nothing is cancelled unless the check below ran.
	snap, ok := s.jobOf(w, r)
	if !ok {
		return
	}
	j, id := snap.job, snap.job.ID
	u, _ := auth.UserFrom(r.Context())
	if !s.mayCancel(r.Context(), j, snap.items, u) {
		s.logf("web: cancel refused: job=%d subject=%q: not its creator, and no %s", id, u.Subject, strings.Join(pods.OfferPrivileges(pods.Kind(j.Kind)), " or "))
		s.errorPage(w, r, http.StatusForbidden, "Not allowed", "Only whoever started this job, or someone who holds "+strings.Join(pods.OfferPrivileges(pods.Kind(j.Kind)), " or ")+
			" on each of its VMs in Proxmox, can cancel it.", logHref(id), "Back to the job")
		return
	}
	err := s.st.RequestCancel(r.Context(), id, actor(u))
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound, "No such job", fmt.Sprintf("There is no job %d.", id), logsPath, "Back to the logs")
		return
	case errors.Is(err, store.ErrNotActive):
		s.setFlash(w, flashError, fmt.Sprintf("Job %d had already finished, so there was nothing to cancel.", id))
	case err != nil:
		s.serverError(w, r, "cancelling job "+strconv.FormatInt(id, 10), err)
		return
	default:
		s.logf("web: cancel requested: job=%d subject=%q", id, u.Subject)
		s.setFlash(w, flashSuccess, fmt.Sprintf("Cancel requested for job %d. Steps already under way finish; nothing new starts.", id))
	}
	http.Redirect(w, r, logHref(id), http.StatusSeeOther)
}

// mayCancel reports whether u may cancel j: its starter, or anyone holding the
// operation's privilege on every VM of the job (on the VM or its team pool,
// for VMs a deploy has yet to create), as Proxmox resolves it.
func (s *Server) mayCancel(ctx context.Context, j store.Job, items []store.Item, u auth.User) bool {
	if j.CreatedBy == actor(u) {
		return true
	}
	acc := s.accessFor(ctx)
	if acc == nil {
		return true // a test fake that can't read privileges
	}
	for _, it := range items {
		if !pods.HoldsOffered(ctx, acc, pods.Kind(j.Kind), it.VMID, s.naming.Pool(it.Team)) {
			return false
		}
	}
	return len(items) > 0
}

// notPermitted says why a job's operation isn't offered to the viewer.
func notPermitted(kind string) string {
	return "You don't have " + strings.Join(pods.OfferPrivileges(pods.Kind(kind)), " or ") + " in Proxmox."
}

// actsAs is who a job acts as: the Proxmox user or token, or the role for
// jobs from before roles were dropped.
func actsAs(j store.Job) string {
	if j.CreatedAs != "" {
		return j.CreatedAs
	}
	return j.CreatedRole
}

// retryJob previews re-running a job for the VMs it didn't finish. Confirming
// goes through the operation's usual confirm and checks.
func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.jobOf(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	j, id := snap.job, snap.job.ID
	op, ok := operationFor(pods.Kind(j.Kind))
	if !ok {
		s.errorPage(w, r, http.StatusConflict, "Can't retry this job", fmt.Sprintf("Job %d is a %q job, which battleship can't run.", id, j.Kind), logHref(id), "Back to the job")
		return
	}
	retry, err := jobs.RetryInputs(j, snap.items)
	if errors.Is(err, jobs.ErrNotRetryable) || errors.Is(err, jobs.ErrNothingToRetry) {
		s.setFlash(w, flashError, sentence(err.Error()))
		http.Redirect(w, r, logHref(id), http.StatusSeeOther)
		return
	}
	if err != nil {
		s.serverError(w, r, "working out job "+strconv.FormatInt(id, 10)+"'s retry", err)
		return
	}
	plan, err := jobs.BuildPlan(ctx, s.planner(r.Context()), retry)
	if err != nil {
		s.renderForm(w, r, http.StatusUnprocessableEntity, op, retry, "Couldn't make a plan: "+sentence(proxmox.Describe(err)))
		return
	}
	s.showPreview(w, r, http.StatusOK, op, retry, plan, s.coversAllTeams(ctx, plan.Teams), previewOptions{retryOf: id})
}

// jobSnapTimeout bounds a shared job read.
const jobSnapTimeout = 30 * time.Second

// sharedJobSnap returns job id as read at or after hub sequence need, shared
// with the job's other streams (see seqFlight). It runs on the server's
// lifetime, not the request's, since other streams may wait for it.
func (s *Server) sharedJobSnap(ctx context.Context, id int64, need uint64) (jobSnap, error) {
	return s.jobSnaps.get(ctx, id, need, s.hub.Seq, func() (jobSnap, error) {
		s.jobReads.Add(1)
		rctx, cancel := context.WithTimeout(s.life, jobSnapTimeout)
		defer cancel()
		snap, err := s.readJobSnap(rctx, id)
		if err != nil {
			return jobSnap{}, err
		}
		// Every viewer's VM table is the same and has no time words that go stale:
		// render it once.
		var v jobView
		v.Items = jobItemsOf(snap.job, snap.items)
		snap.itemsHTML, err = fragment("job", "job-items", v)
		return snap, err
	})
}
