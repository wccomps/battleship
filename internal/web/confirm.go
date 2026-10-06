package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// opConfirm submits a previewed plan as a job. It trusts nothing the form
// says about the plan: it plans the posted inputs again, as the user (with
// their privileges), and submits it only if all of these hold:
//
//   - the form names a preview of this session (by its nonce) that no job
//     has submitted yet;
//   - the posted inputs and fingerprint are the ones that preview stored,
//     so a form edited after the preview is refused;
//   - the new plan has the preview's fingerprint, so the cluster hasn't
//     changed since; otherwise, or once the preview is older than
//     previewTTL, the new plan is shown to be checked again;
//   - the team range was typed, where that is required.
//
// The job consumes the preview in the same transaction that stores it, so
// a double-clicked confirm makes one job; the second click lands on it.
func (s *Server) opConfirm(op operation) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		u, _ := auth.UserFrom(ctx)
		form := postedForm(r)
		in := formInputs(op.Kind, form)
		if err := in.Validate(); err != nil {
			s.refuseConfirm(w, r, op, "the inputs are invalid: "+err.Error())
			return
		}
		plan, err := jobs.BuildPlan(ctx, s.planner(r.Context()), in)
		if err != nil {
			s.errorPage(w, r, http.StatusConflict, "Couldn't check the plan again",
				"Battleship couldn't plan this again, so nothing was submitted: "+sentence(proxmox.Describe(err)),
				formQuery(op, in), "Back to "+op.Title)
			return
		}
		allTeams := s.coversAllTeams(ctx, plan.Teams)

		// A retry's preview shown again still names the job it retries.
		// The number is only shown; nothing depends on it.
		var retryOf int64
		if n, err := strconv.ParseInt(form.Get(fieldRetryOf), 10, 64); err == nil && n > 0 {
			retryOf = n
		}

		grid := fromGrid(form) // shapes the preview if it is shown again; not what is confirmed

		nonce := form.Get(fieldNonce)
		sessionID := auth.SessionID(ctx)
		prev, err := s.st.Preview(ctx, sessionID, auth.HashToken(nonce))
		switch {
		case nonce == "" || errors.Is(err, store.ErrPreviewNotFound):
			s.logf("web: confirm refused: subject=%q kind=%s: no such preview in this session", u.Subject, op.Kind)
			s.errorPage(w, r, http.StatusConflict, "Preview it again",
				"This confirm doesn't match a preview you made in this login (you may have logged in again since), so nothing was submitted. Preview it again, check it, then confirm.",
				formQuery(op, in), "Back to "+op.Title)
			return
		case err != nil:
			s.serverError(w, r, "reading a preview", err)
			return
		case prev.JobID != 0:
			s.alreadySubmitted(w, r, prev.JobID)
			return
		}
		if problem := s.differsFromPreview(prev, op, in, form.Get(fieldFingerprint)); problem != "" {
			s.refuseConfirm(w, r, op, problem)
			return
		}

		// expired shows the plan again, as a new preview, when the preview
		// had expired: here, or in the store as the job was stored.
		expired := func() {
			s.showPreview(w, r, http.StatusConflict, op, in, plan, allTeams, previewOptions{retryOf: retryOf, fromGrid: grid, problem: &banner{
				Level: "warn", Title: "This preview had expired",
				Text: []string{fmt.Sprintf("Previews can be confirmed for %s. Here is the plan again, read from the cluster just now. Nothing was submitted; check it and confirm again.", spoken(previewTTL))},
			}})
		}
		fp := jobs.Fingerprint(plan)
		switch {
		case !s.now().Before(prev.ExpiresAt):
			expired()
			return
		case fp != prev.Fingerprint:
			s.showPreview(w, r, http.StatusConflict, op, in, plan, allTeams, previewOptions{retryOf: retryOf, fromGrid: grid, problem: &banner{
				Level: "bad", Title: "The cluster changed since your preview; check it again",
				Text: []string{"Nothing was submitted. Below is the plan as it is now. Read it again, and confirm if it is still right."},
			}})
			return
		}
		if op.Kind.TypedConfirm() {
			typed := strings.TrimSpace(form.Get(fieldTyped))
			if typed != in.Teams {
				msg := fmt.Sprintf("To confirm, type the team range exactly as shown: %s.", in.Teams)
				if typed != "" {
					msg = fmt.Sprintf("You typed %q, which isn't the team range. Type it exactly as shown: %s.", typed, in.Teams)
				}
				s.showPreview(w, r, http.StatusUnprocessableEntity, op, in, plan, allTeams, previewOptions{nonce: nonce, retryOf: retryOf, fromGrid: grid, problem: &banner{
					Level: "bad", Title: "Type the team range to confirm", Text: []string{msg + " Nothing was submitted."},
				}})
				return
			}
		}

		if s.beforeSubmit != nil {
			s.beforeSubmit()
		}
		// The store checks the preview's expiry again as it stores the job,
		// in case it expired since the check above.
		// The job runs as the user: it carries their Proxmox ticket.
		cred, _ := auth.ProxmoxCredential(ctx)
		id, err := jobs.Submit(ctx, s.st, in, plan, jobs.Submitter{
			User:       actor(u),
			Preview:    &store.PreviewClaim{SessionID: sessionID, ID: prev.ID, At: s.now()},
			Credential: cred,
			Seal:       s.creds,
		})
		switch {
		case errors.Is(err, store.ErrPreviewExpired):
			expired()
			return
		case errors.Is(err, store.ErrPreviewUsed):
			// Another click of the same confirm got there first.
			if again, err := s.st.Preview(ctx, sessionID, prev.ID); err == nil && again.JobID != 0 {
				s.alreadySubmitted(w, r, again.JobID)
				return
			}
			s.serverError(w, r, "finding the job of a submitted preview", err)
			return
		case errors.Is(err, jobs.ErrNothingToRun):
			s.showPreview(w, r, http.StatusConflict, op, in, plan, allTeams, previewOptions{retryOf: retryOf, fromGrid: grid})
			return
		case err != nil:
			s.serverError(w, r, "submitting a job", err)
			return
		}
		s.logf("web: job %d submitted: subject=%q as=%q kind=%s teams=%s", id, u.Subject, cred.User, op.Kind, in.Teams)
		s.setFlash(w, flashSuccess, fmt.Sprintf("Job %d submitted: %s. It starts as soon as a worker is free.", id, summaryOf(op, in, plan)))
		http.Redirect(w, r, logHref(id), http.StatusSeeOther)
	})
}

