package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrEmptySession    = errors.New("session needs an ID, a subject, a refresh token, a CSRF token and an expiry")
)

// Session is a logged-in browser. The store keeps what it is given; the
// caller decides expiry and computes times from its own clock.
type Session struct {
	ID      string // SHA-256 hex of the cookie token, never the token itself
	Subject string // the identity provider's user ID
	// Username is preferred_username; the Proxmox user is <username>@<realm>.
	Username     string
	Name         string
	Email        string
	Groups       []string // identity-provider groups as of the last login or refresh
	RefreshToken string   // opaque to the store; auth seals it (see auth's refreshtoken.go)
	CSRF         string   // the token every state-changing request must carry
	CreatedAt    time.Time
	LastSeen     time.Time
	RefreshedAt  time.Time // last successful group check
	ExpiresAt    time.Time // the earliest of the idle and absolute expiry
	// RefreshRetryAt, if set, is when a new refresh may start (one is running
	// or backing off until then).
	RefreshRetryAt time.Time
	// RefreshFailedAt is the last failure to reach the identity provider
	// since the last success; zero if none.
	RefreshFailedAt time.Time
	// PVE is the user's Proxmox ticket; zero until the Proxmox login.
	PVE SessionTicket
	// An in-progress Proxmox login: SHA-256 hex of its state, the redirect
	// after it, and the endpoint that started it, which alone can finish it
	// (it keeps the login state on its own disk).
	PVELoginState    string
	PVELoginNext     string
	PVELoginEndpoint string
}

// SessionTicket is a session's Proxmox ticket. Ticket and CSRF are sealed by
// auth.
type SessionTicket struct {
	User     string
	Ticket   string
	CSRF     string
	IssuedAt time.Time
	// LoginAt is the original login time; renewals keep it.
	LoginAt time.Time
}

// ErrTicketChanged means the ticket was renewed or cleared since the caller
// read it.
var ErrTicketChanged = errors.New("the session's Proxmox ticket changed")

// SessionUpdate is what a refresh learns from the identity provider.
type SessionUpdate struct {
	Username     string
	Name         string
	Email        string
	Groups       []string
	RefreshToken string // the provider may rotate it
	RefreshedAt  time.Time
}

const sessionColumns = `id, subject, name, email, groups, refresh_token, csrf,
	created_at, last_seen, refreshed_at, expires_at, refresh_retry_at, refresh_failed_at, username`

const sessionPVEColumns = `pve_user, pve_ticket_sealed, pve_csrf_sealed, pve_issued_at, pve_login_at,
	pve_login_state, pve_login_next, pve_login_endpoint`

// CreateSession stores a new session. It fails if the ID is taken.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	if sess.ID == "" || sess.Subject == "" || sess.RefreshToken == "" || sess.CSRF == "" || sess.ExpiresAt.IsZero() {
		return ErrEmptySession
	}
	groups := sess.Groups
	if groups == nil {
		groups = []string{}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (`+sessionColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		sess.ID, sess.Subject, sess.Name, sess.Email, groups, sess.RefreshToken, sess.CSRF,
		sess.CreatedAt, sess.LastSeen, sess.RefreshedAt, sess.ExpiresAt,
		nullTime(sess.RefreshRetryAt), nullTime(sess.RefreshFailedAt), sess.Username)
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

// Session returns the session by ID (token hash), even if expired, or
// ErrSessionNotFound.
func (s *Store) Session(ctx context.Context, id string) (Session, error) {
	var x Session
	var retry, failed, issued, loginAt *time.Time
	err := s.pool.QueryRow(ctx, `SELECT `+sessionColumns+`, `+sessionPVEColumns+` FROM sessions WHERE id = $1`, id).Scan(
		&x.ID, &x.Subject, &x.Name, &x.Email, &x.Groups, &x.RefreshToken, &x.CSRF,
		&x.CreatedAt, &x.LastSeen, &x.RefreshedAt, &x.ExpiresAt, &retry, &failed, &x.Username,
		&x.PVE.User, &x.PVE.Ticket, &x.PVE.CSRF, &issued, &loginAt, &x.PVELoginState, &x.PVELoginNext, &x.PVELoginEndpoint)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("reading session: %w", err)
	}
	if retry != nil {
		x.RefreshRetryAt = *retry
	}
	if failed != nil {
		x.RefreshFailedAt = *failed
	}
	if issued != nil {
		x.PVE.IssuedAt = *issued
	}
	if loginAt != nil {
		x.PVE.LoginAt = *loginAt
	}
	return x, nil
}

// SetSessionTicket stores the Proxmox ticket and ends any login in progress.
// It replaces only a ticket issued at prev (zero: any), so of two concurrent
// renewals one gets ErrTicketChanged.
func (s *Store) SetSessionTicket(ctx context.Context, id string, prev time.Time, t SessionTicket) error {
	if t.User == "" || t.Ticket == "" || t.CSRF == "" || t.IssuedAt.IsZero() || t.LoginAt.IsZero() {
		return ErrEmptySession
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET pve_user = $2, pve_ticket_sealed = $3, pve_csrf_sealed = $4,
			pve_issued_at = $5, pve_login_at = $7, pve_login_state = '', pve_login_next = '', pve_login_endpoint = ''
		WHERE id = $1 AND ($6::timestamptz IS NULL OR pve_issued_at = $6)`,
		id, t.User, t.Ticket, t.CSRF, t.IssuedAt, nullTime(prev), t.LoginAt)
	if err != nil {
		return fmt.Errorf("storing the Proxmox ticket: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Session(ctx, id); err != nil {
			return err
		}
		return ErrTicketChanged
	}
	return nil
}

