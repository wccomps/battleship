// Package web is the volunteer web app that battleship serve runs: the live status
// grid, VM pages, the operation forms with their preview and confirm steps,
// the job pages, and health checks, all server-rendered with html/template.
// Pages work without JavaScript; one small script, served from /static,
// applies live updates that arrive as server-sent events, and lets the grid
// select VMs and show previews in a side panel.
//
// Routes are registered in groups:
//
//   - authRoutes: login, logout and the Proxmox sign-in;
//   - healthRoutes: liveness and readiness probes;
//   - assetRoutes: the embedded stylesheet, script, fonts and icon;
//   - volunteerRoutes: the grid, operation forms and job pages.
package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
)

// Deps are what the web app runs on. Everything but Clock, Logf and
// AccessLog is required.
type Deps struct {
	Config config.Config
	Store  *store.Store
	// As is the Proxmox API acting as the person whose credential it is
	// given: previews, confirms and forms read the cluster as the
	// signed-in user (auth.ProxmoxCredential), never as battleship.
	As func(cred proxmox.Credential) pods.API
	// Credentials seals the credential a confirmed job carries.
	Credentials jobs.Credentials
	// Auth logs users in and guards routes. New has it render its pages
	// in this app's layout (AuthPage).
	Auth *auth.Service
	// Views keep each viewer's grid, read with their own ticket.
	Views *status.Views
	// Hub carries change notifications to the server-sent event streams.
	Hub *status.Hub
	// Clock times readiness, heartbeats and stream lifetimes. Default
	// status.SystemClock.
	Clock status.Clock
	// Logf receives server problems. Default log.Printf.
	Logf func(format string, args ...any)
	// AccessLog, if set, gets a "request" record per request but health
	// probes, without the query string (the login callback's code) or
	// cookies. Without it, Logf gets a line per request other than GET and
	// HEAD, and per server error.
	AccessLog *slog.Logger
}

// Server is the web app. Handler serves it.
type Server struct {
	cfg    config.Config
	st     *store.Store
	as     func(proxmox.Credential) pods.API
	creds  jobs.Credentials
	auth   *auth.Service
	views  *status.Views
	hub    *status.Hub
	clock  status.Clock
	logf   func(format string, args ...any)
	access *slog.Logger // nil: request lines go to logf
	// secureCookies: web.base_url is https, so the flash cookie is Secure.
	secureCookies bool
	naming        pods.Naming
	proxies       []netip.Prefix // web.trusted_proxies
	csp           string

	closeOnce sync.Once
	closed    chan struct{} // closed by CloseStreams

	// life is the server's lifetime, cancelled by CloseStreams at
	// shutdown. Work that outlives the request that started it (the
	// shared cell reads) runs on it.
	life    context.Context
	endLife context.CancelFunc

	draining atomic.Bool // set by Drain: readiness answers 503

	readyMu sync.Mutex
	ready   readiness // last readiness answer, for logging changes only

	// Cell pages' live reads: at most cellReadSlots at once, and one per
	// cell at a time, shared by everyone asking for that cell meanwhile.
	cellSlots  chan struct{}
	cellFlight singleflight.Group
	// cellTimeout bounds each shared cell read: cellReadTimeout, which
	// tests shorten.
	cellTimeout time.Duration
	// afterCellJoin, if set, is called once a request has joined (or
	// started) its cell's read; tests use it.
	afterCellJoin func()
	// beforeSubmit, if set, is called by a confirm after its checks, just
	// before it stores the job. Tests use it to let time pass there.
	beforeSubmit func()

	gridsMu     sync.Mutex
	grids       map[*status.View]*gridCache // each view's latest rendering, while it runs
	gridRenders atomic.Int64                // grid renderings made for event streams; tests read it

	privsMu   sync.Mutex
	privs     map[string]accessEntry // each session's privileges, for accessTTL
	gridViews atomic.Int64           // grid views built for event streams; tests read it

	jobSnaps seqFlight[int64, jobSnap] // by job ID
	jobReads atomic.Int64              // job reads made for event streams; tests read it

	busy      busyCache
	busyReads atomic.Int64 // reads of the busy VMs; tests read it

	// firstPoll bounds how long the grid page waits for a new view's first
	// poll: firstPollWait, which tests set to 0.
	firstPoll time.Duration

	// jitter returns a number in [0, 1) that spreads the ends of event
	// streams; tests replace it.
	jitter func() float64
}

