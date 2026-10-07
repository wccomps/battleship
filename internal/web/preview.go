package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// previewTTL is how long a preview may be confirmed; a later confirm re-reads
// and shows the plan again.
const previewTTL = 30 * time.Minute

// previewPage is a preview's data: what the operation would do, and the
// confirm form.
type previewPage struct {
	Op      operation
	Heading string  // the page's title, e.g. "Force stop"
	Title   opLabel // the sheet's title: the verb, e.g. Deploy kilo.alpha
	// Consequence says in one sentence what confirming does to running VMs. Calm
	// shows it as a note, not a warning, when nothing on the VMs changes (a
	// snapshot).
	Consequence string
	Calm        bool
	// AlreadyN VMs are already as the power action leaves them (e.g. 2 already
	// running); the job checks each VM first and skips those.
	AlreadyN    int
	AlreadyWord string // "running" or "stopped"
	Asked       []fact // the inputs worth a last look
	Templates   []templateRow
	Items       []itemRow // blocked ones first
	Runnable    int
	Blocked     int
	Warnings    []string
	// Capacity compares what the plan starts with the cluster's free resources;
	// nil if it starts nothing or the cluster couldn't be read.
	Capacity *capacityView
	Problem  *banner // why the preview is shown again, if it is
	Nothing  string  // why nothing can run; the page then has no confirm form
	Confirm  *confirmForm
	EditHref string // the form, filled in with these inputs; the grid for a grid selection
	FromGrid bool   // the VMs were ticked on the grid: Back closes the panel
	// Fields are the inputs as hidden fields: submitted by the confirm, and posted
	// back by Reselect.
	Fields []field
	// Reselect, for a reset or snapshot of grid-ticked VMs, labels a button that
	// posts them back to the operation's form to choose again.
	Reselect string
}

// capacityView is the preview's resource summary: a row per node memory or
// clone storage used, plus notices. Over-capacity warnings go in Warnings.
type capacityView struct {
	Rows    []capacityRow
	Notices []string // more than 90% used afterwards
	Notes   []string // e.g. linked clones grow
}

// capacityRow is one usage bar: Used and Need are widths in 5% steps (classes
// w0-w100, since the CSP forbids inline styles).
type capacityRow struct {
	Label string // "n1 memory", "competitions on n1"
	Level string // ok, near or over
	Used  int
	Need  int
	Text  string // "60 GiB used of 64 GiB, needs 12 GiB, 4 GiB free", as the CLI says it
}

// fact is a labelled value, e.g. "Teams: 1-3".
type fact struct{ Label, Value string }

// templateRow is a template a deploy uses.
type templateRow struct {
	Name, Node string
	State      string // "reuse", "create", "rebuild"
	Blocked    string
}

// itemRow is one VM of a plan.
type itemRow struct {
	Name, Node string
	Steps      string
	Blocked    string
}

// confirmForm is what the preview form sends to confirm the plan shown: its
// nonce and fingerprint, checked against the stored preview.
type confirmForm struct {
	Nonce       string
	Fingerprint string
	TypeTeams   string // if set, the team range to type to confirm
	RetryOf     int64  // the job this retries, so a preview shown again by the confirm still says so
	Button      string
	Danger      bool
}

// destructive reports whether in destroys or discards something, so the
// confirm button says so.
func destructive(in jobs.Inputs) bool {
	return in.Kind == pods.KindTeardown || in.Kind == pods.KindReset || (in.Kind == pods.KindPower && in.Action == "stop")
}

// previewOptions are the extras of a preview beyond the plan.
type previewOptions struct {
	problem  *banner
	nonce    string // reuse this preview's nonce instead of storing a new preview
	retryOf  int64
	fromGrid bool // the VMs were ticked on the grid: changing them is done there
	capacity *pods.Capacity
}

