package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// runOp previews and applies (or queues) deploy, teardown, reset, power or
// snapshot. With a database, an applied plan is queued as a job and followed
// (see runQueued).
func runOp(ctx context.Context, cmd string, args []string, d deps) int {
	c, code, ok := parseOp(cmd, args, d.stderr)
	if !ok {
		return code
	}
	return runPlanned(ctx, d, cmd, c.cfgPath, c.in, c.all, c.yes, c.queue)
}

// command is a parsed command line that plans a job.
type command struct {
	in              jobs.Inputs
	cfgPath         string
	all, yes, queue bool // all: -pods all, resolved once the cluster is read
}

// parseOp reads a team operation's flags into the inputs the web form would
// give (Inputs.Normalize); if not ok, the command exits with code.
func parseOp(cmd string, args []string, stderr io.Writer) (c command, code int, ok bool) {
	fs := flag.NewFlagSet("battleship "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "battleship.toml", "config file")
	podSpec := fs.String("pods", "", "pods, e.g. 7, 1-32 or 1,3,5, or all: every pod with VMs (required)")
	teamsAlias := fs.String("teams", "", "the same as -pods")
	hostSpec := fs.String("hosts", "", "comma-separated host filter, e.g. dc,web")
	vmSpec := fs.String("vms", "", "comma-separated exact VM names to act on, e.g. team01-dc,team02-web (to retry just those)")
	yes := fs.Bool("yes", false, "apply the plan without asking")
	queue := fs.Bool("queue", false, "queue the confirmed plan as a job and exit instead of following it")
	in := jobs.Inputs{Kind: pods.Kind(cmd)}
	switch cmd {
	case "deploy":
		fs.StringVar(&in.Pattern, "templates", "", "master VM name pattern, e.g. '*.kilo.alpha' (required)")
		fs.BoolVar(&in.Rebuild, "rebuild", false, "recreate templates from masters")
		fs.BoolVar(&in.NoSnapshot, "no-snapshot", false, "skip the baseline snapshot")
	case "teardown":
		fs.StringVar((*string)(&in.Set), "set", "", "the one template set to tear down, by name, e.g. kilo.alpha (default: every matching VM in the pods)")
	case "reset":
		fs.StringVar(&in.Snapshot, "snapshot", "", "snapshot to roll back to (default: each VM's baseline: deploy.snapshot_name, else the newest deploy.baseline_patterns match)")
	case "power":
		fs.StringVar(&in.Action, "action", "", "start, shutdown, stop or reboot (required)")
	case "snapshot":
		fs.StringVar(&in.Snapshot, "name", "", "the new snapshot's name: a letter, then letters, digits, _ or -, at most 40 (required; never one a VM has, or the baseline's)")
		fs.StringVar(&in.Description, "description", "", "the snapshot's description in Proxmox")
		fs.BoolVar(&in.VMState, "vmstate", false, "also save the RAM of running VMs (slower, larger)")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return c, 0, false
		}
		return c, 2, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q (put flags before any other words)\n", fs.Arg(0))
		return c, 2, false
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	teamSpec := *podSpec
	if given["teams"] {
		if given["pods"] && *teamsAlias != *podSpec {
			fmt.Fprintln(stderr, "-pods and -teams (the same as -pods) differ; give one")
			return c, 2, false
		}
		teamSpec = *teamsAlias
	}
	all := isAllTeams(teamSpec)
	if all && cmd == "deploy" {
		fmt.Fprintln(stderr, "-pods all is for teardown, power, reset and snapshot; name the pods to deploy, e.g. 1-32")
		return c, 2, false
	}
	if _, err := pods.ParseTeams(teamSpec); !all && (teamSpec == "" || err != nil) {
		fmt.Fprintf(stderr, "-pods: %v\n", orRequired(err))
		return c, 2, false
	}
	in.Teams = teamSpec
	in.Hosts = jobs.SplitList(*hostSpec)
	in.VMs = jobs.SplitList(*vmSpec)
	in = in.Normalize()
	if given["hosts"] && len(in.Hosts) == 0 {
		fmt.Fprintln(stderr, "-hosts is empty")
		return c, 2, false
	}
	if given["vms"] && len(in.VMs) == 0 {
		fmt.Fprintln(stderr, "-vms is empty")
		return c, 2, false
	}
	switch cmd {
	case "deploy":
		if in.Pattern == "" {
			fmt.Fprintln(stderr, "-templates is required")
			return c, 2, false
		}
	case "teardown":
		_, err := pods.ParseSet(string(in.Set))
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "-set: %v\n", err)
			return c, 2, false
		case given["set"] && in.Set == "":
			fmt.Fprintln(stderr, "-set is empty; leave it out to tear down every matching VM in the pods")
			return c, 2, false
		}
	case "power":
		if !pods.IsPowerAction(in.Action) {
			fmt.Fprintf(stderr, "-action must be start, shutdown, stop or reboot (got %q)\n", in.Action)
			return c, 2, false
		}
	case "snapshot":
		if in.Snapshot == "" {
			fmt.Fprintln(stderr, "-name is required")
			return c, 2, false
		}
	}
	check := in
	if all {
		check.Teams = "0" // stands in until the cluster is read
	}
	if err := check.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return c, 2, false
	}
	return command{in: in, cfgPath: *cfgPath, all: all, yes: *yes, queue: *queue}, 0, true
}

