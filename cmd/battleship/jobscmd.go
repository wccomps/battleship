package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// runWorker claims and runs queued jobs until interrupted.
func runWorker(ctx context.Context, args []string, d deps) int {
	fs := flag.NewFlagSet("battleship worker", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("config", "battleship.toml", "config file")
	if code, ok := parseFlags(fs, args, 0, d); !ok {
		return code
	}
	cfg, st, closeStore, code := loadWithStore(ctx, *cfgPath, d)
	if st == nil {
		return code
	}
	defer closeStore()
	if err := errors.Join(noServiceToken("battleship worker"), cfg.RequireTLSVerify()); err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	if cfg.Proxmox.InsecureSkipVerify {
		fmt.Fprintln(d.stderr, "warning: Proxmox's certificate is not checked (proxmox.insecure_skip_verify = true); set proxmox.ca_file to the cluster CA instead")
	}
	creds, err := jobs.OpenCredentials(ctx, cfg, st)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	client := d.newAPI(cfg.Proxmox)
	setProxmoxLogf(client, func(format string, a ...any) { fmt.Fprintf(d.stderr, format+"\n", a...) })
	cancels, stopListening := listenForCancels(ctx, st, func(format string, a ...any) { fmt.Fprintf(d.stderr, format+"\n", a...) })
	defer stopListening()
	w := &jobs.Worker{
		Store:       st,
		Cfg:         cfg,
		Bind:        bindAs(client),
		Credentials: creds,
		ID:          jobs.NewWorkerID("worker"),
		Logf:        func(format string, a ...any) { fmt.Fprintf(d.stderr, format+"\n", a...) },
		Cancels:     cancels,
	}
	// Keep the tickets of waiting and running jobs alive, as each serve
	// replica does, so a deployment of workers alone works too.
	if renewer, ok := client.(jobs.TicketRenewer); ok {
		r := &jobs.Renewer{Store: st, Proxmox: renewer, Credentials: creds,
			Logf: func(format string, a ...any) { fmt.Fprintf(d.stderr, format+"\n", a...) }}
		go r.Run(ctx)
	}
	fmt.Fprintf(d.stdout, "worker %s waiting for jobs\n", w.ID)
	if err := w.Run(ctx); err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	fmt.Fprintln(d.stdout, "worker stopped")
	return 0
}

const jobsUsage = "usage: battleship jobs <list|show ID|cancel ID> [-config battleship.toml]\n"

// runJobs lists, shows and cancels jobs.
func runJobs(ctx context.Context, args []string, d deps) int {
	if len(args) == 0 {
		fmt.Fprint(d.stderr, jobsUsage)
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("battleship jobs "+sub, flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("config", "battleship.toml", "config file")
	var limit *int
	wantArgs := 1
	switch sub {
	case "list":
		limit = fs.Int("n", 20, "how many recent jobs to list")
		wantArgs = 0
	case "show", "cancel":
	default:
		fmt.Fprint(d.stderr, jobsUsage)
		return 2
	}
	if code, ok := parseFlags(fs, args[1:], wantArgs, d); !ok {
		return code
	}
	var id int64
	if wantArgs == 1 {
		n, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil || n <= 0 {
			fmt.Fprintf(d.stderr, "job ID must be a positive number, not %q\n", fs.Arg(0))
			return 2
		}
		id = n
	}
	_, st, closeStore, code := loadWithStore(ctx, *cfgPath, d)
	if st == nil {
		return code
	}
	defer closeStore()

	switch sub {
	case "list":
		return listJobs(ctx, d, st, *limit)
	case "show":
		return showJob(ctx, d, st, id)
	default:
		return cancelJob(ctx, d, st, id)
	}
}

// parseFlags parses args, allowing exactly nArgs positional arguments after
// the flags. It returns an exit code and false if the command should stop.
func parseFlags(fs *flag.FlagSet, args []string, nArgs int, d deps) (int, bool) {
	// Accept flags after the positional ID too: "show 7 -config x".
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0, false
			}
			return 2, false
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != nArgs {
		if nArgs == 0 {
			fmt.Fprintf(d.stderr, "unexpected argument %q\n", positional[0])
		} else {
			fmt.Fprintf(d.stderr, "expected %d argument(s), got %d\n", nArgs, len(positional))
		}
		return 2, false
	}
	// Leave positional args where fs.Arg can read them.
	_ = fs.Parse(append([]string{"--"}, positional...))
	return 0, true
}

func loadWithStore(ctx context.Context, path string, d deps) (config.Config, *store.Store, func(), int) {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return cfg, nil, nil, 1
	}
	if err := cfg.RequireDatabase(); err != nil {
		fmt.Fprintln(d.stderr, err)
		return cfg, nil, nil, 1
	}
	st, closeStore, err := d.openStore(ctx, cfg.Database)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return cfg, nil, nil, 1
	}
	return cfg, st, closeStore, 0
}

