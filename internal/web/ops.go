package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// operation is something volunteers do to team VMs: a form (GET Path), a
// preview (POST Path/preview) and a confirm (POST Path/confirm).
type operation struct {
	Kind  pods.Kind
	Path  string // e.g. "/reset"
	Title string // e.g. "Deploy": the form's heading
	Noun  string // e.g. "reset", for the confirm's summary: "reset of team 01"
	Lead  string // the header button of deploy and teardown, which act on whole teams
	// domain is the environment an operation on one (its Path has {domain})
	// was opened for: see at.
	domain string
}

// operations are the operation pages; the header shows deploy and teardown in
// this order.
var operations = []operation{
	{Kind: pods.KindPower, Path: "/power", Title: "Power", Noun: "power action"},
	{Kind: pods.KindReset, Path: "/reset", Title: "Reset", Noun: "reset"},
	{Kind: pods.KindSnapshot, Path: "/snapshot", Title: "Take snapshot", Noun: "snapshot"},
	{Kind: pods.KindDeploy, Path: "/deploy", Title: "Deploy", Noun: "deploy", Lead: "Deploy teams"},
	{Kind: pods.KindTeardown, Path: "/teardown", Title: "Teardown", Noun: "teardown", Lead: "Tear down teams"},
	{Kind: pods.KindEnvCreate, Path: "/environments/new", Title: "Create environment", Noun: "environment"},
	{Kind: pods.KindEnvAdd, Path: "/environments/{domain}/add", Title: "Add hosts", Noun: "new hosts"},
	{Kind: pods.KindEnvDNS, Path: "/environments/{domain}/dns", Title: "Set DNS", Noun: "DNS change"},
	{Kind: pods.KindEnvTeardown, Path: "/environments/{domain}/teardown", Title: "Tear down", Noun: "teardown"},
	{Kind: pods.KindImageRegister, Path: "/images/register", Title: "Register base image", Noun: "registration"},
	{Kind: pods.KindImageEdit, Path: "/images/edit", Title: "Open for editing", Noun: "working copy"},
	{Kind: pods.KindImageDeprecate, Path: "/images/deprecate", Title: "Deprecate base image", Noun: "deprecation"},
	{Kind: pods.KindImageDelete, Path: "/images/delete", Title: "Delete base image", Noun: "deletion"},
}

// page reports whether the operation opens as its own page rather than in
// the grid's side panel: deploy and teardown act on whole teams, not on VMs
// picked in the grid, and their previews are long. These are the header's
// buttons (leadOps) and keep job.html's "Preview again" out of the panel;
// the grid's and VM page's Deploy links leave out data-panel-link for it.
// Image and environment operations have nothing to pick in the grid.
func (o operation) page() bool {
	return o.Kind == pods.KindDeploy || o.Kind == pods.KindTeardown || o.Kind.Teamless()
}

// at is an operation on one environment (pods.Kind.OnEnvironment, its Path
// /environments/{domain}/...) opened for domain, a checked domain
// (pods.CheckDomain): its paths name it, and it overrides any domain its
// forms post.
func (o operation) at(domain string) operation {
	if o.Kind.OnEnvironment() && o.domain == "" {
		o.Path, o.domain = strings.Replace(o.Path, "{domain}", domain, 1), domain
	}
	return o
}

// scope fixes in's domain to the one the operation was opened for.
func (o operation) scope(in jobs.Inputs) jobs.Inputs {
	if o.domain != "" {
		in.Domain = o.domain
	}
	return in
}

func operationFor(kind pods.Kind) (operation, bool) {
	for _, op := range operations {
		if op.Kind == kind {
			return op, true
		}
	}
	return operation{}, false
}

// Form field names, shared by the VM page's quick forms and the confirm's
// hidden fields, so every step reads (formInputs) and writes (inputFields)
// inputs the same way.
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
	fieldTyped       = "typed"    // confirm: the team range or domain, typed (jobs.Inputs.Typed)
	fieldRetryOf     = "retry_of" // confirm: the job a retry preview retries; shown only, never trusted
	fieldFrom        = "from"     // "grid": the grid's form, which sends ticked VMs and no teams
	fieldDomain      = "domain"
	fieldSlot        = "slot"      // env-create, env-teardown: the slot's two digits
	fieldRouter      = "router"    // name=family
	fieldEnvHosts    = "env_hosts" // one name=family[:octet] per line
	fieldDNS         = "dns"
	fieldVMID        = "vmid"
	fieldFamily      = "family"
	fieldImageKind   = "image_kind"
	fieldImage       = "image"
	// The environment forms' router and host rows (envForm), read into
	// Router and EnvHosts when present.
	fieldRouterName  = "router_name"
	fieldRouterImage = "router_image"
	fieldHostName    = "host_name"
	fieldHostImage   = "host_image"
	fieldHostOctet   = "host_octet"
	fieldMore        = "more" // asks the form again with more host rows (blankRows)
	fromGridValue    = "grid"
	yes              = "yes"
)

