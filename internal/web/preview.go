package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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
	// shows it as a note, not a warning, when the operation isn't destructive.
	Consequence string
	Calm        bool
	// AlreadyN VMs are already as the power action leaves them (e.g. 2 already
	// running); the job checks each VM first and skips those.
	AlreadyN        int
	AlreadyWord     string // "running" or "stopped"
	Asked           []fact // the inputs worth a last look
	Templates       []templateRow
	TemplatesOpen   bool      // any template is blocked
	TemplateSummary string    // e.g. "13 to rebuild"
	Items           []itemRow // blocked ones first
	ItemsFold       bool      // fold the VM table behind a summary when nothing is blocked
	ItemsSummary    string    // e.g. "429 VMs"
	Blocked         int
	Warnings        []string
	// Capacity compares what the plan starts with the cluster's free resources;
	// nil if it starts nothing or the cluster couldn't be read.
	Capacity *capacityView
	Problem  *banner // why the preview is shown again, if it is
	Nothing  string  // why nothing can run; the page then has no confirm form
	Confirm  *confirmForm
	EditHref string // the form, filled in with these inputs; the grid for a grid selection
	// CloseHref is where closing it goes: the form for a grid operation,
	// else the page the operation belongs to.
	CloseHref string
	FromGrid  bool // the VMs were ticked on the grid: Back closes the panel
	// Fields are the inputs as hidden fields: submitted by the confirm, and posted
	// back by Reselect.
	Fields []field
	// Reselect, for a reset or snapshot of grid-ticked VMs, labels a button that
	// posts them back to the operation's form to choose again.
	Reselect string
	// Review, if set, replaces the facts, consequence and table: an image,
	// environment or pods teardown operation's review.
	Review *reviewView
}

// capacityView is the preview's resource summary: a row per node memory or
// clone storage used, plus notes.
type capacityView struct {
	Rows    []capacityRow
	Tight   bool     // any row is near or over capacity
	Summary string   // short summary when all rows fit
	Notes   []string // e.g. nodes without Sys.Audit
}

// capacityRow is one usage bar: Used and Need are widths in 5% steps (classes
// w0-w100, since the CSP forbids inline styles).
type capacityRow struct {
	Label string // "n1 memory", "competitions on n1"
	Level string // ok, near or over
	Used  int
	Need  int
	Text  string // "60 GiB used of 64 GiB, needs 12 GiB, 4 GiB free"
}

// fact is a labelled value, e.g. "Pods: 1-3".
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
// nonce (checked against the stored preview), what to type to confirm, and
// display fields if the preview is shown again.
type confirmForm struct {
	Nonce       string
	Fingerprint string
	// Typed, if set, is what to type to confirm (jobs.Inputs.Typed); TypedRE is
	// it as the input's pattern.
	Typed   string
	TypedRE string
	RetryOf int64 // the job this retries, so a preview shown again by the confirm still says so
	Button  string
	Danger  bool
}

func destructive(in jobs.Inputs, items []pods.Item) bool {
	return in.Kind.TearsDown() || in.Kind == pods.KindReset || ((in.Kind == pods.KindPower || in.Kind == pods.KindEnvPower) && in.Action == "stop") ||
		in.Kind == pods.KindImageDelete || slices.ContainsFunc(items, func(it pods.Item) bool { return slices.Contains(it.Steps, pods.StepDelete) })
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
		in := op.scope(formInputs(op.Kind, form))
		if machineInputs(op, form) {
			// Add host without JavaScript, or Review: the form again, with
			// more rows or what is wrong with the fields.
			p, ok := s.catalogForm(r, op, in, "")
			switch {
			case !ok:
				s.noEnv(w, r, in.Domain)
				return
			case form.Get(fieldMore) == yes:
				s.renderCatalogForm(w, r, http.StatusOK, p)
				return
			case p.checkFields():
				s.renderCatalogForm(w, r, http.StatusUnprocessableEntity, p)
				return
			}
		}
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
			s.planRefused(w, r, op, in, err)
			return
		}
		s.showPreview(w, r, http.StatusOK, op, in, plan, previewOptions{fromGrid: fromGrid(form)})
	})
}

