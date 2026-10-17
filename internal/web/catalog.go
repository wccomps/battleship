package web

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"

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
	ReadErr  string                   // why the cluster couldn't be read
	Ready    []pods.RegisterCandidate // VMs to register from (the images page)
	// Env is the environment an env-add or env-dns form works on; Picking,
	// that the form must first ask which.
	Env     *pods.Environment
	Picking bool
	// ExtraRows is how many empty host rows to add: one after Add host
	// without JavaScript.
	ExtraRows int
	vms       []proxmox.VM
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

// EnvDomains are the environments a form can pick from, each once.
func (p catalogPage) EnvDomains() []string {
	var out []string
	for _, e := range p.Envs {
		if e.Domain != "" && !slices.Contains(out, e.Domain) {
			out = append(out, e.Domain)
		}
	}
	return out
}

// AsksRouter is whether the form has a router field: a new environment
// needs one; Add hosts takes one for an environment without a router, or to
// resume the one a failed create left (see pods.EnvAdd), when one was given.
func (p catalogPage) AsksRouter() bool {
	switch p.Op.Kind {
	case pods.KindEnvCreate:
		return true
	case pods.KindEnvAdd:
		return p.Env != nil && (!p.Env.HasRouter() || p.In.Router != "")
	}
	return false
}

// StaleDNS is the environment's DNS choice if it can no longer be chosen
// (its host is gone), or "".
func (p catalogPage) StaleDNS() string {
	if p.Env == nil {
		return ""
	}
	cur := cmp.Or(p.In.DNS, p.Env.DNS())
	if slices.Contains(p.Env.DNSChoices(), cur) {
		return ""
	}
	return cur
}

// Carry is what the picker step carries to the form it leads to: what was
// already typed, less the domain it asks for.
func (p catalogPage) Carry() []field {
	return slices.DeleteFunc(inputFields(p.In), func(f field) bool { return f.Name == fieldDomain })
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
	p.Ready = planner.RegisterCandidates(r.Context(), p.vms, p.Images)
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
	p.Op, p.In, p.Error = op, in, problem
	if r.FormValue(fieldMore) == yes {
		p.ExtraRows = 1
	}
	if op.Kind.OnEnvironment() {
		if e, ok := pods.EnvByDomain(p.Envs, in.Domain); ok {
			p.Env = &e
		} else {
			p.Picking = true
			if in.Domain != "" && p.Error == "" && p.ReadErr == "" {
				p.Error = "There's no environment " + in.Domain + "; pick one."
			}
		}
	}
	if (op.Kind == pods.KindEnvCreate || op.Kind == pods.KindEnvAdd) && !p.Picking {
		p = s.withNotes(r.Context(), p)
	}
	s.render(w, r, status, "teamless", s.newView(w, r, op.Title, string(op.Kind), p))
}

// hostRow is one row of the environment forms: a host's name, base image
// family and optional last address octet.
type hostRow struct{ Name, Image, Octet string }

// rowOf splits a stored name=image[:octet] line into a row.
func rowOf(spec string) hostRow {
	name, rest, _ := strings.Cut(spec, "=")
	image, octet, _ := strings.Cut(rest, ":")
	return hostRow{strings.TrimSpace(name), strings.TrimSpace(image), strings.TrimSpace(octet)}
}

// HostRows are the form's host rows: the hosts given, then ExtraRows empty
// ones, and at least one.
func (p catalogPage) HostRows() []hostRow {
	var rows []hostRow
	for _, spec := range p.In.EnvHosts {
		rows = append(rows, rowOf(spec))
	}
	for range p.ExtraRows {
		rows = append(rows, hostRow{})
	}
	if len(rows) == 0 {
		rows = append(rows, hostRow{})
	}
	return rows
}

// RouterRow is the router given, or the usual name, gw, with no image yet.
func (p catalogPage) RouterRow() hostRow {
	if p.In.Router == "" {
		return hostRow{Name: "gw"}
	}
	return rowOf(p.In.Router)
}
