package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
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
// snapshot. With a database, an applied plan runs here as a job (see runAsJob).
func runOp(ctx context.Context, cmd string, args []string, d deps) int {
	stdout, stderr := d.stdout, d.stderr
	fs := flag.NewFlagSet("battleship "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "battleship.toml", "config file")
	teamSpec := fs.String("teams", "", "teams, e.g. 7, 1-32 or 1,3,5, or all: every team with VMs (required)")
	hostSpec := fs.String("hosts", "", "comma-separated host filter, e.g. dc,web")
	vmSpec := fs.String("vms", "", "comma-separated exact VM names to act on, e.g. team01-dc,team02-web (to retry just those)")
	yes := fs.Bool("yes", false, "apply the plan without asking")
	queue := fs.Bool("queue", false, "store the confirmed plan as a job for a worker instead of running it here")
	in := jobs.Inputs{Kind: pods.Kind(cmd)}
	switch cmd {
	case "deploy":
		fs.StringVar(&in.Pattern, "templates", "", "master VM name pattern, e.g. '*.kilo.alpha' (required)")
		fs.BoolVar(&in.Rebuild, "rebuild", false, "recreate templates from masters")
		fs.BoolVar(&in.NoSnapshot, "no-snapshot", false, "skip the baseline snapshot")
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
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q (put flags before any other words)\n", fs.Arg(0))
		return 2
	}
	all := isAllTeams(*teamSpec)
	if all && cmd == "deploy" {
		fmt.Fprintln(stderr, "-teams all is for teardown, power, reset and snapshot; name the teams to deploy, e.g. 1-32")
		return 2
	}
	if _, err := pods.ParseTeams(*teamSpec); !all && (*teamSpec == "" || err != nil) {
		fmt.Fprintf(stderr, "-teams: %v\n", orRequired(err))
		return 2
	}
	in.Teams = *teamSpec
	in.Hosts = jobs.SplitList(*hostSpec)
	in.VMs = jobs.SplitList(*vmSpec)
	hostsSet, vmsSet := false, false
	fs.Visit(func(f *flag.Flag) {
		hostsSet = hostsSet || f.Name == "hosts"
		vmsSet = vmsSet || f.Name == "vms"
	})
	if hostsSet && len(in.Hosts) == 0 {
		fmt.Fprintln(stderr, "-hosts is empty")
		return 2
	}
	if vmsSet && len(in.VMs) == 0 {
		fmt.Fprintln(stderr, "-vms is empty")
		return 2
	}
	switch cmd {
	case "deploy":
		if in.Pattern == "" {
			fmt.Fprintln(stderr, "-templates is required")
			return 2
		}
	case "power":
		if !pods.IsPowerAction(in.Action) {
			fmt.Fprintf(stderr, "-action must be start, shutdown, stop or reboot (got %q)\n", in.Action)
			return 2
		}
	case "snapshot":
		if in.Snapshot == "" {
			fmt.Fprintln(stderr, "-name is required")
			return 2
		}
		check := in
		if all {
			check.Teams = "0" // stands in until the cluster is read
		}
		if err := check.Validate(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *queue {
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
			fmt.Fprintln(stderr, "-teams all:", proxmox.Describe(err))
			return 1
		}
		fmt.Fprintln(stdout, says)
		fmt.Fprintln(stdout)
		in.Teams = spec
	}
	plan, err := jobs.BuildPlan(ctx, pods.NewPlanner(api, cfg), in)
	if err != nil {
		fmt.Fprintln(stderr, "planning:", proxmox.Describe(err))
		return 1
	}

	printPlan(stdout, pods.NewNaming(cfg.Naming), plan)
	if pods.NeedsCapacity(plan) {
		// Advice only: overcommit may be deliberate, so it never stops the plan.
		if c, err := pods.ReadCapacity(ctx, api, plan, cfg); err != nil {
			fmt.Fprintln(stdout, "\nCapacity: couldn't read the cluster's resources:", proxmox.Describe(err))
		} else {
			printCapacity(stdout, c)
		}
	}
	if missing := jobs.MissingVMs(in, plan); len(missing) > 0 {
		fmt.Fprintf(stdout, "\n%d of the %d -vms no longer exist or match -teams/-hosts, so they are left out: %s\n",
			len(missing), len(in.VMs), strings.Join(missing, ", "))
	}
	blocked := len(plan.Items) - len(plan.Runnable())
	if blocked > 0 {
		fmt.Fprintf(stdout, "\n%d VMs blocked (see reasons above).\n", blocked)
	}
	if len(plan.Items) == 0 {
		if len(in.Hosts) > 0 && cmd != "deploy" {
			fmt.Fprintf(stdout, "\nNo VMs matched -hosts %s for teams %s; check the host names.\n",
				strings.Join(in.Hosts, ","), in.Teams)
			return 1
		}
		fmt.Fprintln(stdout, "\nNothing to do.")
		return 0
	}
	if len(plan.Runnable()) == 0 {
		fmt.Fprintln(stdout, "\nNothing can run.")
		return 1
	}
	if !*yes {
		if !d.interactive {
			fmt.Fprintln(stdout, "\nNothing changed. Re-run with -yes to re-plan and apply.")
			return 0
		}
		if !confirm(d, cmd, in.Teams) || ctx.Err() != nil {
			fmt.Fprintln(stdout, "Cancelled; nothing changed.")
			return 1
		}
	}

	if *queue {
		return queueJob(ctx, d, cfg, in, plan, token)
	}
	if cfg.Database.URL != "" {
		if err := cfg.RequireDatabase(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return runAsJob(ctx, d, cfg, client, in, plan, token)
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

func queueJob(ctx context.Context, d deps, cfg config.Config, in jobs.Inputs, plan *pods.Plan, token proxmox.Credential) int {
	_, _, id, closeStore, err := submitJob(ctx, d, cfg, in, plan, token)
	if err != nil {
		fmt.Fprintln(d.stderr, "-queue:", err)
		return 1
	}
	closeStore()
	fmt.Fprintf(d.stdout, "Queued job %d. A worker (battleship worker) runs it; follow it with: battleship jobs show %d\n", id, id)
	return 0
}

// submitJob stores a job for plan with the user's token sealed, and returns
// the open store (closeStore closes it), the key and the job's ID. On an
// error nothing is left open.
func submitJob(ctx context.Context, d deps, cfg config.Config, in jobs.Inputs, plan *pods.Plan, token proxmox.Credential) (st *store.Store, creds jobs.Credentials, id int64, closeStore func(), err error) {
	if err = cfg.RequireSealKey(); err != nil {
		return nil, creds, 0, nil, err
	}
	if st, closeStore, err = d.openStore(ctx, cfg.Database); err != nil {
		return nil, creds, 0, nil, err
	}
	if creds, err = jobs.OpenCredentials(ctx, cfg, st); err == nil {
		id, err = jobs.Submit(ctx, st, in, plan, jobs.Submitter{User: d.user, Credential: token, Seal: creds})
	}
	if err != nil {
		closeStore()
		return nil, creds, 0, nil, err
	}
	return st, creds, id, closeStore, nil
}

// confirm asks the volunteer to approve the plan just printed. Teardown
// requires typing the -teams value as given, or, for -teams all, the teams
// it stands for; everything else takes "yes".
func confirm(d deps, cmd, teamSpec string) bool {
	want := "yes"
	if pods.Kind(cmd).TypedConfirm() {
		want = strings.TrimSpace(teamSpec)
		fmt.Fprintf(d.stdout, "\nType the team range (%s) to delete these VMs: ", want)
	} else {
		fmt.Fprint(d.stdout, "\nApply this plan? Type yes: ")
	}
	line, _ := bufio.NewReader(d.in).ReadString('\n')
	fmt.Fprintln(d.stdout)
	return strings.TrimSpace(line) == want && want != ""
}

// isAllTeams reports whether a -teams value is "all".
func isAllTeams(spec string) bool { return strings.EqualFold(strings.TrimSpace(spec), "all") }

// resolveAllTeams resolves -teams all to every team with VMs on the cluster,
// as the grid shows them, returning a team range and a line saying so.
func resolveAllTeams(ctx context.Context, api pods.API, cfg config.Config) (spec, says string, err error) {
	vms, err := api.ClusterVMs(ctx)
	if err != nil {
		return "", "", err
	}
	all := pods.TeamsWithVMs(vms, pods.NewNaming(cfg.Naming))
	if len(all) == 0 {
		return "", "", errors.New("no team has VMs, so there are no teams to act on")
	}
	spec = pods.FormatTeams(all)
	return spec, fmt.Sprintf("-teams all is %s: the teams with VMs.", spec), nil
}

func orRequired(err error) error {
	if err == nil {
		return fmt.Errorf("required")
	}
	return err
}
