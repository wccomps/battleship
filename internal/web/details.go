package web

import (
	"context"

	"github.com/wccomps/battleship/internal/pods"
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

// detailChange is a detail an Edit details review shows before and after;
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

// currentDetails are the details of family's newest version, which Edit
// details starts from; none if it can't be read.
func (s *Server) currentDetails(ctx context.Context, images []pods.BaseImage, family string) pods.ImageDetails {
	newest, ok := pods.Newest(images, family)
	if !ok {
		return pods.ImageDetails{}
	}
	return s.planner(ctx).ReadNotes(ctx, []pods.BaseImage{newest})[0].Details
}
