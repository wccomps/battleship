package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/store"
)

type ctxKey struct{}

// requestAuth is what RequireUser puts in the request context.
type requestAuth struct {
	user        User
	csrf        string
	session     string    // the session's store ID
	refreshedAt time.Time // the session's last successful group check
}

// UserFrom returns the logged-in user RequireUser put in ctx.
func UserFrom(ctx context.Context) (User, bool) {
	ra, ok := ctx.Value(ctxKey{}).(*requestAuth)
	if !ok {
		return User{}, false
	}
	return ra.user, true
}

// CSRFToken returns the session's CSRF token for forms to carry in the
// CSRFField field, or "" outside RequireUser.
func CSRFToken(ctx context.Context) string {
	ra, ok := ctx.Value(ctxKey{}).(*requestAuth)
	if !ok {
		return ""
	}
	return ra.csrf
}

// SessionRefreshedAt returns when the request's session last checked the
// user's groups with the identity provider (its login, or its last
// successful refresh, including one this request just made), or the zero
// time outside RequireUser. The next check falls due web.session_refresh
// later; long-lived responses such as event streams end around then (the
// web app's add up to a tenth more at random), so they pass through the
// check again.
func SessionRefreshedAt(ctx context.Context) time.Time {
	ra, ok := ctx.Value(ctxKey{}).(*requestAuth)
	if !ok {
		return time.Time{}
	}
	return ra.refreshedAt
}

// SessionID returns the store ID of the request's session (the hash of
// its cookie token, never the token), or "" outside RequireUser. Records
// that belong to a session, such as the web app's previews, are keyed by
// it.
func SessionID(ctx context.Context) string {
	ra, ok := ctx.Value(ctxKey{}).(*requestAuth)
	if !ok {
		return ""
	}
	return ra.session
}

type subjectKey struct{}

// subjectSlot is where RequireUser notes the subject for TrackSubject.
type subjectSlot struct{ subject string }

// TrackSubject returns a context for a request and a function that, once
// the request is served, says who made it: the subject of the session
// RequireUser or logout accepted, or of the identity the login callback
// verified, even if the request was then refused; "" if none. It is for a
// request log, which wraps the handlers and can't see their context.
func TrackSubject(ctx context.Context) (context.Context, func() string) {
	slot := &subjectSlot{}
	return context.WithValue(ctx, subjectKey{}, slot), func() string { return slot.subject }
}

// noteSubject fills the request's TrackSubject slot, if it has one.
func noteSubject(ctx context.Context, subject string) {
	if slot, ok := ctx.Value(subjectKey{}).(*subjectSlot); ok {
		slot.subject = subject
	}
}

// RequireUser serves next only for a live session, with the User in the
// request context (UserFrom, CSRFToken). For each request it:
//
//   - ends sessions past session_idle or session_max;
//   - refuses state-changing requests without the CSRF token (see
//     csrfProblem), before refreshing the session;
//   - every session_refresh, re-reads the user's groups from the identity
//     provider with the refresh token (see refreshSession for when a
//     failed refresh ends the session);
//   - records activity, sliding the idle expiry;
//   - finds the session's Proxmox ticket, renewing it when it is due, and
//     sends the user through the Proxmox sign-in when there is none
//     usable (see sessionTicket); handlers find it with
//     ProxmoxCredential.
//
// Without a session, GET and HEAD redirect to the login page, which comes
// back to the same URL; event streams and other methods get 401.
func (s *Service) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		sess, err := s.lookup(r)
		if errors.Is(err, errNoSession) {
			if s.hasSessionCookie(r) {
				s.clearSessionCookie(w)
			}
			s.unauthenticated(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, "reading session", err)
			return
		}
		now := s.now()
		if expired(s.cfg.Web, sess, now) {
			s.logf("auth: session expired: subject=%q", sess.Subject)
			s.endSession(w, r, sess)
			s.unauthenticated(w, r)
			return
		}
		noteSubject(ctx, sess.Subject)
		if reason := s.csrfProblem(r, sess.CSRF); reason != "" {
			s.rejectCSRF(w, r, sess.Subject, reason)
			return
		}

		// The refresh finishes even if the browser goes away: the provider
		// rotates the refresh token as it answers, so an abandoned refresh
		// would leave the session with a spent one.
		sess, ok := s.refreshSession(context.WithoutCancel(ctx), w, r, sess, now)
		if !ok {
			return
		}

		if now.Sub(sess.LastSeen) >= touchEvery(s.cfg.Web) {
			exp := expiresAt(s.cfg.Web, sess.CreatedAt, now)
			err := s.st.TouchSession(ctx, sess.ID, now, exp)
			if errors.Is(err, store.ErrSessionNotFound) { // logged out meanwhile
				s.clearSessionCookie(w)
				s.unauthenticated(w, r)
				return
			}
			if err != nil {
				s.serverError(w, r, "recording activity", err)
				return
			}
			c, _ := r.Cookie(s.names.session)
			s.setSessionCookie(w, c.Value, exp.Sub(now))
		}

		cred, ok := s.sessionTicket(context.WithoutCancel(ctx), w, r, sess, now)
		if !ok {
			return
		}

		u := User{Subject: sess.Subject, Name: sess.Name, Email: sess.Email}
		ctx = context.WithValue(ctx, ctxKey{}, &requestAuth{user: u, csrf: sess.CSRF, session: sess.ID, refreshedAt: sess.RefreshedAt})
		ctx = WithProxmoxCredential(ctx, cred)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// refreshBackoff is how long after a claim, or after failing to reach the
