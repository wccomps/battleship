package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// envHostsPath is where each environment host's sheet is served, and
// envHostAddrsPath where its live address and SSH state pieces are served.
const (
	envHostsPath     = envsPath + "/host"
	envHostAddrsPath = envHostsPath + "/addresses"
)

func envHostHref(vmid int) string { return envHostsPath + "?vmid=" + strconv.Itoa(vmid) }

func envEditHref(vmid int) string {
	op, _ := operationFor(pods.KindEnvEdit)
	return formQuery(op, jobs.Inputs{Kind: pods.KindEnvEdit, ImageRequest: pods.ImageRequest{VMID: vmid}})
}

func envHostBack(_ operation, in jobs.Inputs) string {
	if in.VMID >= 100 {
		return envHostHref(in.VMID)
	}
	return envsPath
}

// envHostView is the side-panel sheet for one environment host VM.
type envHostView struct {
	VMID     int
	Name     string
	Domain   string
	Slot     string
	Node     string
	Status   string
	Running  bool
	Router   bool
	Kind     string
	Addr     string
	Gateway  string
	DNS      string
	LiveFill bool
	pods.LiveAddrs
	Image           string
	Family          string
	CPU             string
	FixCPU          string
	Tags            string
	CDROM           bool
	Warnings        []string
	Details         *detailsView
	DetailsFromBase bool
	Notes           string
	Network         []configRow
	LastJob         *lastJobView
	Back            string
	EditHref        string
	MayEdit         bool
	MayPower        bool
	MayDelete       bool
	PowerActions    []powerChoice
	Lacks           []string
}

func (s *Server) readEnvHost(r *http.Request) (pods.HostEdit, []proxmox.VM, int, error) {
	vmid, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(fieldVMID)))
	if err != nil || vmid < 100 {
		return pods.HostEdit{}, nil, 0, pods.ErrNoEnvHost
	}
	planner := s.planner(r.Context())
	vms, err := planner.API.ClusterVMs(r.Context())
	if err != nil {
		return pods.HostEdit{}, nil, vmid, err
	}
	at, err := planner.HostToEdit(r.Context(), vms, vmid)
	return at, vms, vmid, err
}

func newEnvHostView(env pods.Environment, h pods.EnvHost, live pods.LiveAddrs) envHostView {
	kind := h.Kind()
	warnings := slices.Clone(h.Warnings)
	if live.SSHWarning != "" {
		warnings = append(warnings, live.SSHWarning)
	}
	p := envHostView{
		VMID:      h.VMID,
		Name:      h.Name,
		Domain:    cmp.Or(env.Domain, pods.Domain(h.Name)),
		Slot:      env.Slot.Pool,
		Node:      h.Node,
		Status:    cmp.Or(h.Status, "unknown"),
		Running:   h.Status == "running",
		Router:    h.Router,
		Kind:      kind,
		Addr:      pods.AddrText(h.LANAddr(env)),
		Gateway:   pods.AddrText(env.Gateway()),
		LiveAddrs: live,
		Warnings:  warnings,
	}
	if !h.Router && (kind == pods.ImageWindows || kind == pods.ImageLinux) {
		p.DNS = env.DNS()
	}
	return p
}

