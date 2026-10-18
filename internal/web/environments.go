package web

import (
	"cmp"
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// envsView is the Environments page: a block per slot in use, then the free
// slots on one line.
type envsView struct {
	ReadErr string
	Envs    []envBlock
	// Free are the slots Create may take, in runs: "dev-02, dev-05 – dev-10";
	// Create is the first. Queued are free slots a queued job will fill.
	Free    string
	Create  string
	Queued  string
	FreeErr string // why the queued jobs couldn't be read
	// MayCreate, MayAdd, MayDNS and MayTearDown say whether the user holds
	// each action's privilege anywhere; Lacks says, a line each, what they
	// lack.
	MayCreate, MayAdd, MayDNS, MayTearDown bool
	Lacks                                  []string
	// MayLive says the page offers live addresses: there are hosts, and
	// the user may read guest agents somewhere. LiveRead says it has them.
	MayLive, LiveRead bool
}

// envAddrsPath serves the Environments page's address cells with live
// addresses, as pieces for the script to swap in (environmentAddrs);
// catalog.html names it too.
const envAddrsPath = envsPath + "/addresses"

// The Environments page's guest agent reads, bounded so a hung agent
// doesn't hold up the page.
const (
	addrReads       = 4
	addrReadTimeout = 2 * time.Second
	addrReadsWithin = 5 * time.Second
)

// noLiveAddrs says why a user without pods.LiveAddrsPrivilege sees no live
// addresses.
const noLiveAddrs = "Showing live addresses needs " + pods.LiveAddrsPrivilege + ", which you don't have."

// piecesHeader marks a response of pieces, so the script (app.js, which
// names it too) can tell it from anything else (e.g. the login page).
const piecesHeader = "X-Battleship-Pieces"

// envBlock is a slot in use. Domain is set when its VMs share one: Add
// hosts, Set DNS and Tear down then act on it (Acts), unless Twin names the
// slot with the same domain that they act on instead (pods.EnvByDomain).
type envBlock struct {
	ID     string // its anchor: the domain, or slot-NN
	Domain string
	Slot   string // its pool, e.g. dev-01
	Router string
	DNS    string
	Hosts  []envHostRow
	// Users are VMs outside its pool with a NIC on its networks.
	Users []string
	Mixed bool
	Twin  string
}

// Acts reports whether the block offers Add hosts, Set DNS and Tear down.
func (b envBlock) Acts() bool { return b.Domain != "" && b.Twin == "" }

// envHostRow is a VM in a slot: its name (the host part, or the whole VM
// name in a block without one domain), address, power, image, details and
// notes. Its details are those of its own notes, which a clone copies from
// its base image; else, while its base image is there, the image's.
//
// Read says its live addresses were read (pods.LiveAddrs): its address cell
// shows them, or that it isn't live.
type envHostRow struct {
	VMID       int
	Name       string
	Router     bool
	Addr       string
	Running    bool
	Status     string
	Image      string
	Details    pods.ImageDetails
	Unreadable bool
	Notes      string
	Read       bool
	pods.LiveAddrs
}

// environmentsPage is the Environments page with each host's assigned
// address; with ?live=1, its live ones too, which the script otherwise
// fills in after the page loads (environmentAddrs).
func (s *Server) environmentsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := envsView{MayCreate: s.mayRun(ctx, pods.KindEnvCreate), MayAdd: s.mayRun(ctx, pods.KindEnvAdd), MayDNS: s.mayRun(ctx, pods.KindEnvDNS),
		MayTearDown: s.mayRun(ctx, pods.KindEnvTeardown), MayLive: s.mayAnywhere(ctx, pods.LiveAddrsPrivilege)}
	planner := s.planner(ctx)
	envs, vms, err := planner.Environments(ctx)
	if err != nil {
		p.ReadErr = "Couldn't read the cluster: " + sentence(proxmox.Describe(err))
	} else {
		var addrs map[int]pods.LiveAddrs
		if p.MayLive && r.URL.Query().Get("live") == "1" {
			addrs, p.LiveRead = s.liveAddrs(ctx, envs), true
		}
		p.Envs = envBlocks(envs, s.hostBases(ctx, envs, vms), addrs)
		open, queued, err := planner.OpenSlots(ctx, envs)
		if err != nil {
			p.FreeErr = "Couldn't tell which free slots queued jobs will fill: " + sentence(err.Error())
		}
		p.Free, p.Queued = slotRuns(open), slotRuns(queued)
		if len(open) > 0 {
			p.Create = open[0].Pool
		}
	}
	if !p.MayCreate {
		p.Lacks = append(p.Lacks, lacksOf(pods.KindEnvCreate, ""))
	}
	if !slices.ContainsFunc(p.Envs, func(b envBlock) bool { return len(b.Hosts) > 0 }) {
		p.MayLive = false // no address to show
	} else if !p.MayLive {
		p.Lacks = append(p.Lacks, noLiveAddrs)
	}
	if slices.ContainsFunc(p.Envs, envBlock.Acts) {
		if !p.MayAdd {
			p.Lacks = append(p.Lacks, lacksOf(pods.KindEnvAdd, ""))
		}
		if !p.MayDNS {
			p.Lacks = append(p.Lacks, lacksOf(pods.KindEnvDNS, ""))
		}
		if !p.MayTearDown {
			p.Lacks = append(p.Lacks, lacksOf(pods.KindEnvTeardown, ""))
		}
	}
	s.render(w, r, http.StatusOK, "catalog", s.newView(w, r, "Environments", "environments", p))
}

