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

// Preview is a plan shown to a user, submittable once from the same
// session. The store only sees the confirm nonce's hash.
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

// PreviewClaim names the preview a job submits; CreateJob marks it in the
// same transaction, so a preview yields at most one job.
type PreviewClaim struct {
	SessionID string
	ID        string
	// At is the confirm time for the expiry check; zero skips it.
	At time.Time
}

// CreatePreview stores a preview and prunes the session's expired
// unsubmitted ones. Submitted previews stay with the session so a repeated
// confirm (back button) still finds its job.
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

// DeleteExpiredPreviews deletes unsubmitted previews expired by now and
// returns the count, for sessions that never make another preview.
// Submitted ones go when their session does.
func (s *Store) DeleteExpiredPreviews(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM previews WHERE expires_at <= $1 AND job_id IS NULL`, now)
	if err != nil {
		return 0, fmt.Errorf("deleting expired previews: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Preview returns the session's preview, even if expired, or
// ErrPreviewNotFound (also for another session's).
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

// claimPreview locks the preview for tx and checks it is unsubmitted and
// unexpired; a concurrent submit waits here, then finds it submitted.
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
