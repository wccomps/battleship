package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/status"
)

// stateBusy hides a VM's own state while an active job still has to work on it.
const stateBusy = "busy"

// stateOrder is the order states are counted in, as the legend shows them.
var stateOrder = []string{string(status.StateRunning), string(status.StateStopped), string(status.StateMissing), string(status.StateDrifted), stateBusy}

// cellView is one grid cell: a glyph (class is-<State>), with words in Tip
// and Label.
type cellView struct {
	ID   string // element id, e.g. "cell-01-dc"; the script matches cells by it
	Team string
	Host string
	Name string // the team VM's name, also when it is missing
	// Pick is the value of the cell's box on the grid's form: the VM's
	// name, or "" for a missing VM, which has nothing to power or reset.
	Pick string
	// Checked ticks the box. Only gridPage sets it (for ?select), so the shared
	// rendering stays the same for every viewer.
	Checked bool
	// Busy is the active job that still has to work on this VM, if any.
	// A busy cell is a link to that job instead of a box.
	Busy  int64
	Href  string // the VM's page
	State string // running, stopped, missing or drifted: its class is-<State>
	Power string // the Proxmox status of a drifted or stopped VM
	// Reasons say why a drifted VM is drifted, e.g. "network differs".
	Reasons string
	// Lacks lists the grid actions' privileges the viewer lacks on this VM
	// (data-lacks), so the action bar offers only allowed actions.
	Lacks string
}

// Shown is the state the cell shows: busy while a job works on it.
func (c cellView) Shown() string {
	if c.Busy != 0 {
		return stateBusy
	}
	return c.State
}

// Tip is the hover text, e.g. "team09-web · drifted — network differs".
func (c cellView) Tip() string {
	switch {
	case c.Busy != 0:
		return fmt.Sprintf("%s · busy — job %d", c.Name, c.Busy)
	case c.Reasons != "":
		return c.Name + " · " + c.State + " — " + c.Reasons
	case c.State == string(status.StateStopped) && c.Power != "" && c.Power != c.State:
		return c.Name + " · stopped (" + c.Power + ")"
	}
	return c.Name + " · " + c.State
}

// Label is the screen-reader name, e.g. "team09-web, drifted, network differs".
func (c cellView) Label() string {
	switch {
	case c.Busy != 0:
		return fmt.Sprintf("%s, busy, job %d", c.Name, c.Busy)
	case c.Reasons != "":
		return c.Name + ", " + c.State + ", " + c.Reasons
	}
	return c.Name + ", " + c.State
}

// selectable reports whether the cell has an active checkbox on the grid.
func (c cellView) selectable() bool { return c.Pick != "" && c.Busy == 0 }

func allChecked(cells []cellView) bool {
	any := false
	for _, c := range cells {
		if !c.selectable() {
			continue
		}
		if !c.Checked {
			return false
		}
		any = true
	}
	return any
}

// rowView is one team's row.
type rowView struct {
	Team string
	// Grp: the row starts a group of 8 teams: a little gap above it.
	Grp   bool
	Cells []cellView
}

// Pressed reports whether every selectable cell in the row is ticked.
func (r rowView) Pressed() bool { return allChecked(r.Cells) }

// HostPressed reports whether every selectable cell in host's column is ticked.
func (v gridView) HostPressed(host string) bool {
	col := slices.Index(v.Hosts, host)
	if col < 0 {
		return false
	}
	var cells []cellView
	for _, r := range v.Rows {
		if col < len(r.Cells) {
			cells = append(cells, r.Cells[col])
		}
	}
	return allChecked(cells)
}

// stateCount is how many cells show a state.
type stateCount struct {
	State string
	N     int
}

// liveView is the header's live dot (live, or stale until the next good read)
// and the lines of its pop-over.
type liveView struct {
	State string // "live" or "stale"
	Title string // e.g. "No answer from Proxmox"
	// Since is the last read in Unix ms while stale (the script shows how long
	// ago), else 0.
	Since      int64
	LastUpdate string
	Lines      []string
}

// gridView is the live part of the grid page, resent whenever the grid changes.
type gridView struct {
	Live   liveView
	Counts []stateCount
	Hosts  []string
	Rows   []rowView
	Stale  bool
	// GridClass and Tall size the grid from its counts (see app.css).
	GridClass string
	Tall      bool
	// Empty means a read found no team VMs: the page shows Start instead of a
	// grid. Before the first read the grid is unknown, not empty.
	Empty bool
	// Start is the empty grid's template sets (liveGridView).
	Start *startView
}

// gridClass is the grid's size classes for hosts columns and teams rows.
func gridClass(hosts, teams int) string {
	var cls []string
	switch {
	case hosts >= 25:
		cls = append(cls, "cols-xl")
	case hosts >= 17:
		cls = append(cls, "cols-l")
	case hosts >= 13:
		cls = append(cls, "cols-m")
	}
	if teams <= 16 {
		cls = append(cls, "rows-s")
	}
	return strings.Join(cls, " ")
}

