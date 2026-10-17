package web

import (
	"cmp"
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
	// Slots are the slots Create may take and Queued the free ones queued
	// jobs will fill (Planner.OpenSlots), the first of Slots chosen unless
	// In.Slot names one; SlotsErr says why they couldn't be told.
	Slots    []pods.Slot
	Queued   []pods.Slot
	SlotsErr string
	// Here are the hosts Add hosts' environment has, router first.
	Here []hereChip
	// Gateway is the router's octet; FirstOctet to LastOctet, the hosts'.
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

// rawRow is a row as posted: trimmed, its octet without the dot it may be
// typed with. Inputs carry it as its line, so an invalid row comes back.
func rawRow(name, image, octet string) machineRow {
	return machineRow{Name: strings.TrimSpace(name), Image: strings.TrimSpace(image), Octet: strings.TrimPrefix(strings.TrimSpace(octet), ".")}
}

// line is the row's raw line, or "" for a blank row.
func (m machineRow) line() string {
	if m.blank() {
		return ""
	}
	if m.Octet == "" {
		return m.Name + "=" + m.Image
	}
	return m.Name + "=" + m.Image + ":" + m.Octet
}

func rowOfLine(line string) machineRow {
	name, rest, _ := strings.Cut(line, "=")
	image, octet, _ := strings.Cut(rest, ":")
	return rawRow(name, image, octet)
}

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

// newEnvForm reads p's rows from form if it posted them, else from p.In:
// at least blankRows, plus blankRows for Add host without JavaScript.
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
			r := rawRow(form.Get(fieldRouterName), form.Get(fieldRouterImage), "")
			f.Router = &r
		case p.In.Router != "":
			r := rowOfLine(p.In.Router)
			f.Router = &r
		case !posted || p.Op.Kind == pods.KindEnvCreate: // a posted Add hosts without it had it removed
			f.Router = &machineRow{Name: "gw"}
			if len(p.Routers) == 1 {
				f.Router.Image = p.Routers[0].Family
			}
		}
	}
	n := blankRows
	if posted {
		rows := postedRows(form)
		n = max(n, len(rows))
		for _, row := range rows {
			if !row.blank() {
				f.Rows = append(f.Rows, row)
			}
		}
	} else {
		for _, line := range p.In.EnvHosts {
			f.Rows = append(f.Rows, rowOfLine(line))
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
		open, queued, err := s.planner(ctx).OpenSlots(ctx, p.Envs)
		if err != nil {
			f.SlotsErr = "Couldn't tell which free slots queued jobs will fill: " + sentence(err.Error())
		}
		f.Slots, f.Queued = open, queued
	case p.Env != nil:
		var here []envHostRow
		for _, h := range p.Env.Hosts {
			here = append(here, envHostRow{Name: h.Host, Addr: h.Addr})
		}
		slices.SortStableFunc(here, hostOrder)
		for _, h := range here {
			c := hereChip{Name: h.Name}
			if o := pods.LastOctet(h.Addr); o != 0 {
				c.Addr = "." + strconv.Itoa(o)
			}
			f.Here = append(f.Here, c)
		}
	}
	return f
}

// postedRows are the host rows form posts, blank ones too.
func postedRows(form url.Values) []machineRow {
	names, images, octets := form[fieldHostName], form[fieldHostImage], form[fieldHostOctet]
	rows := make([]machineRow, max(len(names), len(images), len(octets)))
	for i := range rows {
		rows[i] = rawRow(nth(names, i), nth(images, i), nth(octets, i))
	}
	return rows
}

// QueuedRuns names the free slots queued jobs will fill (slotRuns).
func (f envForm) QueuedRuns() string { return slotRuns(f.Queued) }

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

// checkFields marks each field the planner would refuse (pods.CheckEnv),
// and each row not filled in as a host, so Review shows it in place; it
// reports whether any is.
func (p *catalogPage) checkFields() bool {
	req, rows := p.request()
	if p.ReadErr == "" {
		for _, pr := range pods.CheckEnv(p.Op.Kind, p.Envs, p.Queued, p.Images, req) {
			p.mark(pr, rows)
		}
	}
	return p.marked()
}

