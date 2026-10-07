package web

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

// allTeamsSpecs are ways to write every team of the harness (01-03).
var allTeamsSpecs = []string{"1-3", "01-03", "3,2,1", "1,2-3", " 1 - 3 ", "0-3", "1-5", "1,1,2,3"}

// drawForm draws a form of any fields the operations read, with values
// that are plausible, odd or hostile.
func drawForm(t *rapid.T, teams []string) url.Values {
	form := url.Values{}
	vms := []string{"team01-dc", "team01-web", "team02-dc", "team02-web", "team03-dc", "team03-web", "team09-dc", "nosuch"}
	fields := map[string]*rapid.Generator[string]{
		"teams":       rapid.SampledFrom(teams),
		"hosts":       rapid.SampledFrom([]string{"dc", "web", "dc,web", "", "*"}),
		"vms":         rapid.SampledFrom(vms),
		"pattern":     rapid.SampledFrom([]string{"*.kilo.alpha", "*", ""}),
		"action":      rapid.SampledFrom([]string{"start", "stop", "shutdown", "reboot", "", "explode"}),
		"snapshot":    rapid.SampledFrom([]string{"", "initial", "before-scoring", "round2"}),
		"description": rapid.SampledFrom([]string{"", "after lunch", "<b>x</b>"}),
		"vmstate":     rapid.SampledFrom([]string{"yes", "no"}),
		"rebuild":     rapid.SampledFrom([]string{"yes", "no"}),
		"baseline":    rapid.SampledFrom([]string{"yes", "no"}),
		"from":        rapid.SampledFrom([]string{"grid", ""}),
		"typed":       rapid.SampledFrom(teams),
		"nonce":       rapid.StringMatching(`[A-Za-z0-9_-]{0,43}`),
		"fingerprint": rapid.StringMatching(`[0-9a-f]{0,64}`),
		"retry_of":    rapid.SampledFrom([]string{"", "1", "-1", "x"}),
		"step":        rapid.SampledFrom([]string{"snapshot", ""}),
	}
	for _, name := range []string{"teams", "hosts", "vms", "pattern", "action", "snapshot", "description", "vmstate", "rebuild", "baseline", "from", "typed", "nonce", "fingerprint", "retry_of", "step"} {
		for i := range rapid.IntRange(0, 3).Draw(t, name+" values") {
			_ = i
			form.Add(name, fields[name].Draw(t, name))
		}
	}
	if !form.Has("teams") && form.Get("from") != "grid" {
		form.Set("teams", rapid.SampledFrom(teams).Draw(t, "teams"))
	}
	return form
}

// Whatever an operator's form says, deploy and teardown, which need
// VM.Clone or VM.Allocate they don't hold, never get a confirm form or a
// job: every VM is blocked with the privilege missing. Nothing answers
// 403 for a role any more; a lead's same forms are never refused.
func TestPropOperatorCannotRunWhatProxmoxWouldRefuse(t *testing.T) {
	h := newHarness(t)
	h.poll()
	addMasters(h)
	op := h.login(asOperator)
	lead := h.login(asLead)
	targets := []string{"/deploy", "/deploy/preview", "/deploy/confirm", "/teardown", "/teardown/preview", "/teardown/confirm"}
	rapid.Check(t, func(t *rapid.T) {
		target := rapid.SampledFrom(targets).Draw(t, "target")
		form := drawForm(t, []string{"1", "2", "1-2", "1-3"})
		send := func(sess *authtest.Session) *httptest.ResponseRecorder {
			if target == "/deploy" || target == "/teardown" { // the forms
				return h.get(sess, target+"?"+form.Encode())
			}
			return h.post(sess, target, form)
		}
		before := len(h.jobsInStore())
		rec := send(&op)
		if n := len(h.jobsInStore()); n != before {
			t.Fatalf("operator POST %s %v stored %d jobs", target, form, n-before)
		}
		if strings.HasSuffix(target, "/preview") && confirmFormRE.MatchString(rec.Body.String()) {
			t.Fatalf("operator's %s %v offers a confirm:\n%s", target, form, rec.Body)
		}
		if rec.Code == http.StatusForbidden {
			t.Fatalf("operator %s = 403: nothing is refused for a role", target)
		}
		if leadRec := send(&lead); leadRec.Code == http.StatusForbidden {
			t.Fatalf("lead POST %s %v = 403\n%s", target, form, leadRec.Body)
		}
	})
}

