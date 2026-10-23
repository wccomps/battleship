package web

import (
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
	Subnet   string
	ReadErr  string // why the cluster couldn't be read
	// Env is the environment an env-add form works on.
	Env *pods.Environment
	// Teardown are the environments a Tear down environment form offers.
	Teardown []teardownEnv
	// Details fill a register's, an Edit notes's, and an Edit host's fields:
	// those given, else those of the VM's notes or the family's newest version.
	Details pods.ImageNotes
	// Edit is what an Edit notes form edits (notesForm).
	Edit *notesEdit
	// Host is what an Edit host form edits (hostEditPrefill).
	Host *hostEdit
	envForm
	vms     []proxmox.VM
	readErr error
}

// newCatalogPage reads images and, when needEnvs is set, environments
// as the user, without the images' notes: only the pages that show them
// read them (withNotes).
func (s *Server) newCatalogPage(ctx context.Context, needEnvs bool) catalogPage {
	p := catalogPage{Subnet: s.cfg.Environments.Subnet.String()}
	var vms []proxmox.VM
	var err error
	if needEnvs {
		p.Envs, vms, err = s.planner(ctx).Environments(ctx)
	} else {
		vms, err = s.planner(ctx).API.ClusterVMs(ctx)
	}
	if err != nil {
		p.readErr = err
		p.ReadErr = "Couldn't read the cluster: " + sentence(proxmox.Describe(err))
		return p
	}
	p.vms = vms
	p.Images = pods.BaseImages(vms)
	p.Families = pods.FamilyNames(p.Images)
	p.Routers = pods.Families(p.Images, pods.ImageRouter)
	for _, k := range []string{pods.ImageWindows, pods.ImageLinux, pods.ImageOther} {
		p.Others = append(p.Others, pods.Families(p.Images, k)...)
	}
	return p
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
// the request (newEnvForm); not ok if op's environment isn't there.
func (s *Server) catalogForm(r *http.Request, op operation, in jobs.Inputs, problem string) (catalogPage, bool) {
	ctx := r.Context()
	p := s.newCatalogPage(ctx, op.Machines || op.Kind.OnEnvironment())
	p.Op, p.In, p.Error = op, in, problem
	if op.domain != "" {
		e, ok := pods.EnvByDomain(p.Envs, in.Domain)
		if !ok && p.ReadErr == "" {
			return p, false
		}
		if ok {
			p.Env = &e
		}
	}
	if op.prefill != nil {
		op.prefill(s, ctx, &p)
	}
	if d := in.Details; d != nil {
		p.Details.ImageDetails, p.Details.From = d.ImageDetails, d.From
		if d.Free != nil {
			p.Details.Free = d.Free
		}
	}
	if op.Machines {
		form := r.URL.Query()
		if r.Method == http.MethodPost {
			form = postedForm(r)
		}
		p.envForm = s.newEnvForm(ctx, p, form)
	}
	return p, true
}

func (s *Server) renderCatalogForm(w http.ResponseWriter, r *http.Request, status int, p catalogPage) {
	if p.Op.Machines {
		p = s.withNotes(r.Context(), p)
	}
	s.render(w, r, status, "teamless", s.newView(w, r, p.Op.Title, p.Op.nav(), p))
}

// Back is where the form's close and Cancel go: an Edit notes on a
// version goes to its family's page, and an Edit host to its host sheet.
func (p catalogPage) Back() string {
	if p.Edit != nil && p.Edit.Image != "" {
		_, href := p.Op.home(p.In)
		return href
	}
	if p.Host != nil && p.Host.Name != "" {
		return envHostHref(p.Host.VMID)
	}
	return p.Op.listPage(p.In)
}
