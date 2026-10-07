package web

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

// driftView is one drift reason, with the job to look at for job drift.
type driftView struct {
	Reason string
	JobID  int64
}

// snapshotView is one of the VM's snapshots.
type snapshotView struct {
	Name string
	// Baseline marks the VM's baseline snapshot or, in the reset picker, the
	// nameless option meaning each VM's own.
	Baseline bool
}

// configRow is one line of the VM's Proxmox config.
type configRow struct{ Key, Value string }

// lastJobView is the newest job that has the VM.
type lastJobView struct {
	ID   int64
	Op   opLabel
	Look statusLook
	When string
	At   string // in full, for a title
}

// cellPage is the data of a VM's page.
type cellPage struct {
	Cell      status.Cell
	State     string // the class suffix of its state, e.g. "drifted"
	Missing   bool
	ReadErr   string // the live read failed; the page shows the grid's cell
	Drift     []driftView
	Snapshots []snapshotView
	// Baseline is the VM's baseline snapshot, or what a baseline is when it has
	// none (e.g. "initial"); HasBaseline says which.
	Baseline    string
	HasBaseline bool
	Network     []configRow
	LastJob     *lastJobView
	// What the buttons offer; the operations check again on submit.
	CanReset, CanPower, CanSnapshot bool
	PowerActions                    []powerChoice // for the VM's power state
	Deploy                          string        // a missing VM: the deploy form for its team and host
}

// networkKey matches the config keys the page shows: NICs and cloud-init
// addresses.
var networkKey = regexp.MustCompile(`^(net|ipconfig)[0-9]+$`)

// powerActionsFor lists the sensible power actions for Proxmox status power:
// a running VM can be shut down, stopped or rebooted; any other started (or
// stopped, if stuck).
func powerActionsFor(power string) []powerChoice {
	var out []pods.PowerAction
	for _, a := range pods.PowerActions {
		running := power == "running"
		if (a.Value == "start") != running || a.Value == "stop" {
			out = append(out, a)
		}
	}
	return powerChoicesOf(out)
}

// cellPage shows one team VM: a live read of its state, drift reasons,
// snapshots, last job, and power/reset/snapshot buttons leading to a preview.
func (s *Server) cellPage(w http.ResponseWriter, r *http.Request) {
	team, host := r.PathValue("team"), r.PathValue("host")
	d, err := s.detail(r.Context(), team, host)
	if errors.Is(err, status.ErrUnknownCell) {
		s.errorPage(w, r, http.StatusNotFound, "No such VM",
			"The grid has no team "+team+" VM for host "+host+".", "/", "Back to the grid")
		return
	}
	c := d.Cell
	p := cellPage{
		Cell:     c,
		State:    string(c.State),
		Missing:  c.State == status.StateMissing,
		Baseline: d.Baseline,
	}
	if err != nil {
		p.ReadErr = err.Error()
	}
	for _, dr := range c.Drift {
		p.Drift = append(p.Drift, driftView{Reason: strings.TrimRight(dr.Reason, ". "), JobID: dr.JobID})
	}
	for _, name := range d.Snapshots {
		p.Snapshots = append(p.Snapshots, snapshotView{Name: name, Baseline: name == d.Baseline})
	}
	p.HasBaseline = d.Baseline != "" && slices.Contains(d.Snapshots, d.Baseline)
	if p.Baseline == "" {
		p.Baseline = s.cfg.Deploy.SnapshotName
	}
	for _, k := range slices.Sorted(maps.Keys(d.Config)) {
		if networkKey.MatchString(k) {
			p.Network = append(p.Network, configRow{k, d.Config[k]})
		}
	}
	if j, it, err := s.st.LastJobOf(r.Context(), c.Name); err == nil {
		now := s.now()
		p.LastJob = &lastJobView{ID: j.ID, Op: opLabelOf(j.Kind, readInputs(j)), Look: lastJobLook(j, it),
			When: minuteTime(now, j.CreatedAt), At: clockTime(now, j.CreatedAt)}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.logf("web: reading the last job of %s: %v", c.Name, err)
	}

	ctx := r.Context()
	if !p.Missing {
		p.CanReset = s.mayOn(ctx, c.VMID, pods.OfferPrivileges(pods.KindReset)...) && len(p.Snapshots) > 0
		p.CanPower = s.mayOn(ctx, c.VMID, pods.OfferPrivileges(pods.KindPower)...)
		p.CanSnapshot = s.mayOn(ctx, c.VMID, pods.OfferPrivileges(pods.KindSnapshot)...)
		p.PowerActions = powerActionsFor(c.Power)
	}
	if p.Missing && s.mayRun(ctx, pods.KindDeploy) {
		op, _ := operationFor(pods.KindDeploy)
		p.Deploy = formQuery(op, jobs.Inputs{Kind: pods.KindDeploy, Teams: team, Hosts: []string{host}, Pattern: s.webTemplates()})
	}

	s.render(w, r, http.StatusOK, "vm", s.newView(w, r, c.Name, "grid", p))
}

