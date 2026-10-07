package web

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// The job record is called Logs everywhere (/logs), so nobody looks there
// for a way to start something.
func TestLogsAreCalledLogs(t *testing.T) {
	h := newHarness(t)
	h.poll()
	op := h.login(asOperator)
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)

	rec := h.get(&op, "/logs")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /logs = %d", rec.Code)
	}
	body := rec.Body.String()
	contains(t, "logs list", body, "<title>Logs · battleship</title>", `<h1 class="ptitle">Logs</h1>`,
		`<a href="/logs" aria-current="page">Logs</a>`, `<a class="job" href="/logs/`+itoa(id)+`">`)
	lacks(t, "logs list", body, ">Jobs<", `href="/jobs`)

	body = h.get(&op, "/logs/"+itoa(id)).Body.String()
	contains(t, "log page", body, "<title>Log #"+itoa(id)+" · battleship</title>",
		`<a class="btn ghost back" href="/logs">`, "Logs</a>", `<a href="/logs" aria-current="page">Logs</a>`)
	lacks(t, "log page", body, ">Jobs<", `href="/jobs`)

	body = h.panelGet(&op, "/logs/"+itoa(id), nil).Body.String()
	contains(t, "log panel", body, `<a class="btn ghost first" href="/logs/`+itoa(id)+`">Log</a>`)
	lacks(t, "log panel", body, "Job page")

	// The grid's busy cell and the VM page lead to the log.
	h.poll()
	contains(t, "grid", h.get(&op, "/").Body.String(), `href="/logs/`+itoa(id)+`"`, `<a href="/logs">Logs</a>`)
	contains(t, "VM page", h.get(&op, "/vm/01/dc").Body.String(), `<a class="lastjob" href="/logs/`+itoa(id)+`"`)

	// A confirm lands on the new job's log.
	action, form, _ := h.preview(&op, "/power", url.Values{"teams": {"2"}, "action": {"start"}})
	if loc := h.post(&op, action, form).Header().Get("Location"); loc != "/logs/"+itoa(id+1) {
		t.Errorf("confirm redirects to %q", loc)
	}
}

// Old links and pages open before the move keep working.
func TestOldJobsAddressesRedirect(t *testing.T) {
	h := newHarness(t)
	op := h.login(asOperator)
	for _, tc := range []struct {
		method, from, to string
		code             int
	}{
		{http.MethodGet, "/jobs", "/logs", http.StatusMovedPermanently},
		{http.MethodGet, "/jobs?before=12", "/logs?before=12", http.StatusMovedPermanently},
		{http.MethodGet, "/jobs/7", "/logs/7", http.StatusMovedPermanently},
		{http.MethodPost, "/jobs/7/cancel", "/logs/7/cancel", http.StatusPermanentRedirect},
		{http.MethodPost, "/jobs/7/retry", "/logs/7/retry", http.StatusPermanentRedirect},
	} {
		rec := h.do(&op, tc.method, tc.from, nil)
		if rec.Code != tc.code || rec.Header().Get("Location") != tc.to {
			t.Errorf("%s %s = %d to %q, want %d to %q", tc.method, tc.from, rec.Code, rec.Header().Get("Location"), tc.code, tc.to)
		}
	}
}
