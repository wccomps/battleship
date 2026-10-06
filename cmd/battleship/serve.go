package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/status"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/web"
)

const (
	// drainDelay is how long readiness fails before the HTTP server stops,
	// so Kubernetes and the ingress take the replica out of rotation first.
	drainDelay = 5 * time.Second
	// httpShutdownTimeout bounds the wait for requests in flight once the
	// HTTP server stops; event streams end at once.
	httpShutdownTimeout = 10 * time.Second
	// oidcStartupWait is how long startup keeps trying to reach the identity
	// provider, so a pod that starts before its network is ready still comes
	// up; oidcRetryEvery is the pause between tries.
	oidcStartupWait = 30 * time.Second
	oidcRetryEvery  = 2 * time.Second
	// cleanupEvery is how often expired sessions and previews are deleted.
	cleanupEvery = time.Hour
	// cleanupTimeout bounds each of a cleanup's database calls.
	cleanupTimeout = 30 * time.Second
	// viewLinger is how long a viewer's grid keeps polling after their last
	// grid page closes, so a reload or the stream of a page just rendered
	// takes it over.
	viewLinger = time.Minute
)

// serveDeps are what serve runs on, replaced in tests. Zero durations and a
// nil drain mean the defaults above.
type serveDeps struct {
	newAPI    func(config.Proxmox) pods.API
	openStore func(ctx context.Context, db config.Database) (*store.Store, func(), error)
	listen    func(network, addr string) (net.Listener, error)
	log       *slog.Logger
	// drain waits between failing readiness and stopping the HTTP server.
	drain     func()
	oidcWait  time.Duration
	oidcRetry time.Duration
}

// runServe is "battleship serve": the web app, the status grid and the job
// workers in one process, until a stop signal.
func runServe(ctx context.Context, args []string, d deps) int {
	fs := flag.NewFlagSet("battleship serve", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("config", "battleship.toml", "config file")
	format := fs.String("log-format", "text", "log format: text or json")
	if code, ok := parseFlags(fs, args, 0, d); !ok {
		return code
	}
	log, err := newLogger(d.stderr, *format)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("battleship serve can't start", "err", err)
		return 1
	}
	sd := serveDeps{newAPI: d.newAPI, openStore: d.openStore, listen: d.listen, log: log, drain: d.drain}
	if err := serve(ctx, cfg, sd); err != nil {
		return 1 // serve logged it
	}
	return 0
}

