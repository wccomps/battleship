package web

import (
	"cmp"
	"context"
	"net/http"
	"slices"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// catalogPage is the data of the image and environment forms.
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
	// Env is the environment an env-add or env-dns form works on.
	Env *pods.Environment
	envForm
	vms []proxmox.VM
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

// withNotes reads the notes of the images an environment form lists.
func (s *Server) withNotes(ctx context.Context, p catalogPage) catalogPage {
	both := s.planner(ctx).ReadNotes(ctx, append(slices.Clone(p.Routers), p.Others...))
	p.Routers, p.Others = both[:len(p.Routers)], both[len(p.Routers):]
	return p
}

// teamlessForm serves an image or environment form, prefilled from the
// query (the pages' links) or the inputs a preview refused. An environment
// that isn't there (any more) sends the user back to Environments.
func (s *Server) teamlessForm(w http.ResponseWriter, r *http.Request, status int, op operation, in jobs.Inputs, problem string) {
	p, ok := s.catalogForm(r, op, in, problem)
	if !ok {
		s.noEnv(w, r, in.Domain)
		return
	}
	s.renderCatalogForm(w, r, status, p)
}

// catalogForm is the data of op's form for in, its machine rows read from
// the request (machineRows); not ok if op's environment isn't there.
func (s *Server) catalogForm(r *http.Request, op operation, in jobs.Inputs, problem string) (catalogPage, bool) {
	ctx := r.Context()
	p := s.newCatalogPage(ctx)
	p.Op, p.In, p.Error = op, in, problem
	if op.Kind.OnEnvironment() {
		e, ok := pods.EnvByDomain(p.Envs, in.Domain)
		if !ok && p.ReadErr == "" {
			return p, false
		}
		if ok {
			p.Env = &e
		}
	}
	if op.Kind == pods.KindEnvCreate || op.Kind == pods.KindEnvAdd {
		form := r.URL.Query()
		if r.Method == http.MethodPost {
			form = postedForm(r)
		}
		p.envForm = s.newEnvForm(ctx, p, form)
	}
	return p, true
}

func (s *Server) renderCatalogForm(w http.ResponseWriter, r *http.Request, status int, p catalogPage) {
	if p.Op.Kind == pods.KindEnvCreate || p.Op.Kind == pods.KindEnvAdd {
		p = s.withNotes(r.Context(), p)
	}
	s.render(w, r, status, "teamless", s.newView(w, r, p.Op.Title, string(p.Op.Kind), p))
}

// Back is where the form's close and Cancel go.
func (p catalogPage) Back() string { return listPage(p.In) }
