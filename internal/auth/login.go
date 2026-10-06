package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"regexp"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/wccomps/battleship/internal/store"
)

// login starts the authorization-code flow: it seals a fresh state, nonce
// and PKCE verifier, with where to go afterwards, into the login cookie and
// sends the browser to the identity provider.
func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	st := loginState{
		State:    randomToken(),
		Nonce:    randomToken(),
		Verifier: oauth2.GenerateVerifier(),
		Next:     safeNext(r.URL.Query().Get("next")),
		Issued:   s.now().Unix(),
	}
	value, err := sealLoginState(s.loginKey, st)
	if err != nil {
		s.serverError(w, r, "starting login", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.names.login, Value: value, Path: s.names.loginPath, MaxAge: int(loginTTL.Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, // Lax: the provider's redirect back is a top-level GET
	})
	noStore(w)
	http.Redirect(w, r, s.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

// errorCode matches an OAuth error code, which is all of the provider's
// error the pages repeat.
var errorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// callback finishes the flow: it checks state against the login cookie,
// exchanges the code with the PKCE verifier, verifies the ID token and its
// nonce, reads the user's groups, and creates the session. Who may sign
// in at all is Authentik's decision (the application's policy bindings).
func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.clearLoginCookie(w) // whatever happens, so this browser can't use it twice

	fail := func(p Page, why string, args ...any) {
		s.logf("auth: login failed: "+why, args...)
		s.render(w, r, p)
	}
	c, err := r.Cookie(s.names.login)
	if err != nil {
		fail(retryPage, "no login cookie")
		return
	}
	st, err := openLoginState(s.loginKey, c.Value, s.now())
	if err != nil {
		fail(retryPage, "login cookie: %v", err)
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		fail(retryPage, "state doesn't match the login cookie")
		return
	}
	if e := q.Get("error"); e != "" {
		if !errorCode.MatchString(e) {
			e = "unknown_error"
		}
		fail(Page{
			Status:  http.StatusForbidden,
			Title:   "Login refused",
			Message: "Authentik refused the login (" + e + "). If you think you should have access, ask a volunteer lead.",
			Link:    "/auth/login", LinkText: "Try logging in again",
		}, "the provider returned error %q", e)
		return
	}
	code := q.Get("code")
	if code == "" {
		fail(retryPage, "no code in the callback")
		return
	}

	ctx, cancel := s.providerContext(r.Context())
	defer cancel()
	tok, err := s.oauth.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		fail(providerPage, "exchanging the code: %s", describe(err))
		return
	}
	id, err := s.identify(ctx, tok, expect{nonce: st.Nonce})
	if err != nil {
		var lf *loginFailure
		if !errors.As(err, &lf) {
			lf = &loginFailure{page: providerPage, err: err}
		}
		fail(lf.page, "%v", lf.err)
		return
	}
	if tok.RefreshToken == "" {
		fail(noRefreshPage, "subject=%q: the provider issued no refresh token; grant the offline_access scope", id.Subject)
		return
	}

	// Once the login has succeeded at the provider, the browser's old
	// session is dropped.
	old, err := r.Cookie(s.names.session)
	hadOld := err == nil
	if hadOld && wellFormed(old.Value) {
		if err := s.st.DeleteSession(context.WithoutCancel(r.Context()), HashToken(old.Value)); err != nil {
			s.logf("auth: deleting the previous session of subject=%q: %v", id.Subject, err)
		}
	}

	noteSubject(r.Context(), id.Subject)
	token, now := randomToken(), s.now()
	sealedRT := s.sealRefreshToken(HashToken(token), tok.RefreshToken)
	sess := store.Session{
		ID:           HashToken(token),
		Subject:      id.Subject,
		Username:     id.Username,
		Name:         id.Name,
		Email:        id.Email,
		Groups:       id.Groups,
		RefreshToken: sealedRT,
		CSRF:         randomToken(),
		CreatedAt:    now,
		LastSeen:     now,
		RefreshedAt:  now,
		ExpiresAt:    expiresAt(s.cfg.Web, now, now),
	}
	if err := s.st.CreateSession(r.Context(), sess); err != nil {
		s.serverError(w, r, "saving your session", err)
		return
	}
	s.setSessionCookie(w, token, sess.ExpiresAt.Sub(now))
	s.logf("auth: login: subject=%q name=%q email=%q", id.Subject, id.Name, id.Email)
	// On to the Proxmox step (proxmoxlogin.go), then where the user was going.
	http.Redirect(w, r, "/auth/proxmox?next="+url.QueryEscape(st.Next), http.StatusSeeOther)
}

// logout ends the session and sends the browser to the provider's logout
// page, if discovery named one, so the next login asks for credentials. It
// needs the CSRF token, so another site can't log users out.
func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	sess, err := s.lookup(r)
	if errors.Is(err, errNoSession) {
		s.clearSessionCookie(w)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.serverError(w, r, "reading session", err)
		return
	}
	noteSubject(r.Context(), sess.Subject)
	if reason := s.csrfProblem(r, sess.CSRF); reason != "" {
		s.rejectCSRF(w, r, sess.Subject, reason)
		return
	}
	if err := s.st.DeleteSession(r.Context(), sess.ID); err != nil {
		s.serverError(w, r, "ending your session", err)
		return
	}
	s.clearSessionCookie(w)
	s.logf("auth: logout: subject=%q", sess.Subject)
	dest := "/"
	if s.endSessionURL != "" {
		dest = s.endSessionURL
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Service) clearLoginCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.names.login, Value: "", Path: s.names.loginPath, MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// Pages the login flow shows. Failures caused by Authentik use 500, not 502:
// Cloudflare replaces an origin 502 with its own "Bad gateway" page, which
// would hide the explanation volunteers and admins need.
var (
	retryPage = Page{
		Status:  http.StatusBadRequest,
		Title:   "Login didn't finish",
		Message: "The login expired, or was started in another tab. Please try again.",
		Link:    "/auth/login", LinkText: "Log in",
	}
	providerPage = Page{
		Status:  http.StatusInternalServerError,
		Title:   "Login didn't finish",
		Message: "Battleship couldn't complete the login with Authentik. Please try again; if it keeps failing, tell a lead.",
		Link:    "/auth/login", LinkText: "Try logging in again",
	}
	unverifiedPage = Page{
		Status:  http.StatusForbidden,
		Title:   "Login refused",
		Message: "The login couldn't be verified, so battleship didn't log you in. Please try again; if it keeps failing, tell a lead.",
		Link:    "/auth/login", LinkText: "Try logging in again",
	}
	groupsPage = Page{
		Status:  http.StatusInternalServerError,
		Title:   "Login didn't finish",
		Message: "Authentik sent the user's groups in a form battleship can't read. An admin should check the groups scope mapping and oidc.groups_claim.",
	}
	noRefreshPage = Page{
		Status: http.StatusInternalServerError,
		Title:  "Login didn't finish",
		Message: "Authentik didn't issue a refresh token, which battleship needs to keep your access up to date. " +
			"An admin must allow the offline_access scope for the battleship provider in Authentik and list it in oidc.scopes.",
	}
)
