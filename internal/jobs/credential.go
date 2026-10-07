package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/seal"
	"github.com/wccomps/battleship/internal/store"
)

// ErrNoCredential: Submit needs the submitter's credential, since a job acts
// only as its submitter.
var ErrNoCredential = errors.New("a job needs its submitter's Proxmox credential")

// lapsedMessage is a job's error when its credential lapsed before it ran.
const lapsedMessage = "authorization lapsed; preview it again"

// Credentials seals and opens job credentials, bound to the job's ID (see
// package seal), and judges renewal and lapse.
type Credentials struct {
	key        seal.Key
	renewAfter time.Duration
	maxAge     time.Duration
}

// credentialPurpose is part of every job credential's key; changing it makes
// stored credentials unreadable.
const credentialPurpose = "rangekiln job credential v1"

// NewCredentials needs database.seal_key, the same in every process that
// submits or runs jobs.
func NewCredentials(cfg config.Config) (Credentials, error) {
	if err := cfg.RequireSealKey(); err != nil {
		return Credentials{}, err
	}
	return Credentials{
		key:        seal.NewKey(cfg.Database.SealKey, credentialPurpose),
		renewAfter: cfg.Proxmox.TicketRenewAfter,
		maxAge:     cfg.Proxmox.TicketMaxAge,
	}, nil
}

// sealed is what a job credential's box holds.
type sealed struct {
	Ticket      string    `json:"t,omitempty"`
	CSRF        string    `json:"c,omitempty"`
	TokenSecret string    `json:"s,omitempty"`
	Issued      time.Time `json:"i"`
	LoginAt     time.Time `json:"l"`
}

func binding(jobID int64) string { return "job:" + strconv.FormatInt(jobID, 10) }

// Seal returns a store.NewJob.Credential that seals cred for the job ID.
func (c Credentials) Seal(cred proxmox.Credential) func(jobID int64) (store.JobCredential, error) {
	return func(jobID int64) (store.JobCredential, error) {
		return c.row(jobID, cred, time.Now())
	}
}

func (c Credentials) row(jobID int64, cred proxmox.Credential, now time.Time) (store.JobCredential, error) {
	if !cred.Usable() {
		return store.JobCredential{}, ErrNoCredential
	}
	plain, err := json.Marshal(sealed{Ticket: cred.Ticket, CSRF: cred.CSRF, TokenSecret: cred.TokenSecret, Issued: cred.Issued, LoginAt: cred.LoginAt})
	if err != nil {
		return store.JobCredential{}, err
	}
	jc := store.JobCredential{JobID: jobID, Kind: store.CredentialTicket, User: cred.User,
		Sealed: c.key.Seal(binding(jobID), plain), IssuedAt: cred.Issued, LoginAt: cred.LoginAt}
	if cred.IsToken() {
		jc.Kind, jc.IssuedAt, jc.LoginAt = store.CredentialToken, now, now
	} else {
		if jc.LoginAt.IsZero() {
			jc.LoginAt = jc.IssuedAt
		}
		jc.RenewAfter = cred.Issued.Add(c.renewAfter)
	}
	return jc, nil
}

// errSealKey means this process's seal key differs from the sealing one; it
// says nothing about lapse.
var errSealKey = errors.New("database.seal_key differs from the key it was sealed with; give every process the same key")

// Open reverses Seal for the job the row belongs to.
func (c Credentials) Open(jc store.JobCredential) (proxmox.Credential, error) {
	plain, err := c.key.Open(binding(jc.JobID), jc.Sealed)
	if err != nil {
		return proxmox.Credential{}, fmt.Errorf("can't open job %d's credential: %w", jc.JobID, errSealKey)
	}
	var s sealed
	if err := json.Unmarshal(plain, &s); err != nil {
		return proxmox.Credential{}, fmt.Errorf("job %d's credential: %w", jc.JobID, err)
	}
	if jc.Kind == store.CredentialToken {
		return proxmox.TokenCredential(jc.User, s.TokenSecret), nil
	}
	cred := proxmox.TicketCredential(jc.User, s.Ticket, s.CSRF, s.Issued)
	cred.LoginAt = s.LoginAt
	return cred, nil
}

// RowLapsed is proxmox.Credential.Lapsed from the row's columns, without
// opening it, so a wrong seal key can't make a credential look lapsed.
func (c Credentials) RowLapsed(jc store.JobCredential, now time.Time) bool {
	view := proxmox.Credential{Issued: jc.IssuedAt, LoginAt: jc.LoginAt} // a ticket: no token secret
	return jc.Kind == store.CredentialTicket && view.Lapsed(now, c.maxAge)
}

