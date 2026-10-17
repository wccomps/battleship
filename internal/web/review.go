package web

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// reviewView is the review of an image or environment operation: one
// sentence saying what will happen, then the facts of what will exist (a
// register) or a row per VM it acts on.
type reviewView struct {
	Verb, Subject string // the title: "Register teak-build"
	Say           string // what confirming does, or why it can't
	Blocked       bool
	Deletes       bool // confirming deletes VMs: Say reads as a warning
	Facts         []reviewFact
	// Rows are the VMs, in the plan's order; Image and Addr say whether the
	// table has those columns.
	Rows        []reviewRow
	Image, Addr bool
	// DNS are a Set DNS's hosts, their DNS server now and after.
	DNS  []dnsRow
	Note string // a line after the rows
	// Kept are VMs a teardown leaves alone, each with why.
	Kept  []string
	Retry int64 // the job this retries
}

// reviewFact is a labelled fact of a review: a name, text and a muted note,
// a link to change it, or notes in a box.
type reviewFact struct {
	Label  string
	Code   string
	Text   string
	Meta   string
	Change string // the form, filled in, to change it
	Notes  string
}

// reviewRow is a VM a review lists: its full name, the exact image version
// it is cloned from, its address's last octet (".4", Picked if the plan
// chose it), and its VMID and node; Blocked says why it won't run.
type reviewRow struct {
	Name    string
	Router  bool
	Image   string
	Addr    string
	Picked  bool
	Where   string
	Blocked string
}

// dnsRow is a host whose DNS a Set DNS changes: the last octets of its DNS
// server now and after (Itself if that is its own address).
type dnsRow struct {
	Host, Now, After string
	Itself           bool
	Blocked          string
}

// kindUses says what each base-image kind is offered as.
var kindUses = map[string]string{
	pods.ImageRouter:  "offered as the router when creating environments",
	pods.ImageWindows: "named and addressed through the guest agent",
	pods.ImageLinux:   "named and addressed through the guest agent",
	pods.ImageOther:   "cloned as it is; Battleship doesn't set its name or address",
}

// review describes plan as an image or environment operation's review, or
// is nil for a team operation. nothing, if set, is why nothing can run.
func (s *Server) review(ctx context.Context, op operation, in jobs.Inputs, plan *pods.Plan, nothing string) *reviewView {
	var r *reviewView
	switch op.Kind {
	case pods.KindImageRegister:
		if len(plan.Items) != 1 {
			return nil
		}
		return s.registerReview(ctx, op, in, plan.Items[0])
	case pods.KindEnvCreate, pods.KindEnvAdd:
		r = s.envReview(op, in, plan)
	case pods.KindEnvDNS:
		r = s.dnsReview(ctx, in, plan)
	case pods.KindEnvTeardown:
		r = s.teardownReview(in, plan)
	case pods.KindImageEdit, pods.KindImageDeprecate, pods.KindImageDelete:
		r = imageReview(op, in, plan)
	default:
		return nil
	}
	if nothing != "" {
		r.Say, r.Blocked = nothing, true
	}
	return r
}

// registerReview describes registering it: the source VM, the image it
// becomes and how, its kind, where the template lands, and the source's
// notes, which the copy keeps.
func (s *Server) registerReview(ctx context.Context, op operation, in jobs.Inputs, it pods.Item) *reviewView {
	src := pods.Source{Name: "?"}
	if it.Source != nil {
		src = *it.Source
	}
	var desc string
	if cfg, err := s.apiFor(ctx).VMConfig(ctx, src.Node, src.VMID); err == nil {
		desc = cfg["description"]
	}
	how := "next version"
	if _, v, ok := pods.ParseImageName(it.Name); ok && v == 1 {
		how = "new family"
	}
	if n, _ := pods.ParseNotes(desc); n.Base != "" {
		if _, v, ok := pods.ParseImageName(n.Base); ok {
			how = "edit copy of v" + strconv.Itoa(v)
		}
	}
	r := &reviewView{Verb: "Register", Subject: src.Name,
		Say: "Copies " + src.Name + " into a new template, " + it.Name + ". The build VM stays as it is."}
	if it.Blocked != "" {
		r.Say, r.Blocked = sentence(it.Blocked), true
	}
	r.Facts = append(r.Facts,
		reviewFact{Label: "From", Code: src.Name, Text: "VMID " + strconv.Itoa(src.VMID) + " · " + src.Node},
		reviewFact{Label: "Becomes", Code: it.Name, Meta: how, Change: formQuery(op, in)})
	for _, k := range pods.Tags(it.Tags) {
		if use, ok := kindUses[k]; ok {
			r.Facts = append(r.Facts, reviewFact{Label: "Kind", Text: k, Meta: use})
		}
	}
	r.Facts = append(r.Facts, reviewFact{Label: "Template", Text: "VMID " + strconv.Itoa(it.VMID) + " on " + it.Node})
	if it.Supersedes != nil {
		r.Facts = append(r.Facts, reviewFact{Label: "Replaces", Code: it.Supersedes.Name, Meta: "deprecated once this is registered"})
	}
	if notes := pods.UserNotes(desc); notes != "" {
		r.Facts = append(r.Facts, reviewFact{Label: "Notes", Notes: notes})
	}
	return r
}

