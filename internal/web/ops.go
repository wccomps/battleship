package web

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// operation is something volunteers do to team VMs, environments or base
// images: a form (GET Path), a preview (POST Path/preview) and a confirm
// (POST Path/confirm).
type operation struct {
	Kind  pods.Kind
	Path  string // e.g. "/reset"
	Title string // e.g. "Deploy": the form's heading
	// Label names its jobs where Title doesn't read as one: "Reset to
	// snapshot", not "Reset".
	Label string
	// Verb is its review's heading verb ("Create", "Tear down"); ConfirmVerb
	// is a single-item image operation's confirm button verb ("Copy as").
	Verb, ConfirmVerb string
	Icon              string // its icon (opIcon)
	Noun              string // e.g. "reset", for the confirm's summary: "reset of pod 01"
	Lead              string // the header button of deploy and teardown, which act on whole teams
	// Section is the page an image or environment operation belongs to:
	// its nav entry, and where its pages close to (listPage).
	Section string
	// Doing names it as what a privilege line says the user can't do:
	// "Adding hosts needs …" (lacksLine).
	Doing string
	// Done is how a VM its job finished reads; "" is "Done".
	Done string
	// Machines is whether its form has machine rows (envForm); Formless,
	// whether it has no form, and opens on its review; PostForm, whether
	// POST Path opens the form (from the grid's ticked VMs, or a register
	// review's "Edit details").
	Machines, Formless, PostForm bool
	// Pick is the field a two-step form's second step (step) chooses,
	// once pods and hosts are chosen: a reset's snapshot, a teardown's set.
	Pick    string
	review  func(operation, jobs.Inputs, *pods.Plan, config.LAN) *reviewView
	home    func(jobs.Inputs) (name, href string)
	back    func(operation, jobs.Inputs) string
	prefill func(*Server, context.Context, *catalogPage)
	step    func(*Server, context.Context, jobs.Inputs, []string, bool) *stepTwo
	// domain is the environment an operation on one (its Path has {domain})
	// was opened for: see at.
	domain string
}

const (
	sectionImages       = "images"
	sectionEnvironments = "environments"
)

