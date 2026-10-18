package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
)

// formPage is the data of an operation's form.
type formPage struct {
	Op        operation
	In        jobs.Inputs
	HostsText string // In.Hosts as typed
	VMsText   string // In.VMs as typed; the field shows only when set
	Error     string // why the last preview couldn't be made
	AllTeams  string // every team that exists (see allTeamsSpec), as a hint
	Baseline  string // deploy.snapshot_name, which a deploy takes

	PowerActions []powerChoice // power
	TemplateSets []setView     // deploy: the master groups found
	SetsErr      string        // deploy: why the master groups couldn't be listed
	// OtherPattern is a deploy's pattern when it is none of the sets'.
	OtherPattern string
	// HostOpts are the host boxes: the grid's columns, or a deploy set's hosts.
	// HostsAll means ticking none means all.
	HostOpts []hostOpt
	HostsAll bool
	Step     *stepTwo // a two-step form's (operation.Pick)
	// FromGrid means the VMs were ticked on the grid: the form lists them instead
	// of asking for teams and hosts.
	FromGrid bool
	// NameMax and DescMax are the server's snapshot name/description limits;
	// NameMaxRest is NameMax less the first letter.
	NameMax, NameMaxRest, DescMax int
}

// hostOpt is a host to tick on a form.
type hostOpt struct {
	Name    string
	Checked bool
}

// hostOpts makes a box per name, ticked if in ticked; ticked names not in
// names are added.
func hostOpts(names, ticked []string) []hostOpt {
	var out []hostOpt
	for _, n := range names {
		out = append(out, hostOpt{Name: n, Checked: slices.Contains(ticked, n)})
	}
	for _, n := range ticked {
		if !slices.Contains(names, n) {
			out = append(out, hostOpt{Name: n, Checked: true})
		}
	}
	return out
}

// stepTwo is the second step of a two-step form (operation.Pick), once
// pods and hosts are chosen (HasPicked): what the cluster holds for them,
// and a choice among it.
type stepTwo struct {
	HasPicked bool
	Held      *heldView // what the pods hold, if the step lists it
	Legend    string    // the choice's: "Snapshot"
	Options   []stepOpt
	Hints     []string // under the choice
	Note      string   // after it, e.g. why the list is short
	// Button is the preview button's label; Danger marks it, with the
	// operation's icon, as destroying something.
	Button string
	Danger bool
}

// stepOpt is a choice of a stepTwo. Sans sets its label apart from names.
type stepOpt struct {
	Value, Label  string
	Checked, Sans bool
}

// heldView lists what a teardown's pods hold, set by set.
type heldView struct {
	Heading string // "In pods 01-32"
	Rows    []heldRow
	Empty   string // when there are none
	Err     string // why the pods couldn't be read
}

// heldRow is one set the pods hold: "tango.delta", "12 hosts in each of
// pods 01-32". Muted is the unknown set.
type heldRow struct {
	Label, Text string
	Muted       bool
}

// opForm serves an operation's form, prefilled from the query string ("change"
// links, retries) or, for reset and snapshot, from the grid's ticked VMs, and
// for a register, from its review's inputs.
func (s *Server) opForm(op operation) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if op.Formless {
			s.reviewPage(w, r, op, op.scope(formInputs(op.Kind, q)))
			return
		}
		if op.Kind.Teamless() {
			if r.Method == http.MethodPost {
				q = postedForm(r)
			}
			s.teamlessForm(w, r, http.StatusOK, op, op.scope(formInputs(op.Kind, q)), "")
			return
		}
		grid := false
		if r.Method == http.MethodPost {
			q = postedForm(r)
			grid = fromGrid(q)
		}
		if op.Kind == pods.KindDeploy && !q.Has(fieldBaseline) {
			q.Set(fieldBaseline, yes) // the baseline snapshot is on by default
		}
		in := formInputs(op.Kind, q)
		if grid {
			if len(in.VMs) == 0 {
				s.nothingTicked(w, r)
				return
			}
			in = s.selected(r.Context(), in)
		}
		p := s.newFormPage(r.Context(), op, in, q.Has(fieldTeams) || grid, op.Pick != "" && q.Has(op.Pick))
		p.FromGrid = grid
		s.render(w, r, http.StatusOK, "opform", s.newView(w, r, op.Title, op.nav(), p))
	})
}

