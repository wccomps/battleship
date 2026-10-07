package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/wccomps/battleship/internal/store"
)

// identity is who the identity provider says the user is.
type identity struct {
	Subject  string
	Username string // preferred_username
	Name     string
	Email    string
	Groups   []string
}

// expect is what identify checks beyond the ID token's signature, issuer,
// audience and expiry.
type expect struct {
	// nonce, at login, must match the ID token's nonce. A login needs an
	// ID token.
	nonce string
	// subject, at refresh, must match the ID token (if the response has
	// one) and userinfo.
	subject string
}

// loginFailure is an identify error with the page the callback shows.
type loginFailure struct {
	page Page
	err  error
}

func (f *loginFailure) Error() string { return f.err.Error() }
func (f *loginFailure) Unwrap() error { return f.err }

// unreachableError means the identity provider gave no usable answer, as
// opposed to a rejection. See oauthRejection and userinfoUnreachable.
type unreachableError struct{ err error }

func (e *unreachableError) Error() string { return e.err.Error() }
func (e *unreachableError) Unwrap() error { return e.err }

// unreachable reports whether err is a failure to reach the provider, as
// opposed to a rejection.
func unreachable(err error) bool {
	var u *unreachableError
	return errors.As(err, &u)
}

// keyFetchKey carries a slot through Verify where keySet records a JWKS
// fetch failure; Verify itself flattens that error.
type keyFetchKey struct{}

// keySet is the provider's JWKS with key-fetch failures made visible, so a
// JWKS outage reads as unreachable rather than as a bad signature.
type keySet struct{ remote *oidc.RemoteKeySet }

func (k keySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	payload, err := k.remote.VerifySignature(ctx, jwt)
	// RemoteKeySet wraps (%w) only key-fetch failures; the auth tests pin this.
	if err != nil && errors.Unwrap(err) != nil {
		if slot, ok := ctx.Value(keyFetchKey{}).(*error); ok {
			*slot = err
		}
	}
	return payload, err
}

func unverified(format string, args ...any) error {
	return &loginFailure{page: unverifiedPage, err: fmt.Errorf(format, args...)}
}

// identify reads the user's identity and groups from a token response.
// Groups fall back to userinfo when the ID token lacks the claim or is
// absent (some refresh responses have none).
func (s *Service) identify(ctx context.Context, tok *oauth2.Token, want expect) (identity, error) {
	var id identity
	claims := map[string]json.RawMessage{}
	raw, _ := tok.Extra("id_token").(string)
	switch {
	case raw != "":
		var fetchErr error
		idt, err := s.verifier.Verify(context.WithValue(ctx, keyFetchKey{}, &fetchErr), raw)
		if fetchErr != nil {
			return id, &loginFailure{page: providerPage, err: &unreachableError{&providerError{"fetching the provider's signing keys", fetchErr}}}
		}
		if err != nil {
			return id, unverified("ID token: %v", err)
		}
		if want.nonce != "" && subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(want.nonce)) != 1 {
			return id, unverified("ID token nonce doesn't match the login")
		}
		if idt.AccessTokenHash != "" {
			if err := idt.VerifyAccessToken(tok.AccessToken); err != nil {
				return id, unverified("ID token: %v", err)
			}
		}
		if err := idt.Claims(&claims); err != nil {
			return id, unverified("ID token claims: %v", err)
		}
		id.Subject = idt.Subject
	case want.nonce != "":
		return id, unverified("the token response has no ID token")
	}
	if want.subject != "" && id.Subject != "" && id.Subject != want.subject {
		return id, unverified("ID token is for subject %q, not %q", id.Subject, want.subject)
	}
	id.Name, id.Email, id.Username = displayName(claims), stringClaim(claims, "email"), stringClaim(claims, "preferred_username")

	groups, found, err := groupsFrom(claims, s.cfg.OIDC.GroupsClaim)
	if err != nil {
		return id, &loginFailure{page: groupsPage, err: fmt.Errorf("ID token: %w", err)}
	}
	if !found {
		info, err := s.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		if err != nil {
			unreach := userinfoUnreachable(err)
			err = &providerError{"reading userinfo", err}
			if unreach {
				err = &unreachableError{err}
			}
			return id, &loginFailure{page: providerPage, err: err}
		}
		switch {
		case info.Subject == "":
			return id, unverified("userinfo has no subject")
		case id.Subject == "":
			id.Subject = info.Subject
		case info.Subject != id.Subject:
			return id, unverified("userinfo is for subject %q, not %q", info.Subject, id.Subject)
		}
		uclaims := map[string]json.RawMessage{}
		if err := info.Claims(&uclaims); err != nil {
			return id, &loginFailure{page: providerPage, err: fmt.Errorf("userinfo claims: %w", err)}
		}
		if groups, _, err = groupsFrom(uclaims, s.cfg.OIDC.GroupsClaim); err != nil {
			return id, &loginFailure{page: groupsPage, err: fmt.Errorf("userinfo: %w", err)}
		}
		if id.Name == "" {
			id.Name = displayName(uclaims)
		}
		if id.Email == "" {
			id.Email = stringClaim(uclaims, "email")
		}
		if id.Username == "" {
			id.Username = stringClaim(uclaims, "preferred_username")
		}
	}
	if id.Subject == "" {
		return id, unverified("no subject")
	}
	if want.subject != "" && id.Subject != want.subject {
		return id, unverified("the refreshed identity is subject %q, not %q", id.Subject, want.subject)
	}
	if id.Name == "" {
		id.Name = id.Subject
	}
	id.Groups = groups
	return id, nil
}