// operations are the operation pages; the header shows deploy and teardown in
// this order.
var operations = []operation{
	{Kind: pods.KindPower, Path: "/power", Title: "Power", Icon: "shutdown", Noun: "power action"},
	{Kind: pods.KindReset, Path: "/reset", Title: "Reset", Label: "Reset to snapshot", Icon: "reset", Noun: "reset",
		Pick: fieldSnapshot, PostForm: true, step: (*Server).snapshotStep},
	{Kind: pods.KindSnapshot, Path: "/snapshot", Title: "Take snapshot", Icon: "snapshot", Noun: "snapshot", PostForm: true},
	{Kind: pods.KindDeploy, Path: "/deploy", Title: "Deploy", Icon: "deploy", Noun: "deploy", Lead: "Deploy pods", Done: "Ready"},
	{Kind: pods.KindTeardown, Path: "/teardown", Title: "Teardown", Verb: "Tear down", Icon: "teardown", Noun: "teardown",
		Lead: "Tear down pods", Done: "Deleted", Pick: fieldSet, review: podTeardownReview, step: (*Server).setStep},
	{Kind: pods.KindEnvCreate, Path: "/environments/new", Title: "Create environment", Verb: "Create", Icon: "deploy", Noun: "environment",
		Section: sectionEnvironments, Doing: "Creating an environment", Done: "Ready", Machines: true, review: envReview, home: envHome},
	{Kind: pods.KindEnvAdd, Path: "/environments/{domain}/add", Title: "Add hosts", Verb: "Add hosts to", Icon: "deploy", Noun: "new hosts",
		Section: sectionEnvironments, Doing: "Adding hosts", Done: "Ready", Machines: true, review: envReview, home: envHome},
	{Kind: pods.KindEnvTeardown, Path: "/environments/{domain}/teardown", Title: "Tear down", Verb: "Tear down", Icon: "teardown", Noun: "teardown",
		Section: sectionEnvironments, Doing: "Tearing down an environment", Done: "Deleted", Formless: true,
		review: teardownReview, home: envHome, back: (operation).listPage},
	{Kind: pods.KindEnvEdit, Path: "/environments/edit", Title: "Edit host", Verb: "Edit", ConfirmVerb: "Save",
		Icon: "edit", Noun: "host edit", Section: sectionEnvironments, Doing: "Editing a host", Done: "Saved",
		review: envEditReview, home: envHome, prefill: (*Server).hostEditPrefill},
	{Kind: pods.KindEnvPower, Path: "/environments/power", Title: "Power", Verb: "Power", Icon: "shutdown", Noun: "power action",
		Section: sectionEnvironments, Doing: "Changing power", Formless: true,
		review: envPowerReview, home: envHome, back: envHostBack},
	{Kind: pods.KindEnvDeleteHost, Path: "/environments/delete", Title: "Delete host", Verb: "Delete", ConfirmVerb: "Delete",
		Icon: "teardown", Noun: "host deletion", Section: sectionEnvironments, Doing: "Deleting a host", Done: "Deleted", Formless: true,
		review: envDeleteHostReview, home: envHome, back: envHostBack},
	{Kind: pods.KindImageRegister, Path: "/images/register", Title: "Register base image", Verb: "Register", ConfirmVerb: "Register",
		Icon: "deploy", Noun: "registration", Section: sectionImages, Doing: "Registering", Done: "Ready", PostForm: true,
		review: registerReview, home: familyHome, back: (operation).listPage, prefill: (*Server).registerPrefill},
	{Kind: pods.KindImageEdit, Path: "/images/edit", Title: "Open for editing", Verb: "Edit", ConfirmVerb: "Copy as",
		Icon: "deploy", Noun: "working copy", Section: sectionImages, Done: "Ready",
		review: imageReview, home: familyHome, back: toHome},
	{Kind: pods.KindImageDeprecate, Path: "/images/deprecate", Title: "Deprecate base image", Label: "Deprecate", Verb: "Deprecate", ConfirmVerb: "Deprecate",
		Icon: "reset", Noun: "deprecation", Section: sectionImages, Doing: "Deprecating",
		review: imageReview, home: familyHome, back: toHome},
	{Kind: pods.KindImageDelete, Path: "/images/delete", Title: "Delete base image", Verb: "Delete", ConfirmVerb: "Delete",
		Icon: "teardown", Noun: "deletion", Section: sectionImages, Doing: "Deleting", Done: "Deleted",
		review: imageReview, home: familyHome, back: toHome},
	{Kind: pods.KindImageDetails, Path: "/images/details", Title: "Edit notes", Verb: "Edit notes of", ConfirmVerb: "Save notes of",
		Icon: "edit", Noun: "notes change", Section: sectionImages, Doing: "Editing notes", Done: "Saved",
		review: detailsReview, home: familyHome, back: toHome, prefill: (*Server).notesPrefill},
}

// nav is the nav entry its pages mark current: its section, or else its
// kind, which marks the header's Lead button for deploy and teardown.
func (o operation) nav() string { return cmp.Or(o.Section, string(o.Kind)) }

// sectionName is its section's name, as the nav says it.
func (o operation) sectionName() string {
	for _, n := range nav(&auth.User{}) {
		if n.Key == o.Section {
			return n.Label
		}
	}
	return ""
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
	fieldSet         = "set"         // teardown: its set, "" for everything
	fieldRebuild     = "rebuild"     // deploy: "yes" rebuilds templates
	fieldBaseline    = "baseline"    // deploy: "yes" takes the baseline snapshot
	fieldSnapshot    = "snapshot"    // reset: the one to roll back to; snapshot: the new one's name
	fieldDescription = "description" // snapshot
	fieldVMState     = "vmstate"     // snapshot: "yes" saves the RAM of running VMs
	fieldAction      = "action"      // power
	fieldNonce       = "nonce"       // confirm: names the preview
	fieldFingerprint = "fingerprint"
	fieldTyped       = "typed"    // confirm: the team range, domain or VM name, typed (jobs.Inputs.Typed)
	fieldRetryOf     = "retry_of" // confirm: the job a retry preview retries; shown only, never trusted
	fieldFrom        = "from"     // "grid": the grid's form, which sends ticked VMs and no teams
	fieldDomain      = "domain"
	fieldSlot        = "slot"        // env-create, env-teardown: the slot's two digits
	fieldRouter      = "router"      // name=family
	fieldEnvHosts    = "env_hosts"   // one name=family[:octet] per line
	fieldEnvHost     = "env_host"    // env-edit, env-delete-host: host label or full VM name
	fieldOctet       = "octet"       // env-edit: LAN octet, or "none" (pods.ClearOctet)
	fieldEjectCDROM  = "eject_cdrom" // env-edit: "yes" ejects attached CD-ROM media
	fieldCPU         = "cpu"         // env-edit: custom CPU model name
	clearOctetValue  = "none"
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
	// The details fields (detailsFields). fieldDetails is yes, or
	// detailsWithNotes when the free text comes too, which a server from
	// before Edit notes refuses rather than drop the text.
	fieldDetails     = "details"
	detailsWithNotes = "with-notes"
	fieldOS          = "detail_os"
	fieldUser        = "detail_user"
	fieldPassword    = "detail_password"
	fieldBuilt       = "detail_built"
	fieldGeneralized = "detail_generalized"
	fieldInstalled   = "detail_installed"
	fieldLicence     = "detail_licence"
	fieldNotes       = "notes"
	fieldNotesFrom   = "notes_from" // the notes an Edit notes form opened on (pods.ImageNotes.From)
	fromGridValue    = "grid"
	yes              = "yes"
)

