package web

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// selAction is one thing a volunteer does to the grid's selection.
type selAction struct {
	Kind string `json:"kind"` // click, shift, select (a header, the corner or the bar's clear), escape
	Box  int    `json:"box"`  // click, shift: the box's index on the page
	What string `json:"what"` // select: "all", "none", "team:07", "host:dc"
}

// selState is what the page shows after an action.
type selState struct {
	Checked  []bool          `json:"checked"`
	Count    string          `json:"count"`
	BarShown bool            `json:"barShown"`
	Pressed  map[string]bool `json:"pressed"` // by header's data-select
	Corner   bool            `json:"corner"`
	Partial  bool            `json:"partial"`
	PowerOff bool            `json:"powerOff"` // the power buttons are disabled
	ResetOff bool            `json:"resetOff"`
	// SnapshotOff: Take snapshot… is disabled; like power, an operator's
	// is over every team.
	SnapshotOff bool   `json:"snapshotOff"`
	Why         string `json:"why"`
}

// selectionScript runs the actions on the grid, the way the browser
// would deliver them, and returns the page's state after each.
const selectionScript = `(function (actions) {
  var form = document.getElementById("grid-form");
  var boxes = Array.prototype.slice.call(form.querySelectorAll('input[name="vms"]'));
  var heads = Array.prototype.slice.call(document.querySelectorAll('#grid-table a.hb[data-select]'));
  var corner = document.querySelector('.all input[data-select="all"]');
  function state() {
    var pressed = {};
    heads.forEach(function (h) { pressed[h.getAttribute("data-select")] = h.getAttribute("aria-pressed") === "true"; });
    var why = document.getElementById("ab-why");
    return {
      checked: boxes.map(function (b) { return b.checked; }),
      count: document.getElementById("sel-count").textContent,
      barShown: getComputedStyle(document.getElementById("actionbar")).display !== "none",
      pressed: pressed, corner: corner.checked, partial: corner.indeterminate,
      powerOff: document.querySelector('#actionbar button[value="start"]').disabled,
      resetOff: document.querySelector('#actionbar button[formaction="/reset"]').disabled,
      snapshotOff: document.querySelector('#actionbar button[formaction="/snapshot"]').disabled,
      why: why.hidden ? "" : document.getElementById("ab-why-text").textContent
    };
  }
  return actions.map(function (a) {
    switch (a.kind) {
    case "click": boxes[a.box].click(); break;
    case "shift": boxes[a.box].dispatchEvent(new MouseEvent("click", {bubbles: true, cancelable: true, shiftKey: true})); break;
    case "escape": document.body.dispatchEvent(new KeyboardEvent("keydown", {key: "Escape", bubbles: true})); break;
    case "select":
      var el = a.what === "all" ? corner : a.what === "none" ? document.querySelector('#actionbar [data-select="none"]')
        : document.querySelector('#grid-table a.hb[data-select="' + a.what + '"]');
      el.click();
      break;
    }
    return state();
  });
})`

// selModel is the selection the script should keep.
type selModel struct {
	boxes   []cellView // the boxes, in page order
	checked []bool
	last    int // the last box clicked, where a shift-click range starts; -1 for none
	// held is, per box, the grid privileges the viewer holds on its VM.
	held []map[string]bool
}

func (m *selModel) apply(a selAction) {
	matches := func(what string, c cellView) bool {
		return what == "all" || what == "none" || what == "team:"+c.Team || what == "host:"+c.Host
	}
	switch a.Kind {
	case "click":
		m.checked[a.Box] = !m.checked[a.Box]
		m.last = a.Box
	case "shift":
		m.checked[a.Box] = !m.checked[a.Box]
		if m.last >= 0 && m.last != a.Box {
			for i := min(m.last, a.Box); i <= max(m.last, a.Box); i++ {
				m.checked[i] = m.checked[a.Box]
			}
		}
		m.last = a.Box
	case "escape":
		clear(m.checked)
	case "select":
		all := true
		for i, c := range m.boxes {
			if matches(a.What, c) {
				all = all && m.checked[i]
			}
		}
		for i, c := range m.boxes {
			if matches(a.What, c) {
				m.checked[i] = a.What != "none" && !all
			}
		}
	}
}