// opPreview plans the posted inputs and shows the preview.
func (s *Server) opPreview(op operation) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		form := postedForm(r)
		in := formInputs(op.Kind, form)
		if fromGrid(form) {
			if len(in.VMs) == 0 {
				s.nothingTicked(w, r)
				return
			}
			in = s.selected(r.Context(), in)
		}
		if err := in.Validate(); err != nil {
			s.renderForm(w, r, http.StatusUnprocessableEntity, op, in, sentence(err.Error()))
			return
		}
		plan, err := jobs.BuildPlan(r.Context(), s.planner(r.Context()), in)
		if err != nil {
			s.renderForm(w, r, http.StatusUnprocessableEntity, op, in, "Couldn't make a plan: "+sentence(proxmox.Describe(err)))
			return
		}
		allTeams := s.coversAllTeams(r.Context(), plan.Teams)
		s.showPreview(w, r, http.StatusOK, op, in, plan, allTeams, previewOptions{fromGrid: fromGrid(form)})
	})
}

// showPreview renders plan's preview. If anything can run, it adds a confirm
// form backed by a stored preview: new, or opts.nonce's when re-shown.
func (s *Server) showPreview(w http.ResponseWriter, r *http.Request, code int, op operation, in jobs.Inputs,
	plan *pods.Plan, allTeams bool, opts previewOptions) {
	if len(plan.Runnable()) > 0 {
		// Advice only, read fresh and never fingerprinted: capacity changes constantly
		// and mustn't make a preview stale.
		c, err := pods.ReadCapacity(r.Context(), s.apiFor(r.Context()), plan, s.cfg)
		if err != nil {
			s.logf("web: reading capacity for a preview: %v", err)
		}
		opts.capacity = c
	}
	p := s.newPreviewPage(r.Context(), op, in, plan, allTeams, opts)
	if p.Nothing == "" && p.Confirm == nil {
		fp := jobs.Fingerprint(plan)
		nonce := opts.nonce
		if nonce == "" {
			var err error
			if nonce, err = s.storePreview(r, in, fp); err != nil {
				s.logf("web: storing a preview: %v", err)
				s.errorPage(w, r, http.StatusInternalServerError, "Something went wrong",
					"Battleship couldn't save this preview, so it can't be confirmed. Try again in a moment.", op.Path, "Back to "+op.Title)
				return
			}
		}
		p.Confirm = &confirmForm{
			Nonce:       nonce,
			Fingerprint: fp,
			RetryOf:     opts.retryOf,
			Button:      confirmLabel(op, in, len(plan.Runnable())),
			Danger:      destructive(in),
		}
		if op.Kind.TypedConfirm() {
			p.Confirm.TypeTeams = in.Teams
		}
	}
	s.render(w, r, code, "preview", s.newView(w, r, p.Heading, string(op.Kind), p))
}

// storePreview records a preview for this session and returns its nonce.
func (s *Server) storePreview(r *http.Request, in jobs.Inputs, fingerprint string) (string, error) {
	nonce, err := newNonce()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	now := s.now()
	return nonce, s.st.CreatePreview(r.Context(), store.Preview{
		ID:          auth.HashToken(nonce),
		SessionID:   auth.SessionID(r.Context()),
		Kind:        string(in.Kind),
		Inputs:      raw,
		Fingerprint: fingerprint,
		CreatedAt:   now,
		ExpiresAt:   now.Add(previewTTL),
	})
}

