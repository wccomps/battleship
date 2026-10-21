package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// envHostsPath is where each environment host's sheet is served.
const envHostsPath = envsPath + "/host"

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
	VMID            int
	Name            string
	Host            string
	Domain          string
	Slot            string
	Node            string
	Status          string
	Running         bool
	Router          bool
	Kind            string
	Addr            string
	DNS             string
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

// envHostPage serves an environment host's side-panel sheet.
func (s *Server) envHostPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vmid, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(fieldVMID)))
	if err != nil || vmid < 100 {
		s.notFound(w, r)
		return
	}
	planner := s.planner(ctx)
	envs, vms, err := planner.Environments(ctx)
	if err != nil {
		s.errorPage(w, r, http.StatusBadGateway, "Environments",
			"Couldn't read the cluster: "+sentence(proxmox.Describe(err)), envsPath, "Back to Environments")
		return
	}
	env, h, ok := pods.EnvHostByID(envs, vmid)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "No such host",
			fmt.Sprintf("There's no environment host with VMID %d. It may have been deleted.", vmid), envsPath, "Back to Environments")
		return
	}
	kind := cmp.Or(h.Notes.OS, pods.ImageOther)
	if h.Router {
		kind = pods.ImageRouter
	}
	addr := pods.AddrText(h.Addr)
	if h.Router {
		addr = pods.AddrText(env.Gateway())
	}
	var family string
	if f, _, ok := pods.ParseImageName(h.Notes.Base); ok {
		if _, exists := pods.Latest(pods.BaseImages(vms), f); exists {
			family = f
		}
	}
	details := detailsOf(h.Details, h.Unreadable)
	fromBase := false
	if details == nil && h.Notes.Base != "" {
		if base, ok := s.hostBases(ctx, []pods.Environment{env}, vms)[h.Notes.Base]; ok && !base.Unreadable {
			details = detailsOf(base.Details, false)
			fromBase = details != nil
		}
	}
	var netRows []configRow
	for _, k := range slices.Sorted(maps.Keys(h.Config)) {
		if networkKey.MatchString(k) {
			netRows = append(netRows, configRow{k, h.Config[k]})
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
	p := envHostView{
		VMID:            h.VMID,
		Name:            h.Name,
		Host:            h.Host,
		Domain:          cmp.Or(env.Domain, pods.Domain(h.Name)),
		Slot:            env.Slot.Pool,
		Node:            h.Node,
		Status:          cmp.Or(h.Status, "unknown"),
		Running:         h.Status == "running",
		Router:          h.Router,
		Kind:            kind,
		Addr:            addr,
		Image:           h.Notes.Base,
		Family:          family,
		CPU:             cpu,
		FixCPU:          fixCPU,
		Tags:            strings.Join(pods.Tags(h.Tags), ", "),
		CDROM:           h.CDROM,
		Warnings:        h.Warnings,
		Details:         details,
		DetailsFromBase: fromBase,
		Notes:           h.UserNotes,
		Network:         netRows,
		Back:            back,
		EditHref:        envEditHref(h.VMID),
		MayEdit:         s.mayOn(ctx, h.VMID, pods.OfferPrivileges(pods.KindEnvEdit)...),
		MayPower:        s.mayOn(ctx, h.VMID, pods.OfferPrivileges(pods.KindEnvPower)...),
		MayDelete:       s.mayOn(ctx, h.VMID, pods.OfferPrivileges(pods.KindEnvDeleteHost)...),
		PowerActions:    powerActionsFor(h.Status),
	}
	if !h.Router && (kind == pods.ImageWindows || kind == pods.ImageLinux) {
		p.DNS = env.DNS()
	}
	if j, it, err := s.st.LastJobOf(ctx, h.Name); err == nil {
		now := s.now()
		p.LastJob = &lastJobView{ID: j.ID, Op: opLabelOf(j.Kind, readInputs(j)), Look: lastJobLook(j, it),
			When: minuteTime(now, j.CreatedAt), At: clockTime(now, j.CreatedAt)}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.logf("web: reading the last job of %s: %v", h.Name, err)
	}
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

// hostEdit is what an Edit host form edits (pods.HostToEdit).
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
func (s *Server) hostEditPrefill(_ context.Context, p *catalogPage) {
	p.ReadErr, p.Host = "", &hostEdit{}
	if p.In.VMID < 100 {
		p.Host.Err = "Choose the environment host to edit: each host on Environments has Edit."
		return
	}
	at, err := pods.HostToEdit(p.Envs, p.vms, p.In.VMID)
	if p.readErr != nil {
		err = p.readErr
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
	case at.Octet() > 0:
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
	if cpuIn == "" && len(cpuModels) > 0 {
		cpuIn = cpuModels[0]
	}
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
	if d := p.In.Details; d != nil {
		p.Details.ImageDetails, p.Details.From = d.ImageDetails, d.From
		if d.Free != nil {
			p.Details.Free = d.Free
		}
	}
}