// formInputs reads an operation's inputs from form values, ignoring those
// that don't belong to kind. A reset without a snapshot uses each VM's own
// baseline (pods.BaselineSnapshot). VMs may come comma-separated (a confirm's
// hidden field), one per ticked box (the grid), or both.
func formInputs(kind pods.Kind, form url.Values) jobs.Inputs {
	if kind.Teamless() {
		in := jobs.Inputs{Kind: kind, Domain: strings.TrimSpace(form.Get(fieldDomain)), Router: strings.TrimSpace(form.Get(fieldRouter)),
			DNS: strings.TrimSpace(form.Get(fieldDNS)), Family: strings.TrimSpace(form.Get(fieldFamily)),
			ImageKind: strings.TrimSpace(form.Get(fieldImageKind)), Image: strings.TrimSpace(form.Get(fieldImage)),
			Slot: strings.TrimSpace(form.Get(fieldSlot)), VMs: jobs.SplitList(strings.Join(form[fieldVMs], ","))}
		for _, line := range strings.Split(form.Get(fieldEnvHosts), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				in.EnvHosts = append(in.EnvHosts, line)
			}
		}
		if form.Has(fieldRouterImage) {
			in.Router = ""
			if r := rawRow(form.Get(fieldRouterName), form.Get(fieldRouterImage), ""); r.Image != "" {
				in.Router = r.line()
			}
		}
		if form.Has(fieldHostName) {
			in.EnvHosts = nil
			for _, row := range postedRows(form) {
				if line := row.line(); line != "" {
					in.EnvHosts = append(in.EnvHosts, line)
				}
			}
		}
		in.VMID, _ = strconv.Atoi(strings.TrimSpace(form.Get(fieldVMID)))
		return in
	}
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

// postedForm is the request's form body; an unparsable body reads as empty
// and fails validation.
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
	if in.Kind.Teamless() {
		var fs []field
		for _, f := range []field{{fieldDomain, in.Domain}, {fieldSlot, in.Slot}, {fieldVMs, strings.Join(in.VMs, ",")}, {fieldRouter, in.Router}, {fieldEnvHosts, strings.Join(in.EnvHosts, "\n")},
			{fieldDNS, in.DNS}, {fieldFamily, in.Family}, {fieldImageKind, in.ImageKind}, {fieldImage, in.Image}} {
			if f.Value != "" {
				fs = append(fs, f)
			}
		}
		if in.VMID != 0 {
			fs = append(fs, field{fieldVMID, strconv.Itoa(in.VMID)})
		}
		return fs
	}
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

// Normalize is in as the form reads it back from a page that posts it:
// values trimmed, lists split and blanks dropped. The CLI passes its flags
// through it, so a command asks for the same job the form would.
func Normalize(in jobs.Inputs) jobs.Inputs {
	form := url.Values{}
	for _, f := range inputFields(in) {
		form.Add(f.Name, f.Value)
	}
	return formInputs(in.Kind, form)
}

// formQuery is an operation's form URL prefilled with in, for "change" links.
// A deploy's baseline box is always written, since the form ticks it when the
// query is silent. An environment's form has its domain in the path; without
// a valid one, it is the Environments page.
func formQuery(op operation, in jobs.Inputs) string {
	if op.Kind.OnEnvironment() && op.domain == "" {
		if pods.CheckDomain(in.Domain) != nil {
			return envsPath
		}
		op = op.at(in.Domain)
	}
	q := url.Values{}
	for _, f := range inputFields(in) {
		if f.Name != fieldDomain || !op.Kind.OnEnvironment() {
			q.Set(f.Name, f.Value)
		}
	}
	if in.Kind == pods.KindDeploy && in.NoSnapshot {
		q.Set(fieldBaseline, "no")
	}
	return op.Path + "?" + q.Encode()
}

// fromGrid reports whether form is the grid's selection form.
func fromGrid(form url.Values) bool { return form.Get(fieldFrom) == fromGridValue }

// selected completes a grid selection's inputs: teams and hosts from the
// ticked VMs, VMs sorted as a retry's are. Only grid teams count, so VMs of
// other teams (an edited form) or non-team names are left for BuildPlan to
// refuse. Typed teams are kept; BuildPlan refuses VMs outside them.
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

// actor is how jobs record a web user: the email (Authentik keeps it unique),
// or the subject when there is none.
func actor(u auth.User) string {
	if u.Email != "" {
		return u.Email
	}
	return u.Subject
}

// envsPath is the Environments page; envHref is an environment's block on it.
const envsPath = "/environments"

func envHref(domain string) string { return envsPath + "#" + domain }

// listPage is the page a job with inputs in belongs to, where closing its
// form, preview or job page goes: Images, Environments at the environment
// changed, or the grid.
func listPage(in jobs.Inputs) string {
	switch in.Kind {
	case pods.KindImageRegister, pods.KindImageEdit, pods.KindImageDeprecate, pods.KindImageDelete:
		return "/images"
	case pods.KindEnvAdd, pods.KindEnvDNS, pods.KindEnvTeardown:
		if pods.CheckDomain(in.Domain) == nil {
			return envHref(in.Domain)
		}
		return envsPath
	case pods.KindEnvCreate:
		return envsPath
	}
	return "/"
}

// nth is vs[i], or "" past its end.
func nth(vs []string, i int) string {
	if i < len(vs) {
		return vs[i]
	}
	return ""
}
