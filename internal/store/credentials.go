package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Kinds of job credential.
const (
	CredentialTicket = "ticket"
	CredentialToken  = "token"
)

// ErrNoCredential means the job ended or the renewer dropped its ticket.
var ErrNoCredential = errors.New("job has no credential")

// JobCredential is a job's Proxmox credential, sealed by the caller.
type JobCredential struct {
	JobID    int64
	Kind     string // CredentialTicket or CredentialToken
	User     string // the Proxmox user, or the token ID
	Sealed   string
	IssuedAt time.Time
	// LoginAt is the submitter's login time (renewals keep it); for a token,
	// when it was stored.
	LoginAt time.Time
	// RenewAfter is when a ticket is due for renewal; zero for a token.
	RenewAfter time.Time
}

func (c JobCredential) valid() bool {
	return (c.Kind == CredentialTicket && !c.RenewAfter.IsZero() || c.Kind == CredentialToken) &&
		c.User != "" && c.Sealed != "" && !c.IssuedAt.IsZero() && !c.LoginAt.IsZero()
}

func insertCredential(ctx context.Context, tx pgx.Tx, c JobCredential) error {
	if !c.valid() {
		return fmt.Errorf("job %d: incomplete credential", c.JobID)
	}
	_, err := tx.Exec(ctx, `INSERT INTO job_credentials (job_id, kind, pve_user, sealed, issued_at, login_at, renew_after)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, c.JobID, c.Kind, c.User, c.Sealed, c.IssuedAt, c.LoginAt, nullTime(c.RenewAfter))
	return err
}

const credentialColumns = `job_id, kind, pve_user, sealed, issued_at, login_at, renew_after`

func scanCredential(row pgx.Row) (JobCredential, error) {
	var c JobCredential
	var renew *time.Time
	err := row.Scan(&c.JobID, &c.Kind, &c.User, &c.Sealed, &c.IssuedAt, &c.LoginAt, &renew)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobCredential{}, ErrNoCredential
	}
	if renew != nil {
		c.RenewAfter = *renew
	}
	return c, err
}

// JobCredential returns the job's credential, or ErrNoCredential.
func (s *Store) JobCredential(ctx context.Context, jobID int64) (JobCredential, error) {
	return scanCredential(s.pool.QueryRow(ctx, `SELECT `+credentialColumns+` FROM job_credentials WHERE job_id = $1`, jobID))
}

// DueCredentials lists, oldest first, up to limit jobs whose tickets are
// due for renewal at now.
func (s *Store) DueCredentials(ctx context.Context, now time.Time, limit int) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT job_id FROM job_credentials
		WHERE renew_after IS NOT NULL AND renew_after <= $1 ORDER BY renew_after, job_id LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Renewal is what a renewal decided.
type Renewal int

const (
	// Renewed: store the credential the renewal returned.
	Renewed Renewal = iota
	// RenewalLapsed: the ticket expired or was refused; drop it.
	RenewalLapsed
	// RenewalFailed: Proxmox unreachable; keep it and retry later.
	RenewalFailed
)

// renewLockID and the job ID (folded to int) key the renewal advisory lock.
const renewLockID = 727073

// RenewCredential calls renew on the job's row if it is due, under a session
// advisory lock so replicas don't renew one ticket twice. It's a session
// lock, not a transaction, because renew calls Proxmox. claimed is false if
// the row is gone, not due, or held elsewhere.
func (s *Store) RenewCredential(ctx context.Context, jobID int64, now time.Time,
	renew func(cur JobCredential) (JobCredential, Renewal, error)) (claimed bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	key := int32(jobID % (1 << 31))
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, int32(renewLockID), key).Scan(&got); err != nil {
		return false, err
	}
	if !got {
		return false, nil
	}
	defer func() {
		if _, uerr := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1, $2)`, int32(renewLockID), key); uerr != nil {
			conn.Conn().Close(context.WithoutCancel(ctx)) //nolint:errcheck // drops the lock with the session
		}
	}()
	cur, err := scanCredential(conn.QueryRow(ctx, `SELECT `+credentialColumns+` FROM job_credentials
		WHERE job_id = $1 AND renew_after IS NOT NULL AND renew_after <= $2`, jobID, now))
	if errors.Is(err, ErrNoCredential) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	next, what, rerr := renew(cur)
	switch what {
	case Renewed:
		if rerr != nil {
			return true, rerr
		}
		next.JobID, next.LoginAt = jobID, cur.LoginAt
		if !next.valid() {
			return true, fmt.Errorf("job %d: renewal returned an incomplete credential", jobID)
		}
		_, err = conn.Exec(ctx, `UPDATE job_credentials SET sealed = $2, issued_at = $3, renew_after = $4
			WHERE job_id = $1 AND issued_at = $5`, jobID, next.Sealed, next.IssuedAt, nullTime(next.RenewAfter), cur.IssuedAt)
	case RenewalLapsed:
		_, err = conn.Exec(ctx, `DELETE FROM job_credentials WHERE job_id = $1 AND issued_at = $2`, jobID, cur.IssuedAt)
	}
	return true, errors.Join(rerr, err)
}

// SealCheck stores sealed if no key check exists yet and returns the stored
// one (possibly another process's).
func (s *Store) SealCheck(ctx context.Context, sealed string) (string, error) {
	if _, err := s.pool.Exec(ctx, `INSERT INTO seal_check (id, sealed) VALUES (1, $1) ON CONFLICT (id) DO NOTHING`, sealed); err != nil {
		return "", fmt.Errorf("storing the seal key check: %w", err)
	}
	var stored string
	if err := s.pool.QueryRow(ctx, `SELECT sealed FROM seal_check WHERE id = 1`).Scan(&stored); err != nil {
		return "", fmt.Errorf("reading the seal key check: %w", err)
	}
	return stored, nil
}