// newNonce is a random preview nonce: 32 bytes, base64url.
func newNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("making a preview nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// confirmLabel is the confirm button's text, e.g. "Force stop 2 VMs".
func confirmLabel(op operation, in jobs.Inputs, n int) string {
	vms := "1 VM"
	if n != 1 {
		vms = strconv.Itoa(n) + " VMs"
	}
	switch op.Kind {
	case pods.KindPower:
		return powerLabel(in.Action) + " " + vms
	case pods.KindTeardown:
		return "Delete " + vms
	case pods.KindSnapshot:
		return "Snapshot " + vms
	default:
		return op.Title + " " + vms
	}
}

// previewTitle is a preview's title, e.g. Force stop, Deploy kilo.alpha.
func previewTitle(op operation, in jobs.Inputs) opLabel {
	return opLabelOf(string(op.Kind), in)
}

// newPreviewPage describes plan for people.
func (s *Server) newPreviewPage(ctx context.Context, op operation, in jobs.Inputs, plan *pods.Plan, allTeams bool, opts previewOptions) previewPage {
	p := previewPage{
		Op:       op,
		Title:    previewTitle(op, in),
		Asked:    previewFacts(in, opts.fromGrid, opts.retryOf),
		Runnable: len(plan.Runnable()),
		Problem:  opts.problem,
		EditHref: formQuery(op, in),
		FromGrid: opts.fromGrid,
		Fields:   inputFields(in),
	}
	p.Heading = p.Title.Label
	if opts.fromGrid {
		p.EditHref = "/"
		switch op.Kind {
		case pods.KindReset:
			p.Reselect = "Change snapshot"
		case pods.KindSnapshot:
			p.Reselect = "Change"
		}
	}
	p.Blocked = len(plan.Items) - p.Runnable
	for _, it := range plan.Items {
		row := itemRow{Name: it.Name, Node: orDashString(it.Node), Steps: stepsText(it), Blocked: it.Blocked}
		if op.Kind == pods.KindPower {
			row.Steps = strings.ToLower(powerLabel(in.Action)) // its one step, "power", by name
		}
		p.Items = append(p.Items, row)
	}
	slices.SortStableFunc(p.Items, func(a, b itemRow) int {
		switch {
		case a.Blocked != "" && b.Blocked == "":
			return -1
		case a.Blocked == "" && b.Blocked != "":
			return 1
		}
		return 0
	})
	var rebuilt, stopped []string
	for _, t := range plan.Templates {
		row := templateRow{Name: t.Name, Node: orDashString(t.Node), State: "create", Blocked: t.Blocked}
		switch {
		case t.Rebuild:
			row.State = "rebuild"
			if t.Blocked == "" {
				rebuilt = append(rebuilt, t.Name)
			}
		case t.Exists:
			row.State = "reuse"
		}
		if t.WillStopMaster {
			row.State += " · stops its master"
			stopped = append(stopped, t.MasterName)
		}
		p.Templates = append(p.Templates, row)
	}
	switch len(rebuilt) {
	case 0:
	case 1:
		p.Warnings = append(p.Warnings, fmt.Sprintf("Template %s will be deleted and built again from its master.", rebuilt[0]))
	default:
		p.Warnings = append(p.Warnings, fmt.Sprintf("%d templates will be deleted and built again from their masters.", len(rebuilt)))
	}
	switch len(stopped) {
	case 0:
	case 1:
		p.Warnings = append(p.Warnings, fmt.Sprintf("Building its template stops master %s, which is running, until the copy finishes.", stopped[0]))
	default:
		p.Warnings = append(p.Warnings, fmt.Sprintf("Building the templates stops %d running masters, each until its copy finishes.", len(stopped)))
	}
	if allTeams {
		p.Warnings = append(p.Warnings, s.everyTeamNote(ctx))
	}
	if missing := jobs.MissingVMs(in, plan); len(missing) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%d of the %d VMs asked for by name no longer exist or match the teams and hosts, so they are left out: %s.",
			len(missing), len(in.VMs), strings.Join(missing, ", ")))
	}

	switch {
	case len(plan.Items) == 0:
		p.Nothing = "No team VMs match these teams and hosts."
		if op.Kind == pods.KindDeploy {
			p.Nothing = "No masters match this template set and these hosts."
		}
	case p.Runnable == 0:
		p.Nothing = "Every VM is blocked: nothing can run."
	}
	if p.Nothing != "" {
		p.Warnings = nil // there is nothing to confirm
	} else {
		p.Capacity = capacityOf(opts.capacity, &p.Warnings)
		p.Consequence = consequence(op.Kind, in, plan.Runnable())
		p.Calm = op.Kind == pods.KindSnapshot
		if op.Kind == pods.KindPower {
			p.AlreadyN, p.AlreadyWord = s.alreadyDone(ctx, in.Action, plan.Runnable())
		}
	}
	return p
}

// previewFacts are the inputs a preview recaps that the title and table don't.
func previewFacts(in jobs.Inputs, fromGrid bool, retryOf int64) []fact {
	var fs []fact
	if !fromGrid {
		fs = append(fs, fact{"Teams", teamsText(in.Teams)})
		if len(in.Hosts) > 0 || in.Kind == pods.KindDeploy || in.Kind == pods.KindTeardown {
			fs = append(fs, fact{"Hosts", hostsText(in.Hosts)})
		}
		if len(in.VMs) > 0 {
			fs = append(fs, fact{"Only these VMs", strings.Join(in.VMs, ", ")})
		}
	}
	fs = append(fs, askedFacts(in)...)
	if retryOf > 0 {
		fs = append(fs, fact{"Retry of", "#" + strconv.FormatInt(retryOf, 10)})
	}
	return fs
}