// OpenCredentials is NewCredentials plus CheckKey.
func OpenCredentials(ctx context.Context, cfg config.Config, st *store.Store) (Credentials, error) {
	c, err := NewCredentials(cfg)
	if err != nil {
		return Credentials{}, err
	}
	return c, c.CheckKey(ctx, st)
}

// keyCheckText is sealed once per database, so it can't change.
const keyCheckText = "rangekiln seal key check v1"

// CheckKey checks this process's seal key against the database's first one,
// so a wrong key fails at startup instead of failing every job.
func (c Credentials) CheckKey(ctx context.Context, st *store.Store) error {
	stored, err := st.SealCheck(ctx, c.key.Seal("check", []byte(keyCheckText)))
	if err != nil {
		return err
	}
	if plain, err := c.key.Open("check", stored); err != nil || string(plain) != keyCheckText {
		return errors.New("database.seal_key (BATTLESHIP_SEAL_KEY) isn't the key the database's jobs are sealed with; give every process the same key")
	}
	return nil
}

// TicketRenewer renews a ticket; *proxmox.Client is one.
type TicketRenewer interface {
	RenewTicket(ctx context.Context, cred proxmox.Credential, now time.Time) (proxmox.Credential, error)
}

// Renewer keeps active jobs' tickets alive. Each replica runs one and claims
// a ticket before renewing it. A ticket that can't be renewed is dropped and
// its job stops as "authorization lapsed".
type Renewer struct {
	Store       *store.Store
	Proxmox     TicketRenewer
	Credentials Credentials
	Logf        func(format string, args ...any)
	Now         func() time.Time
}

const (
	// renewParallel is how many tickets a pass renews at once.
	renewParallel = 8
	renewEvery    = time.Minute
	renewTimeout  = 30 * time.Second
	renewBatch    = 200
)

// Run renews due tickets until ctx is done.
func (r *Renewer) Run(ctx context.Context) {
	for {
		r.RenewDue(ctx)
		if proxmox.SleepContext(ctx, renewEvery) != nil {
			return
		}
	}
}

// RenewDue renews every ticket due now, once.
func (r *Renewer) RenewDue(ctx context.Context) (renewed, lapsed int) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	logf := r.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ids, err := r.Store.DueCredentials(ctx, now(), renewBatch)
	if err != nil {
		if ctx.Err() == nil {
			logf("listing tickets to renew: %v", err)
		}
		return 0, 0
	}
	// Bounded: a backlog must finish within the tickets' remaining hour, but
	// each claim takes a pooled connection.
	var g errgroup.Group
	g.SetLimit(renewParallel)
	var mu sync.Mutex
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			var what store.Renewal
			claimed, err := r.Store.RenewCredential(ctx, id, now(), func(cur store.JobCredential) (next store.JobCredential, _ store.Renewal, err error) {
				next, what, err = r.renew(ctx, cur, now())
				return next, what, err
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				logf("job %d: renewing its Proxmox ticket: %v", id, err)
			case !claimed: // another replica has it, or it is no longer due
			case what == store.Renewed:
				renewed++
			case what == store.RenewalLapsed:
				lapsed++
				logf("job %d: its submitter's Proxmox authorization lapsed; the job will stop", id)
			}
			return nil
		})
	}
	_ = g.Wait() // the goroutines report through logf
	return renewed, lapsed
}

func (r *Renewer) renew(ctx context.Context, cur store.JobCredential, now time.Time) (store.JobCredential, store.Renewal, error) {
	if r.Credentials.RowLapsed(cur, now) {
		return cur, store.RenewalLapsed, nil
	}
	cred, err := r.Credentials.Open(cur)
	if err != nil {
		return cur, store.RenewalFailed, err // never dropped for this: another process may hold the right key
	}
	ctx, cancel := context.WithTimeout(ctx, renewTimeout)
	defer cancel()
	next, err := r.Proxmox.RenewTicket(ctx, cred, now)
	switch {
	case proxmox.RenewalRefused(err):
		return cur, store.RenewalLapsed, nil
	case err != nil:
		return cur, store.RenewalFailed, err
	}
	row, err := r.Credentials.row(cur.JobID, next, now)
	if err != nil {
		return cur, store.RenewalFailed, err
	}
	return row, store.Renewed, nil
}
