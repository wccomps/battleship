package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// pieces splits an event's data into the pieces the script swaps in. Every
// piece must come in its own <template>, so the browser parses each in its
// own context: parsed together, a <td> after a <ul> is dropped, and the
// cell never changes on the page. Anything outside a <template> fails.
func pieces(t testing.TB, data string) []string {
	t.Helper()
	var out []string
	rest := strings.TrimSpace(data)
	for rest != "" {
		if !strings.HasPrefix(rest, "<template>") {
			t.Fatalf("event data has markup outside a <template> piece, which the browser would parse in the wrong context:\n%s", rest)
		}
		end := strings.Index(rest, "</template>")
		if end < 0 {
			t.Fatalf("event data has an unclosed <template>:\n%s", rest)
		}
		inner := rest[len("<template>"):end]
		if strings.Contains(inner, "<template") {
			t.Fatalf("a piece holds another <template>:\n%s", inner)
		}
		out = append(out, inner)
		rest = strings.TrimSpace(rest[end+len("</template>"):])
	}
	if len(out) == 0 {
		t.Fatalf("event data has no pieces: %q", data)
	}
	return out
}

var firstID = regexp.MustCompile(`^<[a-z]+[^>]*? id="([^"]+)"`)

// pieceID is the id of a piece's element, which must open the piece.
func pieceID(t testing.TB, piece string) string {
	t.Helper()
	m := firstID.FindStringSubmatch(piece)
	if m == nil {
		t.Fatalf("piece doesn't start with an element with an id:\n%s", piece)
	}
	return m[1]
}

func TestGridPatchPiecesWrapped(t *testing.T) {
	// The browser case that dropped cells: the totals, then cells, then the
	// scan note, in one patch. Each comes in its own <template>, in page
	// order, exactly as rendered.
	h := newHarness(t)
	h.poll()
	before, err := renderParts(h.srv.newGridView(h.poller.Grid(), h.clock.Now(), nil))
	if err != nil {
		t.Fatal(err)
	}
	h.api.setStatus(10101, "stopped") // team01-dc
	h.api.setStatus(10202, "stopped") // team02-web
	h.poll()
	after, err := renderParts(h.srv.newGridView(h.poller.Grid(), h.clock.Now(), nil))
	if err != nil {
		t.Fatal(err)
	}
	got := pieces(t, after.changedSince(before.pieceSet))
	var ids []string
	for i, p := range got {
		id := pieceID(t, p)
		ids = append(ids, id)
		if p != after.html[id] {
			t.Errorf("piece %d (%s) isn't the rendered piece:\n%s", i, id, p)
		}
	}
	if want := "grid-counts cell-01-dc cell-02-web"; !strings.HasPrefix(strings.Join(ids, " "), want) {
		t.Errorf("pieces = %v, want %s first", ids, want)
	}

	// Everything changed: every piece, each wrapped.
	all := pieces(t, after.changedSince(pieceSet{}))
	if len(all) != len(after.ids) {
		t.Errorf("a patch of everything has %d pieces, want %d", len(all), len(after.ids))
	}
}

func TestEventPiecesWrapped(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)

	// The grid: the whole live grid is one piece, then the header's pop-over
	// and legend; a patch is one piece per changed element.
	g := h.openSSE(&op, "/events/grid")
	ev := g.next()
	if ev.Event != "grid" {
		t.Fatalf("first grid event = %+v", ev)
	}
	if ps := pieces(t, ev.Data); len(ps) != 3 || !strings.Contains(ps[0], `id="grid-table"`) || pieceID(t, ps[1]) != "grid-status" || pieceID(t, ps[2]) != "grid-counts" {
		t.Errorf("grid event = %d pieces, want the whole live grid as one, the pop-over and the legend", len(ps))
	}
	h.api.setStatus(10101, "stopped")
	h.poll()
	ev = g.next()
	if ev.Event != "patch" {
		t.Fatalf("grid event after a change = %+v, want patch", ev)
	}
	var ids []string
	for _, p := range pieces(t, ev.Data) {
		ids = append(ids, pieceID(t, p))
	}
	if strings.Join(ids, " ") != "grid-counts cell-01-dc" {
		t.Errorf("grid patch pieces = %v, want the totals and the cell", ids)
	}

	// A job: the log lines, then the header and the VMs, each wrapped.
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "reboot"}, byOperator)
	h.start(id)
	h.event(id, "team01-dc", "power", "done", "")
	j := h.openSSE(&op, "/events/jobs/"+itoa(id)+"?after=0")
	ev = j.next()
	if ev.Event != "log" {
		t.Fatalf("first job event = %+v, want log", ev)
	}
	for _, p := range pieces(t, ev.Data) {
		if !strings.HasPrefix(strings.TrimSpace(p), `<li id="ev-`) {
			t.Errorf("log piece isn't a log line:\n%s", p)
		}
	}
	ev = j.next()
	if ev.Event != "patch" {
		t.Fatalf("second job event = %+v, want patch", ev)
	}
	ids = nil
	for _, p := range pieces(t, ev.Data) {
		ids = append(ids, pieceID(t, p))
	}
	if strings.Join(ids, " ") != "job-head job-actions job-items" {
		t.Errorf("job patch pieces = %v, want job-head job-actions job-items", ids)
	}

	// The job list: its table, as one piece.
	l := h.openSSE(&op, "/events/jobs")
	ev = l.next()
	if ps := pieces(t, ev.Data); ev.Event != "patch" || len(ps) != 1 || pieceID(t, ps[0]) != "jobs-table" {
		t.Errorf("job list event = %+v, want the table as one piece", ev)
	}
	h.finish(id, store.Outcome{Status: store.StatusSucceeded})
}
