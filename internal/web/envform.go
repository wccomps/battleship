package web

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/pods"
)

// envForm is what Create environment and Add hosts show beyond the other
// catalog forms: the slot and machines, and the hosts already there.
type envForm struct {
	// Router is the router's row: always on Create, and on Add hosts when
	// the environment has none. Rows are the host rows.
	Router *machineRow
	Rows   []machineRow
	// HostImages are the host image choices by kind; RouterImages, the
	// router's. Each is a family's newest usable version.
	HostImages   []imageGroup
	RouterImages []imageOpt
	// Slots are the slots Create may take (Planner.OpenSlots), the first
	// chosen unless In.Slot names one; SlotsErr says why they couldn't be
	// told.
	Slots    []pods.Slot
	SlotsErr string
	// Here are the hosts Add hosts' environment has, router first.
	Here []hereChip
	// Gateway, FirstOctet and LastOctet are the router's and the hosts'
	// last address octets.
	Gateway, FirstOctet, LastOctet int
	// DomainErr and SlotErr say what is wrong with those fields, after
	// Review (checkFields).
	DomainErr, SlotErr string
}

// machineRow is a router or host row as typed: its name, image family and
// last address octet. After Review, Err says what is wrong with it and Bad
// which input that is: name, image or addr. N numbers the host rows from 1.
type machineRow struct {
	N                  int
	Name, Image, Octet string
	Err, Bad           string
}

func (m machineRow) blank() bool { return m.Name == "" && m.Image == "" && m.Octet == "" }

// hereChip is a host already in the environment: its name, and its
// address's last octet as ".10", if it has one.
type hereChip struct{ Name, Addr string }

// imageGroup is one kind's images in an image select.
type imageGroup struct {
	Kind   string // Windows, Linux or Other
	Images []imageOpt
}

// imageOpt is an image choice: a family, shown with its version.
type imageOpt struct{ Family, Label string }

func imageOpts(ims []pods.BaseImage) []imageOpt {
	var out []imageOpt
	for _, im := range ims {
		out = append(out, imageOpt{im.Family, im.Family + " · v" + strconv.Itoa(im.Version)})
	}
	return out
}

// blankRows is how many empty host rows the form starts with, and how many
// Add host adds without JavaScript.
const blankRows = 4

// newEnvForm reads p's machine rows from form, which posted them, or else
// from p.In, filled ones first, as many as were posted and at least
// blankRows; more (Add host without JavaScript) adds blankRows.
func (s *Server) newEnvForm(ctx context.Context, p catalogPage, form url.Values) envForm {
	f := envForm{RouterImages: imageOpts(p.Routers), Gateway: pods.GatewayOctet, FirstOctet: pods.FirstHostOctet, LastOctet: pods.LastHostOctet}
	for _, im := range p.Others { // by kind (newCatalogPage)
		if n := len(f.HostImages); n == 0 || f.HostImages[n-1].Kind != kindLabels[im.Kind] {
			f.HostImages = append(f.HostImages, imageGroup{Kind: kindLabels[im.Kind]})
		}
		g := &f.HostImages[len(f.HostImages)-1]
		g.Images = append(g.Images, imageOpts([]pods.BaseImage{im})...)
	}
	posted := form.Has(fieldHostName) || form.Has(fieldRouterImage)
	if p.Op.Kind == pods.KindEnvCreate || p.Env != nil && !p.Env.HasRouter() {
		switch {
		case form.Has(fieldRouterImage):
			f.Router = &machineRow{Name: strings.TrimSpace(form.Get(fieldRouterName)), Image: strings.TrimSpace(form.Get(fieldRouterImage))}
		case p.In.Router != "":
			f.Router = rowOf(p.In.Router)
		case !posted || p.Op.Kind == pods.KindEnvCreate: // a posted Add hosts without it had it removed
			f.Router = &machineRow{Name: "gw"}
			if len(p.Routers) == 1 {
				f.Router.Image = p.Routers[0].Family
			}
		}
	}
	n := blankRows // rows to show
	if posted {
		names, images, octets := form[fieldHostName], form[fieldHostImage], form[fieldHostOctet]
		n = max(n, len(names), len(images), len(octets))
		for i := range max(len(names), len(images), len(octets)) {
			row := machineRow{Name: strings.TrimSpace(at(names, i)), Image: strings.TrimSpace(at(images, i)), Octet: octetOf(at(octets, i))}
			if !row.blank() {
				f.Rows = append(f.Rows, row)
			}
		}
	} else {
		for _, spec := range p.In.EnvHosts {
			f.Rows = append(f.Rows, *rowOf(spec))
		}
	}
	n = max(n, len(f.Rows))
	if form.Get(fieldMore) == yes {
		n += blankRows
	}
	for len(f.Rows) < n {
		f.Rows = append(f.Rows, machineRow{})
	}
	for i := range f.Rows {
		f.Rows[i].N = i + 1
	}
	switch {
	case p.Op.Kind == pods.KindEnvCreate && p.ReadErr == "":
		open, _, err := s.planner(ctx).OpenSlots(ctx, p.Envs)
		if err != nil {
			f.SlotsErr = "Couldn't tell which free slots queued jobs will fill: " + sentence(err.Error())
		}
		f.Slots = open
	case p.Env != nil:
		for _, h := range envBlocks([]pods.Environment{*p.Env}, p.Subnet)[0].Hosts {
			c := hereChip{Name: h.Name}
			if o := pods.LastOctet(h.Addr); o != 0 {
				c.Addr = "." + strconv.Itoa(o)
			}
			f.Here = append(f.Here, c)
		}
	}
	return f
}