// newGridView prepares g for the templates. lacks, if set, says which grid
// actions' privileges the viewer lacks on a VM.
func (s *Server) newGridView(g status.Grid, now time.Time, lacks func(vmid int) []string) gridView {
	v := gridView{Hosts: g.Hosts, Stale: g.Stale, Empty: len(g.Rows) == 0 && !g.PolledAt.IsZero(),
		GridClass: gridClass(len(g.Hosts), len(g.Rows)), Tall: len(g.Rows) > 36}
	for i, row := range g.Rows {
		rv := rowView{Team: row.Team, Grp: i > 0 && i%8 == 0}
		for _, c := range row.Cells {
			cv := cellViewOf(c)
			if lacks != nil && cv.Pick != "" {
				cv.Lacks = strings.Join(lacks(c.VMID), " ")
			}
			rv.Cells = append(rv.Cells, cv)
		}
		v.Rows = append(v.Rows, rv)
	}
	v.count()

	switch {
	case g.PolledAt.IsZero():
		v.Live = liveView{State: "stale", Title: "Waiting for live data"}
		if g.Err != status.WaitingForPoll {
			v.Live.Lines = append(v.Live.Lines, strings.TrimRight(g.Err, ". "))
		}
	case g.Stale:
		v.Live = liveView{State: "stale", Title: "No answer from Proxmox", Since: g.PolledAt.UnixMilli(),
			LastUpdate: clockTime(now, g.PolledAt), Lines: []string{strings.TrimRight(g.Err, ". ")}}
	default:
		v.Live = liveView{State: "live", Title: "Live", Lines: []string{"reads Proxmox every " + spoken(s.cfg.Web.StatusPoll)}}
		if g.ScannedAt.IsZero() {
			v.Live.Lines = append(v.Live.Lines, "config check: not run yet")
		} else {
			v.Live.Lines = append(v.Live.Lines, fmt.Sprintf("config check %s · every %s", clockTime(now, g.ScannedAt), spoken(s.cfg.Web.DriftScan)))
		}
		if g.ScanErr != "" {
			v.Live.Lines = append(v.Live.Lines, strings.TrimRight(g.ScanErr, ". "))
		}
	}
	return v
}

// count tallies the cells by shown state; the legend lists only those states.
func (v *gridView) count() {
	n := map[string]int{}
	for _, row := range v.Rows {
		for _, c := range row.Cells {
			n[c.Shown()]++
		}
	}
	v.Counts = v.Counts[:0]
	for _, st := range stateOrder {
		if n[st] > 0 {
			v.Counts = append(v.Counts, stateCount{State: st, N: n[st]})
		}
	}
}

