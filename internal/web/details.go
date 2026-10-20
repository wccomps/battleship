package web

import (
	"context"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// detailsView is an image's details as pages show them (the "details"
// template): Login first, its password hidden until asked for, then the
// other fields in the block's order. Unreadable says the notes hold a
// details block Battleship can't read; the page shows the notes as they
// are instead.
type detailsView struct {
	User, Password string
	Rows           []detailRow
	Unreadable     bool
}

// detailRow is a detail other than the login.
type detailRow struct{ Label, Value string }

// detailsOf is d as a page shows it (the template function "details"), or
// nil when there is nothing to show.
func detailsOf(d pods.ImageDetails, unreadable bool) *detailsView {
	if d.Empty() && !unreadable {
		return nil
	}
	v := &detailsView{User: d.User, Password: d.Password, Unreadable: unreadable}
	for _, k := range pods.DetailKeys {
		if val := d.Value(k); k != "Login" && val != "" {
			v.Rows = append(v.Rows, detailRow{k, val})
		}
	}
	return v
}

// detailChange is a detail an Edit notes review shows before and after;
// Password shows them behind the password's toggle.
type detailChange struct {
	Label, Before, After string
	Password, Changed    bool
}

// detailChanges are the details set before or after, login first.
func detailChanges(before, after pods.ImageDetails) []detailChange {
	var out []detailChange
	add := func(label, b, a string, password bool) {
		if b != "" || a != "" {
			out = append(out, detailChange{Label: label, Before: b, After: a, Password: password, Changed: b != a})
		}
	}
	add("User", before.User, after.User, false)
	add("Password", before.Password, after.Password, true)
	for _, k := range pods.DetailKeys {
		if k != "Login" {
			add(k, before.Value(k), after.Value(k), false)
		}
	}
	return out
}

// registerPrefill fills the register form's details from VM vmid's notes
// when none were posted.
func (s *Server) registerPrefill(ctx context.Context, p *catalogPage) {
	if p.In.Details == nil && p.In.VMID != 0 {
		p.Details = pods.ImageNotes{ImageDetails: s.planner(ctx).SourceDetails(ctx, p.In.VMID)}
	}
}

// notesEdit is what an Edit notes form edits: the version it writes and
// what editing starts from (pods.NotesToEdit). Err says why there is
// nothing to edit; the form then offers no save, so it can't post empty
// fields over notes it never read.
type notesEdit struct {
	Image string
	Was   pods.NotesRead
	Err   string
}

// notesPrefill fills Edit notes's form with the family's newest version's
// details and free text, or those a refused preview posted.
func (s *Server) notesPrefill(ctx context.Context, p *catalogPage) {
	p.ReadErr, p.Edit = "", &notesEdit{}
	if p.In.Family == "" {
		p.Edit.Err = "Choose the base image whose notes to edit: each family on Images has Edit."
		return
	}
	at, err := s.planner(ctx).NotesToEdit(ctx, p.In.Family)
	p.Edit.Image = at.Image.Name
	if err != nil {
		p.Edit.Err = "Couldn't read the notes to edit: " + sentence(proxmox.Describe(err))
		return
	}
	p.Edit.Was, p.Details = at.Read, at.Edit()
	if d := p.In.Details; d != nil {
		p.Details.ImageDetails, p.Details.From = d.ImageDetails, d.From
		if d.Free != nil {
			p.Details.Free = d.Free
		}
	}
}

// NoSave is whether the form offers no Review: an Edit notes or Edit host
// with nothing to edit.
func (p catalogPage) NoSave() bool {
	return (p.Edit != nil && p.Edit.Err != "") || (p.Host != nil && p.Host.Err != "")
}
