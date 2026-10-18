package web

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// reviewView is the review of an image or environment operation: what
// will happen, then the facts of what will exist (a
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
	Kept []string
	// Retry says which job this retries, and what of it.
	Retry string
	// Details are what a register writes, DetailsSay where they come from,
	// and EditDetails the register form to change them, which the review's
	// inputs are posted to (keeping a password out of the address).
	Details     *detailsView
	DetailsSay  string
	EditDetails string
	// Changes are an Edit notes's details, before and after; Block, the
	// unreadable block they replace; NotesChange, its free text.
	Changes     []detailChange
	Block       string
	NotesChange *notesChange
	// Notes are the notes people wrote that the copy keeps, in a box.
	Notes string
}

// reviewFact is a labelled fact of a review: a name, text and a muted note,
// and a link to change it.
type reviewFact struct {
	Label  string
	Code   string
	Text   string
	Meta   string
	Change string // the form, filled in, to change it
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
// is nil for a team operation. nothing, if set, is why nothing can run; lan
// is the environments' LAN.
func review(op operation, in jobs.Inputs, plan *pods.Plan, nothing string, lan config.LAN) *reviewView {
	var r *reviewView
	switch {
	case op.Machines:
		r = envReview(op, in, plan, lan)
	case op.Kind == pods.KindImageRegister:
		if len(plan.Items) != 1 {
			return nil
		}
		return registerReview(op, in, plan.Items[0])
	case op.Kind == pods.KindEnvDNS:
		r = dnsReview(in, plan, lan)
	case op.Kind == pods.KindEnvTeardown:
		r = teardownReview(in, plan, lan)
	case op.Kind == pods.KindImageDetails:
		if len(plan.Items) != 1 {
			return nil
		}
		return detailsReview(plan.Items[0])
	case op.Section == sectionImages:
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
// becomes and how, its kind, where the template lands, the details it
// writes, and the source's notes, which the copy keeps.
func registerReview(op operation, in jobs.Inputs, it pods.Item) *reviewView {
	src := pods.Source{Name: "?"}
	if it.Source != nil {
		src = *it.Source
	}
	_, version, _ := pods.ParseImageName(it.Name)
	how := registerHow(version, src.Base)
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
	switch {
	case it.Details != nil:
		r.Details = detailsOf(it.Details.ImageDetails, false)
		if in.Details == nil {
			r.DetailsSay = "Read from the notes of " + src.Name + ": check them."
		}
	case src.Unreadable:
		r.DetailsSay = "The details in the notes of " + src.Name + " are unreadable; the copy keeps them as they are."
	default:
		r.DetailsSay = "None: add them here, or later with Edit notes on its family's page."
	}
	if !r.Blocked {
		r.EditDetails = op.Path
	}
	r.Notes = src.Notes
	return r
}

// notesChange is the free text an Edit notes changes, before and after.
type notesChange struct{ Before, After string }

// detailsReview describes an Edit notes: each detail before and after,
// then the free text if the job writes it.
func detailsReview(it pods.Item) *reviewView {
	var was pods.NotesRead
	if it.DetailsWas != nil {
		was = *it.DetailsWas
	}
	var after pods.ImageNotes
	if it.Details != nil {
		after = *it.Details
	}
	r := &reviewView{Verb: "Edit notes of", Subject: it.Name, Changes: detailChanges(was.Details, after.ImageDetails)}
	switch {
	case it.Blocked != "":
		r.Say, r.Blocked = sentence(it.Blocked), true
	case after.Free == nil:
		r.Say = "Writes these details into the notes of " + it.Name + ", which new hosts copy. The rest of its notes, and older versions, stay as they are."
	default:
		r.Say = "Writes these details and notes into the notes of " + it.Name + ", which new hosts copy. Older versions keep theirs."
	}
	switch {
	case was.Unreadable && after.ImageDetails.Empty():
		r.DetailsSay, r.Block = "Its details block is unreadable: it is removed, and no details are written.", was.Block
	case was.Unreadable:
		r.DetailsSay, r.Block = "Its details block is unreadable: these replace it.", was.Block
	}
	if after.Free != nil {
		r.NotesChange = &notesChange{Before: was.Free, After: *after.Free}
	}
	return r
}

// envReview describes a create or add: a row per machine, and what the
// guest agent sets on the Windows and Linux hosts.
func envReview(op operation, in jobs.Inputs, plan *pods.Plan, lan config.LAN) *reviewView {
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
	var dns netip.Addr                // the guests' DNS server
	windows, atRouter := false, false // whether any is Windows, and whether dns is their gateway
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
			row.Addr = lanOctet(lan, it.Guest.IP)
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
			atRouter = it.Guest.DNS == it.Guest.Gateway
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
		if !atRouter {
			at = lanOctet(lan, dns)
		}
		say = append(say, "Then "+andList(guests)+byCount(len(guests), " gets its", " get their")+
			" name, address, gateway ."+strconv.Itoa(pods.GatewayOctet)+" and DNS ("+at+") through the guest agent.")
	}
	r.Say = strings.Join(say, " ")
	if op.Kind == pods.KindEnvCreate && windows && atRouter {
		r.Note = "Once a domain controller is up, use Set DNS on " + in.Domain + " to point the hosts at it."
	}
	return r
}

// teardownReview describes tearing an environment down: a row per item
// (each VM it deletes or can't, with its image and address from its notes,
// and the disks it frees), and the VMs on its networks it leaves alone.
func teardownReview(in jobs.Inputs, plan *pods.Plan, lan config.LAN) *reviewView {
	r := &reviewView{Verb: "Tear down", Subject: in.Domain, Image: true, Addr: true, Deletes: true}
	pool := poolOf(plan.Items)
	deletes := 0
	var disks []string
	var left pods.Environment // what the slot holds afterwards
	for _, it := range plan.Items {
		row := reviewRow{Name: it.Name, Router: slices.Contains(pods.Tags(it.Tags), pods.ImageRouter),
			Where: orDash(it.VMID) + " · " + orDashString(it.Node), Blocked: it.Blocked}
		if it.Notes != nil {
			row.Image = it.Notes.Base
			if it.Notes.IP.IsValid() {
				row.Addr = lanOctet(lan, it.Notes.IP)
			}
		}
		if row.Router {
			row.Addr = "." + strconv.Itoa(pods.GatewayOctet)
		}
		r.Rows = append(r.Rows, row)
		switch {
		case it.Blocked != "":
			left.Hosts = append(left.Hosts, pods.EnvHost{Name: it.Name})
		case slices.Contains(it.Steps, pods.StepDelete):
			deletes++
		case slices.Contains(it.Steps, pods.StepFreeDisks):
			disks = append(disks, strconv.Itoa(it.VMID))
		}
	}
	for _, k := range plan.Kept {
		r.Kept = append(r.Kept, k.Name+" is "+k.Why+".")
		left.Users = append(left.Users, k.Name)
	}
	var say []string
	if deletes > 0 {
		say = append(say, "Deletes "+countVMs(deletes)+" in "+pool+".")
	}
	if len(disks) > 0 {
		say = append(say, "Frees the disks deleted VMs left at VMID "+andList(disks)+".")
	}
	switch {
	case left.Free():
		say = append(say, "The slot is free again afterwards.")
	case len(left.Hosts) > 0:
		say = append(say, "The blocked "+byCount(len(left.Hosts), "VM stays", "VMs stay")+", so the slot isn't free afterwards.")
	default:
		say = append(say, "VMs outside "+pool+" stay on its networks, so the slot isn't free afterwards.")
	}
	r.Say = strings.Join(say, " ")
	return r
}

// dnsReview describes a Set DNS: each Windows and Linux host's DNS server
// before (Item.DNSWas) and after. The others only record the choice.
func dnsReview(in jobs.Inputs, plan *pods.Plan, lan config.LAN) *reviewView {
	r := &reviewView{Verb: "Set DNS for", Subject: in.Domain}
	var changes []string
	var target netip.Addr
	router := false
	for _, it := range plan.Items {
		router = router || slices.Contains(pods.Tags(it.Tags), pods.ImageRouter)
		if it.Guest == nil {
			continue
		}
		row := dnsRow{Host: it.Host, Now: dnsWas(it.DNSWas, lan), After: lanOctet(lan, it.Guest.DNS),
			Itself: it.Guest.DNS == it.Guest.IP, Blocked: it.Blocked}
		r.DNS = append(r.DNS, row)
		target = it.Guest.DNS
		if it.Blocked == "" {
			changes = append(changes, it.Host)
		}
	}
	at := "the router (." + strconv.Itoa(pods.GatewayOctet) + ")"
	if choice := strings.TrimSpace(in.DNS); choice != "" && choice != pods.DNSRouter {
		at = choice + " (" + lanOctet(lan, target) + ")"
	}
	switch {
	case len(changes) > 0:
		r.Say = "Points " + andList(changes) + " at " + at + " through the guest agent."
	case len(r.DNS) > 0:
		r.Say = "No host's DNS changes now: each one is blocked. Confirming records " + at + " as the choice."
	default:
		r.Say = "No host here is a Windows or Linux host with an address, so no DNS changes. Confirming records " + at + " as the choice."
	}
	if router {
		r.Say += " The router keeps its own settings."
	}
	return r
}

// dnsWas is a host's DNS server before a Set DNS: its address (lanOctet),
// or the host it names if that has none.
func dnsWas(was *pods.DNSServer, lan config.LAN) string {
	switch {
	case was == nil:
		return ""
	case !was.Addr.IsValid():
		return was.Choice
	}
	return lanOctet(lan, was.Addr)
}

// lanOctet is ".N" for a, host N of lan, else a written out.
func lanOctet(lan config.LAN, a netip.Addr) string {
	if o, ok := lan.Octet(a); ok {
		return "." + strconv.Itoa(o)
	}
	return pods.AddrText(a)
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

// reviewBack is where a review's Back goes: the form, filled in; the page
// it was opened from for a register (Images, whose row was the form) and a
// teardown (Environments); and the family's page for an edit, deprecate,
// delete or Edit notes.
func reviewBack(op operation, in jobs.Inputs) string {
	switch op.Kind {
	case pods.KindImageRegister, pods.KindEnvTeardown:
		return listPage(in)
	case pods.KindImageEdit, pods.KindImageDetails:
		return familyHref(in.Family)
	case pods.KindImageDeprecate, pods.KindImageDelete:
		if f, _, ok := pods.ParseImageName(in.Image); ok {
			return familyHref(f)
		}
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

func byCount(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
