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
	RefreshToken string // opaque to the store; auth seals it (see auth's refreshtoken.go)
	IDToken      string // the latest ID token, sealed by auth; may be empty
	CSRF         string // the token every state-changing request must carry
	CreatedAt    time.Time
	LastSeen     time.Time
	RefreshedAt  time.Time // last successful check with the identity provider
	ExpiresAt    time.Time // the earliest of the idle and absolute expiry
	// RefreshRetryAt, if set, is when a new refresh may start (one is running
	// or backing off until then).
	RefreshRetryAt time.Time
	// RefreshFailedAt is the last failure to reach the identity provider
	// since the last success; zero if none.
	RefreshFailedAt time.Time
	// An in-progress Proxmox login: SHA-256 hex of its state, the redirect
	// after it, and the endpoint that started it, which alone can finish it
	// (it keeps the login state on its own disk).
	PVELoginState    string
	PVELoginNext     string
	PVELoginEndpoint string
}

// SessionUpdate is what a refresh learns from the identity provider.
type SessionUpdate struct {
	Username     string
	Name         string
	Email        string
	RefreshToken string // the provider may rotate it
	IDToken      string // sealed
	RefreshedAt  time.Time
}

const sessionColumns = `id, subject, name, email, refresh_token, csrf,
	created_at, last_seen, refreshed_at, expires_at, refresh_retry_at, refresh_failed_at, username, id_token_sealed`

const sessionPVEColumns = `pve_login_state, pve_login_next, pve_login_endpoint`

// CreateSession stores a new session. It fails if the ID is taken.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	if sess.ID == "" || sess.Subject == "" || sess.RefreshToken == "" || sess.CSRF == "" || sess.ExpiresAt.IsZero() {
		return ErrEmptySession
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (`+sessionColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		sess.ID, sess.Subject, sess.Name, sess.Email, sess.RefreshToken, sess.CSRF,
		sess.CreatedAt, sess.LastSeen, sess.RefreshedAt, sess.ExpiresAt,
		nullTime(sess.RefreshRetryAt), nullTime(sess.RefreshFailedAt), sess.Username, sess.IDToken)
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

// Session returns the session by ID (token hash), even if expired, or
// ErrSessionNotFound.
func (s *Store) Session(ctx context.Context, id string) (Session, error) {
	var x Session
	var retry, failed *time.Time
	err := s.pool.QueryRow(ctx, `SELECT `+sessionColumns+`, `+sessionPVEColumns+` FROM sessions WHERE id = $1`, id).Scan(
		&x.ID, &x.Subject, &x.Name, &x.Email, &x.RefreshToken, &x.CSRF,
		&x.CreatedAt, &x.LastSeen, &x.RefreshedAt, &x.ExpiresAt, &retry, &failed, &x.Username, &x.IDToken,
		&x.PVELoginState, &x.PVELoginNext, &x.PVELoginEndpoint)
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
	return x, nil
}

// EndProxmoxLogin clears the session's Proxmox login in progress.
func (s *Store) EndProxmoxLogin(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET pve_login_state = '', pve_login_next = '', pve_login_endpoint = ''
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("ending the Proxmox login: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
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

// UpdateSession stores what a successful refresh learned, and clears any
// refresh claim, backoff and failure.
func (s *Store) UpdateSession(ctx context.Context, id string, u SessionUpdate) error {
	if u.RefreshToken == "" {
		return ErrEmptySession
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sessions
		SET name = $2, email = $3, refresh_token = $4, refreshed_at = $5,
			refresh_retry_at = NULL, refresh_failed_at = NULL, username = $6,
			id_token_sealed = $7
		WHERE id = $1`, id, u.Name, u.Email, u.RefreshToken, u.RefreshedAt, u.Username, u.IDToken)
	if err != nil {
		return fmt.Errorf("updating session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// EndedSession is a session as it was when removed: its ID and its
// ID token, still sealed.
type EndedSession struct {
	ID, IDToken string
}

// EndSession removes a session and returns it as it was then (a refresh may
// have just rotated its tokens), or ok false if it was already gone. With
// revoke, it queues the refresh token for revocation in the same statement.
func (s *Store) EndSession(ctx context.Context, id string, revoke bool) (_ EndedSession, ok bool, err error) {
	rows, err := s.pool.Query(ctx, `WITH ended AS (DELETE FROM sessions WHERE id = $1 RETURNING id, refresh_token, id_token_sealed),
		queued AS (INSERT INTO revocations (session_id, refresh_token) SELECT id, refresh_token FROM ended WHERE $2)
		SELECT id, id_token_sealed FROM ended`, id, revoke)
	if err != nil {
		return EndedSession{}, false, fmt.Errorf("ending session: %w", err)
	}
	ended, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[EndedSession])
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return EndedSession{}, false, nil
	case err != nil:
		return EndedSession{}, false, fmt.Errorf("ending session: %w", err)
	}
	return ended, true, nil
}

