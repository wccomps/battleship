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

// ErrNoCredential is returned by Submit without the submitter's Proxmox
// credential: a job acts only as the person who submitted it.
var ErrNoCredential = errors.New("a job needs its submitter's Proxmox credential")

// lapsedMessage is a job's error when its credential lapsed before it ran.
const lapsedMessage = "authorization lapsed; preview it again"

// Credentials seals the credential a job runs with into the job's row
// and opens it again, bound to the job's ID (see package seal), and knows
// when a ticket is due for renewal and when it has lapsed.
type Credentials struct {
	key        seal.Key
	renewAfter time.Duration
	maxAge     time.Duration
}

// credentialPurpose is part of every stored job credential's key: changing
// its text makes those unreadable.
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

// Seal returns what store.NewJob.Credential wants: cred sealed for the job
// ID it is given.
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

// errSealKey means a credential didn't open: this process's
// database.seal_key isn't the one it was sealed with. It says nothing about
// whether the credential has lapsed.
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

// RowLapsed is proxmox.Credential.Lapsed decided from a row's own columns,
// without opening it, so a process with the wrong seal key can't take a
// credential for lapsed.
func (c Credentials) RowLapsed(jc store.JobCredential, now time.Time) bool {
	view := proxmox.Credential{Issued: jc.IssuedAt, LoginAt: jc.LoginAt} // a ticket: no token secret
	return jc.Kind == store.CredentialTicket && view.Lapsed(now, c.maxAge)
}

// OpenCredentials is NewCredentials checked against st's key (CheckKey):
// what every process that submits or runs jobs uses.
func OpenCredentials(ctx context.Context, cfg config.Config, st *store.Store) (Credentials, error) {
	c, err := NewCredentials(cfg)
	if err != nil {
		return Credentials{}, err
	}
	return c, c.CheckKey(ctx, st)
}

// keyCheckText is what the seal key check seals; a database stores it
// sealed once, so its text can't change.
const keyCheckText = "rangekiln seal key check v1"

// CheckKey checks this process's seal key against the one the database
// was first used with (storing it the first time), so a process with
// another key fails at startup instead of failing every job.
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

// Renewer keeps the tickets of waiting and running jobs alive. Each
// replica runs one; replicas claim each ticket before renewing it, so
// they don't renew one twice. A ticket that can't be renewed (it expired,
// its login is past proxmox.ticket_max_age, or Proxmox refuses it) is
// dropped, and its job then stops as "authorization lapsed" (see Worker).
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

// RenewDue renews every ticket due now, once, and says how many it renewed
// and how many lapsed.
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
	// A few at once, so a backlog of due tickets is renewed well within
	// the hour they have left; each claim takes a pooled connection.
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