// New checks d and builds the app.
func New(d Deps) (*Server, error) {
	var errs []error
	if d.Store == nil {
		errs = append(errs, errors.New("a store"))
	}
	if d.As == nil {
		errs = append(errs, errors.New("a Proxmox API"))
	}
	if d.Auth == nil {
		errs = append(errs, errors.New("an auth service"))
	}
	if d.Views == nil {
		errs = append(errs, errors.New("status views"))
	}
	if d.Hub == nil {
		errs = append(errs, errors.New("a hub"))
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("web: missing %w", errors.Join(errs...))
	}
	proxies, err := d.Config.Web.TrustedProxyPrefixes()
	if err != nil {
		return nil, err
	}
	issuer, err := url.Parse(d.Config.OIDC.Issuer)
	if err != nil || issuer.Scheme == "" || issuer.Host == "" {
		return nil, fmt.Errorf("oidc.issuer %q is not a URL with a host", d.Config.OIDC.Issuer)
	}
	s := &Server{
		cfg:    d.Config,
		st:     d.Store,
		as:     d.As,
		creds:  d.Credentials,
		auth:   d.Auth,
		views:  d.Views,
		hub:    d.Hub,
		clock:  d.Clock,
		logf:   d.Logf,
		access: d.AccessLog,

		secureCookies: auth.SecureBaseURL(d.Config.Web.BaseURL),
		naming:        pods.NewNaming(d.Config.Naming),
		proxies:       proxies,
		csp:           contentSecurityPolicy(issuer.Scheme + "://" + issuer.Host),
		closed:        make(chan struct{}),

		grids:       map[*status.View]*gridCache{},
		privs:       map[string]accessEntry{},
		cellSlots:   make(chan struct{}, cellReadSlots),
		cellTimeout: cellReadTimeout,
		firstPoll:   firstPollWait,
		jitter:      rand.Float64,
	}
	s.life, s.endLife = context.WithCancel(context.Background())
	if s.clock == nil {
		s.clock = status.SystemClock
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	d.Auth.SetRender(s.AuthPage)
	return s, nil
}

// Handler serves the whole app: every route group behind the forwarded
// header handling, the security headers and the request log.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.authRoutes(mux)
	s.healthRoutes(mux)
	s.assetRoutes(mux)
	s.volunteerRoutes(mux)
	mux.HandleFunc("/", s.notFound)
	return forwarded(s.proxies, s.secure(s.logRequests(mux)))
}

// CloseStreams ends every open server-sent event stream and refuses new
// ones, and ends the server's lifetime, cancelling the shared cell reads
// (the pages waiting on them show the grid's copy of the cell), so an HTTP
// server shutting down isn't held open by either. Call it from
// http.Server.RegisterOnShutdown. It may be called more than once.
func (s *Server) CloseStreams() {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.endLife()
	})
}

// Drain makes readiness answer 503 from now on, so load balancers stop
// sending this replica new requests while it shuts down. Everything else,
// including liveness and open event streams, keeps working until the HTTP
// server stops (see CloseStreams). It may be called more than once.
func (s *Server) Drain() { s.draining.Store(true) }

// authRoutes are the login flow and the Proxmox sign-in (from auth).
func (s *Server) authRoutes(mux *http.ServeMux) {
	s.auth.Routes(mux)
	// The access page is gone: who may use battleship is Authentik's
	// decision. Old links land on the grid.
	mux.Handle("GET /access", http.RedirectHandler("/", http.StatusSeeOther))
}

// healthRoutes are the Kubernetes probes. They need no login.
func (s *Server) healthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
}

// volunteerRoutes are the pages for signed-in users. Battleship enforces
// nothing about who may do what: Proxmox does, with each user's ticket
// (previews show what it would refuse). Every POST is checked for the
// session's CSRF token before its handler runs.
func (s *Server) volunteerRoutes(mux *http.ServeMux) {
	mux.Handle("GET /{$}", s.require(http.HandlerFunc(s.gridPage)))
	mux.Handle("GET /events/grid", s.require(http.HandlerFunc(s.gridEvents)))
	mux.Handle("GET /vm/{team}/{host}", s.require(http.HandlerFunc(s.cellPage)))
	// Old links to the removed help page land on the grid.
	mux.Handle("GET /help", http.RedirectHandler("/", http.StatusSeeOther))

	for _, op := range operations {
		mux.Handle("GET "+op.Path, s.require(s.opForm(op)))
		if op.Kind == pods.KindReset || op.Kind == pods.KindSnapshot {
			// The grid's "Reset to snapshot…" and "Take snapshot…" post
			// the ticked VMs here, for the snapshot picker or the name.
			mux.Handle("POST "+op.Path, s.require(s.opForm(op)))
		}
		mux.Handle("POST "+op.Path+"/preview", s.require(s.opPreview(op)))
		mux.Handle("POST "+op.Path+"/confirm", s.require(s.opConfirm(op)))
	}

	// What ran is shown as Logs, at /logs, so nobody looks there for a way
	// to start something. Old /jobs addresses move there for good.
	mux.Handle("GET /logs", s.require(http.HandlerFunc(s.jobsPage)))
	mux.Handle("GET /logs/{id}", s.require(http.HandlerFunc(s.jobPage)))
	mux.Handle("POST /logs/{id}/cancel", s.require(http.HandlerFunc(s.cancelJob)))
	mux.Handle("POST /logs/{id}/retry", s.require(http.HandlerFunc(s.retryJob)))
	mux.HandleFunc("GET /jobs", movedToLogs(http.StatusMovedPermanently))
	mux.HandleFunc("GET /jobs/{id}", movedToLogs(http.StatusMovedPermanently))
	mux.HandleFunc("POST /jobs/{id}/{action}", movedToLogs(http.StatusPermanentRedirect))
	mux.Handle("GET /events/jobs", s.require(http.HandlerFunc(s.jobsEvents)))
	mux.Handle("GET /events/jobs/{id}", s.require(http.HandlerFunc(s.jobEvents)))
}