// environmentAddrs serves the Environments page's address cells, and the
// line above them, with live addresses, each in its own <template> (piece).
// A user who may read no guest agent gets what the page says instead.
func (s *Server) environmentAddrs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.mayAnywhere(ctx, pods.LiveAddrsPrivilege) {
		http.Error(w, noLiveAddrs, http.StatusForbidden)
		return
	}
	envs, _, err := s.planner(ctx).Environments(ctx)
	if err != nil {
		http.Error(w, "Couldn't read the cluster: "+sentence(proxmox.Describe(err)), http.StatusBadGateway)
		return
	}
	var b strings.Builder
	add := func(name string, data any) bool {
		h, err := fragment("catalog", name, data)
		if err != nil {
			s.logf("web: %v", err)
			http.Error(w, "internal error rendering the addresses", http.StatusInternalServerError)
			return false
		}
		b.WriteString(piece(h))
		return true
	}
	for _, block := range envBlocks(envs, nil, s.liveAddrs(ctx, envs)) {
		for _, row := range block.Hosts {
			if !add("env-addr", row) {
				return
			}
		}
	}
	if !add("env-live", envsView{MayLive: true, LiveRead: true}) {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set(piecesHeader, "1")
	_, _ = io.WriteString(w, b.String()) // the client went away; nothing to do
}

// liveAddrs are envs' hosts' live addresses by VMID, read through the
// user's own credential.
func (s *Server) liveAddrs(ctx context.Context, envs []pods.Environment) map[int]pods.LiveAddrs {
	ctx, cancel := context.WithTimeout(ctx, s.addrWithin)
	defer cancel()
	return s.planner(ctx).LiveAddrs(ctx, envs, addrReads, s.addrTimeout)
}

// hostBases are, by name, the base images of envs's hosts whose own notes
// have no details, with their notes read.
func (s *Server) hostBases(ctx context.Context, envs []pods.Environment, vms []proxmox.VM) map[string]pods.BaseImage {
	want := map[string]bool{}
	for _, e := range envs {
		for _, h := range e.Hosts {
			if h.Details.Empty() && !h.Unreadable && h.Notes.Base != "" {
				want[h.Notes.Base] = true
			}
		}
	}
	var images []pods.BaseImage
	for _, im := range pods.BaseImages(vms) {
		if want[im.Name] {
			images = append(images, im)
		}
	}
	out := map[string]pods.BaseImage{}
	for _, im := range s.planner(ctx).ReadNotes(ctx, images) {
		out[im.Name] = im
	}
	return out
}

