package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// runAsJob runs a confirmed plan in this process, but as a job: it is stored
// first and starts only once no older job uses its teams or templates, so a
// direct run never overlaps a worker's job or another direct run.
//
// The job carries the user's token like a queued one, so a worker that
// takes it over runs it as the user too.
func runAsJob(ctx context.Context, d deps, cfg config.Config, client pods.API, in jobs.Inputs, plan *pods.Plan, token proxmox.Credential) int {
	bg := context.WithoutCancel(ctx)
	st, creds, id, closeStore, err := submitJob(ctx, d, cfg, in, plan, token)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	defer closeStore()
	fmt.Fprintf(d.stdout, "\nJob %d queued; it runs here once no other job uses its teams or templates.\n", id)

	// The executor calls OnEvent from many goroutines.
	var evMu sync.Mutex
	cancels, stopListening := listenForCancels(ctx, st, func(format string, a ...any) { fmt.Fprintf(d.stderr, format+"\n", a...) })
	defer stopListening()
	w := &jobs.Worker{
		Store:       st,
		Cfg:         cfg,
		Bind:        bindAs(client),
		Credentials: creds,
		ID:          jobs.NewWorkerID("cli"),
		Logf:        func(format string, a ...any) { fmt.Fprintf(d.stderr, format+"\n", a...) },
		Cancels:     cancels,
		OnEvent: func(e pods.Event) {
			evMu.Lock()
			defer evMu.Unlock()
			printEvent(d.stdout, e)
		},
	}
	job, err := waitForClaim(ctx, d, cfg, st, w.ID, id, jobs.LockKeys(plan))
	switch {
	case errors.Is(err, store.ErrNotActive):
		// Cancelled while it waited, or a worker took it; show how it goes.
		followJob(ctx, d, cfg, st, id)
	case err != nil:
		fmt.Fprintf(d.stderr, "job %d: %v\n", id, err)
		return 1
	default:
		w.RunJob(ctx, job)
	}
	return reportJob(bg, d, st, id)
}

// waitForClaim claims job id for worker once nothing blocks it, saying which
// jobs it waits for. On Ctrl-C it cancels the job (see cancelWaiting).
func waitForClaim(ctx context.Context, d deps, cfg config.Config, st *store.Store, worker string, id int64, keys []string) (*store.Job, error) {
	bg := context.WithoutCancel(ctx)
	shown := ""
	for {
		if ctx.Err() != nil {
			return nil, cancelWaiting(bg, d, st, id)
		}
		reapStale(bg, d, cfg, st)
		// Not ctx: a claim that commits as Ctrl-C arrives must be seen, so
		// the job isn't left running with no one to run it.
		callCtx, stop := context.WithTimeout(bg, jobs.DBCallTimeout)
		job, err := st.ClaimJob(callCtx, id, worker)
		stop()
		switch {
		case job != nil && ctx.Err() != nil:
			finCtx, stop := context.WithTimeout(bg, jobs.DBCallTimeout)
			defer stop()
			if err := st.Finish(finCtx, id, worker, store.Outcome{Status: store.StatusCancelled, Error: "cancelled before it started"}); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("job %d: %w", id, store.ErrNotActive)
		case job != nil:
			return job, nil
		case errors.Is(err, store.ErrNotActive), errors.Is(err, store.ErrNotFound):
			return nil, err
		case err != nil:
			fmt.Fprintf(d.stderr, "job %d: claiming: %v; trying again\n", id, err)
		default:
			if msg := blockersMessage(ctx, st, id, keys); msg != "" && msg != shown {
				fmt.Fprintln(d.stdout, msg)
				shown = msg
			}
		}
		_ = proxmox.SleepContext(ctx, cfg.Jobs.Poll)
	}
}

// reapStale marks jobs whose runners went silent as interrupted, as a worker
// does each poll, so a force-quit direct run can't block its teams forever
// when no worker is running.
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

// blockersMessage says which jobs job id waits for, and on what, or "". It
// also says how to give up on job id, from here (Ctrl-C) or another terminal.
func blockersMessage(ctx context.Context, st *store.Store, id int64, keys []string) string {
	callCtx, stop := context.WithTimeout(ctx, jobs.DBCallTimeout)
	defer stop()
	blockers, err := st.Blockers(callCtx, id)
	if err != nil || len(blockers) == 0 {
		return ""
	}
	mine := map[string]bool{}
	for _, k := range keys {
		mine[k] = true
	}
	shared := map[string]bool{}
	for _, b := range blockers {
		if j, err := st.Job(callCtx, b); err == nil {
			for _, k := range j.LockKeys {
				if mine[k] {
					shared[k] = true
				}
			}
		}
	}
	on := make([]string, 0, len(shared))
	for k := range shared {
		on = append(on, k)
	}
	sort.Strings(on)
	return fmt.Sprintf("waiting for job(s) %s on %s (to give up on job %d: Ctrl-C, or battleship jobs cancel %d)", joinIDs(blockers), strings.Join(on, ", "), id, id)
}

// followJob prints the progress of a job another worker runs until it
// finishes. Ctrl-C asks that worker to cancel it.
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
				printEvent(d.stdout, pods.Event{Time: ev.At, Item: ev.Item, Step: pods.Step(ev.Step),
					Status: pods.EventStatus(ev.Status), Message: ev.Message})
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

// reportJob prints how job id ended and returns the exit code: 0 only if it
// succeeded.
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
