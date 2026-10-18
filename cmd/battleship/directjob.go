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

// runQueued stores a confirmed plan as a job for a worker (battleship serve)
// to run as the user, then follows it unless follow is false.
func runQueued(ctx context.Context, d deps, cfg config.Config, in jobs.Inputs, plan *pods.Plan, token proxmox.Credential, follow bool) int {
	st, id, closeStore, err := submitJob(ctx, d, cfg, in, plan, token)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	defer closeStore()
	if !follow {
		fmt.Fprintf(d.stdout, "Queued job %d. A worker (battleship serve) runs it; follow it with: battleship jobs show %d\n", id, id)
		return 0
	}
	fmt.Fprintf(d.stdout, "\nJob %d queued; a worker (battleship serve) runs it once no other job uses its pods or templates. "+
		"Following it here; Ctrl-C cancels it.\n", id)
	followJob(ctx, d, cfg, st, id)
	return reportJob(context.WithoutCancel(ctx), d, st, id)
}

// requestCancel cancels job id after Ctrl-C: a job still pending never runs;
// a running one's worker is asked to stop.
func requestCancel(bg context.Context, d deps, st *store.Store, id int64) {
	ctx, stop := context.WithTimeout(bg, jobs.DBCallTimeout)
	defer stop()
	if err := st.RequestCancel(ctx, id, d.user); err != nil && !errors.Is(err, store.ErrNotActive) {
		fmt.Fprintf(d.stderr, "job %d: cancelling: %v; cancel it with: battleship jobs cancel %d\n", id, err, id)
	}
}

// waitingMessage says why pending job id hasn't started: the jobs it waits
// for, or, once it has waited idle polls with none, that no worker took it.
func waitingMessage(ctx context.Context, st *store.Store, id int64, idle int) string {
	blockers, err := st.Blockers(ctx, id)
	switch {
	case err != nil:
		return ""
	case len(blockers) > 0:
		return fmt.Sprintf("Waiting for job(s) %s, which use the same pods or templates.", joinIDs(blockers))
	case idle >= noWorkerPolls:
		return "No worker has taken it yet; is battleship serve running?"
	}
	return ""
}

// noWorkerPolls is how many polls a pending job with no blockers waits before
// its follower says no worker took it.
const noWorkerPolls = 5

// followJob prints the progress of a job a worker runs until it finishes.
// Ctrl-C cancels it.
func followJob(ctx context.Context, d deps, cfg config.Config, st *store.Store, id int64) {
	bg := context.WithoutCancel(ctx)
	const page = 1000
	var after int64
	announced, cancelled := false, false
	shown, idle := "", 0
	for {
		callCtx, stop := context.WithTimeout(bg, jobs.DBCallTimeout)
		j, err := st.Job(callCtx, id)
		var evs []store.Event
		if err == nil {
			evs, err = st.Events(callCtx, id, after, page)
		}
		msg := ""
		if err == nil && j.Status == store.StatusPending {
			idle++
			msg = waitingMessage(callCtx, st, id, idle)
		}
		stop()
		if err != nil {
			fmt.Fprintf(d.stderr, "job %d: %v\n", id, err)
			if ctx.Err() != nil {
				return
			}
		} else {
			if msg != "" && msg != shown {
				fmt.Fprintln(d.stdout, msg)
				shown = msg
			}
			if j.Status == store.StatusRunning && !announced {
				fmt.Fprintf(d.stdout, "Worker %s is running job %d; following it here.\n", clean(j.ClaimedBy), id)
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
			requestCancel(bg, d, st, id)
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
	line := fmt.Sprintf("\nJob %d %s", id, clean(strings.ReplaceAll(j.Status, "_", " ")))
	if j.Error != "" {
		line += ": " + cleanLines(j.Error, "  ")
	}
	fmt.Fprintln(d.stdout, line+".")
	if j.Status != store.StatusSucceeded {
		return 1
	}
	return 0
}
