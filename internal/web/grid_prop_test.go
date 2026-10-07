package web

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

// clusterModel is a random cluster and what the grid should make of it.
type clusterModel struct {
	templates string // web.templates
	vms       []proxmox.VM
	hist      map[string]store.ItemResult
	// byCell are the team VMs of each (team, host).
	byCell map[[2]string][]proxmox.VM
	hosts  []string // the columns the grid should have
	rows   []string // the rows the grid should have: the teams with a team VM
}

// drawCluster draws a cluster with odd team VMs (unpadded, wrong pool,
// duplicated, templates) and masters of this set and another.
func drawCluster(t *rapid.T) clusterModel {
	var m clusterModel
	hostPool := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z][a-z0-9]{0,4}(-[a-z0-9]{1,2})?`), 1, 8, rapid.ID).Draw(t, "hosts")
	m.templates = rapid.SampledFrom([]string{"", "*.kilo.alpha"}).Draw(t, "web.templates")
	m.hist = map[string]store.ItemResult{}
	m.byCell = map[[2]string][]proxmox.VM{}
	vmid := 10000
	nVMs := rapid.IntRange(0, 40).Draw(t, "vm count")
	seen := map[string]bool{}
	for i := range nVMs {
		vmid += rapid.IntRange(1, 3).Draw(t, "vmid step")
		host := rapid.SampledFrom(hostPool).Draw(t, "host")
		label := fmt.Sprintf("vm %d", i)
		switch rapid.IntRange(0, 9).Draw(t, label+" kind") {
		case 0, 1: // a master of web.templates' set or of another, tagged or not
			set := rapid.SampledFrom([]string{"kilo.alpha", "other.set"}).Draw(t, label+" set")
			tags := rapid.SampledFrom([]string{"dev", "", "prod;dev"}).Draw(t, label+" tags")
			vm := proxmox.VM{VMID: vmid, Name: host + "." + set, Node: "n1", Status: "running", Tags: tags}
			m.vms = append(m.vms, vm)
			hasDev := slices.Contains(strings.Split(tags, ";"), "dev")
			if m.templates != "" && set == "kilo.alpha" && hasDev {
				seen[host] = true
			}
			continue
		}
		n := rapid.IntRange(0, 45).Draw(t, label+" team")
		team := pods.FormatTeam(n)
		name := "team" + team + "-" + host
		unpadded := n < 10 && rapid.IntRange(0, 5).Draw(t, label+" unpadded") == 0
		if unpadded {
			name = "team" + strconv.Itoa(n) + "-" + host
		}
		vm := proxmox.VM{VMID: vmid, Name: name, Node: "n1",
			Status: rapid.SampledFrom([]string{"running", "stopped", "paused"}).Draw(t, label+" status"),
			Pool:   rapid.SampledFrom([]string{"pool-" + team, "", "pool-99", "elsewhere"}).Draw(t, label+" pool"),
		}
		vm.Template = rapid.IntRange(0, 9).Draw(t, label+" template") == 0
		m.vms = append(m.vms, vm)
		if !unpadded && !vm.Template {
			key := [2]string{team, host}
			m.byCell[key] = append(m.byCell[key], vm)
			seen[host] = true
		}
	}
	for key := range m.byCell {
		if rapid.IntRange(0, 4).Draw(t, "failed job "+key[0]+key[1]) == 0 {
			m.hist["team"+key[0]+"-"+key[1]] = store.ItemResult{JobID: 7, JobKind: "deploy", Status: store.ItemFailed, Step: "network",
				Error: hostile(t, "job error")}
		}
	}
	for h := range seen {
		m.hosts = append(m.hosts, h)
	}
	slices.Sort(m.hosts)
	for key := range m.byCell {
		if !slices.Contains(m.rows, key[0]) {
			m.rows = append(m.rows, key[0])
		}
	}
	slices.Sort(m.rows)
	return m
}

// wantState is the state the model gives a cell, and why it is drifted.
func (m clusterModel) wantState(team, host string) (status.State, []string) {
	vms := slices.Clone(m.byCell[[2]string{team, host}])
	if len(vms) == 0 {
		return status.StateMissing, nil
	}
	slices.SortFunc(vms, func(a, b proxmox.VM) int { return a.VMID - b.VMID })
	vm := vms[0]
	var why []string
	if vm.Pool != "" && vm.Pool != "pool-"+team {
		why = append(why, "pool")
	}
	if len(vms) > 1 {
		why = append(why, "several VMs")
	}
	if _, ok := m.hist["team"+team+"-"+host]; ok {
		why = append(why, "failed in deploy job 7")
	}
	switch {
	case len(why) > 0:
		return status.StateDrifted, why
	case vm.Status == "running":
		return status.StateRunning, nil
	}
	return status.StateStopped, nil
}

// For any cluster, the grid's rows, columns, cells, legend and ?select
// ticks match the model; busy and missing cells aren't tickable.
func TestPropGridViewOfAnyCluster(t *testing.T) {
	ctx := context.Background()
	rapid.Check(t, func(t *rapid.T) {
		m := drawCluster(t)
		cfg := testConfig()
		cfg.Web.Templates = m.templates
		api := newFakeAPI()
		for _, vm := range m.vms {
			api.add(vm, nil)
		}
		clock := newFakeClock()
		hist := &fakeHistory{clock: clock, results: m.hist}
		poller, err := status.NewPoller(api, hist, apply.NewLimits(cfg.Concurrency), cfg, status.Options{Clock: clock, Logf: func(string, ...any) {}})
		if err != nil {
			t.Fatal(err)
		}
		if err := poller.Poll(ctx); err != nil {
			t.Fatal(err)
		}
		g := poller.Grid()
		s := &Server{cfg: cfg}
		v := s.newGridView(g, clock.Now(), nil)

		if !slices.Equal(v.Hosts, m.hosts) {
			t.Fatalf("columns = %q, want %q", v.Hosts, m.hosts)
		}
		if v.Empty != (len(m.rows) == 0) {
			t.Fatalf("Empty = %v with %d rows", v.Empty, len(m.rows))
		}
		if len(v.Rows) != len(m.rows) {
			t.Fatalf("%d rows for %d teams (%v)", len(v.Rows), len(m.rows), m.rows)
		}
		checkAllTeams(t, pods.FormatTeams(poller.Teams()), m)

		// Some VMs are busy with jobs, including missing ones a deploy
		// is making.
		busy := map[string]int64{}
		for _, row := range v.Rows {
			for _, c := range row.Cells {
				if rapid.IntRange(0, 5).Draw(t, "busy "+c.Name) == 0 {
					busy[c.Name] = int64(rapid.IntRange(1, 999).Draw(t, "job of "+c.Name))
				}
			}
		}
		v.markBusy(busy)

		ids := map[string]bool{}
		shown := map[string]int{}
		deployed := false
		for i, row := range v.Rows {
			if row.Team != m.rows[i] {
				t.Fatalf("row %d is team %s, want %s", i, row.Team, m.rows[i])
			}
			if row.Grp != (i > 0 && i%8 == 0) {
				t.Fatalf("row %d: Grp = %v; a gap starts every 8 teams", i, row.Grp)
			}
			checkRowHeader(t, row)
			if len(row.Cells) != len(m.hosts) {
				t.Fatalf("team %s has %d cells for %d hosts", row.Team, len(row.Cells), len(m.hosts))
			}
			for j, c := range row.Cells {
				if c.Team != row.Team || c.Host != m.hosts[j] || c.Name != "team"+c.Team+"-"+c.Host {
					t.Fatalf("cell %d of team %s is %s/%s %s", j, row.Team, c.Team, c.Host, c.Name)
				}
				if ids[c.ID] {
					t.Fatalf("two cells have id %s", c.ID)
				}
				ids[c.ID] = true
				state, why := m.wantState(c.Team, c.Host)
				if c.State != string(state) {
					t.Fatalf("%s is %s, want %s (%v)", c.Name, c.State, state, why)
				}
				deployed = deployed || state != status.StateMissing
				if (c.Pick == "") != (state == status.StateMissing) {
					t.Fatalf("%s (%s) has Pick %q", c.Name, state, c.Pick)
				}
				if c.Busy != busy[c.Name] {
					t.Fatalf("%s: Busy %d, want %d", c.Name, c.Busy, busy[c.Name])
				}
				checkCellWords(t, c, why)
				shown[c.Shown()]++
				checkCellMarkup(t, c)
			}
		}
		if len(m.rows) > 0 && !deployed {
			t.Fatal("the grid has rows but no team VM")
		}

		total := 0
		var want []string // the states some cell shows, in legend order
		for _, st := range stateOrder {
			if shown[st] > 0 {
				want = append(want, st)
			}
		}
		if len(v.Counts) != len(want) {
			t.Fatalf("legend has %d entries, want one per state shown: %v", len(v.Counts), want)
		}
		for i, sc := range v.Counts {
			if sc.State != want[i] {
				t.Fatalf("legend entry %d is %s, want %s", i, sc.State, want[i])
			}
			if sc.N != shown[sc.State] {
				t.Fatalf("legend says %d %s, the cells show %d", sc.N, sc.State, shown[sc.State])
			}
			total += sc.N
		}
		if total != len(m.rows)*len(m.hosts) {
			t.Fatalf("legend counts %d cells of %d", total, len(m.rows)*len(m.hosts))
		}

		// ?select ticks exactly the boxes it names that exist and are free.
		var choices []string
		choices = append(choices, "all", "team:", "host:", "team:99", "nonsense")
		for _, team := range m.rows {
			choices = append(choices, "team:"+team)
		}
		for _, h := range m.hosts {
			choices = append(choices, "host:"+h)
		}
		sel := rapid.SliceOfN(rapid.SampledFrom(choices), 0, 4).Draw(t, "select")
		n, last := v.markSelected(sel)
		ticked := 0
		var lastWant *cellView
		for i := range v.Rows {
			for j := range v.Rows[i].Cells {
				c := &v.Rows[i].Cells[j]
				want := c.Pick != "" && c.Busy == 0 &&
					(slices.Contains(sel, "all") || slices.Contains(sel, "team:"+c.Team) || slices.Contains(sel, "host:"+c.Host))
				if c.Checked != want {
					t.Fatalf("?select=%q: %s (%s, busy %d) ticked %v", sel, c.Name, c.State, c.Busy, c.Checked)
				}
				if want {
					ticked++
					lastWant = c
				}
			}
		}
		if n != ticked || last != lastWant {
			t.Fatalf("markSelected = %d, %v; ticked %d", n, last, ticked)
		}

		// The whole live fragment, with the header's pieces, is safe markup.
		frag, err := gridFragment(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := checkMarkup(frag); err != nil {
			t.Fatalf("grid fragment: %v", err)
		}
	})
}

// checkAllTeams checks that "all teams" is exactly the teams with a team VM.
func checkAllTeams(t *rapid.T, spec string, m clusterModel) {
	all, err := pods.ParseTeams(spec)
	if spec == "" {
		all, err = nil, nil
	}
	if err != nil {
		t.Fatalf("all teams %q: %v", spec, err)
	}
	if !slices.Equal(all, m.rows) {
		t.Fatalf("all teams = %v, want the teams with VMs: %v", all, m.rows)
	}
	if !coversTeams(all, all) || !coversTeams(all, append(slices.Clone(all), "99")) {
		t.Fatalf("all teams %v, or more, doesn't count as every team", all)
	}
	if len(all) == 0 {
		if !coversTeams(all, nil) {
			t.Fatal("with no teams, no range counts as every team")
		}
		return
	}
	if left := rapid.SampledFrom(all).Draw(t, "a team left out"); coversTeams(all, slices.DeleteFunc(slices.Clone(all), func(x string) bool { return x == left })) {
		t.Fatalf("all teams but %s counts as every team", left)
	}
}

// checkRowHeader checks a row's header: it selects the team.
func checkRowHeader(t *rapid.T, row rowView) {
	h, err := execute("grid-row-head", row)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := checkMarkup(h)
	if err != nil {
		t.Fatalf("row header of team %s: %v", row.Team, err)
	}
	for _, tag := range tags {
		if tag.Attrs["data-select"] == "team:"+row.Team {
			return
		}
	}
	t.Fatalf("row header of team %s doesn't select it:\n%s", row.Team, h)
}

// checkCellWords checks a cell's hover text and screen-reader label name
// the VM, its state and (drifted only) the reasons.
func checkCellWords(t *rapid.T, c cellView, why []string) {
	for _, words := range []string{c.Tip(), c.Label()} {
		if !strings.HasPrefix(words, c.Name) || !strings.Contains(words, c.Shown()) {
			t.Fatalf("%s (%s): words %q", c.Name, c.Shown(), words)
		}
		if c.Busy != 0 && !strings.Contains(words, "job "+strconv.FormatInt(c.Busy, 10)) {
			t.Fatalf("%s busy with job %d: words %q", c.Name, c.Busy, words)
		}
	}
	if (c.Reasons != "") != (c.State == string(status.StateDrifted)) {
		t.Fatalf("%s (%s) has reasons %q", c.Name, c.State, c.Reasons)
	}
	for _, w := range why {
		if !strings.Contains(c.Reasons, w) {
			t.Fatalf("%s: reasons %q don't say %q", c.Name, c.Reasons, w)
		}
		if c.Busy == 0 && (!strings.Contains(c.Tip(), w) || !strings.Contains(c.Label(), w)) {
			t.Fatalf("%s: words %q / %q don't say %q", c.Name, c.Tip(), c.Label(), w)
		}
	}
}

// checkCellMarkup checks a rendered cell: a busy one is a link to its
// job, a missing one an outline, and only the others have a box.
func checkCellMarkup(t *rapid.T, c cellView) {
	h, err := execute("grid-cell", c)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := checkMarkup(h)
	if err != nil {
		t.Fatalf("cell %s: %v", c.Name, err)
	}
	if tags[0].Attrs["id"] != c.ID {
		t.Fatalf("cell %s opens with %+v", c.Name, tags[0])
	}
	var boxes, links int
	for _, tag := range tags {
		switch {
		case tag.Name == "input":
			boxes++
			if tag.Attrs["name"] != "vms" || tag.Attrs["value"] == "" {
				t.Fatalf("cell %s has box %+v", c.Name, tag)
			}
		case tag.Name == "a":
			links++
			if want := "/logs/" + strconv.FormatInt(c.Busy, 10); tag.Attrs["href"] != want {
				t.Fatalf("cell %s links to %q, want %q", c.Name, tag.Attrs["href"], want)
			}
		}
	}
	wantBox := c.Busy == 0 && c.State != string(status.StateMissing)
	if (boxes == 1) != wantBox || boxes > 1 || (links == 1) != (c.Busy != 0) || links > 1 {
		t.Fatalf("cell %s (%s, busy %d) has %d boxes and %d links", c.Name, c.State, c.Busy, boxes, links)
	}
}

// The size classes only ever step up with more hosts (narrower cells),
// and rows-s, for few teams, only ever goes away with more teams.
func TestPropGridClassMonotone(t *testing.T) {
	cols := map[string]int{"": 0, "cols-m": 1, "cols-l": 2, "cols-xl": 3}
	level := func(class string) (col int, short bool) {
		for _, c := range strings.Fields(class) {
			if c == "rows-s" {
				short = true
			} else if n, ok := cols[c]; ok {
				col = n
			}
		}
		return col, short
	}
	rapid.Check(t, func(t *rapid.T) {
		h1 := rapid.IntRange(0, 60).Draw(t, "hosts")
		h2 := rapid.IntRange(h1, 61).Draw(t, "more hosts")
		t1 := rapid.IntRange(0, 100).Draw(t, "teams")
		t2 := rapid.IntRange(t1, 101).Draw(t, "more teams")
		c1, s1 := level(gridClass(h1, t1))
		c2, s2 := level(gridClass(h2, t2))
		if c2 < c1 || (s2 && !s1) {
			t.Fatalf("gridClass(%d, %d) = %q but gridClass(%d, %d) = %q", h1, t1, gridClass(h1, t1), h2, t2, gridClass(h2, t2))
		}
	})
}
