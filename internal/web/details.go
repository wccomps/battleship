package web

import (
	"context"

	"github.com/wccomps/battleship/internal/jobs"
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

// registerDetails are the details the register form starts with for VM
// vmid: those its notes give (pods.RegisterDetails), or none if it can't
// be read.
func (s *Server) registerDetails(ctx context.Context, vmid int) pods.ImageDetails {
	planner := s.planner(ctx)
	vms, err := planner.API.ClusterVMs(ctx)
	if err != nil {
		return pods.ImageDetails{}
	}
	for _, vm := range vms {
		if vm.VMID == vmid {
			cfg, err := planner.API.VMConfig(ctx, vm.Node, vm.VMID)
			if err != nil {
				return pods.ImageDetails{}
			}
			d, _ := pods.RegisterDetails(cfg["description"])
			return d
		}
	}
	return pods.ImageDetails{}
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

// notesForm is the data of Edit notes's form for in: the family's newest
// version's details and free text, or those a refused preview posted. It
// reads the cluster's VMs and that version's config, nothing more.
func (s *Server) notesForm(ctx context.Context, op operation, in jobs.Inputs, problem string) catalogPage {
	p := catalogPage{Op: op, In: in, Error: problem, Edit: &notesEdit{}}
	if in.Family == "" {
		p.Edit.Err = "Choose the base image whose notes to edit: each family on Images has Edit."
		return p
	}
	at, err := s.planner(ctx).NotesToEdit(ctx, in.Family)
	p.Edit.Image = at.Image.Name
	if err != nil {
		p.Edit.Err = "Couldn't read the notes to edit: " + sentence(proxmox.Describe(err))
		return p
	}
	p.Edit.Was, p.Details = at.Read, at.Read.Edit()
	if d := in.Details; d != nil {
		p.Details.ImageDetails = d.ImageDetails
		if d.Free != nil {
			p.Details.Free = d.Free
		}
	}
	return p
}

// Notes is the free text the form's notes field holds.
func (p catalogPage) Notes() string {
	if p.Details.Free == nil {
		return ""
	}
	return *p.Details.Free
}

// NoSave is whether the form offers no Review: an Edit notes with nothing
// to edit.
func (p catalogPage) NoSave() bool { return p.Edit != nil && p.Edit.Err != "" }