// planRefused shows op's form again with why planning in failed: on the
// field it names, for a pods.EnvProblem on a form with machine rows.
func (s *Server) planRefused(w http.ResponseWriter, r *http.Request, op operation, in jobs.Inputs, err error) {
	var pr pods.EnvProblem
	if errors.As(err, &pr) && op.Machines {
		if p, ok := s.catalogForm(r, op, in, ""); ok {
			_, rows := p.request()
			p.mark(pr, rows)
			p.marked()
			s.renderCatalogForm(w, r, http.StatusUnprocessableEntity, p)
			return
		}
	}
	s.renderForm(w, r, http.StatusUnprocessableEntity, op, in, "Couldn't make a plan: "+sentence(proxmox.Describe(err)))
}

func (s *Server) showPreview(w http.ResponseWriter, r *http.Request, code int, op operation, in jobs.Inputs,
	plan *pods.Plan, opts previewOptions) {
	if len(plan.Runnable()) > 0 {
		// Advice only, read fresh and never fingerprinted: capacity changes constantly
		// and mustn't make a preview stale.
		c, err := pods.ReadCapacity(r.Context(), s.apiFor(r.Context()), plan, s.cfg)
		if err != nil {
			s.logf("web: reading capacity for a preview: %v", err)
		}
		opts.capacity = c
	}
	p := s.newPreviewPage(r.Context(), op, in, plan, opts)
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
			Button:      confirmLabel(op, in, plan.Runnable()),
			Danger:      destructive(in, plan.Runnable()),
		}
		if op.Kind.TypedConfirm() {
			p.Confirm.Typed = in.Typed()
			p.Confirm.TypedRE = regexp.QuoteMeta(p.Confirm.Typed)
		}
	}
	s.render(w, r, code, "preview", s.newView(w, r, p.Heading, op.nav(), p))
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

// confirmLabel is the confirm button's text, e.g. "Force stop 2 VMs" or
// "Register teak-v2".
func confirmLabel(op operation, in jobs.Inputs, runnable []pods.Item) string {
	n := len(runnable)
	if n == 1 && op.ConfirmVerb != "" {
		return op.ConfirmVerb + " " + runnable[0].Name
	}
	vms := countVMs(n)
	switch op.Kind {
	case pods.KindEnvCreate:
		return "Create " + vms
	case pods.KindEnvAdd:
		return "Add " + strconv.Itoa(n) + byCount(n, " host", " hosts")
	case pods.KindPower:
		return powerLabel(in.Action) + " " + vms
	case pods.KindEnvPower:
		if n == 1 {
			return powerLabel(in.Action) + " " + runnable[0].Name
		}
		return powerLabel(in.Action) + " " + vms
	case pods.KindTeardown:
		return "Delete " + vms
	case pods.KindEnvTeardown:
		return "Tear down " + in.Domain
	case pods.KindSnapshot:
		return "Snapshot " + vms
	default:
		return op.Title + " " + vms
	}
}

