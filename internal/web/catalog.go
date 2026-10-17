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
	Subnet   string
	ReadErr  string // why the cluster couldn't be read
	// Env is the environment an env-add or env-dns form works on.
	Env *pods.Environment
	// DNSCards are Set DNS's choices; DNSHint says whose DNS it changes.
	DNSCards []dnsCard
	DNSHint  string
	envForm
}

// newCatalogPage reads environments and images as the user, without the
// images' notes: only the pages that show them read them (withNotes).
func (s *Server) newCatalogPage(ctx context.Context) catalogPage {
	p := catalogPage{Subnet: s.cfg.Environments.Subnet}
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

// StaleDNS is the environment's DNS choice if it can no longer be chosen
// (its host is gone), or "".
func (p catalogPage) StaleDNS() string {
	if p.Env == nil {
		return ""
	}
	cur := p.Env.DNS()
	if slices.Contains(p.Env.DNSChoices(), cur) {
		return ""
	}
	return cur
}

// dnsCard is a Set DNS choice (pods.Environment.DNSChoices): the router or
// a host, its address and image. Current is the environment's choice;
// Checked, the one the form shows chosen.
type dnsCard struct {
	Value, Name, Addr, Image string
	Router, Current, Checked bool
}

// dnsForm fills in Set DNS's cards, In.DNS (else the current choice)
// checked, and the hint naming the hosts whose DNS it sets.
func (p *catalogPage) dnsForm(gateway string) {
	e := p.Env
	cur := e.DNS()
	chosen := cmp.Or(p.In.DNS, cur)
	choices := e.DNSChoices()
	for _, c := range choices {
		card := dnsCard{Value: c, Current: c == cur, Checked: c == chosen}
		if c == pods.DNSRouter {
			card.Router, card.Addr = true, gateway
			if r, ok := e.Host(e.RouterName()); ok {
				card.Name, card.Image = r.Host, r.Notes.Base
			}
		} else if h, ok := e.Host(c + "." + e.Domain); ok {
			card.Name, card.Addr, card.Image = c, h.Notes.IP, h.Notes.Base
		}
		p.DNSCards = append(p.DNSCards, card)
	}
	set := choices[1:]
	var keep []string // the hosts that keep their own settings
	for _, h := range e.Hosts {
		switch {
		case h.Router:
			keep = append([]string{"the router"}, keep...)
		case !slices.Contains(set, h.Host):
			keep = append(keep, h.Host)
		}
	}
	if len(set) == 0 {
		p.DNSHint = "No host here is a Windows or Linux host with an address: the choice is only recorded."
		return
	}
	p.DNSHint = "Changes " + andList(set) + " through the guest agent."
	if len(keep) > 0 {
		p.DNSHint += " " + sentence(andList(keep)+byCount(len(keep), " keeps its", " keep their")+" own settings")
	}
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
			if op.Kind == pods.KindEnvDNS {
				p.dnsForm(s.gateway())
			}
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