// movedToLogs redirects an old /jobs address to its /logs one, query
// included, with code: 301 for pages, 308 for a cancel or retry posted by
// a page loaded before the move, so the post is made again there.
func movedToLogs(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := logsPath + strings.TrimPrefix(r.URL.Path, "/jobs")
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, code)
	}
}

// logsPath is where the jobs are listed; logHref is a job's log.
const logsPath = "/logs"

func logHref(id int64) string { return logsPath + "/" + strconv.FormatInt(id, 10) }

// heartbeatEvery is how often an idle event stream sends a comment, so
// proxies don't close it as idle.
const heartbeatEvery = 15 * time.Second

// streamJitter is the most, as a fraction of web.session_refresh, that an
// event stream's end is put back by.
const streamJitter = 0.1

// minStreamLifetime is the shortest an event stream lasts, when its
// session's group check is overdue (another request is making it, or the
// identity provider is unreachable and it is backing off for a minute):
// ending at once would have the browser reconnect every couple of seconds.
const minStreamLifetime = time.Minute

// streamLifetime is how long an event stream opened now lasts: see
// streamEnd, with up to streamJitter of session_refresh added at random so
// pages opened together don't reconnect together. refreshedAt is zero
// outside a session.
func (s *Server) streamLifetime(refreshedAt time.Time) time.Duration {
	r := s.cfg.Web.SessionRefresh
	extra := time.Duration(s.jitter() * streamJitter * float64(r))
	return streamEnd(s.now(), refreshedAt, r, extra)
}

// streamEnd is how long after start a stream ends: at the earlier of
// start+refresh and the session's next check (refreshedAt+refresh), plus
// extra, but no sooner than minStreamLifetime (unless refresh+extra is
// shorter still).
func streamEnd(start, refreshedAt time.Time, refresh, extra time.Duration) time.Duration {
	d := refresh
	if !refreshedAt.IsZero() {
		if due := refreshedAt.Add(refresh).Sub(start); due < d {
			d = due
		}
	}
	return min(max(d+extra, minStreamLifetime), refresh+extra)
}

func (s *Server) now() time.Time { return s.clock.Now() }

// accessTTL is how long a session's privileges are kept before Proxmox is
// asked again.
const accessTTL = 60 * time.Second

type accessEntry struct {
	acc *pods.UserAccess
	at  time.Time
}

// accessFor is the signed-in user's Proxmox privileges, read with their
// ticket and kept per session for accessTTL; nil if the API can't read
// privileges (only test fakes can't).
func (s *Server) accessFor(ctx context.Context) *pods.UserAccess {
	r, ok := s.apiFor(ctx).(pods.PermissionReader)
	if !ok {
		return nil
	}
	id, now := auth.SessionID(ctx), s.now()
	s.privsMu.Lock()
	defer s.privsMu.Unlock()
	if e, ok := s.privs[id]; ok && now.Sub(e.at) < accessTTL {
		return e.acc
	}
	for k, e := range s.privs {
		if now.Sub(e.at) >= accessTTL {
			delete(s.privs, k)
		}
	}
	acc := pods.NewAccess(r)
	s.privs[id] = accessEntry{acc: acc, at: now}
	return acc
}

// planner plans as the signed-in user, checking their privileges.
func (s *Server) planner(ctx context.Context) pods.Planner {
	p := pods.NewPlanner(s.apiFor(ctx), s.cfg)
	if acc := s.accessFor(ctx); acc != nil {
		p.Access = acc
	}
	return p
}

// apiFor is the Proxmox API acting as the request's signed-in user, with
// their ticket (see auth.RequireUser). Outside a session it carries no
// credential, and its calls fail without reaching Proxmox.
func (s *Server) apiFor(ctx context.Context) pods.API {
	cred, _ := auth.ProxmoxCredential(ctx)
	return s.as(cred)
}