// serve runs battleship serve with cfg until ctx is done (the stop signal), then
// shuts down, and logs and returns any error. At startup it checks the
// config, opens and migrates the database, and reaches the identity
// provider, failing clearly if any of that doesn't work. It then runs, all
// sharing one Proxmox client and one set of concurrency limits:
//
//   - web.workers job workers;
//   - each viewer's status grid (polls and drift scans with their own
//     ticket, while they have the grid open), and the ticket renewer;
//   - one hub, which holds the process's only LISTEN connection;
//   - an hourly cleanup of expired sessions and previews;
//   - the HTTP server.
//
// On the stop signal, readiness fails at once and the running jobs are
// told to stop: each stops starting steps, runs its cleanup and ends
// interrupted. After a short drain for the load balancer, the HTTP server
// stops, ending the event streams. serve then waits for the workers until
// web.shutdown_timeout after the signal, and exits anyway after that,
// returning an error.
func serve(ctx context.Context, cfg config.Config, d serveDeps) error {
	if d.listen == nil {
		d.listen = net.Listen
	}
	if d.drain == nil {
		d.drain = func() { time.Sleep(drainDelay) }
	}
	if d.oidcWait <= 0 {
		d.oidcWait = oidcStartupWait
	}
	if d.oidcRetry <= 0 {
		d.oidcRetry = oidcRetryEvery
	}
	log := d.log
	cantStart := func(err error) error {
		log.Error("battleship serve can't start", "err", err)
		return err
	}

	if err := errors.Join(cfg.RequireDatabase(), cfg.RequireWeb(), cfg.RequireSealKey(), noServiceToken("battleship serve"), cfg.RequireTLSVerify()); err != nil {
		return cantStart(err)
	}
	if cfg.Proxmox.InsecureSkipVerify {
		log.Warn("Proxmox's certificate is not checked", "because", "proxmox.insecure_skip_verify = true",
			"risk", "people's tickets go to whatever answers at proxmox.url", "fix", "set proxmox.ca_file to the cluster CA (/etc/pve/pve-root-ca.pem)")
	}
	log.Info("starting", "base_url", cfg.Web.BaseURL,
		"workers", cfg.Web.Workers, "shutdown_timeout", cfg.Web.ShutdownTimeout.String())
	if cfg.Web.ShutdownTimeout <= pods.StopBudget {
		log.Warn("web.shutdown_timeout is not longer than running jobs may take to stop, so a shutdown can cut their cleanup short and leave half-built VMs or a master stopped",
			"shutdown_timeout", cfg.Web.ShutdownTimeout.String(), "stop_budget", pods.StopBudget.String())
	}

	st, closeStore, err := d.openStore(ctx, cfg.Database)
	if err != nil {
		return cantStart(err)
	}
	creds, err := jobs.OpenCredentials(ctx, cfg, st)
	if err != nil {
		closeStore()
		return cantStart(err)
	}
	keepStore := false // set when abandoned workers may still use it
	defer func() {
		if !keepStore {
			closeStore()
		}
	}()
	api := d.newAPI(cfg.Proxmox)
	setProxmoxLogf(api, logfFor(log, "proxmox"))
	login, ok := api.(auth.ProxmoxLogin)
	if !ok {
		return cantStart(errors.New("this Proxmox client can't sign users in to Proxmox"))
	}
	svc, err := connectAuth(ctx, cfg, st, login, d)
	if err != nil {
		return cantStart(err)
	}
	lim := pods.NewClusterLimits(cfg.Concurrency, st)
	hub := status.NewHub(st.Notifications, status.HubOptions{Logf: logfFor(log, "status")})
	views, err := status.NewViews(func(cred func() proxmox.Credential) pods.API { return asUser(api, cred, nil) },
		st, lim, cfg, status.Options{Hub: hub, Logf: logfFor(log, "status")}, viewLinger)
	if err != nil {
		return cantStart(err)
	}
	app, err := web.New(web.Deps{
		Config: cfg, Store: st, Auth: svc, Views: views, Hub: hub, Credentials: creds,
		As:        func(cred proxmox.Credential) pods.API { return asToken(api, cred) },
		Logf:      logfFor(log, "web"),
		AccessLog: log.With("component", "http"),
	})
	if err != nil {
		return cantStart(err)
	}
	ln, err := d.listen("tcp", cfg.Web.Listen)
	if err != nil {
		return cantStart(fmt.Errorf("web.listen: %w", err))
	}
	httpSrv := &http.Server{
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.With("component", "http").Handler(), slog.LevelWarn),
	}
	httpSrv.RegisterOnShutdown(app.CloseStreams)

	// Background work stops after the HTTP server; the workers stop first,
	// when the signal comes. Neither uses ctx, which the signal cancels.
	bgCtx, stopBackground := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBackground()
	var bg sync.WaitGroup
	background := []func(context.Context){hub.Run, func(ctx context.Context) { <-ctx.Done(); views.Close() }, func(ctx context.Context) { cleanupLoop(ctx, st, log) }}
	if renewer, ok := api.(jobs.TicketRenewer); ok {
		r := &jobs.Renewer{Store: st, Proxmox: renewer, Credentials: creds, Logf: logfFor(log, "renewer")}
		background = append(background, r.Run)
	}
	for _, run := range background {
		bg.Add(1)
		go func() { defer bg.Done(); run(bgCtx) }()
	}
	workCtx, stopWorkers := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWorkers()
	workersDone := startWorkers(workCtx, cfg, st, api, creds, lim, hub, log)
	served := make(chan error, 1)
	go func() { served <- httpSrv.Serve(ln) }()
	log.Info("serving", "listen", ln.Addr().String(), "workers", cfg.Web.Workers)

	var failure error
	select {
	case <-ctx.Done():
		log.Info("shutting down", "shutdown_timeout", cfg.Web.ShutdownTimeout.String(),
			"note", "readiness fails now; running jobs stop starting steps, clean up and end interrupted; a second signal exits at once, skipping their cleanup")
	case err := <-served:
		failure = fmt.Errorf("serving HTTP: %w", err)
		log.Error("shutting down", "err", failure)
	}
	start := time.Now()
	deadline := start.Add(cfg.Web.ShutdownTimeout)

	app.Drain()
	stopWorkers()
	if failure == nil {
		d.drain()
	}
	httpCtx, stopHTTP := context.WithDeadline(context.Background(), minTime(time.Now().Add(httpShutdownTimeout), deadline))
	if err := httpSrv.Shutdown(httpCtx); err != nil {
		log.Warn("requests still running when the HTTP server stopped; cutting them off", "err", err)
		_ = httpSrv.Close()
	}
	stopHTTP()
	stopBackground()

	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()
	select {
	case <-workersDone:
		log.Info("workers stopped", "took", time.Since(start).Round(time.Millisecond).String())
	case <-timeout.C:
		keepStore = true
		err := fmt.Errorf("jobs were still cleaning up when web.shutdown_timeout (%s) ran out; exiting anyway. "+
			"Another replica marks them interrupted after jobs.stale_after; check their VMs and re-run them", cfg.Web.ShutdownTimeout)
		log.Error("shutdown timed out", "err", err)
		return errors.Join(failure, err)
	}
	bgDone := make(chan struct{})
	go func() { bg.Wait(); close(bgDone) }()
	select {
	case <-bgDone:
	case <-timeout.C:
		keepStore = true // closing it would wait for their connections
		log.Warn("the status views or hub didn't stop in time")
	}
	log.Info("stopped", "took", time.Since(start).Round(time.Millisecond).String())
	return failure
}

