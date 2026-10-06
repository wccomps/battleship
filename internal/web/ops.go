package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// operation is one of the things volunteers do to team VMs. Each has
// a form (GET Path), a preview (POST Path/preview) and a confirm (POST
// Path/confirm).
type operation struct {
	Kind  pods.Kind
	Path  string // e.g. "/reset"
	Title string // e.g. "Deploy": the form's heading, and the header button for deploy and teardown
	Noun  string // e.g. "reset", for the confirm's summary: "reset of team 01"
}

// operations are the operation pages, in order: the header shows deploy
// and teardown's buttons in this order.
var operations = []operation{
	{Kind: pods.KindPower, Path: "/power", Title: "Power", Noun: "power action"},
	{Kind: pods.KindReset, Path: "/reset", Title: "Reset", Noun: "reset"},
	{Kind: pods.KindSnapshot, Path: "/snapshot", Title: "Take snapshot", Noun: "snapshot"},
	{Kind: pods.KindDeploy, Path: "/deploy", Title: "Deploy", Noun: "deploy"},
	{Kind: pods.KindTeardown, Path: "/teardown", Title: "Teardown", Noun: "teardown"},
}

func operationFor(kind pods.Kind) (operation, bool) {
	for _, op := range operations {
		if op.Kind == kind {
			return op, true
		}
	}
	return operation{}, false
}

// Form field names. The VM page's quick forms and the confirm form's
// hidden fields use the same names, so every step reads inputs the same
// way (formInputs) and writes them the same way (inputFields).
const (
	fieldTeams       = "teams"
	fieldHosts       = "hosts"       // comma-separated
	fieldVMs         = "vms"         // comma-separated exact VM names, e.g. a retry's
	fieldPattern     = "pattern"     // deploy
	fieldRebuild     = "rebuild"     // deploy: "yes" rebuilds templates
	fieldBaseline    = "baseline"    // deploy: "yes" takes the baseline snapshot
	fieldSnapshot    = "snapshot"    // reset: the one to roll back to; snapshot: the new one's name
	fieldDescription = "description" // snapshot
	fieldVMState     = "vmstate"     // snapshot: "yes" saves the RAM of running VMs
	fieldAction      = "action"      // power
	fieldNonce       = "nonce"       // confirm: names the preview
	fieldFingerprint = "fingerprint"
	fieldTyped       = "typed"    // confirm: the team range, typed
	fieldRetryOf     = "retry_of" // confirm: the job a retry preview retries; shown only, never trusted
	fieldFrom        = "from"     // "grid": the grid's form, which sends ticked VMs and no teams
	fromGridValue    = "grid"
	yes              = "yes"
)

// formInputs reads an operation's inputs from form values. Values that
// don't belong to kind are ignored. A reset without a snapshot rolls each
// VM back to its own baseline (see pods.BaselineSnapshot). VMs may come as
// one comma-separated value (a confirm's hidden field) or one value per
// ticked box (the grid's form), or both.
func formInputs(kind pods.Kind, form url.Values) jobs.Inputs {
	in := jobs.Inputs{
		Kind:  kind,
		Teams: strings.TrimSpace(form.Get(fieldTeams)),
		Hosts: jobs.SplitList(strings.Join(form[fieldHosts], ",")),
		VMs:   jobs.SplitList(strings.Join(form[fieldVMs], ",")),
	}
	switch kind {
	case pods.KindDeploy:
		in.Pattern = strings.TrimSpace(form.Get(fieldPattern))
		in.Rebuild = form.Get(fieldRebuild) == yes
		in.NoSnapshot = form.Get(fieldBaseline) != yes
	case pods.KindReset:
		in.Snapshot = strings.TrimSpace(form.Get(fieldSnapshot))
	case pods.KindSnapshot:
		in.Snapshot = strings.TrimSpace(form.Get(fieldSnapshot))
		in.Description = strings.TrimSpace(form.Get(fieldDescription))
		in.VMState = form.Get(fieldVMState) == yes
	case pods.KindPower:
		in.Action = strings.TrimSpace(form.Get(fieldAction))
	}
	return in
}