func (s *Server) newPreviewPage(ctx context.Context, op operation, in jobs.Inputs, plan *pods.Plan, opts previewOptions) previewPage {
	runnable := len(plan.Runnable())
	p := previewPage{
		Op:        op,
		Title:     opLabelOf(string(op.Kind), in),
		Asked:     previewFacts(in, opts.fromGrid, opts.retryOf),
		Problem:   opts.problem,
		EditHref:  formQuery(op, in),
		FromGrid:  opts.fromGrid,
		CloseHref: formQuery(op, in),
		Fields:    inputFields(in),
	}
	p.Heading = p.Title.Label
	if op.Kind.Teamless() {
		p.CloseHref = op.listPage(in)
	}
	if opts.fromGrid {
		p.EditHref, p.CloseHref = "/", "/"
		switch op.Kind {
		case pods.KindReset:
			p.Reselect = "Change snapshot"
		case pods.KindSnapshot:
			p.Reselect = "Change"
		}
	}
	p.Blocked = len(plan.Items) - runnable
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
	var nCreate, nReuse, nBlocked int
	for _, t := range plan.Templates {
		row := templateRow{Name: t.Name, Node: orDashString(t.Node), State: "create", Blocked: t.Blocked}
		switch {
		case t.Blocked != "":
			p.TemplatesOpen = true
			nBlocked++
		case t.Rebuild:
			row.State = "rebuild"
			rebuilt = append(rebuilt, t.Name)
		case t.Exists:
			row.State = "reuse"
			nReuse++
		default:
			nCreate++
		}
		if t.WillStopMaster {
			row.State += " · stops its master"
			stopped = append(stopped, t.MasterName)
		}
		p.Templates = append(p.Templates, row)
	}
	var tplParts []string
	for _, c := range []struct {
		n    int
		word string
	}{{nCreate, "to create"}, {len(rebuilt), "to rebuild"}, {nReuse, "reused"}, {nBlocked, "blocked"}} {
		if c.n > 0 {
			tplParts = append(tplParts, fmt.Sprintf("%d %s", c.n, c.word))
		}
	}
	p.TemplateSummary = strings.Join(tplParts, ", ")
	p.ItemsFold = p.Blocked == 0 && (op.Kind == pods.KindDeploy || len(p.Items) > 8)
	p.ItemsSummary = countVMs(len(p.Items))
	if len(rebuilt) > 0 && len(stopped) > 0 && nCreate == 0 {
		if len(rebuilt) == 1 && len(stopped) == 1 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Rebuilding %s deletes the old template and stops master %s, which is running, until the copy finishes.", rebuilt[0], stopped[0]))
		} else {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Rebuilding %d %s deletes the old %s and stops %d running %s, each until its copy finishes.",
				len(rebuilt), byCount(len(rebuilt), "template", "templates"), byCount(len(rebuilt), "one", "ones"), len(stopped), byCount(len(stopped), "master", "masters")))
		}
	} else {
		switch {
		case len(rebuilt) == 1:
			p.Warnings = append(p.Warnings, fmt.Sprintf("Template %s will be deleted and built again from its master.", rebuilt[0]))
		case len(rebuilt) > 1:
			p.Warnings = append(p.Warnings, fmt.Sprintf("%d templates will be deleted and built again from their masters.", len(rebuilt)))
		}
		switch {
		case len(stopped) == 1:
			p.Warnings = append(p.Warnings, fmt.Sprintf("Building its template stops master %s, which is running, until the copy finishes.", stopped[0]))
		case len(stopped) > 1:
			p.Warnings = append(p.Warnings, fmt.Sprintf("Building the templates stops %d running masters, each until its copy finishes.", len(stopped)))
		}
	}
	if !op.Kind.Teamless() && op.Kind != pods.KindDeploy {
		if all := s.polledGrid(ctx).Teams; coversTeams(all, plan.Teams) {
			p.Warnings = append(p.Warnings, fmt.Sprintf("This covers every pod (%s).", teamsText(pods.FormatTeams(all))))
		}
	}
	if missing := jobs.MissingVMs(in, plan); len(missing) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%d of the %d VMs asked for by name no longer exist or match the pods and hosts, so they are left out: %s.",
			len(missing), len(in.VMs), strings.Join(missing, ", ")))
	}
	switch {
	case len(plan.Items) == 0:
		p.Nothing = "No VMs in these pods match these hosts."
		if op.Kind.Teamless() {
			p.Nothing = "Nothing to do."
		}
		if op.Kind == pods.KindDeploy {
			p.Nothing = "No masters match this template set and these hosts."
		}
		if op.Kind == pods.KindTeardown && in.Set != "" {
			p.Nothing = "No VMs of " + string(in.Set) + " are in these pods and hosts."
		}
	case runnable == 0:
		p.Nothing = "Every VM is blocked: nothing can run."
	}
	if p.Nothing != "" {
		p.Warnings = nil // nothing to confirm
	} else {
		p.Capacity = capacityOf(opts.capacity, &p.Warnings)
		p.Consequence = consequence(op.Kind, in, plan.Runnable())
		p.Calm = !destructive(in, plan.Runnable())
		if op.Kind == pods.KindPower {
			p.AlreadyN, p.AlreadyWord = s.alreadyDone(ctx, in.Action, plan.Runnable())
		}
	}
	p.Warnings = append(p.Warnings, plan.Notices...) // what was left alone, even when nothing runs
	if p.Review = review(op, in, plan, p.Nothing, s.cfg.Environments.Subnet); p.Review != nil {
		p.Heading = p.Review.Verb + " " + p.Review.Subject
		p.EditHref = reviewBack(op, in)
		if opts.retryOf > 0 {
			p.Review.Retry = "Retry of #" + strconv.FormatInt(opts.retryOf, 10) + ": only the VMs that didn't finish."
			if op.Kind.RetriedWhole() {
				p.Review.Retry = "Retry of #" + strconv.FormatInt(opts.retryOf, 10) + ": the " + op.Noun + " of what is left."
			}
		}
	}

	return p
}

