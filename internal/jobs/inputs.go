// Package jobs turns confirmed requests into stored jobs and runs them:
// a job stores what the user asked for and a fingerprint of the plan they
// confirmed; the worker re-plans at run time and runs only if the plan still
// matches.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/wccomps/battleship/internal/pods"
)

// Inputs is what a user asked for. It is stored with the job and planned
// again when the job runs.
type Inputs struct {
	Kind       pods.Kind `json:"kind"`
	Teams      string    `json:"teams"` // as typed, e.g. "1-32"
	Hosts      []string  `json:"hosts,omitempty"`
	Pattern    string    `json:"pattern,omitempty"`     // deploy
	Rebuild    bool      `json:"rebuild,omitempty"`     // deploy
	NoSnapshot bool      `json:"no_snapshot,omitempty"` // deploy
	// Snapshot is a reset's snapshot to roll back to, empty for each VM's
	// baseline (pods.BaselineSnapshot), or a snapshot job's new snapshot.
	Snapshot    string `json:"snapshot,omitempty"`
	Description string `json:"description,omitempty"` // snapshot
	VMState     bool   `json:"vmstate,omitempty"`     // snapshot: also save the RAM of running VMs
	Action      string `json:"action,omitempty"`      // power
	// VMs, if set, are the exact team VMs to act on, by name, e.g. a
	// retry's failed VMs: the plan keeps only these of the VMs Teams and
	// Hosts select. Each must be a team VM name of one of Teams.
	VMs []string `json:"vms,omitempty"`
}

// MaxSnapshotDescription is the longest snapshot description, in
// characters, a job takes: a note, not a document.
const MaxSnapshotDescription = 500

// Validate checks the inputs without contacting Proxmox.
func (in Inputs) Validate() error {
	var errs []error
	switch in.Kind {
	case pods.KindDeploy:
		if strings.TrimSpace(in.Pattern) == "" {
			errs = append(errs, errors.New("deploy needs a template pattern"))
		}
	case pods.KindPower:
		if !pods.IsPowerAction(in.Action) {
			errs = append(errs, fmt.Errorf("power action must be start, shutdown, stop or reboot, not %q", in.Action))
		}
	case pods.KindSnapshot:
		if err := pods.CheckSnapshotName(in.Snapshot); err != nil {
			errs = append(errs, err)
		}
		if n := utf8.RuneCountInString(in.Description); n > MaxSnapshotDescription {
			errs = append(errs, fmt.Errorf("the snapshot description is too long: %d characters, at most %d", n, MaxSnapshotDescription))
		}
	case pods.KindTeardown, pods.KindReset:
	default:
		errs = append(errs, fmt.Errorf("unknown job kind %q", in.Kind))
	}
	if _, err := pods.ParseTeams(in.Teams); err != nil {
		errs = append(errs, fmt.Errorf("teams: %w", err))
	}
	for _, h := range in.Hosts {
		if strings.TrimSpace(h) == "" {
			errs = append(errs, errors.New("hosts must not contain blank entries"))
			break
		}
	}
	for _, v := range in.VMs {
		if strings.TrimSpace(v) == "" {
			errs = append(errs, errors.New("vms must not contain blank entries"))
			break
		}
	}
	return errors.Join(errs...)
}

// BuildPlan plans the inputs against the cluster's current state. With
// VMs set, it checks each is a team VM name of one of the teams, then
// keeps only those VMs of the plan (see keepVMs). With p.Access, it then
// blocks what the planning user lacks privileges for, so a preview and the
// job's own plan, made with the same person's privileges, agree.
func BuildPlan(ctx context.Context, p pods.Planner, in Inputs) (*pods.Plan, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	teams, _ := pods.ParseTeams(in.Teams)
	if err := checkVMs(in, teams, p.Naming); err != nil {
		return nil, err
	}
	plan, err := planFor(ctx, p, in, teams)
	if err != nil {
		return nil, err
	}
	keepVMs(plan, in.VMs)
	if p.Access != nil {
		if err := pods.BlockUnpermitted(ctx, plan, p.Access, p.Cfg); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func checkVMs(in Inputs, teams []string, naming pods.Naming) error {
	var errs []error
	for _, name := range in.VMs {
		team, _, ok := naming.ParseVMName(name)
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("vms: %q is not a team VM name", name))
		case !slices.Contains(teams, team):
			errs = append(errs, fmt.Errorf("vms: %s is not in teams %s", name, in.Teams))
		}
	}
	return errors.Join(errs...)
}

// keepVMs narrows plan to the items named in vms, if any, and a deploy's
// templates to those the kept items clone from. Plan teams stay as asked,
// so the job still locks every team of them.
func keepVMs(plan *pods.Plan, vms []string) {
	if len(vms) == 0 {
		return
	}
	var items []pods.Item
	used := map[string]bool{}
	for _, it := range plan.Items {
		if slices.Contains(vms, it.Name) {
			items = append(items, it)
			used[it.Template] = true
		}
	}
	plan.Items = items
	var templates []pods.TemplateSpec
	for _, t := range plan.Templates {
		if used[t.Name] {
			templates = append(templates, t)
		}
	}
	plan.Templates = templates
}

// MissingVMs lists, sorted, the names in in.VMs that plan has no item
// for: VMs that no longer exist or no longer match the teams and hosts.
func MissingVMs(in Inputs, plan *pods.Plan) []string {
	var out []string
	for _, name := range in.VMs {
		if !slices.ContainsFunc(plan.Items, func(it pods.Item) bool { return it.Name == name }) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func planFor(ctx context.Context, p pods.Planner, in Inputs, teams []string) (*pods.Plan, error) {
	switch in.Kind {
	case pods.KindDeploy:
		return p.Deploy(ctx, pods.DeployRequest{
			Pattern: in.Pattern, Teams: teams, Hosts: in.Hosts, Rebuild: in.Rebuild, Snapshot: !in.NoSnapshot,
		})
	case pods.KindTeardown:
		return p.Teardown(ctx, teams, in.Hosts)
	case pods.KindReset:
		return p.Reset(ctx, teams, in.Hosts, in.Snapshot)
	case pods.KindSnapshot:
		return p.Snapshot(ctx, pods.SnapshotRequest{
			Teams: teams, Hosts: in.Hosts, Name: in.Snapshot, Description: in.Description, VMState: in.VMState,
		})
	default: // KindPower; Validate rejected anything else
		return p.Power(ctx, teams, in.Hosts, in.Action)
	}
}

// SplitList splits a comma-separated list of hosts or VMs, as the command
// line's flags and the web forms take them, trimming spaces and dropping
// empty entries.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