// request is the environment request the form's fields make, and the rows
// of its hosts by pods.EnvProblem.Row (0 the router). A row not filled in
// as a host is marked and left out.
func (p *catalogPage) request() (pods.EnvRequest, map[int]*machineRow) {
	create := p.Op.Kind == pods.KindEnvCreate
	req := pods.EnvRequest{Domain: p.In.Domain}
	if p.Env != nil {
		req.Domain = p.Env.Domain
	}
	if create && p.In.Slot != "" {
		n, ok := pods.ParseSlotName(p.In.Slot)
		if !ok {
			p.SlotErr = "Choose a slot from the list."
		}
		req.Slot = n
	}
	rows := map[int]*machineRow{}
	if r := p.Router; r != nil && (create || r.Image != "") {
		switch {
		case !shapeOK(r, "the router"):
		case r.Image == "":
			r.Err, r.Bad = "Choose the router's image.", "image"
		default:
			req.Router, rows[0] = pods.HostSpec{Host: r.Name, Family: r.Image}, r
		}
	}
	for i := range p.Rows {
		m := &p.Rows[i]
		switch {
		case m.blank(), !shapeOK(m, "this host"):
		case m.Image == "":
			m.Err, m.Bad = "Choose an image for "+m.Name+", or clear its row.", "image"
		default:
			o, _ := strconv.Atoi(m.Octet)
			req.Hosts = append(req.Hosts, pods.HostSpec{Host: m.Name, Family: m.Image, Octet: o})
			rows[len(req.Hosts)] = m
		}
	}
	return req, rows
}

// mark puts pr on the field it names: the domain, the slot, or the input
// of the row (of rows, by request) it is about; the form's error if the
// form has no such field.
func (p *catalogPage) mark(pr pods.EnvProblem, rows map[int]*machineRow) {
	why := sentence(pr.Why)
	m := rows[pr.Row]
	if m == nil && pr.Row == 0 {
		m = p.Router
	}
	switch {
	case pr.Field == pods.FieldDomain:
		p.DomainErr = cmp.Or(p.DomainErr, why)
	case pr.Field == pods.FieldSlot:
		p.SlotErr = cmp.Or(p.SlotErr, why)
	case m == nil:
		p.Error = cmp.Or(p.Error, why)
	case m.Err != "":
	case pr.Field == pods.FieldAddress:
		m.Err, m.Bad = why+" Leave it blank to have one picked.", "addr"
	case pr.Field == pods.FieldImage:
		m.Err, m.Bad = why, "image"
	default:
		m.Err, m.Bad = why, "name"
	}
}

// marked reports whether a field is marked, and if so says so above them.
func (p *catalogPage) marked() bool {
	bad := p.DomainErr != "" || p.SlotErr != "" || p.Error != "" || p.Router != nil && p.Router.Err != "" ||
		slices.ContainsFunc(p.Rows, func(m machineRow) bool { return m.Err != "" })
	if bad && p.Error == "" {
		p.Error = "Fix the marked fields, then review again."
	}
	return bad
}

// shapeOK marks a row (what names it in the message) whose name is
// missing or no DNS label, or whose address isn't a host's; it reports
// whether neither is.
func shapeOK(m *machineRow, what string) bool {
	o, err := strconv.Atoi(m.Octet)
	switch {
	case m.Name == "":
		m.Err, m.Bad = "Name "+what+", or clear its row.", "name"
	case pods.CheckHostname(m.Name) != nil:
		m.Err, m.Bad = "Use lower-case letters, digits and dashes, like dc or wks-1.", "name"
	case m.Octet != "" && (err != nil || strconv.Itoa(o) != m.Octet || o < pods.FirstHostOctet || o > pods.LastHostOctet):
		m.Err, m.Bad = "Hosts take ."+strconv.Itoa(pods.FirstHostOctet)+" to ."+strconv.Itoa(pods.LastHostOctet)+". Leave it blank to have one picked.", "addr"
	default:
		return true
	}
	return false
}

// machineInputs reports whether form is op's machine rows.
func machineInputs(op operation, form url.Values) bool {
	return op.Machines && (form.Has(fieldHostName) || form.Has(fieldRouterImage))
}
