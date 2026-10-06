package main

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/proxmox"
)

func (f *fakeAPI) CreateSnapshot(_ context.Context, _ string, vmid int, r proxmox.SnapshotRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots[vmid] = append(f.snapshots[vmid], r.Name)
	f.snapReqs = append(f.snapReqs, r)
	return "UPID:x", nil
}

func TestSnapshotCommand(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"), vm(10702, "team07-web"))
	e.api.snapshots[10702] = []string{"initial", "round2"}
	code := e.run("snapshot", "-teams", "7", "-name", "round2", "-description", "after lunch", "-vmstate", "-yes")
	if code != 1 { // team07-web is blocked
		t.Fatalf("code = %d, want 1 (one VM blocked)\n%s%s", code, e.stdout, e.stderr)
	}
	out := e.stdout.String()
	for _, want := range []string{
		"Plan: snapshot, teams 07",
		"team07-dc   10701  n1  snapshot round2 with RAM",
		`team07-web  10702  n1  BLOCKED: already has a snapshot named "round2"; choose another name`,
		"1 VMs blocked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	want := []proxmox.SnapshotRequest{{Name: "round2", Description: "after lunch", VMState: true}}
	if !slices.Equal(e.api.snapReqs, want) {
		t.Errorf("requests = %+v, want %+v", e.api.snapReqs, want)
	}

	// Interactive, it asks first.
	e = newEnv(t, true, "yes\n", vm(10701, "team07-dc"))
	if code := e.run("snapshot", "-teams", "7", "-hosts", "dc", "-name", "round3"); code != 0 {
		t.Fatalf("code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	if !strings.Contains(e.stdout.String(), "Apply this plan? Type yes: ") || len(e.api.snapReqs) != 1 || e.api.snapReqs[0].VMState {
		t.Errorf("requests = %+v\n%s", e.api.snapReqs, e.stdout)
	}
}

func TestSnapshotCommandChecksTheName(t *testing.T) {
	for args, want := range map[string]string{
		"snapshot -teams 1":                   "-name is required",
		"snapshot -teams 1 -name 1bad":        "must start with a letter",
		"snapshot -teams 1 -name current":     "reserved",
		"snapshot -teams 1 -name ok2 -vms , ": "-vms is empty",
	} {
		var stdout, stderr bytes.Buffer
		full := append(strings.Fields(args), "-config", filepath.Join(t.TempDir(), "missing.toml"))
		if code := run(context.Background(), full, deps{stdout: &stdout, stderr: &stderr}); code != 2 || !strings.Contains(stderr.String(), want) {
			t.Errorf("run(%s) = %d, stderr %q; want 2 and %q", args, code, stderr.String(), want)
		}
	}
	// The baseline's name is refused once the config says what it is.
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	if code := e.run("snapshot", "-teams", "7", "-name", "initial", "-yes"); code != 1 || !strings.Contains(e.stderr.String(), "is the baseline snapshot a deploy takes") {
		t.Errorf("code = %d, stderr = %q", code, e.stderr)
	}
	if len(e.api.snapReqs) != 0 {
		t.Errorf("requests = %+v", e.api.snapReqs)
	}
}

func TestDescribeSnapshotJob(t *testing.T) {
	got := describeInputs([]byte(`{"kind":"snapshot","teams":"7","hosts":["dc"],"snapshot":"round2"}`))
	if got != "round2 teams 7 hosts dc" {
		t.Errorf("describeInputs = %q", got)
	}
}
