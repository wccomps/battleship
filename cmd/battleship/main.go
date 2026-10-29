// Command battleship runs the volunteer web app and job workers in one
// process per replica. On SIGTERM it fails readiness, claims no more jobs,
// lets running jobs finish (stopping them cleanly if web.shutdown_timeout is
// about to run out) and exits within web.shutdown_timeout; a second signal
// exits at once.
//
//	battleship [-config battleship.toml] [-log-format text|json]
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

func main() {
	ctx, stop := stopOnSignal(func() {})
	defer stop()
	d := deps{
		newCluster: newCluster,
		openStore:  openStore,
		stderr:     os.Stderr,
	}
	os.Exit(run(ctx, os.Args[1:], d))
}

// stopOnSignal returns a context done at the first interrupt, SIGTERM or
// SIGHUP. Default handling then comes back, so a second one kills the
// process at once, while the executor winds down; onFirst runs after that.
func stopOnSignal(onFirst func()) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-ctx.Done()
		stop()
		onFirst()
	}()
	return ctx, stop
}

// newCluster connects to p's nodes, logging failovers to logf.
func newCluster(p config.Proxmox, logf func(format string, args ...any)) cluster {
	c := proxmox.New(proxmox.Options{
		URLs: p.Endpoints(), InsecureSkipVerify: p.InsecureSkipVerify, RootCAs: p.RootCAs, Logf: logf,
	})
	return proxmoxCluster{ProxmoxLogin: c, client: c}
}

// deps are the process-level dependencies of run, replaced in tests.
type deps struct {
	newCluster func(p config.Proxmox, logf func(format string, args ...any)) cluster
	// openStore connects to and migrates the job database; the returned func
	// closes it.
	openStore func(ctx context.Context, db config.Database) (*store.Store, func(), error)
	stderr    io.Writer
	// listen opens the HTTP listener, and drain waits between failing
	// readiness and stopping the HTTP server. Nil means net.Listen and
	// drainDelay.
	listen func(network, addr string) (net.Listener, error)
	drain  func()
}

func openStore(ctx context.Context, db config.Database) (*store.Store, func(), error) {
	st, err := store.Open(ctx, db.URL, store.Options{MaxConns: db.MaxConns})
	if err != nil {
		return nil, nil, err
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("migrating database: %w", err)
	}
	return st, st.Close, nil
}