// rowOf reads a stored name=image[:octet] line as a row.
func rowOf(spec string) *machineRow {
	name, rest, _ := strings.Cut(spec, "=")
	image, octet, _ := strings.Cut(rest, ":")
	return &machineRow{Name: strings.TrimSpace(name), Image: strings.TrimSpace(image), Octet: octetOf(octet)}
}

// octetOf is a typed address octet, without the dot it may be typed with.
func octetOf(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), ".") }

// ChosenSlot is the slot the Slot select shows chosen: In.Slot, else the
// first free one.
func (p catalogPage) ChosenSlot() string {
	if p.In.Slot != "" || len(p.Slots) == 0 {
		return p.In.Slot
	}
	return p.Slots[0].Name
}

// StaleSlot is whether In.Slot isn't one of the free slots offered.
func (p catalogPage) StaleSlot() bool {
	return p.In.Slot != "" && !slices.ContainsFunc(p.Slots, func(s pods.Slot) bool { return s.Name == p.In.Slot })
}

// checkFields marks the domain, slot and machine rows with what is wrong
// with them, as the planner would refuse or block it, and reports whether
// anything is: a half-filled row, a name that isn't a DNS label or is used
// twice, an image with no usable version, a Windows name too long, an
// address out of range or someone else's.
func (p *catalogPage) checkFields() bool {
	bad := false
	if p.Op.Kind == pods.KindEnvCreate {
		if e, ok := pods.EnvByDomain(p.Envs, p.In.Domain); ok {
			p.DomainErr = p.In.Domain + " already exists, in " + e.Slot.Pool + ": add hosts to it from Environments instead."
		} else if pods.CheckDomain(p.In.Domain) != nil {
			p.DomainErr = "Write two or more lower-case labels joined by dots, like kilo.alpha."
		}
		if p.StaleSlot() {
			p.SlotErr = "Slot " + p.In.Slot + " isn't free any more; choose another."
		}
		bad = p.DomainErr != "" || p.SlotErr != ""
	}
	domain := p.In.Domain
	if p.Env != nil {
		domain = p.Env.Domain
	}
	here := map[string]bool{} // the environment's host names
	owner := map[int]string{} // octet -> the host with it
	for _, h := range p.Here {
		here[h.Name] = true
		if o, err := strconv.Atoi(strings.TrimPrefix(h.Addr, ".")); err == nil {
			owner[o] = h.Name
		}
	}
	seen := map[string]bool{} // the names of the rows checked
	name := func(m *machineRow, what string) bool {
		switch {
		case m.Name == "":
			m.Err = "Name " + what + ", or clear its row."
		case pods.CheckHostname(m.Name) != nil:
			m.Err = "Use lower-case letters, digits and dashes, like dc or wks-1."
		case here[m.Name]:
			m.Err = m.Name + " is already in " + domain + "."
		case seen[m.Name]:
			m.Err = m.Name + " is named twice."
		default:
			seen[m.Name] = true
			return true
		}
		m.Bad = "name"
		return false
	}
	if r := p.Router; r != nil && (p.Op.Kind == pods.KindEnvCreate || r.Image != "") {
		switch {
		case !name(r, "the router"):
		case r.Image == "":
			r.Err, r.Bad = "Choose the router's image.", "image"
		case !slices.ContainsFunc(p.Routers, func(im pods.BaseImage) bool { return im.Family == r.Image }):
			r.Err, r.Bad = r.Image+" has no usable router version; choose another image.", "image"
		}
		bad = bad || r.Err != ""
	}
	for i := range p.Rows {
		m := &p.Rows[i]
		if m.blank() {
			continue
		}
		im, found := pods.Newest(p.Others, m.Image)
		switch {
		case !name(m, "this host"):
		case m.Image == "":
			m.Err, m.Bad = "Choose an image for "+m.Name+", or clear its row.", "image"
		case !found:
			m.Err, m.Bad = m.Image+" has no usable host version; choose another image.", "image"
		case im.Kind == pods.ImageWindows && len(m.Name) > pods.MaxWindowsHostname:
			m.Err, m.Bad = "Windows host names are at most "+strconv.Itoa(pods.MaxWindowsHostname)+" characters.", "name"
		case m.Octet != "":
			o, err := strconv.Atoi(m.Octet)
			switch {
			case err != nil || strconv.Itoa(o) != m.Octet || o < pods.FirstHostOctet || o > pods.LastHostOctet:
				m.Err = "Hosts take ." + strconv.Itoa(pods.FirstHostOctet) + " to ." + strconv.Itoa(pods.LastHostOctet) + ". Leave it blank to have one picked."
			case owner[o] != "":
				m.Err = "." + m.Octet + " is " + owner[o] + "'s address. Leave it blank to have one picked."
			default:
				owner[o] = m.Name
			}
			if m.Err != "" {
				m.Bad = "addr"
			}
		}
		bad = bad || m.Err != ""
	}
	if bad {
		p.Error = "Fix the marked fields, then review again."
	}
	return bad
}

// machineInputs reports whether form is an environment form's rows.
func machineInputs(kind pods.Kind, form url.Values) bool {
	return (kind == pods.KindEnvCreate || kind == pods.KindEnvAdd) && (form.Has(fieldHostName) || form.Has(fieldRouterImage))
}