// EndExpiredSessions removes up to limit sessions expired at now, oldest
// first, as EndSession does, and returns how many it removed. Sessions
// locked by another statement (another sweep, or a refresh updating one)
// are left for a later sweep.
func (s *Store) EndExpiredSessions(ctx context.Context, now time.Time, limit int, revoke bool) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `WITH ended AS (DELETE FROM sessions WHERE id IN (
			SELECT id FROM sessions WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		RETURNING id, refresh_token),
		queued AS (INSERT INTO revocations (session_id, refresh_token) SELECT id, refresh_token FROM ended WHERE $3)
		SELECT count(*) FROM ended`, now, limit, revoke).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("ending expired sessions: %w", err)
	}
	return n, nil
}

// Revocation is a leased, queued refresh token (see EndSession).
type Revocation struct {
	ID           int64
	SessionID    string
	RefreshToken string    // sealed for SessionID
	Attempts     int       // tries before this one
	Lease        time.Time // when the lease runs out; it identifies the lease
}

// QueueRevocation queues a sealed refresh token for revocation.
func (s *Store) QueueRevocation(ctx context.Context, sessionID, refreshToken string) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO revocations (session_id, refresh_token) VALUES ($1, $2)`, sessionID, refreshToken); err != nil {
		return fmt.Errorf("queueing a revocation: %w", err)
	}
	return nil
}

// LeaseRevocations takes up to limit queued revocations that are due, for
// lease: another caller doesn't get them until it runs out. Each counts as
// an attempt. DoneRevocation, RetryRevocation and ReleaseRevocation act on
// a revocation only while its lease holds.
func (s *Store) LeaseRevocations(ctx context.Context, limit int, lease time.Duration) ([]Revocation, error) {
	rows, err := s.pool.Query(ctx, `UPDATE revocations SET attempts = attempts + 1, next_at = now() + make_interval(secs => $2)
		WHERE id IN (SELECT id FROM revocations WHERE next_at <= now() ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING id, session_id, refresh_token, attempts - 1, next_at`, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("leasing revocations: %w", err)
	}
	revs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Revocation])
	if err != nil {
		return nil, fmt.Errorf("leasing revocations: %w", err)
	}
	return revs, nil
}

// DoneRevocation removes a leased revocation that was sent, or given up on.
func (s *Store) DoneRevocation(ctx context.Context, rv Revocation) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM revocations WHERE id = $1 AND next_at = $2`, rv.ID, rv.Lease); err != nil {
		return fmt.Errorf("removing a revocation: %w", err)
	}
	return nil
}

// RetryRevocation makes a leased revocation due again after wait.
func (s *Store) RetryRevocation(ctx context.Context, rv Revocation, wait time.Duration) error {
	if _, err := s.pool.Exec(ctx, `UPDATE revocations SET next_at = now() + make_interval(secs => $3) WHERE id = $1 AND next_at = $2`,
		rv.ID, rv.Lease, wait.Seconds()); err != nil {
		return fmt.Errorf("rescheduling a revocation: %w", err)
	}
	return nil
}

// ReleaseRevocation makes a leased revocation due now, without counting
// the attempt: one that wasn't really tried.
func (s *Store) ReleaseRevocation(ctx context.Context, rv Revocation) error {
	if _, err := s.pool.Exec(ctx, `UPDATE revocations SET next_at = now(), attempts = attempts - 1 WHERE id = $1 AND next_at = $2`,
		rv.ID, rv.Lease); err != nil {
		return fmt.Errorf("releasing a revocation: %w", err)
	}
	return nil
}