// noServiceToken refuses to run what (battleship serve or battleship worker)
// with a Proxmox token in the environment: they act only as the people who
// asked, so a token there is a leftover of the old service token (or a
// user's own token, which only their CLI runs may use), and must not linger.
func noServiceToken(what string) error {
	for _, k := range []string{"BATTLESHIP_PROXMOX_TOKEN_ID", "BATTLESHIP_PROXMOX_TOKEN_SECRET"} {
		if os.Getenv(k) != "" {
			return fmt.Errorf("%s is set, but %s holds no Proxmox token: it acts as each person who asked. "+
				"Remove it (these variables are only for a person's own token on the command line), and delete the old service token in Proxmox", k, what)
		}
	}
	return nil
}

// connectAuth builds the auth service, which discovers the identity
// provider. It retries for d.oidcWait, so a pod that starts before the
// provider is reachable still comes up; after that, startup fails with a
// hint to check oidc.issuer.
func connectAuth(ctx context.Context, cfg config.Config, st *store.Store, login auth.ProxmoxLogin, d serveDeps) (*auth.Service, error) {
	opts := auth.Options{Logf: logfFor(d.log, "auth"), Proxmox: login} // web.New renders its pages
	wait, cancel := context.WithTimeout(ctx, d.oidcWait)
	defer cancel()
	for {
		svc, err := auth.NewService(wait, cfg, st, opts)
		if err == nil {
			return svc, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("stopped while reaching the identity provider: %w", err)
		}
		if wait.Err() == nil {
			d.log.Warn("identity provider not reachable yet; retrying", "err", err)
		}
		retry := time.NewTimer(d.oidcRetry)
		select {
		case <-wait.Done():
			retry.Stop()
			if ctx.Err() != nil { // the stop signal came during the pause
				return nil, fmt.Errorf("stopped while reaching the identity provider: %w", err)
			}
			return nil, fmt.Errorf("%w (gave up after %s; check oidc.issuer, and that the identity provider is reachable from here)", err, d.oidcWait)
		case <-retry.C:
		}
	}
}

