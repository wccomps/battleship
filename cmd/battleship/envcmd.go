package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	kinds := map[string]pods.Kind{"register": pods.KindImageRegister, "edit": pods.KindImageEdit,
		"deprecate": pods.KindImageDeprecate, "delete": pods.KindImageDelete}
	if len(args) == 0 || kinds[args[0]] == "" {
		fmt.Fprintln(d.stderr, "usage: battleship image register|edit|deprecate|delete [flags]")
		return 2
	}
	in := jobs.Inputs{Kind: kinds[args[0]]}
	fs := flag.NewFlagSet("battleship image "+args[0], flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath, yes, queue := commonFlags(fs)
	switch in.Kind {
	case pods.KindImageRegister:
		fs.IntVar(&in.VMID, "vm", 0, "VMID of the stopped, generalized VM to copy in (required)")
		fs.StringVar(&in.Family, "family", "", "image family, e.g. debian-12; an existing family gets its next version (required)")
		fs.StringVar(&in.ImageKind, "kind", "", "router, windows, linux or other (required for a new family)")
	case pods.KindImageEdit:
		fs.StringVar(&in.Family, "family", "", "family whose newest version to copy for editing (required)")
	default:
		fs.StringVar(&in.Image, "image", "", "base image, e.g. debian-12-v1 (required)")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if in.Kind == pods.KindImageRegister && in.VMID == 0 {
		fmt.Fprintln(d.stderr, "-vm is required: the VMID of the VM to register")
		return 2
	}
	return validated(ctx, d, args[0], *cfgPath, in, *yes, *queue)
}

// runEnv handles battleship env create|add|dns.
func runEnv(ctx context.Context, args []string, d deps) int {
	kinds := map[string]pods.Kind{"create": pods.KindEnvCreate, "add": pods.KindEnvAdd, "dns": pods.KindEnvDNS}
	if len(args) == 0 || kinds[args[0]] == "" {
		fmt.Fprintln(d.stderr, "usage: battleship env create|add|dns [flags]")
		return 2
	}
	in := jobs.Inputs{Kind: kinds[args[0]]}
	fs := flag.NewFlagSet("battleship env "+args[0], flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath, yes, queue := commonFlags(fs)
	fs.StringVar(&in.Domain, "domain", "", "the environment's domain, e.g. kilo.alpha (required)")
	var hosts stringList
	switch in.Kind {
	case pods.KindEnvCreate, pods.KindEnvAdd:
		fs.StringVar(&in.Router, "router", "", "router as name=image, e.g. gw=opnsense (required to create)")
		fs.Var(&hosts, "host", "host as name=image or name=image:octet, e.g. dc=win2022:10; repeat for each")
	case pods.KindEnvDNS:
		fs.StringVar(&in.DNS, "server", "router", "router, or the host to use as DNS server, e.g. dc")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	in.EnvHosts = hosts
	return validated(ctx, d, args[0], *cfgPath, in, *yes, *queue)
}

func commonFlags(fs *flag.FlagSet) (cfgPath *string, yes, queue *bool) {
	return fs.String("config", "battleship.toml", "config file"),
		fs.Bool("yes", false, "apply the plan without asking"),
		fs.Bool("queue", false, "queue the confirmed plan as a job and exit instead of following it")
}

// validated checks in before contacting anything, then plans and applies.
func validated(ctx context.Context, d deps, cmd, cfgPath string, in jobs.Inputs, yes, queue bool) int {
	if err := in.Validate(); err != nil {
		fmt.Fprintln(d.stderr, err)
		return 2
	}
	return runPlanned(ctx, d, cmd, cfgPath, in, false, yes, queue)
}