// webTemplates is web.templates, the set the grid shows, as a pattern.
func (s *Server) webTemplates() string { return strings.TrimSpace(s.cfg.Web.Templates) }

// lastJobLook shows a VM's last job by the job's status while active or if it
// never touched the VM, else by how it left this VM.
func lastJobLook(j store.Job, it store.Item) statusLook {
	if j.Active() {
		return lookOf(jobLooks, j.Status)
	}
	switch it.Status {
	case store.ItemDone:
		return lookOf(jobLooks, store.StatusSucceeded)
	case store.ItemFailed, store.ItemRemoved:
		return lookOf(jobLooks, store.StatusFailed)
	}
	return lookOf(jobLooks, j.Status)
}

// cellReadSlots caps concurrent cell reads (cell pages, snapshot picker). Each
// read takes config-call slots one at a time, so a crowd of cell pages holds
// at most this many of the slots job executors share.
const cellReadSlots = 2

// cellReadTimeout bounds a shared cell read, which outlives its starting
// request for the others waiting on it.
const cellReadTimeout = time.Minute

// detail reads a cell live (status.Poller.Detail), joining any read already
// under way, at most cellReadSlots at once. The read runs on the server's
// lifetime, bounded by s.cellTimeout; the caller stops waiting when ctx ends.
// A failed read's Detail still carries the grid's copy.
func (s *Server) detail(ctx context.Context, team, host string) (status.Detail, error) {
	view := s.view(ctx) // the viewer's: their ticket, their grid
	ch := s.cellFlight.DoChan(view.User+"\x00"+team+"/"+host, func() (any, error) {
		rctx, cancel := context.WithTimeout(s.life, s.cellTimeout)
		defer cancel()
		select {
		case s.cellSlots <- struct{}{}:
			defer func() { <-s.cellSlots }()
		case <-rctx.Done():
			return status.Detail{}, s.cutOff(rctx, "waiting for a turn to read it")
		}
		d, err := view.Detail(rctx, team, host)
		if err != nil && rctx.Err() != nil && !errors.Is(err, status.ErrUnknownCell) {
			err = s.cutOff(rctx, "reading it")
		}
		return d, err
	})
	if s.afterCellJoin != nil {
		s.afterCellJoin()
	}
	var d status.Detail
	var err error
	select {
	case res := <-ch:
		d, _ = res.Val.(status.Detail)
		err = res.Err
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil && d.Cell.Name == "" && !errors.Is(err, status.ErrUnknownCell) {
		c, ok := view.Grid().Cell(team, host)
		if !ok {
			return status.Detail{}, fmt.Errorf("team %q host %q: %w", team, host, status.ErrUnknownCell)
		}
		d = status.Detail{Cell: c}
	}
	return d, err
}

// cutOff explains why a shared cell read's context ended, and during what.
func (s *Server) cutOff(rctx context.Context, doing string) error {
	if s.life.Err() != nil {
		return fmt.Errorf("the server is shutting down, which stopped %s", doing)
	}
	if errors.Is(rctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("gave up after %s %s", s.cellTimeout, doing)
	}
	return rctx.Err()
}