// envHostPage serves an environment host's side-panel sheet.
func (s *Server) envHostPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	at, vms, vmid, err := s.readEnvHost(r)
	if errors.Is(err, pods.ErrNoEnvHost) {
		if vmid == 0 {
			s.notFound(w, r)
			return
		}
		s.errorPage(w, r, http.StatusNotFound, "No such host",
			fmt.Sprintf("There's no environment host with VMID %d. It may have been deleted.", vmid), envsPath, "Back to Environments")
		return
	}
	if err != nil {
		s.errorPage(w, r, http.StatusBadGateway, "Environments",
			"Couldn't read the cluster: "+sentence(proxmox.Describe(err)), envsPath, "Back to Environments")
		return
	}
	env, h := at.Env, at.Host
	var family string
	if f, _, ok := pods.ParseImageName(h.Notes.Base); ok {
		if _, exists := pods.Latest(pods.BaseImages(vms), f); exists {
			family = f
		}
	}
	details := detailsOf(h.Details, h.Unreadable)
	fromBase := false
	if details == nil && h.Notes.Base != "" {
		if base, ok := s.hostBases(ctx, []pods.Environment{{Hosts: []pods.EnvHost{h}}}, vms)[h.Notes.Base]; ok && !base.Unreadable {
			details = detailsOf(base.Details, false)
			fromBase = details != nil
		}
	}
	back := envsPath
	if env.Domain != "" {
		back = envHref(env.Domain)
	}
	var cpu, fixCPU string
	if h.Config["cpu"] != "" || len(h.CPUModels) > 0 {
		cpu = cmp.Or(pods.CPUModel(h.Config["cpu"], h.CPUModels), pods.CPUType(h.Config["cpu"]), "default (kvm64)")
	}
	if len(h.CPUModels) > 0 && pods.CPUModel(h.Config["cpu"], h.CPUModels) == "" {
		fixCPU = h.CPUModels[0]
	}
	var live pods.LiveAddrs
	liveFill := false
	if s.mayAnywhere(ctx, pods.LiveAddrsPrivilege) && pods.HostAsksAgent(env, h) {
		if r.URL.Query().Get("live") == "1" {
			live = s.hostLiveAddrs(ctx, env, h)
		} else {
			liveFill = true
		}
	}
	p := newEnvHostView(env, h, live)
	p.LiveFill = liveFill
	p.Image = h.Notes.Base
	p.Family = family
	p.CPU = cpu
	p.FixCPU = fixCPU
	p.Tags = strings.Join(pods.Tags(h.Tags), ", ")
	p.CDROM = h.CDROM
	p.Details = details
	p.DetailsFromBase = fromBase
	p.Notes = h.UserNotes
	p.Network = networkRows(h.Config)
	p.LastJob = s.lastJobView(ctx, h.Name)
	p.Back = back
	p.EditHref = envEditHref(h.VMID)
	p.MayEdit = s.mayOn(ctx, h.VMID, pods.OfferPrivileges(pods.KindEnvEdit)...)
	p.MayPower = s.mayOn(ctx, h.VMID, pods.OfferPrivileges(pods.KindEnvPower)...)
	p.MayDelete = s.mayOn(ctx, h.VMID, pods.OfferPrivileges(pods.KindEnvDeleteHost)...)
	p.PowerActions = powerActionsFor(h.Status)
	if !p.MayEdit {
		p.Lacks = append(p.Lacks, lacksOf(pods.KindEnvEdit, h.Name))
	}
	if !p.MayPower {
		p.Lacks = append(p.Lacks, lacksOf(pods.KindEnvPower, h.Name))
	}
	if !p.MayDelete {
		p.Lacks = append(p.Lacks, lacksOf(pods.KindEnvDeleteHost, h.Name))
	}
	s.render(w, r, http.StatusOK, "envhost", s.newView(w, r, h.Name, "environments", p))
}

func (s *Server) hostLiveAddrs(ctx context.Context, env pods.Environment, h pods.EnvHost) pods.LiveAddrs {
	ctx, cancel := context.WithTimeout(ctx, s.addrWithin)
	defer cancel()
	return s.planner(ctx).HostLiveAddrs(ctx, env, h, s.addrTimeout)
}

// envHostAddrs serves a host sheet's live address cell and, when its SSH check
// reports a warning, its updated warnings list, each in its own <template>.
func (s *Server) envHostAddrs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.mayAnywhere(ctx, pods.LiveAddrsPrivilege) {
		http.Error(w, noLiveAddrs, http.StatusForbidden)
		return
	}
	at, _, _, err := s.readEnvHost(r)
	if errors.Is(err, pods.ErrNoEnvHost) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Couldn't read the cluster: "+sentence(proxmox.Describe(err)), http.StatusBadGateway)
		return
	}
	env, h := at.Env, at.Host
	p := newEnvHostView(env, h, s.hostLiveAddrs(ctx, env, h))
	var b strings.Builder
	add := func(name string) bool {
		frag, err := fragment("envhost", name, p)
		if err != nil {
			s.logf("web: %v", err)
			http.Error(w, "internal error rendering the host address", http.StatusInternalServerError)
			return false
		}
		b.WriteString(piece(frag))
		return true
	}
	if !add("host-addr") {
		return
	}
	if p.SSHWarning != "" && !add("host-warn") {
		return
	}
	writePieces(w, b.String())
}