// reviewPage is a formless operation's page, such as an environment's
// teardown: it has nothing to fill in, so it opens on its review.
func (s *Server) reviewPage(w http.ResponseWriter, r *http.Request, op operation, in jobs.Inputs) {
	if err := in.Validate(); err != nil {
		s.renderForm(w, r, http.StatusUnprocessableEntity, op, in, sentence(err.Error()))
		return
	}
	plan, err := jobs.BuildPlan(r.Context(), s.planner(r.Context()), in)
	if err != nil {
		s.renderForm(w, r, http.StatusUnprocessableEntity, op, in, "Couldn't make a plan: "+sentence(proxmox.Describe(err)))
		return
	}
	s.showPreview(w, r, http.StatusOK, op, in, plan, false, previewOptions{})
}

// newFormPage prepares op's form for in. On a two-step form, picked shows
// step 2 and chosen marks op.Pick as already given (even when "").
func (s *Server) newFormPage(ctx context.Context, op operation, in jobs.Inputs, picked, chosen bool) formPage {
	p := formPage{
		Op:        op,
		In:        in,
		HostsText: strings.Join(in.Hosts, ","),
		VMsText:   strings.Join(in.VMs, ","),
		AllTeams:  s.allTeamsSpec(ctx),
		Baseline:  s.cfg.Deploy.SnapshotName,

		NameMax:     pods.MaxSnapshotName,
		NameMaxRest: pods.MaxSnapshotName - 1,
		DescMax:     jobs.MaxSnapshotDescription,
	}
	switch op.Kind {
	case pods.KindPower:
		p.PowerActions = powerChoicesOf(pods.PowerActions)
	case pods.KindDeploy:
		p.TemplateSets, p.SetsErr = s.deploySets(ctx)
		inSet, hasSet := pods.SetOfPattern(in.Pattern)
		var chosen *setView
		for i := range p.TemplateSets {
			if p.TemplateSets[i].Pattern == in.Pattern || (hasSet && p.TemplateSets[i].Name == string(inSet)) || (in.Pattern == "" && i == 0) {
				p.TemplateSets[i].Checked = true
				chosen = &p.TemplateSets[i]
			}
		}
		if chosen == nil && in.Pattern != "" {
			p.OtherPattern = in.Pattern
		}
		if chosen != nil {
			ticked := in.Hosts
			if len(ticked) == 0 {
				ticked = chosen.Hosts // a deploy builds all of a set's hosts unless told otherwise
			}
			p.HostOpts = hostOpts(chosen.Hosts, ticked)
		} else {
			p.HostOpts, p.HostsAll = hostOpts(nil, in.Hosts), true
		}
	}
	if op.Pick != "" {
		p.Step = &stepTwo{}
		if teams, err := pods.ParseTeams(in.Teams); picked && err == nil {
			p.Step = s.stepTwo(ctx, op, in, teams, chosen)
			p.Step.HasPicked = true
		}
	}
	if op.Kind != pods.KindDeploy {
		p.HostOpts, p.HostsAll = hostOpts(s.view(ctx).Grid().Hosts, in.Hosts), true
	}
	return p
}

// renderForm shows the form again with a problem, keeping what was typed.
func (s *Server) renderForm(w http.ResponseWriter, r *http.Request, status int, op operation, in jobs.Inputs, problem string) {
	if op.Formless { // its review is its page
		s.errorPage(w, r, status, "Can't "+strings.ToLower(op.Title)+" "+in.Domain, problem, listPage(in), "Back to "+op.sectionName())
		return
	}
	if op.Kind.Teamless() {
		s.teamlessForm(w, r, status, op, in, problem)
		return
	}
	p := s.newFormPage(r.Context(), op, in, in.Teams != "", true)
	p.Error = problem
	s.render(w, r, status, "opform", s.newView(w, r, op.Title, op.nav(), p))
}

// deploySets lists the template sets on the viewer's grid, matching the grid's
// Deploy links.
func (s *Server) deploySets(ctx context.Context) ([]setView, string) {
	g := s.polledGrid(ctx)
	switch {
	case g.PolledAt.IsZero():
		return nil, "Couldn't list the master VMs: " + g.Err
	case len(g.Sets) == 0:
		return nil, "No master VMs tagged " + s.cfg.Deploy.MasterTag + " were found."
	}
	return s.startOf(g, false).Sets, ""
}

// stepTwo reads what op's second step offers for in's teams.
func (s *Server) stepTwo(ctx context.Context, op operation, in jobs.Inputs, teams []string, chosen bool) *stepTwo {
	if op.Kind == pods.KindTeardown {
		return s.setStep(ctx, in, teams, chosen)
	}
	return s.snapshotStep(ctx, in, teams)
}