// formInputs reads an operation's inputs from form values, ignoring those
// that don't belong to kind. A reset without a snapshot uses each VM's own
// baseline (pods.BaselineSnapshot). VMs may come comma-separated (a confirm's
// hidden field), one per ticked box (the grid), or both.
func formInputs(kind pods.Kind, form url.Values) jobs.Inputs {
	if kind.Teamless() {
		in := jobs.Inputs{
			Kind:       kind,
			Domain:     form.Get(fieldDomain),
			Slot:       form.Get(fieldSlot),
			Router:     form.Get(fieldRouter),
			EnvHosts:   []string{form.Get(fieldEnvHosts)},
			EnvHost:    form.Get(fieldEnvHost),
			EjectCDROM: form.Get(fieldEjectCDROM) == yes,
			CPU:        form.Get(fieldCPU),
			Action:     form.Get(fieldAction),
			VMs:        form[fieldVMs],
			ImageRequest: pods.ImageRequest{
				Family:    form.Get(fieldFamily),
				ImageKind: form.Get(fieldImageKind),
				Image:     form.Get(fieldImage),
			},
		}
		if (kind == pods.KindEnvCreate || kind == pods.KindEnvAdd) && form.Has(fieldRouterImage) {
			in.Router = ""
			if r := rawRow(form.Get(fieldRouterName), form.Get(fieldRouterImage), ""); r.Image != "" {
				in.Router = r.line()
			}
		}
		if (kind == pods.KindEnvCreate || kind == pods.KindEnvAdd) && form.Has(fieldHostName) {
			in.EnvHosts = nil
			for _, row := range postedRows(form) {
				if line := row.line(); line != "" {
					in.EnvHosts = append(in.EnvHosts, line)
				}
			}
		}
		if s := strings.TrimPrefix(strings.TrimSpace(form.Get(fieldOctet)), "."); strings.EqualFold(s, clearOctetValue) {
			in.Octet = pods.ClearOctet
		} else {
			in.Octet, _ = strconv.Atoi(s)
		}
		in.VMID, _ = strconv.Atoi(strings.TrimSpace(form.Get(fieldVMID)))
		if posted := form.Get(fieldDetails); posted == yes || posted == detailsWithNotes {
			n := pods.ImageNotes{ImageDetails: pods.ImageDetails{OS: form.Get(fieldOS), User: form.Get(fieldUser), Password: form.Get(fieldPassword),
				Built: form.Get(fieldBuilt), Generalized: form.Get(fieldGeneralized), Installed: form.Get(fieldInstalled),
				Licence: form.Get(fieldLicence)}}
			if posted == detailsWithNotes {
				free := form.Get(fieldNotes)
				n.Free = &free
			}
			n.From = form.Get(fieldNotesFrom)
			in.Details = &n
		}
		return in.Normalize()
	}
	in := jobs.Inputs{
		Kind:        kind,
		Teams:       form.Get(fieldTeams),
		Hosts:       form[fieldHosts],
		VMs:         form[fieldVMs],
		Pattern:     form.Get(fieldPattern),
		Rebuild:     form.Get(fieldRebuild) == yes,
		NoSnapshot:  form.Get(fieldBaseline) != yes,
		Set:         pods.Set(form.Get(fieldSet)),
		Snapshot:    form.Get(fieldSnapshot),
		Description: form.Get(fieldDescription),
		VMState:     form.Get(fieldVMState) == yes,
		Action:      form.Get(fieldAction),
	}
	return in.Normalize()
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
			{fieldEnvHost, in.EnvHost}, {fieldCPU, in.CPU}, {fieldAction, in.Action}, {fieldFamily, in.Family}, {fieldImageKind, in.ImageKind}, {fieldImage, in.Image}} {
			if f.Value != "" {
				fs = append(fs, f)
			}
		}
		if in.Octet != 0 {
			fs = append(fs, field{fieldOctet, strconv.Itoa(in.Octet)})
		}
		if in.EjectCDROM {
			fs = append(fs, field{fieldEjectCDROM, yes})
		}
		if in.VMID != 0 {
			fs = append(fs, field{fieldVMID, strconv.Itoa(in.VMID)})
		}
		return append(fs, detailsFields(in.Details)...)
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
	case pods.KindTeardown:
		// Always, "" too: a "Change" link then has everything chosen.
		fs = append(fs, field{fieldSet, string(in.Set)})
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