// runPlanned plans in, prints the plan, asks, then queues or runs it.
func runPlanned(ctx context.Context, d deps, cmd, cfgPath string, in jobs.Inputs, all, yes, queue bool) int {
	stdout, stderr := d.stdout, d.stderr
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if queue {
		if err := cfg.RequireDatabase(); err != nil {
			fmt.Fprintln(stderr, "-queue:", err)
			return 1
		}
	}
	token, err := userToken()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	client := d.newAPI(cfg.Proxmox)
	setProxmoxLogf(client, func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) })
	api := asToken(client, token)
	if all {
		spec, says, err := resolveAllTeams(ctx, api, cfg)
		if err != nil {
			fmt.Fprintln(stderr, "-pods all:", cleanLines(proxmox.Describe(err), "  "))
			return 1
		}
		fmt.Fprintln(stdout, says)
		fmt.Fprintln(stdout)
		in.Teams = spec
	}
	planner := pods.NewPlanner(api, cfg)
	if cfg.Database.URL != "" {
		// The job will queue: don't plan a VMID or slot a queued job will create.
		planner.Reserved = func(ctx context.Context) (map[string]bool, error) {
			st, closeStore, err := d.openStore(ctx, cfg.Database)
			if err != nil {
				return nil, err
			}
			defer closeStore()
			return st.ReservedKeys(ctx, 0)
		}
	}
	plan, err := jobs.BuildPlan(ctx, planner, in)
	if err != nil {
		fmt.Fprintln(stderr, "planning:", cleanLines(proxmox.Describe(err), "  "))
		return 1
	}

	printPlan(stdout, pods.NewNaming(cfg.Naming), plan)
	if cmd == "teardown" {
		what := "everything in the pods"
		if len(in.Hosts) > 0 || len(in.VMs) > 0 {
			what = "every matching VM in the pods"
		}
		if in.Set != "" {
			what = "set " + clean(string(in.Set)) + " only, keeping every VM whose one set tag isn't " + clean(string(in.Set)) + "'s"
		}
		fmt.Fprintf(stdout, "\nTearing down %s.\n", what)
	}
	if pods.NeedsCapacity(plan) {
		// Advice only: overcommit may be deliberate, so it never stops the plan.
		if c, err := pods.ReadCapacity(ctx, api, plan, cfg); err != nil {
			fmt.Fprintln(stdout, "\nCapacity: couldn't read the cluster's resources:", cleanLines(proxmox.Describe(err), "  "))
		} else {
			printCapacity(stdout, c)
		}
	}
	if missing := jobs.MissingVMs(in, plan); len(missing) > 0 {
		fmt.Fprintf(stdout, "\n%d of the %d -vms no longer exist or match -pods/-hosts, so they are left out: %s\n",
			len(missing), len(in.VMs), cleanJoin(missing, ", "))
	}
	blocked := len(plan.Items) - len(plan.Runnable())
	if blocked > 0 {
		fmt.Fprintf(stdout, "\n%d VMs blocked (see reasons above).\n", blocked)
	}
	if len(plan.Items) == 0 {
		if len(in.Hosts) > 0 && cmd != "deploy" {
			fmt.Fprintf(stdout, "\nNo VMs matched -hosts %s for pods %s; check the host names.\n",
				cleanJoin(in.Hosts, ","), clean(in.Teams))
			return 1
		}
		fmt.Fprintln(stdout, "\nNothing to do.")
		return 0
	}
	if len(plan.Runnable()) == 0 {
		fmt.Fprintln(stdout, "\nNothing can run.")
		return 1
	}
	if !yes {
		if !d.interactive {
			fmt.Fprintln(stdout, "\nNothing changed. Re-run with -yes to re-plan and apply.")
			return 0
		}
		if !confirm(d, in) || ctx.Err() != nil {
			fmt.Fprintln(stdout, "Cancelled; nothing changed.")
			return 1
		}
	}

	if queue || cfg.Database.URL != "" {
		if err := cfg.RequireDatabase(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return runQueued(ctx, d, cfg, in, plan, token, !queue)
	}
	// No database: run in-process, trusting no one else is on these teams or templates.
	// The executor calls OnEvent from many goroutines.
	var evMu sync.Mutex
	exec := &apply.Executor{API: api, Cfg: cfg, OnEvent: func(e apply.Event) {
		evMu.Lock()
		defer evMu.Unlock()
		printEvent(stdout, e)
	}}
	res := exec.Run(ctx, plan)
	printResult(stdout, res)
	// CleanupFailed counts on its own: a master a template build left
	// stopped is a cleanup failure with no failed item.
	if len(res.Failed) > 0 || len(res.Interrupted) > 0 || len(res.Blocked) > 0 || len(res.CleanupFailed) > 0 {
		return 1
	}
	return 0
}