// askedFacts are the inputs beyond teams and hosts that the title (opLabelOf)
// doesn't say.
func askedFacts(in jobs.Inputs) []fact {
	var fs []fact
	switch in.Kind {
	case pods.KindDeploy:
		fs = append(fs, fact{"Rebuild templates", onOff(in.Rebuild)}, fact{"Baseline snapshot", onOff(!in.NoSnapshot)})
	case pods.KindReset:
		if in.Snapshot == "" {
			fs = append(fs, fact{"Snapshot", "baseline"})
		}
	case pods.KindSnapshot:
		if in.Description != "" {
			fs = append(fs, fact{"Description", in.Description})
		}
		fs = append(fs, fact{"Include RAM", onOff(in.VMState)})
	}
	return fs
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// capacityOf describes c for the page, appending a warning per usage that
// would go over. It is nil if c lists nothing.
func capacityOf(c *pods.Capacity, warnings *[]string) *capacityView {
	if c == nil || len(c.Usage)+len(c.Notes) == 0 {
		return nil
	}
	v := &capacityView{Notes: c.Notes}
	over := false
	for _, w := range c.Warnings() {
		if w.Over {
			*warnings = append(*warnings, w.Text+".")
			over = true
		} else {
			v.Notices = append(v.Notices, w.Text)
		}
	}
	if over {
		*warnings = append(*warnings, "You can still confirm if the overcommit is deliberate.")
	}
	for _, u := range c.Usage {
		row := capacityRow{Label: u.Label(), Level: "ok",
			Text: fmt.Sprintf("%s used of %s, needs %s, %s free", pods.GiB(u.Used), pods.GiB(u.Total), pods.GiB(u.Needed), pods.GiB(u.Free()))}
		if u.Kind == pods.UsageMemory {
			row.Label += " memory"
		}
		switch {
		case u.Over():
			row.Level = "over"
		case u.Near():
			row.Level = "near"
		}
		row.Used = step5(u.Used, u.Total)
		row.Need = min(step5(u.Needed, u.Total), 100-row.Used)
		if row.Need == 0 && u.Needed > 0 && row.Used < 100 {
			row.Need = 5 // small, but there
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

// step5 is part/total as a percentage, rounded to a step of 5, capped at 100.
func step5(part, total int64) int {
	if total <= 0 || part <= 0 {
		return 0
	}
	pct := int((part*20 + total/2) / total * 5)
	return min(pct, 100)
}

// alreadyDone counts the items the grid shows already in the power action's
// end state (and says which: running or stopped). The job skips those, so the
// preview says so.
func (s *Server) alreadyDone(ctx context.Context, action string, items []pods.Item) (int, string) {
	want := ""
	if a, ok := pods.PowerActionOf(action); ok {
		want = a.Leaves
	}
	if want == "" {
		return 0, ""
	}
	names := map[string]bool{}
	for _, it := range items {
		names[it.Name] = true
	}
	n := 0
	for _, row := range s.view(ctx).Grid().Rows {
		for _, c := range row.Cells {
			if names[c.Name] && c.Power == want {
				n++
			}
		}
	}
	return n, want
}

// consequence says in one sentence what confirming does to running VMs, e.g.
// "2 VMs lose power at once. Unsaved work is lost."
func consequence(kind pods.Kind, in jobs.Inputs, items []pods.Item) string {
	n := len(items)
	vms := countVMs(n)
	one := n == 1
	pick := func(many, single string) string {
		if one {
			return single
		}
		return many
	}
	switch kind {
	case pods.KindPower:
		switch in.Action {
		case "start":
			return vms + " will be turned on."
		case "shutdown":
			return vms + pick(" will be asked to shut down by their operating systems.", " will be asked to shut down by its operating system.")
		case "stop":
			return vms + pick(" lose power at once. Unsaved work is lost.", " loses power at once. Unsaved work is lost.")
		case "reboot":
			return vms + pick(" will be asked to restart by their operating systems.", " will be asked to restart by its operating system.")
		}
		return vms + " get the power action " + in.Action + "."
	case pods.KindReset:
		if in.Snapshot == "" {
			return vms + pick(" roll back to their baseline snapshots and start. Work since then is lost.",
				" rolls back to its baseline snapshot and starts. Work since then is lost.")
		}
		return vms + pick(" roll back to snapshot ", " rolls back to snapshot ") + in.Snapshot + pick(" and start. Work since then is lost.", " and starts. Work since then is lost.")
	case pods.KindDeploy:
		var nodes []string
		for _, it := range items {
			if it.Node != "" && !slices.Contains(nodes, it.Node) {
				nodes = append(nodes, it.Node)
			}
		}
		slices.Sort(nodes)
		on := ""
		if len(nodes) > 0 {
			on = " on " + strings.Join(nodes, ", ")
		}
		return vms + " will be created or repaired from " + strings.TrimPrefix(in.Pattern, "*.") + on + "."
	case pods.KindTeardown:
		return vms + pick(" will be stopped and deleted with their disks. This can't be undone.", " will be stopped and deleted with its disks. This can't be undone.")
	case pods.KindSnapshot:
		msg := vms + pick(" get a new snapshot, ", " gets a new snapshot, ") + in.Snapshot + "."
		if in.VMState {
			msg += pick(" Their RAM is saved too if they are running, which takes longer.", " Its RAM is saved too if it is running, which takes longer.")
		}
		return msg
	}
	return ""
}

// powerLabels are the power actions' names on the forms.
var powerLabels = map[string]string{
	"start":    "Start",
	"shutdown": "Shut down",
	"stop":     "Force stop",
	"reboot":   "Reboot",
}

// powerLabel is a power action's name on the forms, e.g. "Force stop".
func powerLabel(action string) string {
	if l, ok := powerLabels[action]; ok {
		return l
	}
	return action
}

// powerChoice is a power action as a form offers it.
type powerChoice struct {
	Value string // jobs.Inputs.Action, and the icon's name
	Label string
}

// powerChoicesOf are actions as forms offer them, in the same order.
func powerChoicesOf(actions []pods.PowerAction) []powerChoice {
	out := make([]powerChoice, len(actions))
	for i, a := range actions {
		out[i] = powerChoice{Value: a.Value, Label: powerLabel(a.Value)}
	}
	return out
}

// stepsText lists an item's steps, e.g. "stop · rollback · start".
func stepsText(it pods.Item) string {
	parts := make([]string, len(it.Steps))
	for i, st := range it.Steps {
		parts[i] = string(st)
		if st == pods.StepRollback && it.Snapshot != "" {
			parts[i] += " to " + pods.SnapshotLabel(it)
		}
		if st == pods.StepSnapshot && it.VMState {
			parts[i] += " with RAM"
		}
	}
	return strings.Join(parts, " · ")
}

// everyTeamNote notes that a plan covers every team: which teams have VMs, or
// that none does yet.
func (s *Server) everyTeamNote(ctx context.Context) string {
	all := s.view(ctx).Teams()
	if len(all) == 0 {
		return "No team has VMs on the grid, so this counts as every team."
	}
	return fmt.Sprintf("This covers every team (%s).", teamsText(pods.FormatTeams(all)))
}

// teamsText shows a team range with two-digit teams: "1-3,7" reads "01-03, 07".
// An unparsable range is shown as is.
func teamsText(spec string) string {
	teams, err := pods.ParseTeams(spec)
	if err != nil || len(teams) == 0 {
		return spec
	}
	var parts []string
	for _, r := range pods.TeamRanges(teams) {
		if r[0] == r[1] {
			parts = append(parts, pods.FormatTeam(r[0]))
		} else {
			parts = append(parts, pods.FormatTeam(r[0])+"-"+pods.FormatTeam(r[1]))
		}
	}
	return strings.Join(parts, ", ")
}

func countVMs(n int) string {
	if n == 1 {
		return "1 VM"
	}
	return strconv.Itoa(n) + " VMs"
}

// orDash shows "-" for a VM that doesn't exist yet and has no VMID.
func orDash(vmid int) string {
	if vmid == 0 {
		return "-"
	}
	return strconv.Itoa(vmid)
}

func orDashString(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