// previewFacts are the inputs a preview recaps that the title and table don't.
func previewFacts(in jobs.Inputs, fromGrid bool, retryOf int64) []fact {
	var fs []fact
	switch {
	case in.Kind.Teamless():
		return nil // its review says it all
	case !fromGrid:
		fs = append(fs, fact{"Pods", teamsText(in.Teams)})
		if len(in.Hosts) > 0 {
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

// teamlessFacts are an image or environment operation's inputs; a slot is
// named by its pool, from the pattern pool.
func teamlessFacts(in jobs.Inputs, pool string) []fact {
	var fs []fact
	add := func(k, v string) {
		if v != "" {
			fs = append(fs, fact{k, v})
		}
	}
	add("Domain", in.Domain)
	if n, ok := pods.ParseSlotName(in.Slot); ok {
		add("Slot", pods.ExpandSlot(pool, n))
	}
	add("Router", in.Router)
	add("Hosts", strings.Join(in.EnvHosts, ", "))
	add("Host", in.EnvHost)
	add("Only these VMs", strings.Join(in.VMs, ", "))
	if in.VMID != 0 {
		add("VM", strconv.Itoa(in.VMID))
	}
	if in.Octet == pods.ClearOctet {
		add("Address", "none")
	} else if in.Octet > 0 {
		add("Address", "."+strconv.Itoa(in.Octet))
	}
	if in.EjectCDROM {
		add("CD-ROM", "eject")
	}
	if in.CPU != "" {
		add("CPU", strings.TrimPrefix(in.CPU, "custom-"))
	}
	if in.Kind == pods.KindEnvPower {
		add("Action", in.Action)
	}
	add("Family", in.Family)
	add("Kind", in.ImageKind)
	add("Image", in.Image)
	return fs
}

// askedFacts are the non-default inputs beyond teams and hosts that the title
// (opLabelOf) doesn't say.
func askedFacts(in jobs.Inputs) []fact {
	var fs []fact
	switch in.Kind {
	case pods.KindDeploy:
		if in.Rebuild {
			fs = append(fs, fact{"Rebuild templates", "on"})
		}
		if in.NoSnapshot {
			fs = append(fs, fact{"Baseline snapshot", "off"})
		}
	case pods.KindReset:
		if in.Snapshot == "" {
			fs = append(fs, fact{"Snapshot", "baseline"})
		}
	case pods.KindSnapshot:
		if in.Description != "" {
			fs = append(fs, fact{"Description", in.Description})
		}
		if in.VMState {
			fs = append(fs, fact{"Include RAM", "on"})
		}
	}
	return fs
}

// capacityOf describes c for the page, warning when any usage goes over. It is
// nil if c lists nothing.
func capacityOf(c *pods.Capacity, warnings *[]string) *capacityView {
	if c == nil || len(c.Usage)+len(c.Notes) == 0 {
		return nil
	}
	v := &capacityView{Notes: c.Notes}
	over := false
	var nodes, storages int
	var lastStorage string
	for _, u := range c.Usage {
		text := fmt.Sprintf("%s used of %s, needs %s, %s free", pods.GiB(u.Used), pods.GiB(u.Total), pods.GiB(u.Needed), pods.GiB(u.Free()))
		level := "ok"
		switch {
		case u.Over():
			level = "over"
			over = true
		case u.Near():
			level = "near"
			pct := float64(u.Used+u.Needed) * 100 / float64(u.Total)
			text += fmt.Sprintf(" (%.0f%% used)", pct)
		}
		v.Tight = v.Tight || level != "ok"
		row := capacityRow{Label: u.Label(), Level: level, Text: text}
		if u.Kind == pods.UsageMemory {
			row.Label += " memory"
			nodes++
		} else {
			storages++
			lastStorage = u.Label()
		}
		row.Used = step5(u.Used, u.Total)
		row.Need = min(step5(u.Needed, u.Total), 100-row.Used)
		if row.Need == 0 && u.Needed > 0 && row.Used < 100 {
			row.Need = 5 // small, but there
		}
		v.Rows = append(v.Rows, row)
	}
	if over {
		*warnings = append(*warnings, "Over capacity: you can still confirm if the overcommit is deliberate.")
	}
	if !v.Tight {
		var where []string
		if nodes > 0 {
			where = append(where, fmt.Sprintf("%d %s", nodes, byCount(nodes, "node", "nodes")))
		}
		if storages == 1 {
			where = append(where, lastStorage)
		} else if storages > 1 {
			where = append(where, fmt.Sprintf("%d storages", storages))
		}
		if len(where) > 0 {
			v.Summary = "fits on " + strings.Join(where, " and ")
		}
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
	for _, row := range s.polledGrid(ctx).Rows {
		for _, c := range row.Cells {
			if names[c.Name] && c.Power == want {
				n++
			}
		}
	}
	return n, want
}

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
		deleted := 0
		for _, it := range items {
			if it.Node != "" && !slices.Contains(nodes, it.Node) {
				nodes = append(nodes, it.Node)
			}
			if slices.Contains(it.Steps, pods.StepDelete) {
				deleted++
			}
		}
		slices.Sort(nodes)
		on := ""
		if len(nodes) > 0 {
			on = " on " + strings.Join(nodes, ", ")
		}
		from := in.Pattern
		if set, ok := pods.SetOfPattern(in.Pattern); ok {
			from = string(set)
		}
		switch {
		case deleted > 0 && deleted == n:
			return vms + " will be torn down and recreated from " + from + on + "."
		case deleted > 0:
			return vms + " (" + countVMs(deleted) + " torn down and recreated) will be created or repaired from " + from + on + "."
		}
		return vms + " will be created or repaired from " + from + on + "."
	case pods.KindSnapshot:
		msg := vms + pick(" get a new snapshot, ", " gets a new snapshot, ") + in.Snapshot + "."
		if in.VMState {
			msg += pick(" Their RAM is saved too if they are running, which takes longer.", " Its RAM is saved too if it is running, which takes longer.")
		}
		return msg
	}
	return ""
}

// poolOf is the pool new environment VMs go into.
func poolOf(items []pods.Item) string {
	for _, it := range items {
		if it.Pool != "" {
			return it.Pool
		}
	}
	return "the slot"
}

func itemNames(items []pods.Item) string {
	var n []string
	for _, it := range items {
		n = append(n, it.Name)
	}
	return strings.Join(n, ", ")
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
