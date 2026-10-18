package web

import (
	"context"
	"net/http"
	"net/url"
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
	// NotesLack names the privilege Edit notes needs on the families whose
	// rows' Edit is disabled.
	NotesLack string
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
// retired family, its newest) and what it is: the OS its details give,
// else the first line of its notes. MayNotes is whether the user may edit
// the current version's notes, which its row offers.
type familyRow struct {
	Image    pods.BaseImage
	What     string
	MayNotes bool
}

// kindLabels are the catalogue's group headings.
var kindLabels = map[string]string{pods.ImageRouter: "Router", pods.ImageWindows: "Windows", pods.ImageLinux: "Linux", pods.ImageOther: "Other"}

// readyRows says what each candidate becomes when registered.
func readyRows(cands []pods.RegisterCandidate, images []pods.BaseImage) []readyRow {
	out := make([]readyRow, len(cands))
	for i, c := range cands {
		r := readyRow{RegisterCandidate: c}
		if c.Family != "" {
			version := 1
			if latest, ok := pods.Latest(images, c.Family); ok {
				version = latest.Version + 1
			}
			r.Becomes, r.How = pods.ImageName(c.Family, version), registerHow(version, c.Base)
		}
		out[i] = r
	}
	return out
}

// registerHow says how registering makes version of a family from a VM
// whose notes name base: as a new family, its next version, or as an edit
// copy of base (see pods.ImageEdit).
func registerHow(version int, base string) string {
	switch _, v, ok := pods.ParseImageName(base); {
	case ok:
		return "edit copy of v" + strconv.Itoa(v)
	case version == 1:
		return "new family"
	}
	return "next version"
}

// catalogue groups images' families by kind, in pods.ImageKinds order, each
// family once by its current version; families with none are retired.
func catalogue(images []pods.BaseImage) (groups []kindGroup, retired []familyRow) {
	byKind := map[string][]familyRow{}
	for _, f := range pods.FamilyNames(images) {
		if cur, ok := pods.Newest(images, f); ok {
			byKind[cur.Kind] = append(byKind[cur.Kind], familyRow{Image: cur, What: what(cur)})
		} else if latest, ok := pods.Latest(images, f); ok {
			retired = append(retired, familyRow{Image: latest, What: what(latest)})
		}
	}
	for _, k := range pods.ImageKinds {
		if rows := byKind[k]; len(rows) > 0 {
			groups = append(groups, kindGroup{kindLabels[k], rows})
		}
	}
	return groups, retired
}

// what says what im is: the OS its details give, else the first line of
// its notes with text; when its details are unreadable, that they are (the
// first line would be the block's).
func what(im pods.BaseImage) string {
	if im.Unreadable {
		return "Details unreadable: open the family to see its notes."
	}
	if im.Details.OS != "" {
		return im.Details.OS
	}
	for line := range strings.Lines(im.Notes) {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// lacksOf is lacksLine for kind's operation, as its Doing says it.
func lacksOf(kind pods.Kind, on string) string {
	op, _ := operationFor(kind)
	return lacksLine(op.Doing, kind, on)
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
		p.Lacks = lacksOf(pods.KindImageRegister, "")
	}
	var lack []string
	rows := 0
	for _, g := range p.Groups {
		for i := range g.Families {
			f := &g.Families[i]
			rows++
			if f.MayNotes = s.mayOn(r.Context(), f.Image.VMID, pods.OfferPrivileges(pods.KindImageDetails)...); !f.MayNotes {
				lack = append(lack, f.Image.Name)
			}
		}
	}
	switch {
	case len(lack) == 0:
	case len(lack) == rows && rows > 1:
		p.NotesLack = lacksOf(pods.KindImageDetails, "the base images")
	default:
		p.NotesLack = lacksOf(pods.KindImageDetails, andList(lack))
	}
	s.render(w, r, http.StatusOK, "images", s.newView(w, r, "Images", "images", p))
}

// familyView is a family's page: its current version, its details and
// notes, how to make the next one, and its versions.
type familyView struct {
	Family, Kind string
	Current      *pods.BaseImage // nil when every version is deprecated
	// Details, Unreadable and Notes are the current version's, else the
	// newest's (see pods.BaseImage).
	Details    pods.ImageDetails
	Unreadable bool
	Notes      string
	Next       int // the next version's number
	// Edit is an open edit copy (see pods.ImageEdit), registered as Next.
	Edit *pods.RegisterCandidate
	// Source is the VM the newest version whose source VM still exists was
	// registered from, to copy in again as Next.
	Source   *proxmox.VM
	Versions []versionRow // newest first
	// MayClone is whether the user may make the next version, either way
	// (register and edit need the same); Lacks says, a line each, what they
	// lack.
	MayClone bool
	Lacks    []string
	// MayNotes is whether the user may edit the current version's notes
	// (Edit notes); NotesLack says what they lack if not.
	MayNotes  bool
	NotesLack string
}

// versionRow is a version of a family, with its one action: Deprecate on
// the current version, Delete on deprecated ones, and whether the user may.
type versionRow struct {
	pods.BaseImage
	Current bool
	May     bool
}

// familiesPath is where each family's page is, under its name (familyHref),
// apart from the image operations' paths.
const familiesPath = "/images/family/"

func familyHref(family string) string { return familiesPath + url.PathEscape(family) }

// notesHref is Edit notes's form for family.
func notesHref(family string) string {
	op, _ := operationFor(pods.KindImageDetails)
	return op.Path + "?" + url.Values{fieldFamily: {family}}.Encode()
}

// notesButton is a family's Edit notes button (the "notes-button"
// template): a link to its form, or disabled when the user may not.
type notesButton struct {
	Family string
	May    bool
}

// oldFamilyPage sends a family page's old address, /images/{family}, to
// its page.
func (s *Server) oldFamilyPage(w http.ResponseWriter, r *http.Request) {
	family := r.PathValue("family")
	if pods.CheckFamily(family) != nil {
		s.notFound(w, r)
		return
	}
	http.Redirect(w, r, familyHref(family), http.StatusMovedPermanently)
}

// familyPage serves a family's page (familyHref): a page, or the panel
// over Images.
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
	if p.Current != nil {
		if p.MayNotes = s.mayOn(ctx, p.Current.VMID, pods.OfferPrivileges(pods.KindImageDetails)...); !p.MayNotes {
			p.NotesLack = lacksOf(pods.KindImageDetails, p.Current.Name)
		}
	}
	if noDeprecate {
		p.Lacks = append(p.Lacks, lacksOf(pods.KindImageDeprecate, p.Current.Name))
	}
	if noDelete {
		p.Lacks = append(p.Lacks, lacksOf(pods.KindImageDelete, "its versions"))
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
	shown := latest
	p := familyView{Family: family, Kind: latest.Kind, Next: latest.Version + 1}
	if cur, ok := pods.Newest(rd.images, family); ok {
		p.Current, shown = &cur, cur
	}
	p.Details, p.Unreadable, p.Notes = shown.Details, shown.Unreadable, shown.Notes
	sourced := 0
	for _, im := range rd.images {
		if im.Family != family {
			continue
		}
		p.Versions = append(p.Versions, versionRow{BaseImage: im, Current: p.Current != nil && im.VMID == p.Current.VMID})
		if im.Source != 0 && im.Version > sourced {
			if i := slices.IndexFunc(rd.vms, func(v proxmox.VM) bool { return v.VMID == im.Source && !v.Template }); i >= 0 {
				p.Source, sourced = &rd.vms[i], im.Version
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