func describeInputs(raw json.RawMessage) string {
	var in jobs.Inputs
	if err := json.Unmarshal(raw, &in); err != nil {
		return "?"
	}
	s := "teams " + in.Teams
	if len(in.Hosts) > 0 {
		s += " hosts " + strings.Join(in.Hosts, ",")
	}
	switch {
	case in.Pattern != "":
		s = in.Pattern + " " + s
	case in.Action != "":
		s = in.Action + " " + s
	case in.Kind == pods.KindSnapshot:
		s = in.Snapshot + " " + s
	}
	return s
}

func when(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func listJobs(ctx context.Context, d deps, st *store.Store, n int) int {
	list, err := st.Jobs(ctx, n)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	if len(list) == 0 {
		fmt.Fprintln(d.stdout, "No jobs yet.")
		return 0
	}
	tw := tabwriter.NewWriter(d.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tKIND\tWHAT\tBY\tCREATED")
	for _, j := range list {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", j.ID, j.Status, j.Kind, describeInputs(j.Inputs), j.CreatedBy, when(&j.CreatedAt))
	}
	tw.Flush()
	return 0
}

func showJob(ctx context.Context, d deps, st *store.Store, id int64) int {
	j, err := st.Job(ctx, id)
	if err != nil {
		fmt.Fprintf(d.stderr, "job %d: %v\n", id, err)
		return 1
	}
	w := d.stdout
	fmt.Fprintf(w, "Job %d: %s %s (%s)\n", j.ID, j.Kind, describeInputs(j.Inputs), j.Status)
	fmt.Fprintf(w, "Created by %s at %s; started %s; finished %s\n", j.CreatedBy, when(&j.CreatedAt), when(j.StartedAt), when(j.FinishedAt))
	if j.Status == store.StatusPending {
		if blockers, err := st.Blockers(ctx, id); err == nil && len(blockers) > 0 {
			fmt.Fprintf(w, "Waiting for job(s) %s, which use the same teams or templates.\n", joinIDs(blockers))
		}
	}
	switch {
	case !j.CancelRequested:
	case j.Status == store.StatusRunning:
		fmt.Fprintf(w, "Cancel requested by %s; the job is stopping.\n", j.CancelledBy)
	case j.CancelCameTooLate():
		fmt.Fprintf(w, "Cancel requested by %s came too late to stop anything.\n", j.CancelledBy)
	}
	if j.Error != "" {
		fmt.Fprintf(w, "Error: %s\n", j.Error)
	}
	items, err := st.Items(ctx, id)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	fmt.Fprintf(w, "\nVMs (%d):\n", len(items))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, it := range items {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", it.Name, orDash(it.VMID), it.Status, orDashStr(it.Step), it.Error)
	}
	tw.Flush()
	events, err := st.Events(ctx, id, 0, 100000)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	const tail = 30
	if len(events) > tail {
		fmt.Fprintf(w, "\nLast %d of %d events:\n", tail, len(events))
		events = events[len(events)-tail:]
	} else if len(events) > 0 {
		fmt.Fprintln(w, "\nEvents:")
	}
	for _, ev := range events {
		item := ev.Item
		if item == "" {
			item = "job"
		}
		line := fmt.Sprintf("  %s %-8s %s", ev.At.Local().Format("15:04:05"), ev.Status, item)
		if ev.Step != "" {
			line += " " + ev.Step
		}
		if ev.Message != "" {
			line += ": " + ev.Message
		}
		fmt.Fprintln(w, line)
	}
	return 0
}

func cancelJob(ctx context.Context, d deps, st *store.Store, id int64) int {
	switch err := st.RequestCancel(ctx, id, d.user); {
	case errors.Is(err, store.ErrNotFound):
		fmt.Fprintf(d.stderr, "job %d not found\n", id)
		return 1
	case errors.Is(err, store.ErrNotActive):
		fmt.Fprintf(d.stderr, "job %d already finished; nothing to cancel\n", id)
		return 1
	case err != nil:
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	j, err := st.Job(ctx, id)
	if err == nil && j.Status == store.StatusCancelled {
		fmt.Fprintf(d.stdout, "Job %d cancelled before it started.\n", id)
		return 0
	}
	fmt.Fprintf(d.stdout, "Cancel requested for job %d; its worker stops starting new steps, finishes or removes anything half-built, and records it as cancelled.\n", id)
	return 0
}

func joinIDs(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(s, ", ")
}
