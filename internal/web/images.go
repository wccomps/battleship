package web

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// imagesView is the Images page: the VMs ready to register, then the
// catalogue, one row per family grouped by kind.
type imagesView struct {
	ReadErr string
	Ready   []readyRow
	Groups  []kindGroup
	Retired []familyRow // families whose every version is deprecated
	// Lacks names the privilege Register needs when the user holds it
	// nowhere; the buttons are then disabled.
	Lacks string
}

// readyRow is a VM ready to register: a form of its own.
type readyRow struct {
	pods.RegisterCandidate
	Becomes string // the image it would become, e.g. teak-v2; "" without a family
	How     string // "new family", "next version" or "edit copy of v1"
}

// kindGroup is the catalogue's families of one kind.
type kindGroup struct {
	Label    string // "Router", "Windows", "Linux" or "Other"
	Families []familyRow
}

// familyRow is a family in the catalogue: its current version (for a
// retired family, its newest) and the first line of that version's notes.
type familyRow struct {
	Image     pods.BaseImage
	FirstLine string
}

// kindLabels are the catalogue's group headings.
var kindLabels = map[string]string{pods.ImageRouter: "Router", pods.ImageWindows: "Windows", pods.ImageLinux: "Linux", pods.ImageOther: "Other"}

// readyRows says what each candidate becomes when registered.
func readyRows(cands []pods.RegisterCandidate, images []pods.BaseImage) []readyRow {
	out := make([]readyRow, len(cands))
	for i, c := range cands {
		r := readyRow{RegisterCandidate: c}
		if c.Family != "" {
			version, how := 1, "new family"
			if latest, ok := pods.Latest(images, c.Family); ok {
				version, how = latest.Version+1, "next version"
			}
			if _, v, ok := pods.ParseImageName(c.Base); ok {
				how = "edit copy of v" + strconv.Itoa(v)
			}
			r.Becomes, r.How = pods.ImageName(c.Family, version), how
		}
		out[i] = r
	}
	return out
}

// catalogue groups images' families by kind, in pods.ImageKinds order, each
// family once by its current version; families with none are retired.
func catalogue(images []pods.BaseImage) (groups []kindGroup, retired []familyRow) {
	byKind := map[string][]familyRow{}
	for _, f := range pods.FamilyNames(images) {
		if cur, ok := pods.Newest(images, f); ok {
			byKind[cur.Kind] = append(byKind[cur.Kind], familyRow{cur, firstLine(cur.Notes)})
		} else if latest, ok := pods.Latest(images, f); ok {
			retired = append(retired, familyRow{latest, firstLine(latest.Notes)})
		}
	}
	for _, k := range pods.ImageKinds {
		if rows := byKind[k]; len(rows) > 0 {
			groups = append(groups, kindGroup{kindLabels[k], rows})
		}
	}
	return groups, retired
}