// differsFromPreview says how a confirm differs from the preview it names,
// or "" if it doesn't: same operation, same inputs, same fingerprint.
func (s *Server) differsFromPreview(prev store.Preview, op operation, in jobs.Inputs, fingerprint string) string {
	if prev.Kind != string(op.Kind) {
		return fmt.Sprintf("the preview was of a %s, not a %s", prev.Kind, op.Kind)
	}
	var was jobs.Inputs
	if err := json.Unmarshal(prev.Inputs, &was); err != nil {
		return "the stored preview can't be read"
	}
	a, _ := json.Marshal(was) // plain struct: can't fail
	b, _ := json.Marshal(in)
	if string(a) != string(b) {
		return "the inputs differ from the preview's"
	}
	if fingerprint != prev.Fingerprint {
		return "the fingerprint differs from the preview's"
	}
	return ""
}

// refuseConfirm answers a confirm that doesn't match its preview: someone
// edited the form, or it is broken. Nothing is submitted.
func (s *Server) refuseConfirm(w http.ResponseWriter, r *http.Request, op operation, why string) {
	u, _ := auth.UserFrom(r.Context())
	s.logf("web: confirm refused: subject=%q kind=%s: %s", u.Subject, op.Kind, why)
	s.errorPage(w, r, http.StatusBadRequest, "Confirm refused",
		"This confirm doesn't match the preview it came from, so nothing was submitted. Start again from the form and confirm the new preview.",
		op.Path, "Back to "+op.Title)
}

// alreadySubmitted sends a repeated confirm to the job it already made.
func (s *Server) alreadySubmitted(w http.ResponseWriter, r *http.Request, jobID int64) {
	s.setFlash(w, flashInfo, fmt.Sprintf("You already confirmed this preview: it is job %d. Nothing new was submitted.", jobID))
	http.Redirect(w, r, logHref(jobID), http.StatusSeeOther)
}

// serverError logs err and shows a 500 page.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.logf("web: %s: %v", what, err)
	s.errorPage(w, r, http.StatusInternalServerError, "Something went wrong",
		"Battleship had a problem "+what+". Nothing more was done. Try again in a moment, and tell a lead if it keeps happening.", "/", "Back to the grid")
}

// summaryOf describes a submitted job in a few words, e.g. "reset of team
// 01 to initial (2 VMs)".
func summaryOf(op operation, in jobs.Inputs, plan *pods.Plan) string {
	teams := "team " + strings.Join(plan.Teams, ", ")
	if len(plan.Teams) != 1 {
		teams = "teams " + strings.Join(plan.Teams, ", ")
	}
	what := op.Noun + " of " + teams
	switch op.Kind {
	case pods.KindPower:
		what = strings.ToLower(powerLabel(in.Action)) + " on " + teams
	case pods.KindReset:
		what += " to " + in.Snapshot
	case pods.KindSnapshot:
		what = "snapshot " + in.Snapshot + " of " + teams
	}
	return what + " (" + countVMs(len(plan.Runnable())) + ")"
}
