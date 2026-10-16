package web

import (
	"context"
	"net/http"
	"slices"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// catalogPage is the data of the Images and Environments pages and of their
// forms.
type catalogPage struct {
	Op       operation // set on forms
	In       jobs.Inputs
	Error    string
	Envs     []pods.Environment
	Images   []pods.BaseImage // every version, by family then version
	Routers  []pods.BaseImage // newest usable version of each router family
	Others   []pods.BaseImage // newest usable version of each other family
	Families []string         // every family, once
	Kinds    []string
	Subnet   string
	ReadErr  string              // why the cluster couldn't be read
	Ready    []pods.Unregistered // VMs ready to register (the images page)
	vms      []proxmox.VM
}

// newCatalogPage reads environments and images as the user, without the
// images' notes: only the pages that show them read them (withNotes).
func (s *Server) newCatalogPage(ctx context.Context) catalogPage {
	p := catalogPage{Kinds: pods.ImageKinds, Subnet: s.cfg.Environments.Subnet}
	envs, vms, err := s.planner(ctx).Environments(ctx)
	if err != nil {
		p.ReadErr = "Couldn't read the cluster: " + sentence(proxmox.Describe(err))
		return p
	}
	p.Envs, p.vms = envs, vms
	p.Images = pods.BaseImages(vms)
	p.Families = pods.FamilyNames(p.Images)
	p.Routers = pods.Families(p.Images, pods.ImageRouter)
	for _, k := range []string{pods.ImageWindows, pods.ImageLinux, pods.ImageOther} {
		p.Others = append(p.Others, pods.Families(p.Images, k)...)
	}
	return p
}

// Env is the environment the form's domain names, if it is one whose hosts
// share that domain.
func (p catalogPage) Env() *pods.Environment {
	for i, e := range p.Envs {
		if p.In.Domain != "" && e.Domain == p.In.Domain && !e.Mixed {
			return &p.Envs[i]
		}
	}
	return nil
}

// EnvDomains are the environments a form can pick from.
func (p catalogPage) EnvDomains() []string {
	var out []string
	for _, e := range p.Envs {
		if e.Domain != "" && !e.Mixed {
			out = append(out, e.Domain)
		}
	}
	return out
}

// CurrentImages are the versions that aren't deprecated.
func (p catalogPage) CurrentImages() []pods.BaseImage {
	return slices.DeleteFunc(slices.Clone(p.Images), func(im pods.BaseImage) bool { return im.Deprecated })
}

// DeprecatedImages are the deprecated versions, which the images page folds
// away.
func (p catalogPage) DeprecatedImages() []pods.BaseImage {
	return slices.DeleteFunc(slices.Clone(p.Images), func(im pods.BaseImage) bool { return !im.Deprecated })
}

// catalogView is a catalog page showing "images" or "environments".
type catalogView struct {
	catalogPage
	Show string
}

func (s *Server) environmentsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "catalog", s.newView(w, r, "Environments", "environments",
		catalogView{s.newCatalogPage(r.Context()), "environments"}))
}

func (s *Server) imagesPage(w http.ResponseWriter, r *http.Request) {
	p := s.newCatalogPage(r.Context())
	planner := s.planner(r.Context())
	p.Images = planner.ReadNotes(r.Context(), p.Images)
	p.Ready = planner.ReadUnregistered(r.Context(), pods.UnregisteredVMs(p.vms, s.cfg.Images.Pool, p.Images))
	s.render(w, r, http.StatusOK, "catalog", s.newView(w, r, "Images", "images", catalogView{p, "images"}))
}

// withNotes reads the notes of the images an environment form lists.
func (s *Server) withNotes(ctx context.Context, p catalogPage) catalogPage {
	both := s.planner(ctx).ReadNotes(ctx, append(slices.Clone(p.Routers), p.Others...))
	p.Routers, p.Others = both[:len(p.Routers)], both[len(p.Routers):]
	return p
}

// teamlessForm serves an image or environment form, prefilled from the
// query (the pages' links) or the inputs a preview refused.
func (s *Server) teamlessForm(w http.ResponseWriter, r *http.Request, status int, op operation, in jobs.Inputs, problem string) {
	p := s.newCatalogPage(r.Context())
	if op.Kind == pods.KindEnvCreate || op.Kind == pods.KindEnvAdd {
		p = s.withNotes(r.Context(), p)
	}
	p.Op, p.In, p.Error = op, in, problem
	if (op.Kind == pods.KindEnvAdd || op.Kind == pods.KindEnvDNS) && in.Domain != "" && p.Env() == nil && p.Error == "" && p.ReadErr == "" {
		p.Error = "There's no environment " + in.Domain + "; pick one."
	}
	s.render(w, r, status, "teamless", s.newView(w, r, op.Title, string(op.Kind), p))
}