// spoken writes a duration as people say it: "5 seconds", "12 hours".
func spoken(d time.Duration) string {
	unit, n := "second", int(d/time.Second)
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		unit, n = "hour", int(d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		unit, n = "minute", int(d/time.Minute)
	case d%time.Second != 0:
		return d.String()
	}
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func cellViewOf(c status.Cell) cellView {
	v := cellView{
		ID:    "cell-" + c.Team + "-" + c.Host,
		Team:  c.Team,
		Host:  c.Host,
		Name:  c.Name,
		Href:  "/vm/" + c.Team + "/" + c.Host,
		State: string(c.State),
		Power: c.Power,
	}
	if c.State != status.StateMissing {
		v.Pick = c.Name
	}
	if c.State == status.StateDrifted {
		reasons := make([]string, len(c.Drift))
		for i, d := range c.Drift {
			reasons[i] = strings.TrimRight(d.Reason, ". ")
		}
		v.Reasons = strings.Join(reasons, "; ")
	}
	return v
}

// markBusy marks the cells of VMs active jobs still have to work on, and
// recounts.
func (v *gridView) markBusy(jobs map[string]int64) {
	for i := range v.Rows {
		for j := range v.Rows[i].Cells {
			c := &v.Rows[i].Cells[j]
			c.Busy = jobs[c.Name]
		}
	}
	v.count()
}

// markSelected ticks the boxes ?select asks for ("all", "team:07", "host:dc",
// or a VM's name or "07/dc"), so selection works without the script. Missing
// and busy VMs have no box. It returns how many it ticked, and the last one.
func (v *gridView) markSelected(sel []string) (n int, last *cellView) {
	for i := range v.Rows {
		for j := range v.Rows[i].Cells {
			c := &v.Rows[i].Cells[j]
			for _, s := range sel {
				if c.selectable() && (s == "all" || s == "team:"+c.Team || s == "host:"+c.Host || s == c.Name || s == c.Team+"/"+c.Host) {
					c.Checked = true
				}
			}
			if c.Checked {
				n++
				last = c
			}
		}
	}
	return n, last
}

// gridPageView is the grid page: the live part, shared by one viewer's pages,
// plus the per-request parts rendered outside it.
type gridPageView struct {
	Live    gridView
	Details string // the page of the one VM ?select ticked, if just one
}

// startView is the empty grid's next step: the template sets to deploy.
type startView struct {
	MasterTag string
	Sets      []setView
	Others    []string // tagged masters in no set
}

// setView is one template set the grid offers.
type setView struct {
	Name    string   // e.g. "kilo.alpha"
	Pattern string   // its masters, e.g. "*.kilo.alpha"
	Hosts   []string // e.g. dc, web
	Masters int
	Running int
	// Deploy opens the deploy form for this set; "" if the viewer may not deploy.
	Deploy  string
	Checked bool // the deploy form's choice
}

// HostsText is the set's hosts, space-separated, for the deploy form's script.
func (v setView) HostsText() string { return strings.Join(v.Hosts, " ") }

// RunDots and StopDots draw one running glyph per running master, then one
// stopped glyph per other master.
func (v setView) RunDots() []struct{}  { return make([]struct{}, max(v.Running, 0)) }
func (v setView) StopDots() []struct{} { return make([]struct{}, max(v.Masters-v.Running, 0)) }

// deployHref opens the deploy form filled in for pattern. Teams are left blank:
// no team has VMs yet.
func (s *Server) deployHref(pattern string) string {
	op, _ := operationFor(pods.KindDeploy)
	return formQuery(op, jobs.Inputs{Kind: pods.KindDeploy, Pattern: pattern})
}

// startOf prepares g's template sets, with Deploy links if canDeploy.
func (s *Server) startOf(g status.Grid, canDeploy bool) startView {
	v := startView{MasterTag: s.cfg.Deploy.MasterTag, Others: g.OtherMasters}
	for _, set := range g.Sets {
		sv := setView{Name: string(set.Name), Pattern: set.Name.Pattern(), Hosts: set.Hosts, Masters: set.Masters, Running: set.Running}
		if canDeploy {
			sv.Deploy = s.deployHref(sv.Pattern)
		}
		v.Sets = append(v.Sets, sv)
	}
	return v
}

// allTeamsSpec is every team the viewer can see (teams with VMs in their grid's
// last good poll), as a typed range.
func (s *Server) allTeamsSpec(ctx context.Context) string {
	return pods.FormatTeams(s.polledGrid(ctx).Teams)
}

func coversTeams(all, teams []string) bool {
	if len(all) <= 1 {
		return false
	}
	for _, t := range all {
		if !slices.Contains(teams, t) {
			return false
		}
	}
	return true
}

// liveGridView is newGridView for the request's user: their lacking privileges
// and, on an empty grid, the template sets.
func (s *Server) liveGridView(ctx context.Context, g status.Grid, now time.Time) gridView {
	v := s.newGridView(g, now, func(vmid int) []string { return s.lacks(ctx, vmid) })
	if v.Empty {
		start := s.startOf(g, s.mayRun(ctx, pods.KindDeploy))
		v.Start = &start
	}
	return v
}

const firstPollWait = 5 * time.Second

// polledGrid is the viewer's grid once its first poll is in, or after
// s.firstPoll.
func (s *Server) polledGrid(ctx context.Context) status.Grid {
	return s.polledView(ctx).Grid()
}

func (s *Server) gridPage(w http.ResponseWriter, r *http.Request) {
	g := s.polledGrid(r.Context())
	v := s.liveGridView(r.Context(), g, s.now())
	v.markBusy(s.sharedBusy(r.Context()).jobs)
	ticked, last := v.markSelected(r.URL.Query()["select"])
	p := gridPageView{Live: v}
	if ticked == 1 {
		p.Details = last.Href
	}
	page := s.newView(w, r, "Pods", "grid", p)
	page.Live = v.Live.State
	s.render(w, r, http.StatusOK, "grid", page)
}

// initial is the first letter of a name, for the account button.
func initial(name string) string {
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return string(unicode.ToUpper(r))
		}
	}
	if r, _ := utf8.DecodeRuneInString(name); r != utf8.RuneError {
		return string(r)
	}
	return "?"
}

// gridFragment renders the live part of the grid page, followed by the header
// pieces outside it (pop-over, legend), each in a <template> (see piece).
func gridFragment(v gridView) (string, error) {
	var b strings.Builder
	for _, name := range []string{"grid-live", "grid-status", "grid-counts"} {
		h, err := fragment("grid", name, v)
		if err != nil {
			return "", err
		}
		b.WriteString(piece(h))
	}
	return b.String(), nil
}

// gridParts is the live grid rendered piece by piece: each element the script
// can swap by id, in page order, plus the table's shape.
type gridParts struct {
	shape string // hosts, teams, staleness and emptiness: a change needs the whole grid
	pieceSet
}

