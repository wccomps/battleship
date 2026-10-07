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
	Picker   *snapshotPicker
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

// snapshotPicker is the reset form's step 2: snapshots read from the reset's
// first VM or, for exact VMs, those they have in common.
type snapshotPicker struct {
	Source  string         // where the list comes from, e.g. "The snapshots of team01-dc, the first VM this covers."
	Options []snapshotView // the first, with no name, is each VM's own baseline
	// BaselineText explains the baseline option, e.g. "initial, else the newest
	// fresh_clone_*".
	BaselineText string
	// BaselineIs is the snapshot that is every source VM's baseline, if it is one
	// snapshot; it isn't offered again under its own name.
	BaselineIs string
	Selected   string // "" is the baseline
	Note       string // e.g. why the list is short
	HasPicked  bool   // teams (and hosts) were chosen, so step 2 shows
}

// opForm serves an operation's form, prefilled from the query string ("change"
// links, retries) or, for reset and snapshot, from the grid's ticked VMs.
func (s *Server) opForm(op operation) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
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
		p := s.newFormPage(r.Context(), op, in, q.Has(fieldTeams) || grid)
		p.FromGrid = grid
		s.render(w, r, http.StatusOK, "opform", s.newView(w, r, op.Title, string(op.Kind), p))
	})
}

// newFormPage prepares an operation's form for in. picked means the reset
// form's first step was submitted, so snapshots are listed.
func (s *Server) newFormPage(ctx context.Context, op operation, in jobs.Inputs, picked bool) formPage {
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
		var chosen *setView
		for i := range p.TemplateSets {
			if p.TemplateSets[i].Pattern == in.Pattern || (in.Pattern == "" && i == 0) {
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
	case pods.KindReset:
		p.Picker = s.snapshotPicker(ctx, in, picked)
	}
	if op.Kind != pods.KindDeploy {
		p.HostOpts, p.HostsAll = hostOpts(s.view(ctx).Grid().Hosts, in.Hosts), true
	}
	return p
}

// renderForm shows the form again with a problem, keeping what was typed.
func (s *Server) renderForm(w http.ResponseWriter, r *http.Request, status int, op operation, in jobs.Inputs, problem string) {
	p := s.newFormPage(r.Context(), op, in, in.Teams != "")
	p.Error = problem
	s.render(w, r, status, "opform", s.newView(w, r, op.Title, string(op.Kind), p))
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

// snapshotPicker reads the snapshots of the first VM a reset of in covers
// (first team, first matching grid host). The per-VM baseline is always
// offered first, preselected unless in names another snapshot.
func (s *Server) snapshotPicker(ctx context.Context, in jobs.Inputs, picked bool) *snapshotPicker {
	p := &snapshotPicker{HasPicked: picked, Selected: in.Snapshot, BaselineText: pods.BaselineText(s.cfg.Deploy)}
	if !picked {
		return p
	}
	teams, err := pods.ParseTeams(in.Teams)
	if err != nil {
		p.HasPicked = false
		return p
	}
	var snaps []string
	if len(in.VMs) > 0 {
		snaps, p.BaselineIs = s.commonSnapshots(ctx, p, in.VMs)
	} else if team, host, ok := s.firstVM(ctx, teams, in.Hosts); ok {
		d, err := s.detail(ctx, team, host)
		p.Source = "The snapshots of " + d.Cell.Name + ", the first VM this covers."
		switch {
		case err != nil:
			p.Note = "Couldn't read the snapshots of " + d.Cell.Name + ": " + proxmox.Describe(err) + ". Only the baseline is offered."
		default:
			snaps = d.Snapshots
			p.BaselineIs = d.Baseline
			if d.Baseline == "" {
				p.Note = d.Cell.Name + " has no baseline snapshot."
			}
		}
	} else {
		p.Note = "The grid shows no VM for those teams and hosts, so only the baseline is offered."
	}
	names := append([]string{""}, slices.DeleteFunc(slices.Clone(snaps), func(n string) bool {
		return n == p.BaselineIs && n != p.Selected
	})...)
	if !slices.Contains(names, p.Selected) {
		names = append(names, p.Selected)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			p.Options = append(p.Options, snapshotView{Name: n, Baseline: n == ""})
		}
	}
	return p
}

// pickerReads caps the VMs the picker reads for exact VMs: each read takes a
// cell-read slot, and VMs lacking the chosen snapshot show as blocked in the
// preview anyway.
const pickerReads = 4

// commonSnapshots reads up to pickerReads of the named grid VMs and returns
// the snapshots all have, in the first one's order. It records the source in
// p, and their shared baseline if all were read and it is one snapshot.
func (s *Server) commonSnapshots(ctx context.Context, p *snapshotPicker, names []string) (_ []string, baseline string) {
	var cells []status.Cell
	for _, row := range s.view(ctx).Grid().Rows {
		for _, c := range row.Cells {
			if c.State != status.StateMissing && slices.Contains(names, c.Name) {
				cells = append(cells, c)
			}
		}
	}
	if len(cells) == 0 {
		p.Note = "The grid shows none of these VMs, so only the baseline is offered."
		return nil, ""
	}
	var common, read []string
	allBaseline := true
	for i, c := range cells[:min(len(cells), pickerReads)] {
		d, err := s.detail(ctx, c.Team, c.Host)
		if err != nil {
			p.Note = "Couldn't read the snapshots of " + c.Name + ": " + proxmox.Describe(err) + ". Only the baseline is offered."
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
		p.Source = fmt.Sprintf("The snapshots that %s, the first %d of the %d VMs, all have.", strings.Join(read, ", "), len(read), len(cells))
	} else if len(read) == 1 {
		p.Source = "The snapshots of " + read[0] + "."
	} else {
		p.Source = "The snapshots that " + strings.Join(read, ", ") + " all have."
	}
	if !allBaseline {
		p.Note = "Not all of them have a baseline snapshot."
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