// envBlocks are the slots in use, in slot order; bases are the hosts' base
// images by name, for hosts whose notes have no details (hostBases), and
// addrs their live addresses by VMID, or nil if they weren't read.
func envBlocks(envs []pods.Environment, bases map[string]pods.BaseImage, addrs map[int]pods.LiveAddrs) []envBlock {
	var out []envBlock
	for _, e := range envs {
		if e.Free() {
			continue
		}
		b := envBlock{ID: "slot-" + e.Slot.Name, Domain: e.Domain, Slot: e.Slot.Pool, Users: e.Users, Mixed: e.Mixed}
		if e.Domain != "" {
			if first, _ := pods.EnvByDomain(envs, e.Domain); first.Slot.N != e.Slot.N {
				b.Twin = first.Slot.Pool
			} else {
				b.ID = e.Domain
			}
			b.DNS = e.DNS()
		}
		for _, h := range e.Hosts {
			row := envHostRow{VMID: h.VMID, Name: h.Name, Router: h.Router, Addr: h.Addr, Running: h.Status == "running",
				Status: cmp.Or(h.Status, "unknown"), Image: h.Notes.Base, Details: h.Details, Unreadable: h.Unreadable, Notes: h.UserNotes,
				Read: addrs != nil, LiveAddrs: addrs[h.VMID]}
			if base, ok := bases[h.Notes.Base]; ok && h.Details.Empty() && !h.Unreadable && !base.Unreadable {
				row.Details = base.Details
			}
			if e.Domain != "" {
				row.Name = h.Host
			}
			if h.Router && b.Router == "" {
				b.Router = row.Name
			}
			b.Hosts = append(b.Hosts, row)
		}
		for _, h := range e.HalfDeleted {
			b.Hosts = append(b.Hosts, envHostRow{VMID: h.VMID, Name: h.Name, Status: cmp.Or(h.Status, "unknown")})
		}
		slices.SortStableFunc(b.Hosts, hostOrder)
		out = append(out, b)
	}
	return out
}

// hostOrder is by address, the router's .2 first, then hosts without one,
// by name.
func hostOrder(a, b envHostRow) int {
	oa, ob := pods.LastOctet(a.Addr), pods.LastOctet(b.Addr)
	switch {
	case (oa == 0) != (ob == 0):
		if oa == 0 {
			return 1
		}
		return -1
	case oa != ob:
		return cmp.Compare(oa, ob)
	}
	return strings.Compare(a.Name, b.Name)
}

// slotRuns names slots by pool, a run of three or more in a row as its
// first and last: dev-02, dev-03, dev-05 – dev-10.
func slotRuns(slots []pods.Slot) string {
	var out []string
	for i := 0; i < len(slots); {
		j := i
		for j+1 < len(slots) && slots[j+1].N == slots[j].N+1 {
			j++
		}
		if j-i >= 2 {
			out = append(out, slots[i].Pool+" – "+slots[j].Pool)
		} else {
			for k := i; k <= j; k++ {
				out = append(out, slots[k].Pool)
			}
		}
		i = j + 1
	}
	return strings.Join(out, ", ")
}

// inEnv serves next for op opened for the environment its path names
// (operation.at). A path that can't name one goes back to Environments
// without reading the cluster.
func (s *Server) inEnv(op operation, next func(operation) http.Handler) http.Handler {
	if !op.Kind.OnEnvironment() {
		return next(op)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		domain := r.PathValue("domain")
		if pods.CheckDomain(domain) != nil {
			s.noEnv(w, r, "")
			return
		}
		next(op.at(domain)).ServeHTTP(w, r)
	})
}

// noEnv sends an environment operation without an environment (domain, if
// it names one that doesn't exist) back to Environments, saying why.
func (s *Server) noEnv(w http.ResponseWriter, r *http.Request, domain string) {
	msg := "Add hosts, Set DNS and Tear down start from an environment: pick one below."
	if domain != "" {
		msg = "There's no environment " + domain + ". It may have been torn down or renamed; pick one below."
	}
	s.setFlash(w, flashError, msg)
	http.Redirect(w, r, envsPath, http.StatusSeeOther)
}

// oldEnvRoutes send op's old addresses, which named the environment in a
// domain field, to its path: links to the form, and a preview or confirm
// posted from a page opened before, with its body.
func (s *Server) oldEnvRoutes(mux *http.ServeMux, op operation) {
	old := strings.Replace(op.Path, "/{domain}", "", 1)
	mux.Handle("GET "+old, s.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		domain := strings.TrimSpace(q.Get(fieldDomain))
		if pods.CheckDomain(domain) != nil {
			s.noEnv(w, r, "")
			return
		}
		q.Del(fieldDomain)
		to := op.at(domain).Path
		if len(q) > 0 {
			to += "?" + q.Encode()
		}
		http.Redirect(w, r, to, http.StatusMovedPermanently)
	})))
	for _, step := range []string{"/preview", "/confirm"} {
		mux.Handle("POST "+old+step, s.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			domain := strings.TrimSpace(postedForm(r).Get(fieldDomain))
			if pods.CheckDomain(domain) != nil {
				s.noEnv(w, r, "")
				return
			}
			http.Redirect(w, r, op.at(domain).Path+step, http.StatusTemporaryRedirect)
		})))
	}
}
