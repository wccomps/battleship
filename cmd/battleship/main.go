// Command battleship deploys, resets, powers and tears down team pods on Proxmox.
//
// Every operation prints its plan, asks for confirmation (teardown wants the
// team range typed), and applies exactly that plan. -yes skips the prompt;
// without -yes and without a terminal, nothing changes. Any character device
// on stdin (even /dev/null) counts as a terminal, so automation should pass
// -yes or redirect stdin from a file.
//
// -queue stores the plan as a job for battleship worker, which replans and
// refuses to run if the cluster changed. With a database configured, direct
// runs are jobs too and wait for older jobs on the same teams or templates.
//
// battleship serve runs the volunteer web app and job workers in one process
// per replica. On SIGTERM it fails readiness, stops running jobs cleanly and
// exits within web.shutdown_timeout; a second signal exits at once.
//
//	battleship deploy   -config battleship.toml -templates '*.kilo.alpha' -teams 1-32
//	battleship teardown -config battleship.toml -teams 1-32 -yes
//	battleship teardown -config battleship.toml -teams all -yes
//	battleship reset    -config battleship.toml -teams 7 -hosts dc -yes
//	battleship power    -config battleship.toml -teams 1-32 -action start -yes -queue
//	battleship worker   -config battleship.toml
//	battleship jobs list|show ID|cancel ID -config battleship.toml
//	battleship serve    -config battleship.toml -log-format json
//	battleship nodes    -config battleship.toml
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
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// After the first signal, restore default handling so a second one kills
	// the process while the executor winds down.
	serving := len(os.Args) > 1 && os.Args[1] == "serve"
	go func() {
		<-ctx.Done()
		if !serving {
			fmt.Fprintln(os.Stderr, "Interrupting... finishing or removing anything half-built. "+
				"Press Ctrl-C again to force quit (may leave half-built VMs and a stopped master). "+
				"Proxmox tasks already started keep running.")
		}
		stop()
	}()
	fi, err := os.Stdin.Stat()
	d := deps{
		newAPI:      newProxmoxAPI,
		openStore:   openStore,
		user:        cliUser(),
		in:          os.Stdin,
		interactive: err == nil && fi.Mode()&os.ModeCharDevice != 0,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
	}
	os.Exit(run(ctx, os.Args[1:], d))
}

// newProxmoxAPI is the Proxmox client for p. It holds no credential: every
// call goes through asUser, as a person.
func newProxmoxAPI(p config.Proxmox) pods.API {
	return proxmox.New(proxmox.Options{
		URLs: p.Endpoints(), InsecureSkipVerify: p.InsecureSkipVerify, RootCAs: p.RootCAs,
	})
}

// deps are the process-level dependencies of run, replaced in tests.
type deps struct {
	newAPI func(config.Proxmox) pods.API
	// openStore connects to and migrates the job database; the returned func
	// closes it.
	openStore      func(ctx context.Context, db config.Database) (*store.Store, func(), error)
	user           string // recorded on jobs this process queues or cancels
	in             io.Reader
	interactive    bool // stdin is a terminal, so we can ask for confirmation
	stdout, stderr io.Writer
	// battleship serve: listen opens the HTTP listener, and drain waits between
	// failing readiness and stopping the HTTP server. Nil means net.Listen
	// and drainDelay.
	listen func(network, addr string) (net.Listener, error)
	drain  func()
}

const usage = `usage: battleship <deploy|teardown|reset|power|snapshot> [flags]
       battleship worker [flags]
       battleship jobs <list|show ID|cancel ID> [flags]
       battleship serve [flags]
       battleship nodes [flags]

Every operation prints its plan first, then asks you to confirm it (-yes
skips the question and applies the plan; -queue stores it as a job for a
worker to run). -teams is required; for teardown, power, reset and snapshot,
-teams all means every team that has VMs (e.g. 0-32 with a test team 00),
worked out from the cluster and printed with the plan. Put flags
before any other words. Run
"battleship <command> -h" for flags. battleship serve runs the web app and job workers.
battleship nodes lists the cluster's nodes and prints a proxmox.urls line.

  battleship deploy   -config battleship.toml -templates '*.kilo.alpha' -teams 1-32
  battleship teardown -config battleship.toml -teams 1-32 -yes
  battleship teardown -config battleship.toml -teams all -yes
  battleship reset    -config battleship.toml -teams 7 -hosts dc -yes
  battleship power    -config battleship.toml -teams 1-32 -action start -yes -queue
  battleship snapshot -config battleship.toml -teams 7 -hosts dc -name before-scoring -yes
  battleship worker   -config battleship.toml
  battleship jobs list -config battleship.toml
  battleship serve    -config battleship.toml -log-format json
  battleship nodes    -config battleship.toml
`

func run(ctx context.Context, args []string, d deps) int {
	if len(args) == 0 {
		fmt.Fprint(d.stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Fprint(d.stdout, usage)
		return 0
	case "deploy", "teardown", "reset", "power", "snapshot":
		return runOp(ctx, args[0], args[1:], d)
	case "worker":
		return runWorker(ctx, args[1:], d)
	case "jobs":
		return runJobs(ctx, args[1:], d)
	case "serve":
		return runServe(ctx, args[1:], d)
	case "nodes":
		return runNodes(ctx, args[1:], d)
	}
	fmt.Fprint(d.stderr, usage)
	return 2
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

// cliUser names whoever runs the CLI, for the job record.
func cliUser() string {
	for _, k := range []string{"SUDO_USER", "USER", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			return "cli:" + v
		}
	}
	return "cli:unknown"
}
