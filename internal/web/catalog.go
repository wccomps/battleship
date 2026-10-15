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
	ReadErr  string // why the cluster couldn't be read
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
	p.Envs = envs
	p.Images = pods.BaseImages(vms)
	p.Families = pods.FamilyNames(p.Images)
	p.Routers = pods.Families(p.Images, pods.ImageRouter)
	for _, k := range []string{pods.ImageWindows, pods.ImageLinux, pods.ImageOther} {
		p.Others = append(p.Others, pods.Families(p.Images, k)...)
	}
	return p
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
	p.Images = s.planner(r.Context()).ReadNotes(r.Context(), p.Images)
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
	s.render(w, r, status, "teamless", s.newView(w, r, op.Title, string(op.Kind), p))
}