// detailsFields are details as the form fields formInputs reads, for
// register's "Edit details" post-back, and fieldDetails, which says
// details were given even when none is set.
func detailsFields(d *pods.ImageNotes) []field {
	if d == nil {
		return nil
	}
	fs := []field{{fieldDetails, yes}}
	for _, f := range []field{{fieldOS, d.OS}, {fieldUser, d.User}, {fieldPassword, d.Password}, {fieldBuilt, d.Built},
		{fieldGeneralized, d.Generalized}, {fieldInstalled, d.Installed}, {fieldLicence, d.Licence}} {
		if f.Value != "" {
			fs = append(fs, f)
		}
	}
	return fs
}

// formQuery is an operation's form URL prefilled with in, for "change"
// links. A deploy's baseline box is always written, since the form ticks it
// when the query is silent. Details and notes are left out, a password and
// long text kept out of addresses: the form reads them again. An
// environment's form has its domain in the path; without a valid one, it
// is the Environments page.
func formQuery(op operation, in jobs.Inputs) string {
	if op.Kind.OnEnvironment() && op.domain == "" {
		if pods.CheckDomain(in.Domain) != nil {
			return envsPath
		}
		op = op.at(in.Domain)
	}
	q := url.Values{}
	in.Details = nil
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

// envHome is the environment in acts on and its block on Environments, or
// envsPath when in names no valid domain.
func envHome(in jobs.Inputs) (string, string) {
	if pods.CheckDomain(in.Domain) == nil {
		return in.Domain, envHref(in.Domain)
	}
	return "", envsPath
}

// familyHome is the image family in acts on and its page, or "/images"
// when in names no valid family.
func familyHome(in jobs.Inputs) (string, string) {
	f := cmp.Or(in.Family, familyOf(in.Image))
	if pods.CheckFamily(f) == nil {
		return f, familyHref(f)
	}
	return "", "/images"
}

func familyOf(image string) string {
	f, _, _ := pods.ParseImageName(image)
	return f
}

func toHome(op operation, in jobs.Inputs) string {
	_, href := op.home(in)
	return href
}

// listPage is the page a job of o with inputs in belongs to, where closing
// its form, preview or job page goes: its section (Images, or Environments
// at the environment it acted on), else the grid.
func (o operation) listPage(in jobs.Inputs) string {
	switch o.Section {
	case sectionImages:
		return "/images"
	case sectionEnvironments:
		if o.Kind.OnEnvironment() {
			_, href := envHome(in)
			return href
		}
		return envsPath
	}
	return "/"
}

// result is the page a finished job of o with inputs in shows its work
// on: its environment or image family; nil for a team job, and for a
// teardown, whose work is gone.
func (o operation) result(in jobs.Inputs) *link {
	if o.home == nil || o.Kind.TearsDown() {
		return nil
	}
	if name, href := o.home(in); name != "" {
		return &link{href, "Open " + name}
	}
	return nil
}

func nth(vs []string, i int) string {
	if i < len(vs) {
		return vs[i]
	}
	return ""
}