// ClearSessionTicket drops the Proxmox ticket, forcing a new login. A
// non-zero prev drops only a ticket issued at prev, so a stale refusal can't
// wipe a newer ticket.
func (s *Store) ClearSessionTicket(ctx context.Context, id string, prev time.Time) error {
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET pve_ticket_sealed = '', pve_csrf_sealed = '', pve_issued_at = NULL,
		pve_login_at = NULL WHERE id = $1 AND ($2::timestamptz IS NULL OR pve_issued_at = $2)`, id, nullTime(prev)); err != nil {
		return fmt.Errorf("clearing the Proxmox ticket: %w", err)
	}
	return nil
}

// SetSessionPVELogin records a Proxmox login in progress.
func (s *Store) SetSessionPVELogin(ctx context.Context, id, stateHash, next, endpoint string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET pve_login_state = $2, pve_login_next = $3, pve_login_endpoint = $4
		WHERE id = $1`, id, stateHash, next, endpoint)
	if err != nil {
		return fmt.Errorf("starting the Proxmox login: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// nullTime stores a zero time as NULL.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// TouchSession records activity: the time it was seen and the new expiry.
func (s *Store) TouchSession(ctx context.Context, id string, lastSeen, expiresAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen = $2, expires_at = $3 WHERE id = $1`,
		id, lastSeen, expiresAt)
	if err != nil {
		return fmt.Errorf("touching session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// ClaimSessionRefresh claims the session's next refresh until until. It wins
// only if refreshed_at still equals prev and no claim or backoff is in force,
// so only one replica spends a rotating refresh token. A dead refresher's
// claim lapses at until.
func (s *Store) ClaimSessionRefresh(ctx context.Context, id string, prev, now, until time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET refresh_retry_at = $4
		WHERE id = $1 AND refreshed_at = $2 AND (refresh_retry_at IS NULL OR refresh_retry_at <= $3)`,
		id, prev, now, until)
	if err != nil {
		return false, fmt.Errorf("claiming session refresh: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordSessionRefreshFailure records a failed refresh and when to retry. A
// non-empty refreshToken replaces the stored one: the provider may have
// rotated it before a later step failed.
func (s *Store) RecordSessionRefreshFailure(ctx context.Context, id, refreshToken string, failedAt, retryAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET refresh_failed_at = $2, refresh_retry_at = $3,
		refresh_token = coalesce(nullif($4, ''), refresh_token) WHERE id = $1`,
		id, failedAt, retryAt, refreshToken)
	if err != nil {
		return fmt.Errorf("recording refresh failure: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// UpdateSessionGroups stores what a successful refresh learned, and clears
// any refresh claim, backoff and failure.
func (s *Store) UpdateSessionGroups(ctx context.Context, id string, u SessionUpdate) error {
	if u.RefreshToken == "" {
		return ErrEmptySession
	}
	groups := u.Groups
	if groups == nil {
		groups = []string{}
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sessions
		SET name = $2, email = $3, groups = $4, refresh_token = $5, refreshed_at = $6,
			refresh_retry_at = NULL, refresh_failed_at = NULL, username = $7
		WHERE id = $1`, id, u.Name, u.Email, groups, u.RefreshToken, u.RefreshedAt, u.Username)
	if err != nil {
		return fmt.Errorf("updating session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// DeleteSession removes a session; a missing one is not an error.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions expired at now and returns the count.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("deleting expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
