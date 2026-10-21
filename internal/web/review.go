package web

import (
	"cmp"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// reviewView is the review of an image or environment operation, or of a
// pods teardown: what will happen, then the facts of what will exist (a
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
	Set         bool   // a teardown of everything: each VM's set
	Note        string // a line after the rows
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
	Set     string
	Image   string
	Addr    string
	Picked  bool
	Where   string
	Blocked string
}

// kindUses says what each base-image kind is offered as.
var kindUses = map[string]string{
	pods.ImageRouter:  "offered as the router when creating environments",
	pods.ImageWindows: "named and addressed through the guest agent",
	pods.ImageLinux:   "named and addressed through the guest agent",
	pods.ImageOther:   "cloned as it is; Battleship doesn't set its name or address",
}

// review describes plan as an image or environment operation's review, or
// a pods teardown, or nil for another pod operation. nothing, if set, is
// why nothing can run; lan is the environments' LAN.
func review(op operation, in jobs.Inputs, plan *pods.Plan, nothing string, lan config.LAN) *reviewView {
	if op.review == nil {
		return nil
	}
	r := op.review(op, in, plan, lan)
	if r != nil && nothing != "" && !r.Blocked {
		r.Say, r.Blocked = nothing, true
	}
	return r
}

// registerReview describes registering a base image: the source VM, the
// image it becomes and how, its kind, where the template lands, the details
// it writes, and the source's notes, which the copy keeps.
func registerReview(op operation, in jobs.Inputs, plan *pods.Plan, _ config.LAN) *reviewView {
	if len(plan.Items) != 1 {
		return nil
	}
	it := plan.Items[0]
	src := pods.Source{Name: "?"}
	if it.Source != nil {
		src = *it.Source
	}
	_, version, _ := pods.ParseImageName(it.Name)
	how := registerHow(version, src.Base)
	r := &reviewView{Verb: op.Verb, Subject: src.Name,
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
func detailsReview(op operation, _ jobs.Inputs, plan *pods.Plan, _ config.LAN) *reviewView {
	if len(plan.Items) != 1 {
		return nil
	}
	it := plan.Items[0]
	var was pods.NotesRead
	if it.DetailsWas != nil {
		was = *it.DetailsWas
	}
	var after pods.ImageNotes
	if it.Details != nil {
		after = *it.Details
	}
	r := &reviewView{Verb: op.Verb, Subject: it.Name}
	r.applyNotesDiff(was, after)
	switch {
	case it.Blocked != "":
		r.Say, r.Blocked = sentence(it.Blocked), true
	case after.Free == nil:
		r.Say = "Writes these details into the notes of " + it.Name + ", which new hosts copy. The rest of its notes, and older versions, stay as they are."
	default:
		r.Say = "Writes these details and notes into the notes of " + it.Name + ", which new hosts copy. Older versions keep theirs."
	}
	return r
}

func (r *reviewView) applyNotesDiff(was pods.NotesRead, after pods.ImageNotes) {
	r.Changes = detailChanges(was.Details, after.ImageDetails)
	switch {
	case was.Unreadable && after.ImageDetails.Empty():
		r.DetailsSay, r.Block = "Its details block is unreadable: it is removed, and no details are written.", was.Block
	case was.Unreadable:
		r.DetailsSay, r.Block = "Its details block is unreadable: these replace it.", was.Block
	}
	if after.Free != nil && *after.Free != was.Free {
		r.NotesChange = &notesChange{Before: was.Free, After: *after.Free}
	}
}

// envReview describes a create or add: a row per machine, and what the
// guest agent sets on the Windows and Linux hosts.
func envReview(op operation, in jobs.Inputs, plan *pods.Plan, lan config.LAN) *reviewView {
	r := &reviewView{Verb: op.Verb, Subject: in.Domain, Image: true, Addr: true}
	specs := map[string]pods.HostSpec{}
	router, _ := pods.ParseHostSpec(in.Router)
	for _, line := range append([]string{in.Router}, in.EnvHosts...) {
		if h, err := pods.ParseHostSpec(line); err == nil {
			specs[h.Host] = h
		}
	}
	var clones, resumes, guests []string
	var dns netip.Addr // the guests' DNS server
	atRouter := false  // whether dns is their gateway
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
	return r
}

// envEditReview describes editing an environment host: its renamed name,
// kind, address, base image, network/CD-ROM repairs, guest reconfiguration,
// and any details or free notes written.
func envEditReview(op operation, _ jobs.Inputs, plan *pods.Plan, lan config.LAN) *reviewView {
	if len(plan.Items) != 1 {
		return nil
	}
	it := plan.Items[0]
	var was pods.NotesRead
	if it.DetailsWas != nil {
		was = *it.DetailsWas
	}
	r := &reviewView{Verb: op.Verb, Subject: it.Name}
	if it.Rename != "" {
		r.Facts = append(r.Facts, reviewFact{Label: "Rename", Code: it.Name + " -> " + it.Rename, Text: "VMID " + strconv.Itoa(it.VMID) + " · " + it.Node})
	} else {
		r.Facts = append(r.Facts, reviewFact{Label: "Host", Code: it.Name, Text: "VMID " + strconv.Itoa(it.VMID) + " · " + it.Node})
	}
	if it.Notes != nil {
		router := slices.Contains(pods.Tags(it.Tags), pods.ImageRouter)
		kind := cmp.Or(it.Notes.OS, pods.ImageOther)
		if router {
			kind = pods.ImageRouter
		}
		addr := "none"
		switch {
		case router:
			addr = "." + strconv.Itoa(pods.GatewayOctet)
		case it.Notes.IP.IsValid():
			addr = lanOctet(lan, it.Notes.IP)
		}
		r.Facts = append(r.Facts,
			reviewFact{Label: "Kind", Text: kind},
			reviewFact{Label: "Address", Code: addr},
			reviewFact{Label: "Base image", Code: cmp.Or(it.Notes.Base, "none")},
		)
	}
	if slices.Contains(it.Steps, pods.StepNetwork) {
		r.Facts = append(r.Facts, reviewFact{Label: "Network", Text: "moves NICs onto " + it.Pool + "'s bridges"})
	}
	if slices.Contains(it.Steps, pods.StepCDROM) {
		r.Facts = append(r.Facts, reviewFact{Label: "CD-ROM", Text: "ejects attached install media"})
	}
	if slices.Contains(it.Steps, pods.StepCPU) {
		r.Facts = append(r.Facts, reviewFact{Label: "CPU", Code: strings.TrimPrefix(it.CPU, "custom-")})
	}
	if it.Guest != nil {
		r.Facts = append(r.Facts, reviewFact{Label: "Guest agent", Text: "sets hostname " + it.Guest.Hostname + " and address " + it.Guest.IP.String() + " inside the guest"})
	}
	if it.Details != nil {
		r.applyNotesDiff(was, *it.Details)
	}
	switch {
	case it.Blocked != "":
		r.Say, r.Blocked = sentence(it.Blocked), true
	case it.Guest != nil:
		r.Say = "Updates " + it.Name + " in " + it.Pool + " and configures its hostname and address through the guest agent."
	case slices.Contains(it.Steps, pods.StepCDROM) && len(it.Steps) == 1:
		r.Say = "Ejects the attached CD-ROM from " + it.Name + "."
	case slices.Contains(it.Steps, pods.StepCPU) && len(it.Steps) == 1:
		r.Say = "Sets the CPU of " + it.Name + " to " + strings.TrimPrefix(it.CPU, "custom-") + "."
	default:
		r.Say = "Updates " + it.Name + " in " + it.Pool + "."
	}
	return r
}

// envPowerReview describes a power action on one environment host.
func envPowerReview(_ operation, in jobs.Inputs, plan *pods.Plan, _ config.LAN) *reviewView {
	if len(plan.Items) != 1 {
		return nil
	}
	it := plan.Items[0]
	r := &reviewView{Verb: powerLabel(in.Action), Subject: it.Name, Deletes: in.Action == "stop"}
	r.Rows = []reviewRow{{Name: it.Name, Where: orDash(it.VMID) + " · " + orDashString(it.Node), Blocked: it.Blocked}}
	if it.Blocked != "" {
		r.Say, r.Blocked = sentence(it.Blocked), true
	} else {
		r.Say = consequence(pods.KindPower, in, plan.Runnable())
	}
	return r
}

// envDeleteHostReview describes stopping and deleting one environment host.
func envDeleteHostReview(op operation, _ jobs.Inputs, plan *pods.Plan, lan config.LAN) *reviewView {
	if len(plan.Items) != 1 {
		return nil
	}
	it := plan.Items[0]
	pool := poolOf(plan.Items)
	r := &reviewView{Verb: op.Verb, Subject: it.Name, Image: true, Addr: true, Deletes: true}
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
	r.Rows = []reviewRow{row}
	if it.Blocked != "" {
		r.Say, r.Blocked = sentence(it.Blocked), true
	} else {
		r.Say = "Stops and deletes " + it.Name + " in " + pool + " with its disks. The other VMs in " + pool + " stay as they are. This can't be undone."
	}
	return r
}

// tallyTeardown builds r.Rows and r.Kept from plan, calling fill on each
// row, and returns the number of VMs deleted and the sentence naming any
// leftover disks freed.
func (r *reviewView) tallyTeardown(plan *pods.Plan, fill func(pods.Item, *reviewRow)) (deletes int, diskSay []string) {
	var disks []string
	for _, it := range plan.Items {
		row := reviewRow{Name: it.Name, Where: orDash(it.VMID) + " · " + orDashString(it.Node), Blocked: it.Blocked}
		fill(it, &row)
		r.Rows = append(r.Rows, row)
		switch {
		case it.Blocked != "":
		case slices.Contains(it.Steps, pods.StepDelete):
			deletes++
		case slices.Contains(it.Steps, pods.StepFreeDisks):
			disks = append(disks, strconv.Itoa(it.VMID))
		}
	}
	r.Kept = pods.KeptLines(plan.Kept)
	if len(disks) > 0 {
		diskSay = []string{"Frees the disks deleted VMs left at VMID " + andList(disks) + "."}
	}
	return deletes, diskSay
}

// teardownReview describes tearing an environment down: a row per item
// (each VM it deletes or can't, with its image and address from its notes,
// and the disks it frees), and the VMs on its networks it leaves alone.
func teardownReview(op operation, in jobs.Inputs, plan *pods.Plan, lan config.LAN) *reviewView {
	r := &reviewView{Verb: op.Verb, Subject: in.Domain, Image: true, Addr: true, Deletes: true}
	pool := poolOf(plan.Items)
	var left pods.Environment // what the slot holds afterwards
	deletes, diskSay := r.tallyTeardown(plan, func(it pods.Item, row *reviewRow) {
		row.Router = slices.Contains(pods.Tags(it.Tags), pods.ImageRouter)
		if it.Notes != nil {
			row.Image = it.Notes.Base
			if it.Notes.IP.IsValid() {
				row.Addr = lanOctet(lan, it.Notes.IP)
			}
		}
		if row.Router {
			row.Addr = "." + strconv.Itoa(pods.GatewayOctet)
		}
		if it.Blocked != "" {
			left.Hosts = append(left.Hosts, pods.EnvHost{Name: it.Name})
		}
	})
	for _, k := range plan.Kept {
		left.Users = append(left.Users, k.Name)
	}
	var say []string
	if deletes > 0 {
		say = append(say, "Deletes "+countVMs(deletes)+" in "+pool+".")
	}
	say = append(say, diskSay...)
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

// podTeardownReview describes tearing down pods: what it takes from which
// pods, a row per item (each VM it deletes or can't, and the disks it
// frees), and the VMs it leaves, with why.
func podTeardownReview(op operation, in jobs.Inputs, plan *pods.Plan, _ config.LAN) *reviewView {
	where := podsText(plan.Teams)
	set := in.Set
	r := &reviewView{Verb: op.Verb, Subject: string(set), Deletes: true, Set: set == ""}
	what := string(set)
	if set == "" {
		r.Subject, what = where, "everything"
		if len(in.Hosts) > 0 || len(in.VMs) > 0 {
			what = "every matching VM"
		}
	}
	for _, f := range previewFacts(in, false, 0) {
		r.Facts = append(r.Facts, reviewFact{Label: f.Label, Text: f.Value})
	}
	deletes, diskSay := r.tallyTeardown(plan, func(it pods.Item, row *reviewRow) {
		if it.Host != "" {
			row.Set = it.Sets.Label()
		}
	})
	var say []string
	if deletes > 0 {
		say = append(say, "Tears down "+what+" in "+where+": "+countVMs(deletes)+".",
			byCount(deletes, "It is", "Each is")+" stopped and deleted with its disks. This can't be undone.")
	}
	r.Say = strings.Join(append(say, diskSay...), " ")
	return r
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
func imageReview(op operation, in jobs.Inputs, plan *pods.Plan, _ config.LAN) *reviewView {
	r := &reviewView{Verb: op.Verb, Subject: cmp.Or(in.Image, in.Family)}
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
		from := "the newest version"
		if len(r.Rows) == 1 && r.Rows[0].Image != "" {
			from = r.Rows[0].Image
		}
		r.Say = "Copies " + from + " as " + names + " on the edit network, for you to patch and register as the next version."
	case pods.KindImageDeprecate:
		r.Say = names + " will no longer be offered for new hosts. Hosts made from it keep working."
	case pods.KindImageDelete:
		r.Say = names + " will be deleted. Hosts made from it keep working: they are full copies. This can't be undone."
	}
	return r
}

// reviewBack is where a review's Back goes: its operation's back page when
// set, else the form filled in.
func reviewBack(op operation, in jobs.Inputs) string {
	if op.back != nil {
		return op.back(op, in)
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
