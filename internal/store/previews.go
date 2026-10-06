package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrPreviewNotFound = errors.New("preview not found")
	ErrPreviewUsed     = errors.New("preview already submitted")
	ErrPreviewExpired  = errors.New("preview expired")
	ErrEmptyPreview    = errors.New("preview needs an ID, a session, a kind, inputs, a fingerprint and an expiry")
)

// Preview is a plan the web app showed a user, which one confirm from the
// same session may submit. The web app keeps the confirm form's nonce; the
// store only ever sees its hash.
type Preview struct {
	ID          string // SHA-256 hex of the nonce in the confirm form
	SessionID   string // the session it was shown to
	Kind        string
	Inputs      json.RawMessage // what was previewed, as jobs.Inputs
	Fingerprint string          // of the plan the user saw
	CreatedAt   time.Time
	ExpiresAt   time.Time
	JobID       int64 // the job that submitted it; 0 until then
}

// PreviewClaim names the preview a job submits. CreateJob marks it
// submitted in the same transaction that stores the job, so a preview
// yields at most one job.
type PreviewClaim struct {
	SessionID string
	ID        string
	// At is when the confirm was made: a preview expired by then is not
	// submitted (ErrPreviewExpired). Zero skips the check.
	At time.Time
}

// CreatePreview stores a preview. It also deletes the session's
// unsubmitted previews that expired by p.CreatedAt, so a long session
// doesn't collect them. Submitted ones stay, so a late repeat of their
// confirm (the back button) still finds its job; they go with the
// session.
func (s *Store) CreatePreview(ctx context.Context, p Preview) error {
	if p.ID == "" || p.SessionID == "" || p.Kind == "" || len(p.Inputs) == 0 || p.Fingerprint == "" || p.ExpiresAt.IsZero() {
		return ErrEmptyPreview
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM previews WHERE session_id = $1 AND expires_at <= $2 AND job_id IS NULL`,
		p.SessionID, p.CreatedAt); err != nil {
		return fmt.Errorf("clearing expired previews: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO previews (id, session_id, kind, inputs, fingerprint, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.ID, p.SessionID, p.Kind, p.Inputs, p.Fingerprint, p.CreatedAt, p.ExpiresAt); err != nil {
		return fmt.Errorf("storing preview: %w", err)
	}
	return tx.Commit(ctx)
}

// DeleteExpiredPreviews deletes every unsubmitted preview that expired by
// now and returns how many. battleship serve runs it hourly, for sessions that stay
// open without making another preview. A submitted preview stays while its
// session does, so a late repeat of its confirm still finds its job; the
// session's deletion cascades to it (its job is kept).
func (s *Store) DeleteExpiredPreviews(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM previews WHERE expires_at <= $1 AND job_id IS NULL`, now)
	if err != nil {
		return 0, fmt.Errorf("deleting expired previews: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Preview returns the session's preview with id, expired or not, or
// ErrPreviewNotFound. Another session's preview is not found.
func (s *Store) Preview(ctx context.Context, sessionID, id string) (Preview, error) {
	var p Preview
	var jobID *int64
	err := s.pool.QueryRow(ctx, `SELECT id, session_id, kind, inputs, fingerprint, created_at, expires_at, job_id
		FROM previews WHERE session_id = $1 AND id = $2`, sessionID, id).
		Scan(&p.ID, &p.SessionID, &p.Kind, &p.Inputs, &p.Fingerprint, &p.CreatedAt, &p.ExpiresAt, &jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preview{}, ErrPreviewNotFound
	}
	if err != nil {
		return Preview{}, err
	}
	if jobID != nil {
		p.JobID = *jobID
	}
	return p, nil
}

// claimPreview locks the claimed preview for tx and checks it hasn't been
// submitted, nor expired by c.At. A concurrent submit of the same preview
// waits here until tx ends, then finds it submitted.
func claimPreview(ctx context.Context, tx pgx.Tx, c PreviewClaim) error {
	var jobID *int64
	var expiresAt time.Time
	err := tx.QueryRow(ctx, `SELECT job_id, expires_at FROM previews WHERE session_id = $1 AND id = $2 FOR UPDATE`,
		c.SessionID, c.ID).Scan(&jobID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPreviewNotFound
	}
	if err != nil {
		return err
	}
	if jobID != nil {
		return fmt.Errorf("as job %d: %w", *jobID, ErrPreviewUsed)
	}
	if !c.At.IsZero() && !c.At.Before(expiresAt) {
		return ErrPreviewExpired
	}
	return nil
}