// refresh spends the session's refresh token for a fresh identity. It
// returns the (possibly rotated) sealed token to keep, even when identify
// then fails, so a later failure doesn't strand a spent token. A stored
// token that doesn't open is a rejection and is never sent.
func (s *Service) refresh(ctx context.Context, sess store.Session) (identity, string, error) {
	current, err := s.openRefreshToken(sess.ID, sess.RefreshToken)
	if err != nil {
		return identity{}, "", err
	}
	ctx, cancel := s.providerContext(ctx)
	defer cancel()
	tok, err := s.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: current}).Token()
	if err != nil {
		if oauthRejection(err) {
			return identity{}, "", errors.New(describe(err))
		}
		return identity{}, "", &unreachableError{errors.New(describe(err))}
	}
	rt := sess.RefreshToken // not rotated: keep the stored box
	if tok.RefreshToken != "" {
		rt = s.sealRefreshToken(sess.ID, tok.RefreshToken)
	}
	id, err := s.identify(ctx, tok, expect{subject: sess.Subject})
	if err != nil {
		return identity{}, rt, err
	}
	return id, rt, nil
}

// oauthRejection reports whether a token endpoint error is the provider
// saying no: a 400 or 401 with an OAuth error code. Anything else (a 429,
// a proxy's error page, a 5xx) counts as unreachable.
func oauthRejection(err error) bool {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.Response == nil || re.ErrorCode == "" {
		return false
	}
	code := re.Response.StatusCode
	return code == http.StatusBadRequest || code == http.StatusUnauthorized
}

// userinfoUnreachable reports a transport error, timeout or 5xx. go-oidc
// reports a non-200 status only as error text starting with the status.
func userinfoUnreachable(err error) bool {
	var ue *url.Error
	if errors.As(err, &ue) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	code, err := strconv.Atoi(strings.SplitN(err.Error(), " ", 2)[0])
	return err == nil && code >= 500 && code <= 599
}

// providerTextLimit bounds how much of a provider's answer an error repeats.
const providerTextLimit = 200

// providerError quotes and truncates a go-oidc error, whose text can carry
// a response body, so it can't forge or flood log lines.
type providerError struct {
	what string
	err  error
}

func (e *providerError) Error() string {
	return e.what + ": " + strconv.Quote(truncate(e.err.Error(), providerTextLimit))
}
func (e *providerError) Unwrap() error { return e.err }

// groupsFrom reads the named claim as a list of group names. A lone string
// is one group. found is false when the claim is absent or null.
func groupsFrom(claims map[string]json.RawMessage, name string) (groups []string, found bool, err error) {
	raw, ok := claims[name]
	if !ok || string(raw) == "null" {
		return nil, false, nil
	}
	if err := json.Unmarshal(raw, &groups); err == nil {
		return groups, true, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, true, nil
	}
	return nil, true, fmt.Errorf("the %q claim is not a list of group names", name)
}

func stringClaim(claims map[string]json.RawMessage, name string) string {
	var v string
	if raw, ok := claims[name]; ok {
		_ = json.Unmarshal(raw, &v) // a non-string claim reads as empty
	}
	return v
}

// displayName picks what battleship calls the user: name, else
// preferred_username, else email.
func displayName(claims map[string]json.RawMessage) string {
	for _, k := range []string{"name", "preferred_username", "email"} {
		if v := stringClaim(claims, k); v != "" {
			return v
		}
	}
	return ""
}

// describe renders a token endpoint error without its response body,
// which could echo the request.
func describe(err error) string {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		var ue *url.Error
		if errors.As(err, &ue) {
			return err.Error()
		}
		return strconv.Quote(truncate(err.Error(), 200))
	}
	s := "token endpoint refused"
	if re.Response != nil {
		s = fmt.Sprintf("token endpoint answered %d", re.Response.StatusCode)
	}
	if re.ErrorCode != "" {
		code := re.ErrorCode
		if !errorCode.MatchString(code) { // it comes from the body
			code = strconv.Quote(truncate(code, 64))
		}
		s += " " + code
	}
	if re.ErrorDescription != "" {
		s += ": " + strconv.Quote(truncate(re.ErrorDescription, 200))
	}
	return s
}