// drawJob stores a job of any kind, over some or every team, in any state,
// with any item outcomes, and returns it.
func drawJob(t *rapid.T, h *harness) store.Job {
	kind := rapid.SampledFrom([]pods.Kind{pods.KindPower, pods.KindReset, pods.KindSnapshot, pods.KindDeploy, pods.KindTeardown}).Draw(t, "kind")
	in := jobs.Inputs{Kind: kind, Teams: rapid.SampledFrom([]string{"1", "2-3", "1-3"}).Draw(t, "teams")}
	switch kind {
	case pods.KindPower:
		in.Action = rapid.SampledFrom([]string{"start", "stop"}).Draw(t, "action")
	case pods.KindDeploy:
		in.Pattern = "*.kilo.alpha"
	case pods.KindSnapshot:
		in.Snapshot = rapid.SampledFrom([]string{"round2", "round3"}).Draw(t, "snapshot")
	}
	by := rapid.SampledFrom([]jobs.Submitter{byOperator, byLead}).Draw(t, "by")
	plan, err := jobs.BuildPlan(context.Background(), pods.NewPlanner(h.api, h.cfg), in)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	by.Credential, by.Seal = h.ticket("submitter"), h.creds
	id, err := jobs.Submit(ctx, h.st, in, plan, by)
	if err != nil {
		t.Fatal(err)
	}
	final := []string{store.StatusSucceeded, store.StatusCompletedWithFailures, store.StatusFailed, store.StatusCancelled,
		store.StatusInterrupted, store.StatusStale}
	switch st := rapid.SampledFrom(append(final, store.StatusPending, store.StatusRunning, "cancel pending", "cancel running")).Draw(t, "status"); st {
	case store.StatusPending:
	case "cancel pending":
		if err := h.st.RequestCancel(ctx, id, "lena@example.org"); err != nil {
			t.Fatal(err)
		}
	case store.StatusRunning, "cancel running":
		if !claim(t, h, id) {
			break // an earlier job holds its VMs: it stays pending
		}
		if st == "cancel running" {
			if err := h.st.RequestCancel(ctx, id, "lena@example.org"); err != nil {
				t.Fatal(err)
			}
		}
	default:
		if !claim(t, h, id) {
			break
		}
		items := map[string]store.ItemOutcome{}
		for _, it := range plan.Items {
			if o := rapid.SampledFrom([]string{"", store.ItemDone, store.ItemFailed, store.ItemBlocked, store.ItemRemoved}).Draw(t, "outcome of "+it.Name); o != "" {
				items[it.Name] = store.ItemOutcome{Status: o}
			}
		}
		if err := h.st.Finish(ctx, id, "w1", store.Outcome{Status: st, Items: items}); err != nil {
			t.Fatal(err)
		}
	}
	j, err := h.st.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// On any job's page, a user gets cancel, retry and "preview again" for a
// job they could run (they hold its privilege somewhere in Proxmox), or
// started, and a lock in their place otherwise. An operator holds power
// and snapshot privileges, not deploy's or teardown's; a lead holds all.
func TestPropJobActionsFollowPrivileges(t *testing.T) {
	h := newHarness(t)
	h.poll()
	addMasters(h)
	op := h.login(asOperator)
	lead := h.login(asLead)
	rapid.Check(t, func(t *rapid.T) {
		j := drawJob(t, h)
		mayRun := j.Kind != string(pods.KindDeploy) && j.Kind != string(pods.KindTeardown)
		cancellable := j.Active() && !j.CancelRequested
		path := "/logs/" + itoa(j.ID)
		defer endJob(t, h, j)

		actions := func(body string) string {
			start := strings.Index(body, `<div class="pf-acts" id="job-actions">`)
			if start < 0 {
				t.Fatalf("%s (%s, %s) has no actions:\n%s", path, j.Kind, j.Status, body)
			}
			end := strings.Index(body[start:], "</div>")
			return body[start : start+end]
		}
		la, oa := actions(h.get(&lead, path).Body.String()), actions(h.get(&op, path).Body.String())
		if strings.Contains(la, "not permitted") {
			t.Fatalf("a lead sees a lock on %s (%s, %s): %s", path, j.Kind, j.Status, la)
		}
		if cancellable != strings.Contains(la, path+"/cancel") {
			t.Fatalf("lead's cancel on %s (%s, cancel requested %v): %s", path, j.Status, j.CancelRequested, la)
		}
		if cancellable && mayRun != strings.Contains(oa, path+"/cancel") {
			t.Fatalf("operator's cancel on %s job %s: %s", j.Kind, path, oa)
		}
		if cancellable && mayRun == strings.Contains(oa, "Cancel · not permitted") {
			t.Fatalf("operator's cancel lock on %s job %s: %s", j.Kind, path, oa)
		}
		leadCan := strings.Contains(la, path+"/retry") || strings.Contains(la, "Preview again</a>")
		opCan := strings.Contains(oa, path+"/retry") || strings.Contains(oa, "Preview again</a>")
		opLock := strings.Contains(oa, "Retry · not permitted") || strings.Contains(oa, "Preview again · not permitted")
		switch {
		case !mayRun && opCan:
			t.Fatalf("an operator may run %s job %s (%s) again: %s", j.Kind, path, j.Status, oa)
		case !mayRun && leadCan && !opLock:
			t.Fatalf("an operator sees no lock where a lead may run %s job %s (%s) again: %s / %s", j.Kind, path, j.Status, oa, la)
		case mayRun && leadCan != opCan:
			t.Fatalf("%s job %s (%s): lead may run it again %v, operator %v: %s / %s", j.Kind, path, j.Status, leadCan, opCan, la, oa)
		case mayRun && opLock:
			t.Fatalf("an operator sees a lock on %s job %s (%s): %s", j.Kind, path, j.Status, oa)
		}
	})
}

func claim(t *rapid.T, h *harness, id int64) bool {
	j, err := h.st.ClaimJob(context.Background(), id, "w1")
	if err != nil {
		t.Fatalf("claiming job %d: %v", id, err)
	}
	return j != nil
}

func endJob(t *rapid.T, h *harness, j store.Job) {
	ctx := context.Background()
	switch j.Status {
	case store.StatusPending:
		if err := h.st.RequestCancel(ctx, j.ID, "lena@example.org"); err != nil && !errors.Is(err, store.ErrNotActive) {
			t.Fatal(err)
		}
	case store.StatusRunning:
		if err := h.st.Finish(ctx, j.ID, "w1", store.Outcome{Status: store.StatusCancelled}); err != nil {
			t.Fatal(err)
		}
	}
}

// Whatever is deployed, the grid and VM pages give an operator, who holds
// neither VM.Clone nor VM.Allocate, no way to deploy or tear down (no
// Deploy and Teardown header buttons, no Deploy buttons on the template sets or a missing VM); a
// lead gets those. Neither is told they lack a power or snapshot
// privilege on any VM.
func TestPropGridAndVMPagesFollowPrivileges(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Web.Templates = "*.kilo.alpha" })
	addMasters(h)
	h.api.Mu.Lock()
	all := maps.Clone(h.api.VMs)
	h.api.Mu.Unlock()
	op := h.login(asOperator)
	lead := h.login(asLead)
	rapid.Check(t, func(t *rapid.T) {
		kept := map[int]*podstest.VM{}
		for _, id := range slices.Sorted(maps.Keys(all)) {
			vm := all[id]
			if strings.HasPrefix(vm.Name, "team") && !rapid.Bool().Draw(t, "keep "+vm.Name) {
				continue
			}
			kept[id] = vm
		}
		h.api.Mu.Lock()
		h.api.VMs = kept
		h.api.Mu.Unlock()
		h.poll()
		g := h.poller.Grid()
		nothing := true
		for _, row := range g.Rows {
			for _, c := range row.Cells {
				nothing = nothing && c.State == status.StateMissing
			}
		}
		team := rapid.SampledFrom([]string{"01", "02", "03"}).Draw(t, "team")
		host := rapid.SampledFrom([]string{"dc", "web"}).Draw(t, "host")
		cell, _ := g.Cell(team, host)
		vmPath := "/vm/" + team + "/" + host

		for _, who := range []struct {
			sess authtest.Session
			lead bool
		}{{op, false}, {lead, true}} {
			grid := h.get(&who.sess, "/").Body.String()
			vm := h.get(&who.sess, vmPath).Body.String()
			for what, body := range map[string]string{"grid": grid, vmPath: vm} {
				if _, err := checkMarkup(body); err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				if who.lead != strings.Contains(body, "lead-ops") {
					t.Fatalf("lead %v: %s has the Deploy and Teardown buttons %v", who.lead, what, !who.lead)
				}
				if !who.lead && (strings.Contains(body, `href="/deploy`) || strings.Contains(body, `href="/teardown`)) {
					t.Fatalf("an operator's %s links to deploy or teardown", what)
				}
			}
			if strings.Contains(grid, `data-lacks`) {
				t.Fatalf("lead %v: a VM is marked as lacking a privilege they hold everywhere", who.lead)
			}
			// The template sets, with a lead's Deploy buttons, show only
			// while nothing is deployed.
			start := strings.Contains(grid, `<section class="start" id="grid-start"`)
			if deploy := strings.Contains(grid, "set-deploy"); nothing != start || deploy != (who.lead && nothing) {
				t.Fatalf("lead %v, nothing deployed %v: the sets show %v, with Deploy %v", who.lead, nothing, start, deploy)
			}
			missing := cell.State == status.StateMissing
			if who.lead && missing != strings.Contains(vm, `href="/deploy?`) {
				t.Fatalf("%s is %s, but the lead's page offers a deploy %v", vmPath, cell.State, !missing)
			}
		}
	})
}
