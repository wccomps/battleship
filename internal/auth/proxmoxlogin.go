package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// The Proxmox step of signing in. After the battleship login, the service
// signs the user in to Proxmox through its OpenID realm (Authentik already
// knows the user, so nothing is asked):
//
//  1. GET /auth/proxmox gets the auth URL from Proxmox, stores the state's
//     hash and the endpoint that answered, and redirects there.
//  2. GET /auth/proxmox/callback checks the state and sends the code to
//     the same endpoint (Proxmox keeps login state on that node's disk
//     only); the returned ticket is sealed into the session.

// proxmoxCallbackPath is where the identity provider sends the browser
// back; the Authentik provider behind the Proxmox realm must list it.
const proxmoxCallbackPath = "/auth/proxmox/callback"

func (s *Service) proxmoxRedirectURL() string { return s.cfg.Web.BaseURL + proxmoxCallbackPath }

// proxmoxStart is step 1.
func (s *Service) proxmoxStart(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	next := safeNext(r.URL.Query().Get("next"))
	sess, ok := s.liveSession(w, r, next)
	if !ok {
		return
	}
	noteSubject(r.Context(), sess.Subject)
	if sess.Username == "" {
		// Without preferred_username the callback can't tell whose Proxmox
		// sign-in it is. A page, not a redirect, so it can't loop.
		s.logf("auth: proxmox login not started: subject=%q: the session has no username", sess.Subject)
		s.render(w, r, Page{Status: http.StatusForbidden, Title: "Log in again",
			Message: "Battleship doesn't know your Authentik username, so it can't check the Proxmox sign-in is yours. Log in again; if this page comes back, Authentik isn't sending preferred_username.",
			Link:    "/auth/login?next=" + url.QueryEscape(next), LinkText: "Log in again"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), providerTimeout)
	defer cancel()
	authURL, endpoint, err := s.pve.OpenIDAuthURL(ctx, s.cfg.Proxmox.Realm, s.proxmoxRedirectURL())
	if err != nil {
		s.logf("auth: proxmox login failed to start: subject=%q realm=%q: %v", sess.Subject, s.cfg.Proxmox.Realm, err)
		s.render(w, r, proxmoxFailedPage(next))
		return
	}
	u, err := url.Parse(authURL)
	state := ""
	if err == nil {
		state = u.Query().Get("state")
	}
	if state == "" || (u.Scheme != "https" && u.Scheme != "http") {
		s.logf("auth: proxmox login failed to start: subject=%q: Proxmox returned an unusable URL", sess.Subject)
		s.render(w, r, proxmoxFailedPage(next))
		return
	}
	if err := s.st.SetSessionPVELogin(r.Context(), sess.ID, HashToken(state), next, endpoint); err != nil {
		s.serverError(w, r, "starting the Proxmox sign-in", err)
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// proxmoxCallback is step 2.
func (s *Service) proxmoxCallback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
	sess, ok := s.liveSession(w, r, "/")
	if !ok {
		return
	}
	noteSubject(r.Context(), sess.Subject)
	q := r.URL.Query()
	next := safeNext(sess.PVELoginNext)
	if sess.PVELoginState == "" || subtle.ConstantTimeCompare([]byte(HashToken(q.Get("state"))), []byte(sess.PVELoginState)) != 1 {
		if _, ok := s.usableTicket(sess, s.now()); ok {
			// Another tab's sign-in, started later, replaced this one's
			// state and has already finished: the session is signed in.
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		s.logf("auth: proxmox login failed: subject=%q: state doesn't match the session's", sess.Subject)
		s.render(w, r, Page{Status: http.StatusBadRequest, Title: "Proxmox sign-in didn't finish",
			Message: "The Proxmox sign-in expired, or was started in another tab. Please try again.",
			Link:    "/auth/proxmox?next=" + url.QueryEscape(next), LinkText: "Sign in to Proxmox"})
		return
	}
	if e := q.Get("error"); e != "" {
		if !errorCode.MatchString(e) {
			e = "unknown_error"
		}
		s.logf("auth: proxmox login failed: subject=%q: the identity provider returned error %q", sess.Subject, e)
		s.render(w, r, Page{Status: http.StatusForbidden, Title: "Proxmox sign-in refused",
			Message: "Authentik refused the Proxmox sign-in (" + e + "). If you think you should have access, ask a volunteer lead.",
			Link:    "/auth/proxmox?next=" + url.QueryEscape(next), LinkText: "Try again"})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), providerTimeout)
	defer cancel()
	cred, err := s.pve.OpenIDLogin(ctx, sess.PVELoginEndpoint, q.Get("code"), q.Get("state"), s.proxmoxRedirectURL(), s.now())
	if err != nil {
		// Proxmox answers every login failure with a bare 401 and logs the
		// reason in that node's journal.
		s.logf("auth: proxmox login failed: subject=%q node=%s (its journal has the reason): %v", sess.Subject, sess.PVELoginEndpoint, err)
		s.render(w, r, proxmoxFailedPage(next))
		return
	}
	// The realm names users <preferred_username>@<realm>; the browser's
	// Authentik session must be this session's user, or it would act as
	// someone else.
	if want := sess.Username + "@" + s.cfg.Proxmox.Realm; sess.Username == "" || !strings.EqualFold(cred.User, want) {
		s.logf("auth: proxmox login refused: subject=%q username=%q: Proxmox signed in %q", sess.Subject, sess.Username, cred.User)
		s.render(w, r, Page{Status: http.StatusForbidden, Title: "Proxmox sign-in refused",
			Message: "Proxmox signed you in as " + cred.User + ", but you are signed in to battleship as " + orUnknown(sess.Username) + " (" + s.cfg.Proxmox.Realm + "). " +
				"Log out of Authentik, then log in to battleship again as one person.",
			Link: "/auth/login", LinkText: "Log in again"})
		return
	}
	err = s.st.SetSessionTicket(r.Context(), sess.ID, time.Time{}, SealSessionTicket(s.cfg, sess.ID, cred))
	if errors.Is(err, store.ErrSessionNotFound) {
		s.clearSessionCookie(w)
		s.unauthenticated(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, "saving your Proxmox sign-in", err)
		return
	}
	s.logf("auth: proxmox login: subject=%q user=%q node=%s", sess.Subject, cred.User, sess.PVELoginEndpoint)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// liveSession finds the request's unexpired session, or sends the browser
// to log in (back to next) and reports false.
func (s *Service) liveSession(w http.ResponseWriter, r *http.Request, next string) (store.Session, bool) {
	sess, err := s.lookup(r)
	if err == nil && expired(s.cfg.Web, sess, s.now()) {
		s.endSession(w, r, sess)
		err = errNoSession
	}
	switch {
	case errors.Is(err, errNoSession):
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(next), http.StatusSeeOther)
		return sess, false
	case err != nil:
		s.serverError(w, r, "reading session", err)
		return sess, false
	}
	return sess, true
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown user (Authentik sent no preferred_username)"
	}
	return s
}

func proxmoxFailedPage(next string) Page {
	return Page{Status: http.StatusInternalServerError, Title: "Proxmox sign-in failed",
		Message: "Proxmox sign-in failed; try again. If it keeps failing, tell a lead: the Proxmox node's journal says why.",
		Link:    "/auth/proxmox?next=" + url.QueryEscape(next), LinkText: "Try again"}
}

// sessionTicket returns the session's usable Proxmox ticket, renewing it
// once it is ticket_renew_after old. ok is false once it has responded by
// sending the user through the Proxmox step.
func (s *Service) sessionTicket(ctx context.Context, w http.ResponseWriter, r *http.Request, sess store.Session, now time.Time) (proxmox.Credential, bool) {
	cred, ok := s.usableTicket(sess, now)
	if !ok {
		s.needProxmox(w, r)
		return cred, false
	}
	if now.Sub(cred.Issued) < s.cfg.Proxmox.TicketRenewAfter {
		return cred, true
	}
	rctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	next, err := s.pve.RenewTicket(rctx, cred, now)
	switch {
	case proxmox.RenewalRefused(err):
		s.logf("auth: proxmox ticket refused at renewal: subject=%q user=%q: %v", sess.Subject, cred.User, err)
		if err := s.st.ClearSessionTicket(ctx, sess.ID, cred.Issued); err != nil {
			s.logf("auth: clearing the Proxmox ticket of subject=%q: %v", sess.Subject, err)
		}
		s.needProxmox(w, r)
		return cred, false
	case err != nil:
		// Proxmox unreachable: the current ticket is still valid.
		s.logf("auth: renewing the proxmox ticket of subject=%q failed, using the current one: %v", sess.Subject, err)
		return cred, true
	}
	err = s.st.SetSessionTicket(ctx, sess.ID, cred.Issued, SealSessionTicket(s.cfg, sess.ID, next))
	if err != nil && !errors.Is(err, store.ErrTicketChanged) { // changed: another request renewed it first
		s.logf("auth: saving the renewed proxmox ticket of subject=%q: %v", sess.Subject, err)
	}
	return next, true
}

// usableTicket is the session's ticket if it can be used at now: it opens,
// hasn't expired, and its login is younger than proxmox.ticket_max_age.
func (s *Service) usableTicket(sess store.Session, now time.Time) (proxmox.Credential, bool) {
	cred, ok := s.openTicket(sess)
	return cred, ok && !cred.Lapsed(now, s.cfg.Proxmox.TicketMaxAge)
}

// needProxmox sends a session without a usable ticket through the Proxmox
// step, answering like unauthenticated.
func (s *Service) needProxmox(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !wantsEventStream(r) {
		http.Redirect(w, r, "/auth/proxmox?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	s.render(w, r, Page{
		Status:  http.StatusUnauthorized,
		Title:   "Sign in to Proxmox again",
		Message: "Your Proxmox sign-in has ended, so nothing was changed. Reload the page to sign in again, then try again.",
		Link:    "/auth/proxmox?next=/", LinkText: "Sign in to Proxmox",
	})
}
