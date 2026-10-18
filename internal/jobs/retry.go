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
	// ErrNotRetryable: the job is active, ran nothing (start it from its
	// form instead), or finished everything.
	ErrNotRetryable = errors.New("only jobs that completed with failures, were interrupted or were cancelled can be retried")
	// ErrNothingToRetry is returned when every VM of a job finished.
	ErrNothingToRetry = errors.New("nothing to retry: every VM of the job finished")
)

// RetryInputs returns the job's inputs narrowed to its retryable items
// (store.ItemStatus.Retryable), their teams and hosts. A deploy's Rebuild is
// cleared: the planner blocks rebuilding a template team VMs still use,
// which would block every VM of the retry. An environment's create or add
// keeps its hosts, so the plan is the same, and is narrowed to the VMs; a
// create keeps its slot, where the planner resumes it. An environment's
// teardown is planned again whole in its slot (pods.Kind.RetriedWhole).
func RetryInputs(job store.Job, items []store.Item) (Inputs, error) {
	if !store.JobStatus(job.Status).CanRetry() {
		return Inputs{}, fmt.Errorf("job %d is %s: %w", job.ID, job.Status, ErrNotRetryable)
	}
	kind := pods.Kind(job.Kind)
	if kind.Teamless() && kind != pods.KindEnvCreate && kind != pods.KindEnvAdd && kind != pods.KindEnvTeardown {
		return Inputs{}, fmt.Errorf("job %d: run it again from its form; image jobs and DNS changes resume what exists: %w", job.ID, ErrNotRetryable)
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
	if kind.Teamless() {
		return envRetry(job.ID, in, plan, planned, items)
	}
	var teams, hosts, names []string
	wholeTeams := false
	for _, it := range items {
		if !store.ItemStatus(it.Status).Retryable() {
			continue
		}
		p, ok := planned[it.Name]
		switch {
		case ok && it.Team != "" && p.Host == "" && plan.Kind == pods.KindTeardown:
			// A gone VM's disks or half-deleted VM: only a whole-team
			// teardown plans those.
			wholeTeams = true
		case !ok || p.Host == "" || it.Team == "":
			return Inputs{}, fmt.Errorf("job %d's plan has no pod VM named %s", job.ID, it.Name)
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

// envRetry is RetryInputs for an environment's create, add or teardown.
func envRetry(id int64, in Inputs, plan pods.Plan, planned map[string]pods.Item, items []store.Item) (Inputs, error) {
	var names []string
	for _, it := range items {
		if !store.ItemStatus(it.Status).Retryable() {
			continue
		}
		if _, ok := planned[it.Name]; !ok {
			return Inputs{}, fmt.Errorf("job %d's plan has no VM named %s", id, it.Name)
		}
		names = append(names, it.Name)
	}
	if len(names) == 0 {
		return Inputs{}, ErrNothingToRetry
	}
	if !in.Kind.RetriedWhole() {
		in.VMs = sortedUnique(names)
	}
	if (in.Kind == pods.KindEnvCreate || in.Kind == pods.KindEnvTeardown) && in.Slot == "" {
		in.Slot = plan.DevSlot()
	}
	return in, nil
}

func sortedUnique(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}