func writePieces(w http.ResponseWriter, body string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set(piecesHeader, "1")
	_, _ = io.WriteString(w, body)
}

// hostEdit is what an Edit host form edits (pods.Planner.HostToEdit).
type hostEdit struct {
	VMID       int
	Name       string
	Slot       string
	Domain     string
	Gateway    string
	FirstOctet int
	LastOctet  int
	CDROM      bool
	HostInput  string
	KindInput  string
	OctetInput string
	ImageInput string
	CPUModels  []string
	CurrentCPU string
	CPUInput   string
	Was        pods.NotesRead
	Err        string
}

// hostEditPrefill fills Edit host's form from the host VM's current config
// and notes, or from the inputs a refused preview posted.
func (s *Server) hostEditPrefill(ctx context.Context, p *catalogPage) {
	p.ReadErr, p.Host = "", &hostEdit{}
	if p.In.VMID < 100 {
		p.Host.Err = "Choose the environment host to edit: each host on Environments has Edit."
		return
	}
	var at pods.HostEdit
	err := p.readErr
	if err == nil {
		at, err = s.planner(ctx).HostToEdit(ctx, p.vms, p.In.VMID)
	}
	p.Host.VMID, p.Host.Name = at.Host.VMID, at.Host.Name
	if err != nil {
		p.Host.Err = "Couldn't read the host to edit: " + sentence(proxmox.Describe(err))
		return
	}
	hostIn := at.Host.Name
	if at.Env.Domain != "" {
		hostIn = at.Host.Host
	}
	if p.In.EnvHost != "" {
		hostIn = p.In.EnvHost
	}
	octetIn := ""
	switch {
	case p.In.Octet == pods.ClearOctet:
		octetIn = clearOctetValue
	case p.In.Octet > 0:
		octetIn = strconv.Itoa(p.In.Octet)
	case p.In.Octet == 0 && at.Octet() > 0:
		octetIn = strconv.Itoa(at.Octet())
	}
	imageIn := cmp.Or(at.Host.Notes.Base, pods.NoBaseImage)
	if p.In.Image != "" {
		imageIn = p.In.Image
	}
	cpuModels := slices.Clone(at.Host.CPUModels)
	curModel := pods.CPUModel(at.Host.Config["cpu"], cpuModels)
	var curCPU string
	if len(cpuModels) > 0 && curModel == "" {
		curCPU = cmp.Or(pods.CPUType(at.Host.Config["cpu"]), "default (kvm64)")
	}
	cpuIn := curModel
	if p.In.CPU != "" {
		cpuIn = strings.TrimPrefix(p.In.CPU, "custom-")
		if !slices.Contains(cpuModels, cpuIn) {
			cpuModels = append(cpuModels, cpuIn)
		}
	}
	p.Host = &hostEdit{
		VMID:       at.Host.VMID,
		Name:       at.Host.Name,
		Slot:       at.Env.Slot.Pool,
		Domain:     at.Env.Domain,
		Gateway:    strconv.Itoa(pods.GatewayOctet),
		FirstOctet: pods.FirstHostOctet,
		LastOctet:  pods.LastHostOctet,
		CDROM:      at.Host.CDROM,
		HostInput:  hostIn,
		KindInput:  cmp.Or(p.In.ImageKind, at.Kind()),
		OctetInput: octetIn,
		ImageInput: imageIn,
		CPUModels:  cpuModels,
		CurrentCPU: curCPU,
		CPUInput:   cpuIn,
		Was:        at.Read,
	}
	p.Details = at.Edit()
}
