package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/proxmox"
)

func tagged(id int, name, tags string) proxmox.VM {
	v := vm(id, name)
	v.Tags = tags
	return v
}

// twoSetPods is pods 01 and 02, each with a VM of kilo.alpha and one of
// tango.delta, and pod 01 one deployed before sets were tagged.
func twoSetPods() []proxmox.VM {
	return []proxmox.VM{
		tagged(10101, "team01-dc", "set-kilo.alpha"), tagged(10102, "team01-web", "dev;set-tango.delta"), vm(10103, "team01-old"),
		tagged(10201, "team02-dc", "set-kilo.alpha"), tagged(10202, "team02-web", "set-tango.delta"),
	}
}

// teardown -set deletes only that set's VMs, says so, and names the rest.
func TestTeardownSetFlag(t *testing.T) {
	e := newEnv(t, false, "", twoSetPods()...)
	if code := e.run("teardown", "-pods", "1-2", "-set", "Kilo.Alpha", "-yes"); code != 0 {
		t.Fatalf("code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	if got := slices.Sorted(slices.Values(e.api.Deleted)); !slices.Equal(got, []int{10101, 10201}) {
		t.Errorf("deleted %v, want kilo.alpha's", got)
	}
	out := e.stdout.String()
	for _, want := range []string{
		"Plan: teardown, pods 01,02\n",
		"Note: Not touched: team01-web and team02-web are of tango.delta.\n",
		"Note: Not touched: team01-old is of an unknown set, with no single set tag, which only a teardown that names no set removes.\n",
		"Tearing down set kilo.alpha only, keeping every VM whose one set tag isn't kilo.alpha's.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Without -set: everything, the untagged VM too; with -hosts, only matching VMs.
	e = newEnv(t, false, "", twoSetPods()...)
	if code := e.run("teardown", "-teams", "1", "-hosts", "web", "-yes"); code != 0 {
		t.Fatalf("code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	if !strings.Contains(e.stdout.String(), "Tearing down every matching VM in the pods.\n") {
		t.Errorf("output:\n%s", e.stdout)
	}
	e = newEnv(t, false, "", twoSetPods()...)
	if code := e.run("teardown", "-pods", "1", "-yes"); code != 0 {
		t.Fatalf("code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	if got := slices.Sorted(slices.Values(e.api.Deleted)); !slices.Equal(got, []int{10101, 10102, 10103}) {
		t.Errorf("deleted %v, want all of pod 01", got)
	}
	if !strings.Contains(e.stdout.String(), "Tearing down everything in the pods.\n") {
		t.Errorf("output:\n%s", e.stdout)
	}
}

func TestTeardownSetFlagRefusals(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"teardown", "-pods", "1", "-set", ""}, "-set is empty"},
		{[]string{"teardown", "-pods", "1", "-set", " "}, "-set is empty"},
		{[]string{"teardown", "-pods", "1", "-set", "*.kilo.alpha"}, `-set: set "*.kilo.alpha" is a pattern; give one set's name, e.g. kilo.alpha`},
		{[]string{"teardown", "-pods", "1", "-set", "*"}, "is a pattern"},
		{[]string{"teardown", "-pods", "all", "-set", "kilo.*"}, "is a pattern"},
		{[]string{"teardown", "-pods", "1", "-set", "kilo alpha"}, `-set: set "kilo alpha" isn't a set's name`},
		{[]string{"teardown", "-pods", "1", "-set", "\xff"}, "isn't a set's name"},
		{[]string{"teardown", "-pods", "1", "-templates", "*.kilo.alpha"}, "flag provided but not defined: -templates"},
		{[]string{"reset", "-pods", "1", "-set", "kilo.alpha"}, "flag provided but not defined: -set"},
	} {
		e := newEnv(t, false, "", twoSetPods()...)
		if code := e.run(tc.args...); code != 2 || !strings.Contains(e.stderr.String(), tc.want) || len(e.api.Deleted) != 0 {
			t.Errorf("%q: code %d, deleted %v, stderr %q; want 2 and %q", tc.args, code, e.api.Deleted, e.stderr, tc.want)
		}
	}
}