// envReview describes a create or add: a row per machine, and what the
// guest agent sets on the Windows and Linux hosts.
func (s *Server) envReview(op operation, in jobs.Inputs, plan *pods.Plan) *reviewView {
	r := &reviewView{Verb: "Create", Subject: in.Domain, Image: true, Addr: true}
	if op.Kind == pods.KindEnvAdd {
		r.Verb = "Add hosts to"
	}
	specs := map[string]pods.HostSpec{}
	router, _ := pods.ParseHostSpec(in.Router)
	for _, line := range append([]string{in.Router}, in.EnvHosts...) {
		if h, err := pods.ParseHostSpec(line); err == nil {
			specs[h.Host] = h
		}
	}
	var clones, resumes, guests []string
	dns, windows := "", false
	for _, it := range plan.Items {
		spec := specs[it.Host]
		row := reviewRow{Name: it.Name, Router: router.Host != "" && it.Host == router.Host, Image: spec.Family,
			Where: orDash(it.VMID) + " · " + orDashString(it.Node), Blocked: it.Blocked}
		if it.Source != nil {
			row.Image = it.Source.Name
		}
		switch {
		case row.Router:
			row.Addr = "." + strconv.Itoa(pods.GatewayOctet)
		case it.Guest != nil:
			row.Addr = "." + strconv.Itoa(pods.LastOctet(it.Guest.IP))
			row.Picked = spec.Octet == 0 && slices.Contains(it.Steps, pods.StepClone)
		case spec.Octet != 0:
			row.Addr = "." + strconv.Itoa(spec.Octet)
		}
		r.Rows = append(r.Rows, row)
		if it.Blocked != "" {
			continue
		}
		if slices.Contains(it.Steps, pods.StepClone) {
			clones = append(clones, it.Host)
		} else {
			resumes = append(resumes, it.Host)
		}
		if it.Guest != nil {
			guests = append(guests, it.Host)
			dns = it.Guest.DNS
			windows = windows || it.Guest.OS == pods.ImageWindows
		}
	}
	pool := poolOf(plan.Items)
	var say []string
	switch len(clones) {
	case 0:
	case 1:
		say = append(say, "Clones "+clones[0]+" into "+pool+" and starts it.")
	default:
		say = append(say, "Clones "+strconv.Itoa(len(clones))+" VMs into "+pool+" and starts them.")
	}
	if len(resumes) > 0 {
		say = append(say, "Picks up "+andList(resumes)+" where "+byCount(len(resumes), "it", "they")+" stopped.")
	}
	if len(guests) > 0 {
		at := "the router"
		if dns != s.gateway() {
			at = "." + strconv.Itoa(pods.LastOctet(dns))
		}
		say = append(say, "Then "+andList(guests)+byCount(len(guests), " gets its", " get their")+
			" name, address, gateway ."+strconv.Itoa(pods.GatewayOctet)+" and DNS ("+at+") through the guest agent.")
	}
	r.Say = strings.Join(say, " ")
	if op.Kind == pods.KindEnvCreate && windows && dns == s.gateway() {
		r.Note = "Once a domain controller is up, use Set DNS on " + in.Domain + " to point the hosts at it."
	}
	return r
}

// teardownReview describes tearing an environment down: a row per VM it
// deletes, with its image and address from its notes, and the VMs on its
// networks it leaves alone.
func (s *Server) teardownReview(in jobs.Inputs, plan *pods.Plan) *reviewView {
	r := &reviewView{Verb: "Tear down", Subject: in.Domain, Image: true, Addr: true, Deletes: true}
	pool := poolOf(plan.Items)
	deletes, blocked := 0, 0
	var disks []string
	for _, it := range plan.Items {
		row := reviewRow{Name: it.Name, Router: slices.Contains(pods.Tags(it.Tags), pods.ImageRouter),
			Where: orDash(it.VMID) + " · " + orDashString(it.Node), Blocked: it.Blocked}
		if it.Notes != nil {
			row.Image = it.Notes.Base
			if o := pods.LastOctet(it.Notes.IP); o != 0 {
				row.Addr = "." + strconv.Itoa(o)
			}
		}
		if row.Router {
			row.Addr = "." + strconv.Itoa(pods.GatewayOctet)
		}
		r.Rows = append(r.Rows, row)
		switch {
		case it.Blocked != "":
			blocked++
		case slices.Contains(it.Steps, pods.StepDelete):
			deletes++
		case slices.Contains(it.Steps, pods.StepFreeDisks):
			disks = append(disks, strconv.Itoa(it.VMID))
		}
	}
	for _, k := range plan.Kept {
		r.Kept = append(r.Kept, k.Name+" is "+k.Why+".")
	}
	var say []string
	if deletes > 0 {
		say = append(say, "Deletes "+countVMs(deletes)+" in "+pool+".")
	}
	if len(disks) > 0 {
		say = append(say, "Frees the disks deleted VMs left at VMID "+andList(disks)+".")
	}
	switch {
	case blocked > 0:
		say = append(say, "The blocked "+byCount(blocked, "VM stays", "VMs stay")+", so the slot isn't free afterwards.")
	case len(r.Kept) > 0:
		say = append(say, "VMs outside "+pool+" stay on its networks, so the slot isn't free afterwards.")
	default:
		say = append(say, "The slot is free again afterwards.")
	}
	r.Say = strings.Join(say, " ")
	return r
}