// postedForm is the request's form body. A body that can't be parsed reads
// as empty, which fails validation.
func postedForm(r *http.Request) url.Values {
	if err := r.ParseForm(); err != nil {
		return url.Values{}
	}
	return r.PostForm
}

// field is a hidden form field.
type field struct{ Name, Value string }

// inputFields writes inputs as the form fields formInputs reads.
func inputFields(in jobs.Inputs) []field {
	fs := []field{{fieldTeams, in.Teams}}
	if len(in.Hosts) > 0 {
		fs = append(fs, field{fieldHosts, strings.Join(in.Hosts, ",")})
	}
	if len(in.VMs) > 0 {
		fs = append(fs, field{fieldVMs, strings.Join(in.VMs, ",")})
	}
	switch in.Kind {
	case pods.KindDeploy:
		fs = append(fs, field{fieldPattern, in.Pattern})
		if in.Rebuild {
			fs = append(fs, field{fieldRebuild, yes})
		}
		if !in.NoSnapshot {
			fs = append(fs, field{fieldBaseline, yes})
		}
	case pods.KindReset:
		if in.Snapshot != "" {
			fs = append(fs, field{fieldSnapshot, in.Snapshot})
		}
	case pods.KindSnapshot:
		fs = append(fs, field{fieldSnapshot, in.Snapshot})
		if in.Description != "" {
			fs = append(fs, field{fieldDescription, in.Description})
		}
		if in.VMState {
			fs = append(fs, field{fieldVMState, yes})
		}
	case pods.KindPower:
		fs = append(fs, field{fieldAction, in.Action})
	}
	return fs
}

// formQuery is the address of an operation's form filled in with in, for
// "change" links. A deploy's baseline box is written either way, since
// the form ticks it when the query doesn't say.
func formQuery(op operation, in jobs.Inputs) string {
	q := url.Values{}
	for _, f := range inputFields(in) {
		q.Set(f.Name, f.Value)
	}
	if in.Kind == pods.KindDeploy && in.NoSnapshot {
		q.Set(fieldBaseline, "no")
	}
	return op.Path + "?" + q.Encode()
}

// fromGrid reports whether form is the grid's selection form.
func fromGrid(form url.Values) bool { return form.Get(fieldFrom) == fromGridValue }

// selected completes the inputs of a grid selection, which names the
// ticked VMs but no teams: the teams and hosts are those of the VMs, and
// the VMs are sorted, as a retry's are. Only the grid's teams count (the
// teams with VMs), so a VM of any other team (an edited form) is left for
// BuildPlan's check to refuse, as is anything that isn't a team VM name.
// Teams typed into the form are kept as they are; BuildPlan refuses VMs
// outside them.
func (s *Server) selected(ctx context.Context, in jobs.Inputs) jobs.Inputs {
	in.VMs = slices.Compact(slices.Sorted(slices.Values(in.VMs)))
	if in.Teams != "" {
		return in
	}
	var teams, hosts []string
	grid := s.view(ctx).Teams()
	for _, name := range in.VMs {
		if team, host, ok := s.naming.ParseVMName(name); ok && slices.Contains(grid, team) {
			teams = append(teams, team)
			hosts = append(hosts, host)
		}
	}
	if len(teams) > 0 {
		in.Teams = pods.FormatTeams(teams)
		in.Hosts = slices.Compact(slices.Sorted(slices.Values(hosts)))
	}
	return in
}

// nothingTicked answers the grid's form posted with no VM ticked.
func (s *Server) nothingTicked(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusUnprocessableEntity, "No VMs selected",
		"Tick at least one VM on the grid, then choose what to do with it. Nothing has changed.", "/", "Back to the grid")
}

// actor is how jobs record a web user: the email address, which Authentik
// keeps unique, or the subject when there is none.
func actor(u auth.User) string {
	if u.Email != "" {
		return u.Email
	}
	return u.Subject
}