// identity provider, the next refresh may start.
const refreshBackoff = time.Minute

// refreshGrace is how many session_refresh periods a session may go
// without a successful refresh while the identity provider is unreachable.
const refreshGrace = 3

// refreshSession re-checks the session with the identity provider once
// session_refresh has passed. ok is false once it has responded (the
// session ended, or the database failed).
//
//   - Only the request that wins the claim refreshes; concurrent ones, and
//     requests during a backoff, carry on with the groups they read.
//   - A rejection (an OAuth error answer such as invalid_grant, an ID
//     token that fails verification, another subject) ends the session.
//   - If the provider is unreachable, the session keeps its current groups
//     and the next refresh is tried refreshBackoff later; the first try
//     that fails refreshGrace × session_refresh or more after the last
//     success ends it.
func (s *Service) refreshSession(ctx context.Context, w http.ResponseWriter, r *http.Request, sess store.Session, now time.Time) (_ store.Session, ok bool) {
	every := s.cfg.Web.SessionRefresh
	if now.Sub(sess.RefreshedAt) < every {
		return sess, true
	}
	if now.Before(sess.RefreshRetryAt) {
		// Another request is refreshing, or the last try couldn't reach
		// the provider and is backing off.
		return sess, true
	}
	won, err := s.st.ClaimSessionRefresh(ctx, sess.ID, sess.RefreshedAt, now, now.Add(refreshBackoff))
	if err != nil {
		s.serverError(w, r, "claiming session refresh", err)
		return sess, false
	}
	if !won {
		return sess, true
	}
	id, refreshToken, err := s.refresh(ctx, sess)
	if err != nil && !unreachable(err) {
		s.logf("auth: refresh rejected: subject=%q: %v; ending the session", sess.Subject, err)
		s.endSession(w, r, sess)
		s.unauthenticated(w, r)
		return sess, false
	}
	if err != nil {
		failedAt := s.now()
		graceEnd := sess.RefreshedAt.Add(refreshGrace * every)
		if !failedAt.Before(graceEnd) {
			s.logf("auth: refresh failed: subject=%q: %v; no successful refresh since %s, ending the session",
				sess.Subject, err, stamp(sess.RefreshedAt))
			s.endSession(w, r, sess)
			s.unauthenticated(w, r)
			return sess, false
		}
		rerr := s.st.RecordSessionRefreshFailure(ctx, sess.ID, refreshToken, failedAt, failedAt.Add(refreshBackoff))
		if errors.Is(rerr, store.ErrSessionNotFound) { // logged out meanwhile
			s.clearSessionCookie(w)
			s.unauthenticated(w, r)
			return sess, false
		}
		if rerr != nil {
			s.serverError(w, r, "recording refresh failure", rerr)
			return sess, false
		}
		s.logf("auth: refresh failed: subject=%q: %v; the identity provider is unreachable, keeping the session with its current groups until %s at the latest",
			sess.Subject, err, stamp(graceEnd))
		return sess, true
	}
	up := store.SessionUpdate{Username: id.Username, Name: id.Name, Email: id.Email, Groups: id.Groups, RefreshToken: refreshToken, RefreshedAt: now}
	err = s.st.UpdateSessionGroups(ctx, sess.ID, up)
	if errors.Is(err, store.ErrSessionNotFound) { // logged out meanwhile
		s.clearSessionCookie(w)
		s.unauthenticated(w, r)
		return sess, false
	}
	if err != nil {
		s.serverError(w, r, "saving refreshed session", err)
		return sess, false
	}
	sess.Name, sess.Email, sess.Groups, sess.RefreshToken, sess.RefreshedAt = id.Name, id.Email, id.Groups, refreshToken, now
	sess.Username = id.Username
	sess.RefreshRetryAt, sess.RefreshFailedAt = time.Time{}, time.Time{}
	return sess, true
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// csrfProblem returns why an unsafe request fails the CSRF check, or "".
// Requests other than GET and HEAD need the session's CSRF token (want) in
// the CSRFField form field, and any Origin header must match web.base_url.
// DELETE bodies aren't parsed, so a DELETE never passes.
func (s *Service) csrfProblem(r *http.Request, want string) string {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return ""
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" || normalOrigin(u) != s.origin {
			return "origin " + strconv.Quote(truncate(o, 100)) + " is not " + s.origin
		}
	}
	got := r.PostFormValue(CSRFField)
	switch {
	case want == "":
		return "no session"
	case got == "":
		return "missing token"
	case subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1:
		return "wrong token"
	}
	return ""
}

