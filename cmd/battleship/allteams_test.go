package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/proxmox"
)

// leftovers is a cluster after an event: teams 01-32, team 00, the test
// team, deployed with them, and a team past them.
func leftovers() []proxmox.VM {
	return []proxmox.VM{vm(10001, "team00-dc"), vm(10002, "team00-web"), vm(10101, "team01-dc"),
		vm(13201, "team32-dc"), vm(14001, "team40-dc"),
		{VMID: 9001, Name: "dc.kilo.alpha.tpl", Node: "n1", Template: true},
		vm(501, "dc.kilo.alpha")}
}

// -teams all is every team with VMs, resolved when planning and shown: a teardown of all teams leaves no team VM behind.
func TestTeardownAllTeams(t *testing.T) {
	e := newEnv(t, false, "", leftovers()...)
	if code := e.run("teardown", "-teams", "all", "-yes"); code != 0 {
		t.Fatalf("code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	out := e.stdout.String()
	if !strings.Contains(out, "-teams all is 0-1,32,40: the teams with VMs.\n") {
		t.Errorf("the resolved teams aren't shown:\n%s", out)
	}
	if !strings.Contains(out, "Plan: teardown, teams 00,01,32,40\n") {
		t.Errorf("plan line:\n%s", out)
	}
	deleted := slices.Sorted(slices.Values(e.api.deleted))
	if want := []int{10001, 10002, 10101, 13201, 14001}; !slices.Equal(deleted, want) {
		t.Errorf("deleted %v, want every team VM %v", deleted, want)
	}
}

// Asked to confirm, a teardown of all teams wants the resolved range typed,
// so whoever confirms has read it.
func TestInteractiveTeardownAllTeamsNeedsTheResolvedRange(t *testing.T) {
	e := newEnv(t, true, "all\n", leftovers()...)
	if code := e.run("teardown", "-teams", "all"); code != 1 || len(e.api.deleted) != 0 {
		t.Fatalf("typing all: code = %d, deleted %v\n%s", code, e.api.deleted, e.stdout)
	}
	if !strings.Contains(e.stdout.String(), "Type the team range (0-1,32,40) to delete these VMs: ") {
		t.Errorf("prompt:\n%s", e.stdout)
	}
	e = newEnv(t, true, "0-1,32,40\n", leftovers()...)
	if code := e.run("teardown", "-teams", "ALL"); code != 0 || len(e.api.deleted) != 5 {
		t.Fatalf("typing the range: code = %d, deleted %v\n%s%s", code, e.api.deleted, e.stdout, e.stderr)
	}
}

// Power, reset and snapshot take all too.
func TestOtherOperationsTakeAllTeams(t *testing.T) {
	e := newEnv(t, false, "", vm(10101, "team01-dc"), vm(10201, "team02-dc"))
	if code := e.run("power", "-teams", "all", "-action", "start"); code != 0 {
		t.Fatalf("power: code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	if out := e.stdout.String(); !strings.Contains(out, "-teams all is 1-2: the teams with VMs.\n") || !strings.Contains(out, "team02-dc") {
		t.Errorf("power -teams all:\n%s", out)
	}

	e = newEnv(t, false, "", leftovers()...)
	e.api.snapshots[10002] = []string{"initial"}
	if code := e.run("reset", "-teams", "all", "-hosts", "web"); code != 0 {
		t.Fatalf("reset: code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	if out := e.stdout.String(); !strings.Contains(out, "team00-web") {
		t.Errorf("reset -teams all -hosts web leaves out team00-web:\n%s", out)
	}

	e = newEnv(t, false, "", leftovers()...)
	if code := e.run("snapshot", "-teams", "all", "-name", "midday"); code != 0 {
		t.Fatalf("snapshot: code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	if out := e.stdout.String(); !strings.Contains(out, "team40-dc") {
		t.Errorf("snapshot -teams all leaves out team40-dc:\n%s", out)
	}
}

// With no team VMs, -teams all is no teams: an error, not an empty plan.
func TestAllTeamsWithNoTeamVMs(t *testing.T) {
	e := newEnv(t, false, "", vm(501, "dc.kilo.alpha"), proxmox.VM{VMID: 10101, Name: "team01-dc", Node: "n1", Template: true})
	if code := e.run("power", "-teams", "all", "-action", "start"); code != 1 {
		t.Fatalf("code = %d, want 1\n%s%s", code, e.stdout, e.stderr)
	}
	if got := e.stderr.String(); got != "-teams all: no team has VMs, so there are no teams to act on\n" {
		t.Errorf("stderr = %q", got)
	}
}

// A deploy makes teams, so it needs them named.
func TestDeployRefusesAllTeams(t *testing.T) {
	e := newEnv(t, false, "", leftovers()...)
	if code := e.run("deploy", "-teams", "all", "-templates", "*.kilo.alpha"); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(e.stderr.String(), "-teams all is for teardown, power, reset and snapshot") {
		t.Errorf("stderr = %q", e.stderr.String())
	}
}