// Random clicks, shift-clicks, row, column and corner selections, clears
// and Escapes on a grid with missing and busy VMs, as a user whose
// privileges vary by VM: after each, the boxes, count, action bar, pressed
// headers and corner show the model's selection, and an action is offered
// only if a ticked VM allows it, the bar saying when one isn't.
func TestBrowserPropSelection(t *testing.T) {
	h := browserHarness(t)
	h.api.Mu.Lock()
	clear(h.api.VMs)
	h.api.Mu.Unlock()
	hosts := []string{"db", "dc", "web"}
	for i := 1; i <= 9; i++ {
		team := pods.FormatTeam(i)
		for j, host := range hosts {
			if (i == 4 && host == "web") || (host == "db" && i > 6) {
				continue // missing
			}
			h.api.add(teamVM(team, host, 10000+i*100+j+1), cleanConfig(team), "initial")
		}
	}
	h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "2", Hosts: []string{"dc"}, Action: "reboot"}, byOperator)
	h.poll()
	// The viewer's privileges vary by VM: power on teams 01-04, snapshots
	// on 03-06, rollback only on 07; each VM's ACL replaces the one on /.
	cred := h.ticketWith("test-limited", []string{"VM.Audit"})
	privsOf := func(team int) []string {
		privs := []string{"VM.Audit"}
		if team <= 4 {
			privs = append(privs, "VM.PowerMgmt")
		}
		if team >= 3 && team <= 6 {
			privs = append(privs, "VM.Snapshot")
		}
		if team == 7 {
			privs = append(privs, "VM.Snapshot.Rollback")
		}
		return privs
	}
	h.api.Mu.Lock()
	for _, vm := range h.api.VMs {
		team, _ := strconv.Atoi(strings.TrimPrefix(vm.Name[:6], "team"))
		h.pve.Grant(cred.User, "/vms/"+strconv.Itoa(vm.VMID), privsOf(team)...)
	}
	h.api.Mu.Unlock()
	op := authtest.Login(t, h.st, h.cfg, authtest.User{Subject: "test-limited", At: h.clock.Now(), Proxmox: cred})
	h.openView(cred)

	// The boxes the page should have: cells that exist and aren't busy.
	v := h.srv.newGridView(h.poller.Grid(), h.clock.Now(), nil)
	v.markBusy(map[string]int64{"team02-dc": 1})
	var boxes []cellView
	var teams []string
	for _, row := range v.Rows {
		teams = append(teams, row.Team)
		for _, c := range row.Cells {
			if c.Pick != "" && c.Busy == 0 {
				boxes = append(boxes, c)
			}
		}
	}

	b := newBrowser(t, h, &op, 1366, 768, false)
	b.open("/")
	b.waitFor("the live grid", `document.getElementById("app").dataset.live === "live"`)
	var onPage []string
	b.eval(`[...document.querySelectorAll('#grid-form input[name="vms"]')].map(b => b.value)`, &onPage)
	var want []string
	for _, c := range boxes {
		want = append(want, c.Name)
	}
	if !slices.Equal(onPage, want) {
		t.Fatalf("the page's boxes are %v, want %v", onPage, want)
	}
	selects := []string{"all", "none"}
	for _, team := range teams {
		selects = append(selects, "team:"+team)
	}
	for _, host := range hosts {
		selects = append(selects, "host:"+host)
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Start from nothing selected, with box 0 the last one clicked.
		start := []selAction{{Kind: "click", Box: 0}, {Kind: "click", Box: 0}, {Kind: "escape"}}
		actions := rapid.SliceOfN(rapid.Custom(func(rt *rapid.T) selAction {
			switch rapid.IntRange(0, 9).Draw(rt, "kind") {
			case 0, 1, 2:
				return selAction{Kind: "click", Box: rapid.IntRange(0, len(boxes)-1).Draw(rt, "box")}
			case 3, 4:
				return selAction{Kind: "shift", Box: rapid.IntRange(0, len(boxes)-1).Draw(rt, "box")}
			case 5:
				return selAction{Kind: "escape"}
			}
			return selAction{Kind: "select", What: rapid.SampledFrom(selects).Draw(rt, "what")}
		}), 1, 12).Draw(rt, "actions")
		raw, err := json.Marshal(append(start, actions...))
		if err != nil {
			rt.Fatal(err)
		}
		var got []selState
		b.eval(selectionScript+"("+string(raw)+")", &got)
		got = got[len(start):]

		m := &selModel{boxes: boxes, checked: make([]bool, len(boxes)), last: 0}
		for _, c := range boxes {
			team, _ := strconv.Atoi(c.Team)
			held := map[string]bool{}
			for _, p := range privsOf(team) {
				held[p] = true
			}
			m.held = append(m.held, held)
		}
		for i, a := range actions {
			m.apply(a)
			if err := m.check(got[i]); err != nil {
				rt.Fatalf("after %v: %v", actions[:i+1], err)
			}
		}
	})
	b.clean()
}

func (m *selModel) check(s selState) error {
	if !slices.Equal(s.Checked, m.checked) {
		return fmt.Errorf("boxes %v, want %v", s.Checked, m.checked)
	}
	n := 0
	may := map[string]bool{} // a ticked VM allows it
	for i, on := range m.checked {
		if on {
			n++
			for p := range m.held[i] {
				may[p] = true
			}
		}
	}
	powerOff := n == 0 || !may["VM.PowerMgmt"]
	snapOff := n == 0 || !may["VM.Snapshot"]
	resetOff := n == 0 || !may["VM.Snapshot"] && !may["VM.Snapshot.Rollback"]
	denied := n > 0 && (powerOff || snapOff || resetOff)
	switch {
	case s.Count != fmt.Sprint(n):
		return fmt.Errorf("count %q, want %d", s.Count, n)
	case s.BarShown != (n > 0):
		return fmt.Errorf("action bar shown %v with %d selected", s.BarShown, n)
	case s.Corner != (n == len(m.boxes)) || s.Partial != (n > 0 && n < len(m.boxes)):
		return fmt.Errorf("corner %v (partly %v) with %d of %d", s.Corner, s.Partial, n, len(m.boxes))
	case s.ResetOff != resetOff:
		return fmt.Errorf("reset disabled %v with %d selected allowing %v", s.ResetOff, n, may)
	case s.PowerOff != powerOff:
		return fmt.Errorf("power disabled %v with %d selected allowing %v", s.PowerOff, n, may)
	case s.SnapshotOff != snapOff:
		return fmt.Errorf("snapshot disabled %v with %d selected allowing %v", s.SnapshotOff, n, may)
	case denied != (s.Why == "Some actions aren't permitted on these VMs"):
		return fmt.Errorf("why %q with some action not permitted %v", s.Why, denied)
	}
	for what, pressed := range s.Pressed {
		mine, full := 0, true
		for i, c := range m.boxes {
			if what == "team:"+c.Team || what == "host:"+c.Host {
				mine++
				full = full && m.checked[i]
			}
		}
		if want := mine > 0 && full; pressed != want {
			return fmt.Errorf("header %s pressed %v, want %v", what, pressed, want)
		}
	}
	return nil
}