// submitJob stores a job for plan with the user's token sealed, and returns
// the open store (closeStore closes it) and the job's ID. On an error
// nothing is left open.
func submitJob(ctx context.Context, d deps, cfg config.Config, in jobs.Inputs, plan *pods.Plan, token proxmox.Credential) (st *store.Store, id int64, closeStore func(), err error) {
	if err = cfg.RequireSealKey(); err != nil {
		return nil, 0, nil, err
	}
	if st, closeStore, err = d.openStore(ctx, cfg.Database); err != nil {
		return nil, 0, nil, err
	}
	creds, err := jobs.OpenCredentials(ctx, cfg, st)
	if err == nil {
		id, err = jobs.Submit(ctx, st, in, plan, jobs.Submitter{User: d.user, Credential: token, Seal: creds})
	}
	if err != nil {
		closeStore()
		return nil, 0, nil, err
	}
	return st, id, closeStore, nil
}

// confirm asks the volunteer to approve the plan just printed. Teardown
// requires typing the -pods value as given, or, for -pods all, the pods
// it stands for; an environment's teardown its -domain and an environment
// host delete its VM name (jobs.Inputs.Typed); everything else takes "yes".
func confirm(d deps, in jobs.Inputs) bool {
	want := "yes"
	if in.Kind.TypedConfirm() {
		want = in.Typed()
		fmt.Fprintf(d.stdout, "\nType %s (%s) to delete these VMs: ", in.TypedWhat(), want)
	} else {
		fmt.Fprint(d.stdout, "\nApply this plan? Type yes: ")
	}
	line, _ := bufio.NewReader(d.in).ReadString('\n')
	fmt.Fprintln(d.stdout)
	return strings.TrimSpace(line) == want && want != ""
}

// isAllTeams reports whether a -teams value is "all".
func isAllTeams(spec string) bool { return strings.EqualFold(strings.TrimSpace(spec), "all") }

// resolveAllTeams resolves -pods all to every pod with VMs on the cluster,
// as the grid shows them, returning a team range and a line saying so.
func resolveAllTeams(ctx context.Context, api pods.API, cfg config.Config) (spec, says string, err error) {
	vms, err := api.ClusterVMs(ctx)
	if err != nil {
		return "", "", err
	}
	all := pods.TeamsWithVMs(vms, pods.NewNaming(cfg.Naming))
	if len(all) == 0 {
		return "", "", errors.New("no pod has VMs, so there are no pods to act on")
	}
	spec = pods.FormatTeams(all)
	return spec, fmt.Sprintf("-pods all is %s: the pods with VMs.", spec), nil
}

func orRequired(err error) error {
	if err == nil {
		return fmt.Errorf("required")
	}
	return err
}