// renderParts renders every swappable piece of v.
func renderParts(v gridView) (gridParts, error) {
	var p gridParts
	var shape strings.Builder
	fmt.Fprintf(&shape, "stale=%v empty=%v class=%q tall=%v hosts=%q teams=", v.Stale, v.Empty, v.GridClass, v.Tall, v.Hosts)
	add := func(id, tmpl string, data any) error {
		h, err := fragment("grid", tmpl, data)
		if err != nil {
			return err
		}
		p.add(id, h)
		return nil
	}
	if err := add("grid-status", "grid-status", v); err != nil {
		return p, err
	}
	if err := add("grid-counts", "grid-counts", v); err != nil {
		return p, err
	}
	if v.Start != nil {
		if err := add("grid-start", "grid-start", v.Start); err != nil {
			return p, err
		}
	}
	for _, row := range v.Rows {
		shape.WriteString(row.Team + ",")
		for _, c := range row.Cells {
			if err := add(c.ID, "grid-cell", c); err != nil {
				return p, err
			}
		}
	}
	p.shape = shape.String()
	return p, nil
}

// gridEvents streams grid and busy-mark changes. A "grid" event carries the
// whole fragment: first, after a Resync, and when the table's shape changes.
// Otherwise a "patch" event carries only the changed pieces, each in a
// <template> (see piece). A change that alters nothing shown sends nothing.
func (s *Server) gridEvents(w http.ResponseWriter, r *http.Request) {
	var last gridParts
	// Holding the stream open keeps the viewer's view polling (status.Views).
	view := s.view(r.Context())
	s.stream(w, r, []string{status.TopicGrid, status.TopicJobs}, 0, func(sw *sseWriter, resync bool, _ uint64) error {
		cur, frag, err := s.renderedGrid(r.Context(), view, resync || last.shape == "")
		if err != nil {
			s.logf("web: rendering the grid for its event stream: %v", err)
			return err
		}
		if resync || cur.shape != last.shape {
			if frag == "" {
				if cur, frag, err = s.renderedGrid(r.Context(), view, true); err != nil {
					s.logf("web: rendering the grid for its event stream: %v", err)
					return err
				}
			}
			last = cur
			return sw.event("grid", "", frag)
		}
		patch := cur.changedSince(last.pieceSet)
		last = cur
		if patch == "" {
			return nil
		}
		return sw.event("patch", "", patch)
	})
}

// gridCache holds a view's latest rendering so one person's open grid pages
// render each change once. A view is one Proxmox user's (status.Views), so it
// may hold per-viewer content (visible VMs, data-lacks, Deploy links);
// per-request content (CSRF token, ?select ticks) is rendered outside it.
type gridCache struct {
	mu    sync.Mutex
	key   string // the grid version, the day and the busy set
	parts gridParts
	frag  string // the whole live fragment; "" until a stream needs it
}

// renderedGrid returns the grid's pieces and, if whole, the whole fragment,
// re-rendering only when grid version, busy set or day changed (clock times
// name the day). Busy marks are read first because every job change but a log
// line wakes each grid stream, and most change nothing shown.
func (s *Server) renderedGrid(ctx context.Context, view *status.View, whole bool) (gridParts, string, error) {
	busy := s.sharedBusy(ctx)
	now := s.now()
	keyOf := func(version uint64) string {
		return fmt.Sprintf("%d\x00%s\x00%s", version, now.UTC().Format(time.DateOnly), busy.key)
	}
	c := s.gridCacheOf(view)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key != keyOf(view.Version()) || (whole && c.frag == "") {
		g := view.Grid() // may be newer than the version just read
		v := s.liveGridView(ctx, g, now)
		s.gridViews.Add(1)
		v.markBusy(busy.jobs)
		if key := keyOf(g.Version); c.key != key {
			parts, err := renderParts(v)
			if err != nil {
				return gridParts{}, "", err
			}
			s.gridRenders.Add(1)
			c.key, c.parts, c.frag = key, parts, ""
		}
		if whole && c.frag == "" {
			frag, err := gridFragment(v)
			if err != nil {
				return gridParts{}, "", err
			}
			c.frag = frag
		}
	}
	if !whole {
		return c.parts, "", nil
	}
	return c.parts, c.frag, nil
}

// gridCacheOf is view's rendering cache, which goes when the view stops.
func (s *Server) gridCacheOf(view *status.View) *gridCache {
	s.gridsMu.Lock()
	defer s.gridsMu.Unlock()
	c, ok := s.grids[view]
	if !ok {
		c = &gridCache{}
		s.grids[view] = c
		go func() {
			<-view.Done()
			s.gridsMu.Lock()
			delete(s.grids, view)
			s.gridsMu.Unlock()
		}()
	}
	return c
}
