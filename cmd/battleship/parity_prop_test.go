package main

import (
	"io"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/web"
)

// cliFlags are each command's string flags and the Inputs field each sets.
var cliFlags = map[string]map[string]string{
	"deploy":    {"teams": "Teams", "hosts": "Hosts", "vms": "VMs", "templates": "Pattern"},
	"teardown":  {"teams": "Teams", "hosts": "Hosts", "vms": "VMs"},
	"reset":     {"teams": "Teams", "hosts": "Hosts", "vms": "VMs", "snapshot": "Snapshot"},
	"power":     {"teams": "Teams", "hosts": "Hosts", "vms": "VMs", "action": "Action"},
	"snapshot":  {"teams": "Teams", "hosts": "Hosts", "vms": "VMs", "name": "Snapshot", "description": "Description"},
	"register":  {"vm": "VMID", "family": "Family", "kind": "ImageKind"},
	"edit":      {"family": "Family"},
	"deprecate": {"image": "Image"},
	"delete":    {"image": "Image"},
	"create":    {"domain": "Domain", "router": "Router", "host": "EnvHosts"},
	"add":       {"domain": "Domain", "router": "Router", "host": "EnvHosts"},
	"dns":       {"domain": "Domain", "server": "DNS"},
}

// cliBools are each command's boxes.
var cliBools = map[string]map[string]string{
	"deploy": {"rebuild": "Rebuild", "no-snapshot": "NoSnapshot"}, "snapshot": {"vmstate": "VMState"},
}

// cliValues are flag values: valid ones, spaces, lists, signs and other
// bases on numbers, and lines.
var cliValues = []string{"1", " 1-2 ", "1,2", "dc", " dc , web ", ",", "team01-dc", "*.kilo.alpha", " *.kilo.alpha ", "round2", " round2",
	"stop", " stop ", "Stop", "a\nb", "kilo.alpha", " kilo.alpha ", "gw=opnsense", " gw = gw ", "dc=debian-12:20", "", " ", "router", " dc ",
	"7000", " 7000 ", "010", "0x1f4", "+150", "-5", "abc", "1_000", "debian-12", " debian-12", "linux", "debian-12-v1", " debian-12-v1 "}

func parseCLI(sub string, args []string) (command, bool) {
	var c command
	var ok bool
	switch sub {
	case "register", "edit", "deprecate", "delete":
		c, _, ok = parseImage(append([]string{sub}, args...), io.Discard)
	case "create", "add", "dns":
		c, _, ok = parseEnv(append([]string{sub}, args...), io.Discard)
	default:
		c, _, ok = parseOp(sub, args, io.Discard)
	}
	return c, ok
}

// kindOf is the job kind of a command.
func kindOf(sub string) pods.Kind {
	switch sub {
	case "register", "edit", "deprecate", "delete":
		return pods.Kind("image-" + sub)
	case "create", "add", "dns":
		return pods.Kind("env-" + sub)
	}
	return pods.Kind(sub)
}

// checkParity runs sub with flags set to values and checks it asks for
// what the web form would from the same values (web.Normalize), and, where
// the CLI validates, refuses just what the form's validation would.
func checkParity(t interface{ Fatalf(string, ...any) }, sub string, values map[string][]string, boxes map[string]bool) {
	raw := jobs.Inputs{Kind: kindOf(sub)}
	r := reflect.ValueOf(&raw).Elem()
	args := []string{"-config", "/nonexistent.toml"}
	badVM := false
	for flag, field := range cliFlags[sub] {
		for _, v := range values[flag] {
			args = append(args, "-"+flag+"="+v)
			f := r.FieldByName(field)
			switch field {
			case "Hosts", "VMs":
				f.Set(reflect.ValueOf(jobs.SplitList(v)))
			case "EnvHosts":
				f.Set(reflect.Append(f, reflect.ValueOf(v)))
			case "VMID":
				n, err := strconv.Atoi(strings.TrimSpace(v)) // the form reads decimal
				badVM = badVM || err != nil
				f.SetInt(int64(n))
			default:
				f.SetString(v)
			}
		}
	}
	if sub == "dns" && values["server"] == nil {
		raw.DNS = "router" // the form's default too
	}
	for flag, field := range cliBools[sub] {
		if boxes[flag] {
			args = append(args, "-"+flag)
			r.FieldByName(field).SetBool(true)
		}
	}
	want := web.Normalize(raw)
	c, ok := parseCLI(sub, args)
	if ok && !reflect.DeepEqual(c.in, want) {
		t.Fatalf("battleship %s %q asks for %#v; the form, %#v", sub, args, c.in, want)
	}
	formOK := want.Validate() == nil && !badVM
	switch {
	case want.Kind.Teamless():
		if cliOK := ok && c.in.Validate() == nil; cliOK != formOK {
			t.Fatalf("battleship %s %q: CLI accepts %v, the form %v", sub, args, cliOK, formOK)
		}
	case !ok && formOK && (values["hosts"] == nil || len(want.Hosts) > 0) && (values["vms"] == nil || len(want.VMs) > 0):
		t.Fatalf("battleship %s %q refused what the form accepts", sub, args)
	}
}

// A command line asks for the same job as the web form given the same
// values.
func TestPropCLIAsksWhatTheFormDoes(t *testing.T) {
	subs := []string{"deploy", "teardown", "reset", "power", "snapshot", "register", "edit", "deprecate", "delete", "create", "add", "dns"}
	valid := map[string]string{"teams": "1", "templates": "*.x", "action": "stop", "name": "round2", "vm": "7000", "family": "debian-12",
		"image": "debian-12-v1", "domain": "kilo.alpha", "router": "gw=gw"}
	for _, sub := range subs {
		for flag := range cliFlags[sub] {
			for _, v := range cliValues {
				values := map[string][]string{}
				for f := range cliFlags[sub] {
					if valid[f] != "" {
						values[f] = []string{valid[f]}
					}
				}
				values[flag] = []string{v}
				checkParity(t, sub, values, nil)
			}
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		sub := rapid.SampledFrom(subs).Draw(t, "command")
		values := map[string][]string{}
		for _, flag := range slices.Sorted(maps.Keys(cliFlags[sub])) {
			n := rapid.IntRange(0, 2).Draw(t, flag+" count")
			if flag != "host" {
				n = min(n, 1)
			}
			for range n {
				values[flag] = append(values[flag], rapid.OneOf(rapid.SampledFrom(cliValues), rapid.Just(valid[flag])).Draw(t, flag))
			}
		}
		boxes := map[string]bool{}
		for flag := range cliBools[sub] {
			boxes[flag] = rapid.Bool().Draw(t, flag)
		}
		checkParity(t, sub, values, boxes)
	})
}