// snapshotStep offers the snapshots of the first VM a reset of in covers
// (first team, first matching grid host). The per-VM baseline is always
// offered first, chosen unless in names another snapshot.
func (s *Server) snapshotStep(ctx context.Context, in jobs.Inputs, teams []string) *stepTwo {
	step := &stepTwo{Legend: "Snapshot", Button: "Preview"}
	var snaps []string
	var baseline string
	if len(in.VMs) > 0 {
		snaps, baseline = s.commonSnapshots(ctx, step, in.VMs)
	} else if team, host, ok := s.firstVM(ctx, teams, in.Hosts); ok {
		d, err := s.detail(ctx, team, host)
		step.Hints = append(step.Hints, "The snapshots of "+d.Cell.Name+", the first VM this covers.")
		switch {
		case err != nil:
			step.Note = "Couldn't read the snapshots of " + d.Cell.Name + ": " + proxmox.Describe(err) + ". Only the baseline is offered."
		default:
			snaps, baseline = d.Snapshots, d.Baseline
			if d.Baseline == "" {
				step.Note = d.Cell.Name + " has no baseline snapshot."
			}
		}
	} else {
		step.Note = "The grid shows no VM for those teams and hosts, so only the baseline is offered."
	}
	names := append([]string{""}, slices.DeleteFunc(slices.Clone(snaps), func(n string) bool {
		return n == baseline && n != in.Snapshot
	})...)
	if !slices.Contains(names, in.Snapshot) {
		names = append(names, in.Snapshot)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		o := stepOpt{Value: n, Label: n, Checked: n == in.Snapshot}
		if n == "" {
			o.Label = "baseline"
			if baseline != "" {
				o.Label += " (" + baseline + ")"
			}
		}
		step.Options = append(step.Options, o)
	}
	step.Hints = append(step.Hints, "baseline: "+pods.BaselineText(s.cfg.Deploy))
	return step
}

// pickerReads caps the VMs the picker reads for exact VMs: each read takes a
// cell-read slot, and VMs lacking the chosen snapshot show as blocked in the
// preview anyway.
const pickerReads = 4

// commonSnapshots reads up to pickerReads of the named grid VMs and returns
// the snapshots all have, in the first one's order, and their shared baseline
// if all were read and it is one snapshot. It records the source and any note
// on step.
func (s *Server) commonSnapshots(ctx context.Context, step *stepTwo, names []string) (_ []string, baseline string) {
	var cells []status.Cell
	for _, row := range s.view(ctx).Grid().Rows {
		for _, c := range row.Cells {
			if c.State != status.StateMissing && slices.Contains(names, c.Name) {
				cells = append(cells, c)
			}
		}
	}
	if len(cells) == 0 {
		step.Note = "The grid shows none of these VMs, so only the baseline is offered."
		return nil, ""
	}
	var common, read []string
	allBaseline := true
	for i, c := range cells[:min(len(cells), pickerReads)] {
		d, err := s.detail(ctx, c.Team, c.Host)
		if err != nil {
			step.Note = "Couldn't read the snapshots of " + c.Name + ": " + proxmox.Describe(err) + ". Only the baseline is offered."
			return nil, ""
		}
		read = append(read, c.Name)
		allBaseline = allBaseline && d.Baseline != ""
		if i == 0 {
			baseline = d.Baseline
		} else if d.Baseline != baseline {
			baseline = ""
		}
		if i == 0 {
			common = slices.Clone(d.Snapshots) // d is shared with others reading the cell
		} else {
			common = slices.DeleteFunc(common, func(n string) bool { return !slices.Contains(d.Snapshots, n) })
		}
	}
	if len(cells) > len(read) {
		baseline = "" // the unread VMs' baselines may be other snapshots
		step.Hints = append(step.Hints, fmt.Sprintf("The snapshots that %s, the first %d of the %d VMs, all have.", strings.Join(read, ", "), len(read), len(cells)))
	} else if len(read) == 1 {
		step.Hints = append(step.Hints, "The snapshots of "+read[0]+".")
	} else {
		step.Hints = append(step.Hints, "The snapshots that "+strings.Join(read, ", ")+" all have.")
	}
	if !allBaseline {
		step.Note = "Not all of them have a baseline snapshot."
	}
	return common, baseline
}

// firstVM is the first non-missing grid VM of teams whose host matches hosts
// (as the planner matches them).
func (s *Server) firstVM(ctx context.Context, teams, hosts []string) (team, host string, ok bool) {
	g := s.view(ctx).Grid()
	for _, t := range teams {
		for _, row := range g.Rows {
			if row.Team != t {
				continue
			}
			for _, c := range row.Cells {
				if c.State != status.StateMissing && pods.MatchesHost(c.Host, hosts) {
					return c.Team, c.Host, true
				}
			}
		}
	}
	return "", "", false
}