// firstLine is the first line of notes with text, usually the login.
func firstLine(notes string) string {
	for line := range strings.Lines(notes) {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// lacksLine names the privileges an action needs that the user lacks,
// e.g. "Registering needs VM.Clone, which you don't have."
func lacksLine(doing string, kind pods.Kind, on string) string {
	need := strings.Join(pods.OfferPrivileges(kind), " or ")
	if on != "" {
		need += " on " + on
	}
	return doing + " needs " + need + ", which you don't have."
}

// imagesRead is the cluster as the images pages need it: images with their
// notes, and the VMs that could be registered.
type imagesRead struct {
	images []pods.BaseImage
	vms    []proxmox.VM
	cands  []pods.RegisterCandidate
	err    string
}

func (s *Server) readImages(ctx context.Context) imagesRead {
	planner := s.planner(ctx)
	vms, err := planner.API.ClusterVMs(ctx)
	if err != nil {
		return imagesRead{err: "Couldn't read the cluster: " + sentence(proxmox.Describe(err))}
	}
	images := planner.ReadNotes(ctx, pods.BaseImages(vms))
	return imagesRead{images: images, vms: vms, cands: planner.RegisterCandidates(ctx, vms, images)}
}

func (s *Server) imagesPage(w http.ResponseWriter, r *http.Request) {
	rd := s.readImages(r.Context())
	p := imagesView{ReadErr: rd.err, Ready: readyRows(rd.cands, rd.images)}
	p.Groups, p.Retired = catalogue(rd.images)
	if len(p.Ready) > 0 && !s.mayRun(r.Context(), pods.KindImageRegister) {
		p.Lacks = lacksLine("Registering", pods.KindImageRegister, "")
	}
	s.render(w, r, http.StatusOK, "images", s.newView(w, r, "Images", "images", p))
}

// familyView is a family's page: its current version, how to make the
// next one, and its versions.
type familyView struct {
	Family, Kind string
	Current      *pods.BaseImage // nil when every version is deprecated
	Notes        string          // the current version's, else the newest's
	Next         int             // the next version's number
	// Edit is an open edit copy (see pods.ImageEdit), registered as Next.
	Edit *pods.RegisterCandidate
	// Source is the VM the newest version with one was registered from, if it
	// still exists, to copy in again as Next.
	Source   *proxmox.VM
	Versions []versionRow // newest first
	// MayClone is whether the user may make the next version, either way
	// (register and edit need the same); Lacks says, a line each, what they
	// lack.
	MayClone bool
	Lacks    []string
}

// versionRow is a version of a family, with its one action: Deprecate on
// the current version, Delete on deprecated ones, and whether the user may.
type versionRow struct {
	pods.BaseImage
	Current bool
	May     bool
}

// familyPage serves /images/{family}: a page, or the panel over Images.
func (s *Server) familyPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	family := r.PathValue("family")
	if pods.CheckFamily(family) != nil {
		s.notFound(w, r)
		return
	}
	rd := s.readImages(ctx)
	if rd.err != "" {
		s.errorPage(w, r, http.StatusBadGateway, "Images", rd.err, "/images", "Back to Images")
		return
	}
	p, ok := newFamilyView(family, rd)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "No such family",
			"There's no base image family "+family+". It may have been deleted.", "/images", "Back to Images")
		return
	}
	p.MayClone = s.mayRun(ctx, pods.KindImageRegister)
	if !p.MayClone && (p.Edit != nil || p.Source != nil || p.Current != nil) {
		p.Lacks = append(p.Lacks, lacksLine("Making a new version", pods.KindImageRegister, ""))
	}
	var noDeprecate, noDelete bool
	for i := range p.Versions {
		v := &p.Versions[i]
		kind := pods.KindImageDelete
		if v.Current {
			kind = pods.KindImageDeprecate
		}
		if v.Current || v.Deprecated {
			v.May = s.mayOn(ctx, v.VMID, pods.OfferPrivileges(kind)...)
			noDeprecate = noDeprecate || !v.May && v.Current
			noDelete = noDelete || !v.May && v.Deprecated
		}
	}
	if noDeprecate {
		p.Lacks = append(p.Lacks, lacksLine("Deprecating", pods.KindImageDeprecate, p.Current.Name))
	}
	if noDelete {
		p.Lacks = append(p.Lacks, lacksLine("Deleting", pods.KindImageDelete, "its versions"))
	}
	s.render(w, r, http.StatusOK, "family", s.newView(w, r, family, "images", p))
}

// newFamilyView describes family from what was read, or reports there is
// no such family.
func newFamilyView(family string, rd imagesRead) (familyView, bool) {
	latest, ok := pods.Latest(rd.images, family)
	if !ok {
		return familyView{}, false
	}
	p := familyView{Family: family, Kind: latest.Kind, Next: latest.Version + 1, Notes: latest.Notes}
	if cur, ok := pods.Newest(rd.images, family); ok {
		p.Current, p.Notes = &cur, cur.Notes
	}
	sourced := 0
	for _, im := range rd.images {
		if im.Family != family {
			continue
		}
		p.Versions = append(p.Versions, versionRow{BaseImage: im, Current: p.Current != nil && im.VMID == p.Current.VMID})
		if im.Source != 0 && im.Version > sourced {
			if vm, ok := vmWithID(rd.vms, im.Source); ok {
				p.Source, sourced = &vm, im.Version
			}
		}
	}
	slices.Reverse(p.Versions)
	for _, c := range rd.cands {
		if c.Family == family && (c.Base != "" || strings.EqualFold(c.Name, family+pods.EditSuffix)) {
			p.Edit = &c
			break
		}
	}
	return p, true
}

func vmWithID(vms []proxmox.VM, vmid int) (proxmox.VM, bool) {
	i := slices.IndexFunc(vms, func(v proxmox.VM) bool { return v.VMID == vmid && !v.Template })
	if i < 0 {
		return proxmox.VM{}, false
	}
	return vms[i], true
}