// hubCancels is a jobs.Worker's Cancels that wakes on hub's messages
// about a job's cancel requests (status.CancelTopic).
func hubCancels(hub *status.Hub) func(int64) (<-chan struct{}, func()) {
	return func(id int64) (<-chan struct{}, func()) { return hub.Wake(status.CancelTopic(id)) }
}

// listenForCancels runs a hub of its own over st's job notices, for a
// worker outside battleship serve, and returns its Cancels; stop ends the
// hub and its LISTEN connection.
func listenForCancels(ctx context.Context, st *store.Store, logf func(format string, args ...any)) (cancels func(int64) (<-chan struct{}, func()), stop func()) {
	hub := status.NewHub(st.Notifications, status.HubOptions{Logf: logf})
	hubCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); hub.Run(hubCtx) }()
	return hubCancels(hub), func() { cancel(); <-done }
}

// startWorkers starts cfg.Web.Workers job workers sharing api and lim, and
// returns a channel closed once they all return, which they do once ctx is
// done and their running jobs have ended. hub tells them of cancels.
func startWorkers(ctx context.Context, cfg config.Config, st *store.Store, client pods.API, creds jobs.Credentials, lim *pods.Limits, hub *status.Hub, log *slog.Logger) <-chan struct{} {
	var wg sync.WaitGroup
	for range cfg.Web.Workers {
		id := jobs.NewWorkerID("serve")
		wl := log.With("component", "worker", "worker", id)
		w := &jobs.Worker{
			Store: st, Cfg: cfg, ID: id, Limits: lim, Credentials: creds,
			Bind:    bindAs(client),
			Logf:    func(format string, args ...any) { wl.Warn(fmt.Sprintf(format, args...)) },
			Cancels: hubCancels(hub),
			OnStart: func(j *store.Job) {
				wl.Info("job started", "job", j.ID, "kind", j.Kind, "created_by", j.CreatedBy)
			},
			OnFinish: func(j *store.Job, out store.Outcome) {
				level, attrs := slog.LevelInfo, []any{"job", j.ID, "kind", j.Kind, "status", out.Status}
				if out.Status != store.StatusSucceeded {
					level = slog.LevelWarn
				}
				if out.Error != "" {
					attrs = append(attrs, "error", out.Error)
				}
				wl.Log(context.Background(), level, "job finished", attrs...)
			},
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			wl.Info("worker started")
			if err := w.Run(ctx); err != nil {
				wl.Error("worker failed", "err", err)
			}
			wl.Info("worker stopped")
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	return done
}

// cleanupLoop deletes expired sessions and previews at startup and then
// every cleanupEvery, until ctx is done.
func cleanupLoop(ctx context.Context, st *store.Store, log *slog.Logger) {
	for {
		cleanUp(ctx, st, time.Now(), cleanupTimeout, log)
		t := time.NewTimer(cleanupEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// cleaner is the part of the store cleanUp uses.
type cleaner interface {
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
	DeleteExpiredPreviews(ctx context.Context, now time.Time) (int64, error)
}

// cleanUp deletes the sessions and previews that expired by now. Sessions
// take their previews with them; previews also expire on their own, well
// before a long session does.
func cleanUp(parent context.Context, st cleaner, now time.Time, timeout time.Duration, log *slog.Logger) {
	// Each delete gets its own timeout, so one that hangs doesn't use up
	// the other's.
	deleteExpired := func(what string, del func(context.Context, time.Time) (int64, error)) int64 {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		n, err := del(ctx, now)
		if err != nil && parent.Err() == nil {
			log.Warn("cleanup failed", "of", what, "err", err)
		}
		return n
	}
	sessions := deleteExpired("sessions", st.DeleteExpiredSessions)
	previews := deleteExpired("previews", st.DeleteExpiredPreviews)
	if sessions+previews > 0 {
		log.Info("cleaned up", "sessions", sessions, "previews", previews)
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
