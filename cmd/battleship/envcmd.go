package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// runImage handles battleship image register|edit|deprecate|delete.
func runImage(ctx context.Context, args []string, d deps) int {
	c, code, ok := parseImage(args, d.stderr)
	if !ok {
		return code
	}
	return runPlanned(ctx, d, args[0], c.cfgPath, c.in, false, c.yes, c.queue)
}

// parseImage reads an image command's flags into the inputs the web form
// would give (Inputs.Normalize); if not ok, the command exits with code.
func parseImage(args []string, stderr io.Writer) (c command, code int, ok bool) {
	kinds := map[string]pods.Kind{"register": pods.KindImageRegister, "edit": pods.KindImageEdit,
		"deprecate": pods.KindImageDeprecate, "delete": pods.KindImageDelete}
	if len(args) == 0 || kinds[args[0]] == "" {
		fmt.Fprintln(stderr, "usage: battleship image register|edit|deprecate|delete [flags]")
		return c, 2, false
	}
	in := jobs.Inputs{Kind: kinds[args[0]]}
	fs := flag.NewFlagSet("battleship image "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath, yes, queue := commonFlags(fs)
	switch in.Kind {
	case pods.KindImageRegister:
		fs.Func("vm", "VMID of the stopped, generalized VM to copy in (required)", func(v string) (err error) {
			in.VMID, err = strconv.Atoi(strings.TrimSpace(v)) // decimal, as the form reads it
			return err
		})
		fs.StringVar(&in.Family, "family", "", "image family, e.g. debian-12; an existing family gets its next version (required)")
		fs.StringVar(&in.ImageKind, "kind", "", "router, windows, linux or other (required for a new family)")
	case pods.KindImageEdit:
		fs.StringVar(&in.Family, "family", "", "family whose newest version to copy for editing (required)")
	default:
		fs.StringVar(&in.Image, "image", "", "base image, e.g. debian-12-v1 (required)")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return c, 0, false
		}
		return c, 2, false
	}
	return finishTeamless(fs, in, *cfgPath, *yes, *queue, stderr)
}

// runEnv handles battleship env create|add|teardown.
func runEnv(ctx context.Context, args []string, d deps) int {
	c, code, ok := parseEnv(args, d.stderr)
	if !ok {
		return code
	}
	return runPlanned(ctx, d, args[0], c.cfgPath, c.in, false, c.yes, c.queue)
}

// parseEnv reads an environment command's flags into the inputs the web
// form would give (Inputs.Normalize); if not ok, the command exits with code.
func parseEnv(args []string, stderr io.Writer) (c command, code int, ok bool) {
	kinds := map[string]pods.Kind{"create": pods.KindEnvCreate, "add": pods.KindEnvAdd, "teardown": pods.KindEnvTeardown}
	if len(args) == 0 || kinds[args[0]] == "" {
		fmt.Fprintln(stderr, "usage: battleship env create|add|teardown [flags]")
		return c, 2, false
	}
	in := jobs.Inputs{Kind: kinds[args[0]]}
	fs := flag.NewFlagSet("battleship env "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath, yes, queue := commonFlags(fs)
	fs.StringVar(&in.Domain, "domain", "", "the environment's domain, e.g. kilo.alpha (required)")
	var hosts stringList
	switch in.Kind {
	case pods.KindEnvCreate, pods.KindEnvAdd:
		if in.Kind == pods.KindEnvCreate {
			fs.StringVar(&in.Slot, "slot", "", "the dev slot to create it in, e.g. 02: a free one, or the one already holding -domain, to resume it (default: the first free one)")
		}
		fs.StringVar(&in.Router, "router", "", "router as name=image, e.g. gw=opnsense (required to create)")
		fs.Var(&hosts, "host", "host as name=image or name=image:octet, e.g. dc=win2022:10; repeat for each")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return c, 0, false
		}
		return c, 2, false
	}
	in.EnvHosts = hosts
	return finishTeamless(fs, in, *cfgPath, *yes, *queue, stderr)
}

func commonFlags(fs *flag.FlagSet) (cfgPath *string, yes, queue *bool) {
	return fs.String("config", "battleship.toml", "config file"),
		fs.Bool("yes", false, "apply the plan without asking"),
		fs.Bool("queue", false, "queue the confirmed plan as a job and exit instead of following it")
}

func finishTeamless(fs *flag.FlagSet, in jobs.Inputs, cfgPath string, yes, queue bool, stderr io.Writer) (c command, code int, ok bool) {
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q (put flags before any other words)\n", fs.Arg(0))
		return c, 2, false
	}
	in = in.Normalize()
	if err := in.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return c, 2, false
	}
	return command{in: in, cfgPath: cfgPath, yes: yes, queue: queue}, 0, true
}
