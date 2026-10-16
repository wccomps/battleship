package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/wccomps/battleship/internal/store"
)

// login starts the authorization-code flow with PKCE, keeping its state in
// the sealed login cookie.
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

// errorCode matches an OAuth error code, the only part of a provider error
// the pages repeat.
var errorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// callback finishes the flow and creates the session. Who may sign in at
// all is Authentik's decision.
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
		RefreshToken: sealedRT,
		IDToken:      s.sealIDToken(HashToken(token), id.IDToken),
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

// logout ends the session and goes to the provider's logout page, if any,
// so the next login asks for credentials. It needs the CSRF token so
// another site can't log users out.
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
	// The provider's copy of the refresh token outlives the session row, so
	// revoke it. Logout goes on if that fails: the session is gone either way.
	if rt, err := s.openRefreshToken(sess.ID, sess.RefreshToken); err == nil && s.revokeURL != "" {
		if err := s.revoke(r.Context(), rt); err != nil {
			s.logf("auth: logout: subject=%q: revoking the refresh token: %v", sess.Subject, err)
		}
	}
	dest := "/"
	if s.endSessionURL != "" {
		dest = s.endSessionURL
		// id_token_hint tells the provider whose session to end, so it can
		// log the user out without asking which account.
		if hint := s.openIDToken(sess.ID, sess.IDToken); hint != "" {
			if u, err := url.Parse(dest); err == nil {
				q := u.Query()
				q.Set("id_token_hint", hint)
				q.Set("client_id", s.cfg.OIDC.ClientID)
				u.RawQuery = q.Encode()
				dest = u.String()
			}
		}
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// revokeTimeout bounds the revocation a logout waits for: the session is
// already gone, so a slow provider shouldn't hold the logout up for long.
const revokeTimeout = 3 * time.Second

// revoke asks the provider to revoke a refresh token (RFC 7009), as the
// client, the way the token endpoint authenticates it.
func (s *Service) revoke(ctx context.Context, refreshToken string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	form := url.Values{"token": {refreshToken}, "token_type_hint": {"refresh_token"}}
	if s.oauth.Endpoint.AuthStyle == oauth2.AuthStyleInParams {
		form.Set("client_id", s.oauth.ClientID)
		form.Set("client_secret", s.oauth.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.revokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if s.oauth.Endpoint.AuthStyle != oauth2.AuthStyleInParams {
		req.SetBasicAuth(url.QueryEscape(s.oauth.ClientID), url.QueryEscape(s.oauth.ClientSecret))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the provider answered %s", resp.Status)
	}
	return nil
}

func (s *Service) clearLoginCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.names.login, Value: "", Path: s.names.loginPath, MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// Pages the login flow shows. Authentik failures use 500, not 502, since
// Cloudflare replaces an origin 502 with its own page.
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
	noRefreshPage = Page{
		Status: http.StatusInternalServerError,
		Title:  "Login didn't finish",
		Message: "Authentik didn't issue a refresh token, which battleship needs to keep your access up to date. " +
			"An admin must allow the offline_access scope for the battleship provider in Authentik and list it in oidc.scopes.",
	}
)
