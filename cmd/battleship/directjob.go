package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// runQueued stores a confirmed plan as a job and follows it while a worker
// (battleship serve) runs it. The job carries the user's token, so the worker
// runs it as the user.
func runQueued(ctx context.Context, d deps, cfg config.Config, in jobs.Inputs, plan *pods.Plan, token proxmox.Credential) int {
	st, _, id, closeStore, err := submitJob(ctx, d, cfg, in, plan, token)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	defer closeStore()
	fmt.Fprintf(d.stdout, "\nJob %d queued; a worker (battleship serve) runs it once no other job uses its teams or templates. "+
		"Following it here; Ctrl-C cancels it.\n", id)
	followJob(ctx, d, cfg, st, id)
	return reportJob(context.WithoutCancel(ctx), d, st, id)
}

// reapStale marks silent jobs interrupted, as a worker's poll does, so a
// job whose worker died doesn't keep its follower waiting.
func reapStale(bg context.Context, d deps, cfg config.Config, st *store.Store) {
	ctx, stop := context.WithTimeout(bg, jobs.DBCallTimeout)
	defer stop()
	if ids, err := st.ReapStale(ctx, cfg.Jobs.StaleAfter); err != nil {
		fmt.Fprintf(d.stderr, "reaping stale jobs: %v\n", err)
	} else if len(ids) > 0 {
		fmt.Fprintf(d.stderr, "marked jobs %v interrupted: their runners stopped sending heartbeats\n", ids)
	}
}

// cancelWaiting cancels job id after Ctrl-C: a job still pending never runs;
// if a worker claimed it meanwhile, that worker is asked to stop.
func cancelWaiting(bg context.Context, d deps, st *store.Store, id int64) error {
	ctx, stop := context.WithTimeout(bg, jobs.DBCallTimeout)
	defer stop()
	if err := st.RequestCancel(ctx, id, d.user); err != nil && !errors.Is(err, store.ErrNotActive) {
		fmt.Fprintf(d.stderr, "job %d: cancelling: %v; cancel it with: battleship jobs cancel %d\n", id, err, id)
	}
	return fmt.Errorf("job %d: %w", id, store.ErrNotActive)
}

// followJob prints the progress of a job a worker runs until it finishes.
// Ctrl-C cancels it.
func followJob(ctx context.Context, d deps, cfg config.Config, st *store.Store, id int64) {
	bg := context.WithoutCancel(ctx)
	const page = 1000
	var after int64
	announced, cancelled := false, false
	for {
		reapStale(bg, d, cfg, st) // the worker we follow may have died
		callCtx, stop := context.WithTimeout(bg, jobs.DBCallTimeout)
		j, err := st.Job(callCtx, id)
		var evs []store.Event
		if err == nil {
			evs, err = st.Events(callCtx, id, after, page)
		}
		stop()
		if err != nil {
			fmt.Fprintf(d.stderr, "job %d: %v\n", id, err)
			if ctx.Err() != nil {
				return
			}
		} else {
			if j.Status == store.StatusRunning && !announced {
				fmt.Fprintf(d.stdout, "Worker %s is running job %d; following it here.\n", j.ClaimedBy, id)
				announced = true
			}
			for _, ev := range evs {
				printEvent(d.stdout, apply.Event{Time: ev.At, Item: ev.Item, Step: pods.Step(ev.Step),
					Status: apply.EventStatus(ev.Status), Message: ev.Message})
				after = ev.ID
			}
			// The job was read first, so a finished job's events are all in.
			if !j.Active() && len(evs) < page {
				return
			}
		}
		if ctx.Err() != nil && !cancelled {
			cancelled = true
			cancelWaiting(bg, d, st, id) //nolint:errcheck
			fmt.Fprintf(d.stdout, "Cancel requested for job %d; waiting for its worker to stop.\n", id)
		}
		_ = proxmox.SleepContext(bg, cfg.Jobs.Poll)
	}
}

// reportJob prints how job id ended; the exit code is 0 only on success.
func reportJob(ctx context.Context, d deps, st *store.Store, id int64) int {
	j, err := st.Job(ctx, id)
	if err != nil {
		fmt.Fprintf(d.stderr, "job %d: %v\n", id, err)
		return 1
	}
	var sum jobs.Summary
	if len(j.Summary) > 0 && json.Unmarshal(j.Summary, &sum) == nil {
		printSummary(d.stdout, sum)
	}
	line := fmt.Sprintf("\nJob %d %s", id, strings.ReplaceAll(j.Status, "_", " "))
	if j.Error != "" {
		line += ": " + j.Error
	}
	fmt.Fprintln(d.stdout, line+".")
	if j.Status != store.StatusSucceeded {
		return 1
	}
	return 0
}
