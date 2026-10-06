package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

var (
	// ErrNotRetryable is returned for a job that is still active, or that
	// ended without running anything (stale or failed: start it again from
	// its form instead) or with everything done.
	ErrNotRetryable = errors.New("only jobs that completed with failures, were interrupted or were cancelled can be retried")
	// ErrNothingToRetry is returned when every VM of a job finished.
	ErrNothingToRetry = errors.New("nothing to retry: every VM of the job finished")
)

// Retryable reports whether an item of a finished job didn't finish, so a
// retry should run it again: it failed, was blocked, was interrupted, was
// removed after failing half-built, or never ran.
func Retryable(it store.Item) bool {
	switch it.Status {
	case store.ItemFailed, store.ItemBlocked, store.ItemInterrupted, store.ItemRemoved, store.ItemNotRun:
		return true
	}
	return false
}

// CanRetry reports whether a job with this status may be retried.
func CanRetry(status string) bool {
	switch status {
	case store.StatusCompletedWithFailures, store.StatusInterrupted, store.StatusCancelled:
		return true
	}
	return false
}

// RetryInputs works out how to retry a finished job for exactly its items
// that didn't finish (see Retryable): its stored inputs with VMs set to
// those items' names, and Teams and Hosts narrowed to their teams and
// hosts (the hosts come from the job's stored plan; with a teardown item
// that has no host among them, a gone VM's disks or a half-deleted VM,
// every host). A deploy's Rebuild is
// cleared: the first run rebuilt the templates (or was blocked from doing
// so), and the planner blocks rebuilding a template that team VMs, such as
// the finished ones, still use, which would block every VM of the retry. It
// returns ErrNotRetryable or ErrNothingToRetry when there's nothing to retry.
func RetryInputs(job store.Job, items []store.Item) (Inputs, error) {
	if !CanRetry(job.Status) {
		return Inputs{}, fmt.Errorf("job %d is %s: %w", job.ID, job.Status, ErrNotRetryable)
	}
	var in Inputs
	if err := json.Unmarshal(job.Inputs, &in); err != nil {
		return Inputs{}, fmt.Errorf("reading job %d's inputs: %w", job.ID, err)
	}
	var plan pods.Plan
	if err := json.Unmarshal(job.Plan, &plan); err != nil {
		return Inputs{}, fmt.Errorf("reading job %d's plan: %w", job.ID, err)
	}
	planned := map[string]pods.Item{}
	for _, it := range plan.Items {
		planned[it.Name] = it
	}
	var teams, hosts, names []string
	wholeTeams := false
	for _, it := range items {
		if !Retryable(it) {
			continue
		}
		p, ok := planned[it.Name]
		switch {
		case ok && it.Team != "" && p.Host == "" && plan.Kind == pods.KindTeardown:
			// No host: a gone VM's disks, or a half-deleted VM, which only
			// a whole-team teardown plans.
			wholeTeams = true
		case !ok || p.Host == "" || it.Team == "":
			return Inputs{}, fmt.Errorf("job %d's plan has no team VM named %s", job.ID, it.Name)
		}
		teams = append(teams, it.Team)
		hosts = append(hosts, p.Host)
		names = append(names, it.Name)
	}
	if len(names) == 0 {
		return Inputs{}, ErrNothingToRetry
	}
	in.Teams = pods.FormatTeams(teams)
	in.Hosts = sortedUnique(hosts)
	if wholeTeams {
		in.Hosts = nil
	}
	in.VMs = sortedUnique(names)
	in.Rebuild = false
	return in, nil
}

func sortedUnique(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}