// gateway is every environment's router address.
func (s *Server) gateway() string {
	return s.cfg.Environments.Subnet + "." + strconv.Itoa(pods.GatewayOctet)
}

// dnsReview describes a Set DNS: each Windows and Linux host's DNS server
// now and after. The others only record the choice.
func (s *Server) dnsReview(ctx context.Context, in jobs.Inputs, plan *pods.Plan) *reviewView {
	r := &reviewView{Verb: "Set DNS for", Subject: in.Domain}
	envs, _, err := s.planner(ctx).Environments(ctx)
	env, _ := pods.EnvByDomain(envs, in.Domain)
	if err != nil {
		s.logf("web: reading %s's DNS for a review: %v", in.Domain, err)
	}
	var changes []string
	target := ""
	for _, it := range plan.Items {
		if it.Guest == nil {
			continue
		}
		row := dnsRow{Host: it.Host, Now: dnsNow(env, it.Name, s.gateway()), After: "." + strconv.Itoa(pods.LastOctet(it.Guest.DNS)),
			Itself: it.Guest.DNS == it.Guest.IP, Blocked: it.Blocked}
		r.DNS = append(r.DNS, row)
		target = it.Guest.DNS
		if it.Blocked == "" {
			changes = append(changes, it.Host)
		}
	}
	at := "the router (." + strconv.Itoa(pods.GatewayOctet) + ")"
	if choice := strings.TrimSpace(in.DNS); choice != "" && choice != pods.DNSRouter {
		at = choice + " (." + strconv.Itoa(pods.LastOctet(target)) + ")"
	}
	switch {
	case len(changes) > 0:
		r.Say = "Points " + andList(changes) + " at " + at + " through the guest agent."
	case len(r.DNS) > 0:
		r.Say = "No host's DNS changes now: each one is blocked. Confirming records " + at + " as the choice."
	default:
		r.Say = "No host here is a Windows or Linux host with an address, so no DNS changes. Confirming records " + at + " as the choice."
	}
	if env.HasRouter() {
		r.Say += " The router keeps its own settings."
	}
	return r
}

// dnsNow is the last octet of the DNS server the host named name uses now,
// as its notes record it, or the host it names if that has no address.
func dnsNow(env pods.Environment, name, gateway string) string {
	h, ok := env.Host(name)
	if !ok {
		return ""
	}
	addr := gateway
	if c := h.Notes.DNS; c != "" && c != pods.DNSRouter {
		d, ok := env.Host(c + "." + env.Domain)
		if !ok || d.Notes.IP == "" {
			return c
		}
		addr = d.Notes.IP
	}
	return "." + strconv.Itoa(pods.LastOctet(addr))
}

// imageReview describes opening a base image for editing, deprecating it
// or deleting it.
func imageReview(op operation, in jobs.Inputs, plan *pods.Plan) *reviewView {
	r := &reviewView{Subject: in.Image}
	for _, it := range plan.Items {
		row := reviewRow{Name: it.Name, Where: orDash(it.VMID) + " · " + orDashString(it.Node), Blocked: it.Blocked}
		if it.Source != nil {
			row.Image, r.Image = it.Source.Name, true
		}
		r.Rows = append(r.Rows, row)
	}
	names := itemNames(plan.Items)
	switch op.Kind {
	case pods.KindImageEdit:
		r.Verb, r.Subject = "Edit", in.Family
		from := "the newest version"
		if len(r.Rows) == 1 && r.Rows[0].Image != "" {
			from = r.Rows[0].Image
		}
		r.Say = "Copies " + from + " as " + names + " on the edit network, for you to patch and register as the next version."
	case pods.KindImageDeprecate:
		r.Verb = "Deprecate"
		r.Say = names + " will no longer be offered for new hosts. Hosts made from it keep working."
	case pods.KindImageDelete:
		r.Verb = "Delete"
		r.Say = names + " will be deleted. Hosts made from it keep working: they are full copies. This can't be undone."
	}
	return r
}

// reviewBack is where a review's Back goes: the form, filled in; Images
// for a register, whose row was the form; and the family page for the
// operations its versions offer.
func reviewBack(op operation, in jobs.Inputs) string {
	switch op.Kind {
	case pods.KindImageRegister:
		return listPage(in)
	case pods.KindImageEdit:
		return "/images/" + url.PathEscape(in.Family)
	case pods.KindImageDeprecate, pods.KindImageDelete:
		if f, _, ok := pods.ParseImageName(in.Image); ok {
			return "/images/" + url.PathEscape(f)
		}
		return listPage(in)
	case pods.KindEnvTeardown:
		return listPage(in)
	}
	return formQuery(op, in)
}

// andList joins names as prose: "a", "a and b", "a, b and c".
func andList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// byCount is one if n is 1, else many.
func byCount(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