func (s *Service) rejectCSRF(w http.ResponseWriter, r *http.Request, subject, reason string) {
	s.logf("auth: csrf rejected: subject=%q %s %q: %s", subject, r.Method, r.URL.Path, reason)
	s.render(w, r, Page{
		Status:  http.StatusForbidden,
		Title:   "Request refused",
		Message: "This form was out of date or came from another site, so nothing was changed. Go back, reload the page and try again.",
		Link:    "/", LinkText: "Back to battleship",
	})
}

// unauthenticated sends a browser without a session to log in. Only GET and
// HEAD redirect, back to the same URL afterwards; a form post or script gets
// 401, since replaying it after login would be surprising. So does a request
// for an event stream (Accept: text/event-stream): EventSource can't follow
// a redirect to a login page, and the page's script reloads on the 401.
func (s *Service) unauthenticated(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !wantsEventStream(r) {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	s.render(w, r, Page{
		Status:  http.StatusUnauthorized,
		Title:   "Please log in",
		Message: "Your session has ended, so nothing was changed. Log in, then try again.",
		Link:    "/auth/login", LinkText: "Log in",
	})
}

// wantsEventStream reports whether r's Accept header lists
// text/event-stream.
func wantsEventStream(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		for part := range strings.SplitSeq(v, ",") {
			mt, _, _ := strings.Cut(part, ";")
			if strings.EqualFold(strings.TrimSpace(mt), "text/event-stream") {
				return true
			}
		}
	}
	return false
}

// endSession deletes a session and clears its cookie. The delete finishes
// even if the browser has gone. A failed delete is logged; the session
// still can't be used past its expiry.
func (s *Service) endSession(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if err := s.st.DeleteSession(context.WithoutCancel(r.Context()), sess.ID); err != nil {
		s.logf("auth: deleting session of subject=%q: %v", sess.Subject, err)
	}
	s.clearSessionCookie(w)
}

func (s *Service) serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.logf("auth: %s: %v", what, err)
	s.render(w, r, Page{
		Status:  http.StatusInternalServerError,
		Title:   "Something went wrong",
		Message: fmt.Sprintf("Battleship couldn't check your login (%s). Try again in a moment; if it keeps happening, tell a lead.", what),
		Link:    "/", LinkText: "Try again",
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
