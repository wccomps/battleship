package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// fakeAPI is the shared fake cluster on n1, each VM with one disk, and
// deletes that fail as the CLI tests set. Its fields are guarded by the
// fake's Mu.
type fakeAPI struct {
	*podstest.Fake
	deleteErr map[int]error // fails every DeleteVM of these VMIDs
}

func newFakeAPI(vms ...proxmox.VM) *fakeAPI {
	f := &fakeAPI{Fake: podstest.New("n1"), deleteErr: map[int]error{}}
	for _, vm := range vms {
		f.Add(vm, map[string]string{"scsi0": fmt.Sprintf("competitions:%d/vm-%d-disk-0.qcow2", vm.VMID, vm.VMID)})
	}
	f.Gate = f.hold
	return f
}

// hold is the fake's Gate: it fails deletes in deleteErr.
func (f *fakeAPI) hold(_ context.Context, key string) (func(), error) {
	id, ok := strings.CutPrefix(key, "delete:")
	if !ok {
		return nil, nil
	}
	vmid, _ := strconv.Atoi(id)
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return nil, f.deleteErr[vmid]
}

// setSnapshots replaces a VM's snapshots.
func (f *fakeAPI) setSnapshots(vmid int, snaps ...string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.VMs[vmid].Snapshots = snaps
}

// rolled lists the VMIDs rolled back, in order.
func (f *fakeAPI) rolled() []int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var out []int
	for _, c := range f.Calls {
		if rest, ok := strings.CutPrefix(c, "rollback:"); ok {
			id, _, _ := strings.Cut(rest, ":")
			vmid, _ := strconv.Atoi(id)
			out = append(out, vmid)
		}
	}
	return out
}

type env struct {
	api            *fakeAPI
	d              deps
	stdout, stderr *bytes.Buffer
	cfg            string
}

func newEnv(t *testing.T, interactive bool, input string, vms ...proxmox.VM) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "battleship.toml")
	toml := "[proxmox]\nurl = \"https://pve.example:8006\"\ninsecure_skip_verify = true\n" + // the fake has no certificate
		"[retry]\nattempts = 1\nrounds = 0\n"
	if err := os.WriteFile(cfg, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BATTLESHIP_PROXMOX_TOKEN_ID", "tester@auth.example.org!cli") // the user's own token
	t.Setenv("BATTLESHIP_PROXMOX_TOKEN_SECRET", "secret")
	t.Setenv("BATTLESHIP_SEAL_KEY", testSealKey)
	t.Setenv("BATTLESHIP_DATABASE_URL", "") // direct runs stay in-process unless a test adds a store
	e := &env{api: newFakeAPI(vms...),
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, cfg: cfg}
	e.d = deps{
		newAPI:      func(config.Proxmox) pods.API { return e.api },
		in:          strings.NewReader(input),
		interactive: interactive,
		stdout:      e.stdout,
		stderr:      e.stderr,
	}
	return e
}

func (e *env) run(args ...string) int {
	return run(context.Background(), append(args, "-config", e.cfg), e.d)
}

func vm(id int, name string) proxmox.VM {
	return proxmox.VM{VMID: id, Name: name, Node: "n1", Status: "stopped"}
}

func TestRejectsStrayPositionalArgs(t *testing.T) {
	for _, args := range [][]string{
		{"reset", "-teams", "7", "-yes", "dc", "-hosts", "dc"},
		{"teardown", "-teams", "1", "extra"},
	} {
		e := newEnv(t, false, "", vm(10701, "team07-dc"))
		if code := e.run(args...); code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
		if !strings.Contains(e.stderr.String(), "unexpected argument") {
			t.Errorf("stderr = %q", e.stderr.String())
		}
		if len(e.api.Deleted)+len(e.api.rolled()) != 0 {
			t.Errorf("run(%v) changed something", args)
		}
	}
}

func TestEarlyValidationNeedsNoConfig(t *testing.T) {
	for _, args := range [][]string{
		{"deploy", "-teams", "1"},
		{"power", "-teams", "1"},
		{"power", "-teams", "1", "-action", "explode"},
		{"reset", "-teams", "1", "-hosts", " , "},
	} {
		var stdout, stderr bytes.Buffer
		d := deps{stdout: &stdout, stderr: &stderr}
		full := append(args, "-config", filepath.Join(t.TempDir(), "missing.toml"))
		if code := run(context.Background(), full, d); code != 2 {
			t.Errorf("run(%v) = %d, want 2 (stderr: %s)", args, code, stderr.String())
		}
	}
}

func TestHelpReturnsZero(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}, {"reset", "-h"}, {"teardown", "-help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, deps{stdout: &stdout, stderr: &stderr}); code != 0 {
			t.Errorf("run(%v) = %d, want 0", args, code)
		}
		if stdout.Len()+stderr.Len() == 0 {
			t.Errorf("run(%v) printed nothing", args)
		}
	}
}

func TestInteractiveTeardownNeedsTheRange(t *testing.T) {
	vms := []proxmox.VM{vm(10101, "team01-dc"), vm(10201, "team02-dc")}

	e := newEnv(t, true, " 1-2 \n", vms...)
	if code := e.run("teardown", "-teams", "1-2"); code != 0 {
		t.Fatalf("code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	if len(e.api.Deleted) != 2 {
		t.Errorf("deleted = %v", e.api.Deleted)
	}
	if !strings.Contains(e.stdout.String(), "Type the team range (1-2) to delete these VMs: ") {
		t.Errorf("no range prompt:\n%s", e.stdout)
	}

	e = newEnv(t, true, "yes\n", vms...)
	if code := e.run("teardown", "-teams", "1-2"); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if len(e.api.Deleted) != 0 {
		t.Errorf("deleted = %v", e.api.Deleted)
	}
	if !strings.Contains(e.stdout.String(), "Cancelled; nothing changed.") {
		t.Errorf("missing cancel message:\n%s", e.stdout)
	}

	e = newEnv(t, true, "", vms...) // EOF
	if code := e.run("teardown", "-teams", "1-2"); code != 1 || len(e.api.Deleted) != 0 {
		t.Errorf("EOF: code = %d, deleted = %v", code, e.api.Deleted)
	}
}

func TestInteractiveResetAcceptsYes(t *testing.T) {
	e := newEnv(t, true, "yes\n", vm(10701, "team07-dc"))
	e.api.setSnapshots(10701, "initial")
	if code := e.run("reset", "-teams", "7"); code != 0 {
		t.Fatalf("code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	if len(e.api.rolled()) != 1 {
		t.Errorf("rolled = %v", e.api.rolled())
	}
	if !strings.Contains(e.stdout.String(), "Apply this plan? Type yes: ") {
		t.Errorf("no prompt:\n%s", e.stdout)
	}

	e = newEnv(t, true, "y\n", vm(10701, "team07-dc"))
	e.api.setSnapshots(10701, "initial")
	if code := e.run("reset", "-teams", "7"); code != 1 || len(e.api.rolled()) != 0 {
		t.Errorf("code = %d, rolled = %v", code, e.api.rolled())
	}
}

func TestNonInteractiveWithoutYesChangesNothing(t *testing.T) {
	e := newEnv(t, false, "1\n", vm(10101, "team01-dc"))
	if code := e.run("teardown", "-teams", "1"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if len(e.api.Deleted) != 0 {
		t.Errorf("deleted = %v", e.api.Deleted)
	}
	if !strings.Contains(e.stdout.String(), "Nothing changed. Re-run with -yes to re-plan and apply.") {
		t.Errorf("stdout:\n%s", e.stdout)
	}
}

func TestYesExecutes(t *testing.T) {
	e := newEnv(t, false, "", vm(10101, "team01-dc"))
	if code := e.run("teardown", "-teams", "1", "-yes"); code != 0 || len(e.api.Deleted) != 1 {
		t.Errorf("code = %d, deleted = %v", code, e.api.Deleted)
	}
}

func TestAllBlockedExitsOne(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"))
	if code := e.run("reset", "-teams", "7", "-yes"); code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, e.stdout)
	}
	out := e.stdout.String()
	for _, want := range []string{"1 VMs blocked (see reasons above).", "Nothing can run."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestPartlyBlockedYesExitsOne(t *testing.T) {
	e := newEnv(t, false, "", vm(10701, "team07-dc"), vm(10702, "team07-web"))
	e.api.setSnapshots(10701, "initial")
	if code := e.run("reset", "-teams", "7", "-yes"); code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, e.stdout)
	}
	if len(e.api.rolled()) != 1 {
		t.Errorf("rolled = %v", e.api.rolled())
	}
}

func TestEmptyPlanIsNothingToDo(t *testing.T) {
	e := newEnv(t, false, "")
	if code := e.run("teardown", "-teams", "3", "-yes"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(e.stdout.String(), "Nothing to do.") {
		t.Errorf("stdout:\n%s", e.stdout)
	}
}

func TestFailuresPrintSorted(t *testing.T) {
	e := newEnv(t, false, "", vm(10201, "team02-dc"), vm(10101, "team01-dc"))
	e.api.deleteErr[10201] = errors.New("boom two")
	e.api.deleteErr[10101] = errors.New("boom one")
	if code := e.run("teardown", "-teams", "1-2", "-yes"); code != 1 {
		t.Fatalf("code = %d", code)
	}
	out := e.stdout.String()
	i, j := strings.Index(out, "team01-dc: "), strings.Index(out, "team02-dc: ")
	if i < 0 || j < 0 || i > j {
		t.Errorf("failures not sorted:\n%s", out)
	}
}

func TestPrintFailuresIndentsMessages(t *testing.T) {
	var buf bytes.Buffer
	printFailures(&buf, map[string]string{"b": "boom", "a": "first line\nsecond line"})
	if want := "\n2 failed:\n  a: first line\n      second line\n  b: boom\n"; buf.String() != want {
		t.Errorf("printed %q, want %q", buf.String(), want)
	}
}

func TestPrintPlan(t *testing.T) {
	var buf bytes.Buffer
	printPlan(&buf, pods.NewNaming(config.Default().Naming), &pods.Plan{
		Kind:  pods.KindDeploy,
		Teams: []string{"01"},
		Templates: []pods.TemplateSpec{
			{Name: "teak.x.tpl", VMID: 9021, Node: "cedar", Exists: true},
			{Name: "oak.x.tpl", VMID: 9025, Node: "birch", GPU: true},
		},
		Items: []pods.Item{
			{Team: "01", Name: "team01-teak", VMID: 10121, Node: "spruce", Steps: []pods.Step{pods.StepClone, pods.StepStart}},
			{Team: "01", Name: "team01-oak", VMID: 10125, Node: "birch", Blocked: "VMID 10125 is used by x"},
			{Name: "team02-oak", Blocked: "template oak.x.tpl: duplicate"},
		},
	})
	out := buf.String()
	for _, want := range []string{
		"Plan: deploy, teams 01",
		"teak.x.tpl  9021  cedar        reuse",
		"oak.x.tpl   9025  birch (GPU)  create",
		"VMs (3, 1 runnable):",
		"team01-teak  10121  spruce  pool-01  clone > start",
		"team01-oak   10125  birch   pool-01  BLOCKED: VMID 10125 is used by x",
		"team02-oak   -      -       -        BLOCKED: template oak.x.tpl: duplicate",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunRejectsBadArgs(t *testing.T) {
	cases := [][]string{
		{},
		{"explode"},
		{"teardown"},
		{"teardown", "-teams", "5-1"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, deps{stdout: &stdout, stderr: &stderr}); code != 2 {
			t.Errorf("run(%v) = %d, want 2 (stderr: %s)", args, code, stderr.String())
		}
	}
}

func TestTeardownConfirmTrimsRange(t *testing.T) {
	e := newEnv(t, true, "1-2\n", vm(10101, "team01-dc"), vm(10201, "team02-dc"))
	if code := e.run("teardown", "-teams", " 1-2 "); code != 0 || len(e.api.Deleted) != 2 {
		t.Fatalf("code = %d, deleted = %v\n%s%s", code, e.api.Deleted, e.stdout, e.stderr)
	}
	if !strings.Contains(e.stdout.String(), "Type the team range (1-2) to delete") {
		t.Errorf("prompt should show the trimmed range:\n%s", e.stdout)
	}
}

func TestCtrlCAtPromptCancels(t *testing.T) {
	e := newEnv(t, true, "yes\n", vm(10701, "team07-dc"))
	e.api.setSnapshots(10701, "initial")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code := run(ctx, []string{"reset", "-teams", "7", "-config", e.cfg}, e.d)
	if code != 1 || len(e.api.rolled()) != 0 {
		t.Errorf("code = %d, rolled = %v", code, e.api.rolled())
	}
	if !strings.Contains(e.stdout.String(), "Cancelled; nothing changed.") {
		t.Errorf("stdout:\n%s", e.stdout)
	}
}

func TestNoHostMatchIsAnError(t *testing.T) {
	e := newEnv(t, false, "", vm(10101, "team01-dc"))
	code := e.run("teardown", "-teams", "1", "-hosts", "dcc", "-yes")
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, e.stdout)
	}
	want := "No VMs matched -hosts dcc for teams 1; check the host names."
	if !strings.Contains(e.stdout.String(), want) {
		t.Errorf("missing %q:\n%s", want, e.stdout)
	}
}

func TestPrintPlanShowsMasterStop(t *testing.T) {
	var buf bytes.Buffer
	printPlan(&buf, pods.NewNaming(config.Default().Naming), &pods.Plan{
		Kind: pods.KindDeploy, Teams: []string{"01"},
		Templates: []pods.TemplateSpec{
			{Name: "teak.x.tpl", VMID: 9021, Node: "cedar", MasterName: "teak.x", WillStopMaster: true},
		},
	})
	if want := "create (stops master teak.x while building)"; !strings.Contains(buf.String(), want) {
		t.Errorf("missing %q:\n%s", want, buf.String())
	}
}

func TestPrintCleanupSummary(t *testing.T) {
	var out bytes.Buffer
	printResult(&out, apply.Result{
		Completed:     []string{"fern.kilo.alpha.tpl"},
		Removed:       []string{"team01-dc", "team02-dc"},
		AlreadyGone:   []string{"team04-dc"},
		CleanupFailed: map[string]error{"team03-dc": errors.New("still locked (lock: backup)")},
	})
	want := "\nCompleted: fern.kilo.alpha.tpl\n" +
		"\nRemoved half-built VMs: team01-dc, team02-dc\n" +
		"\nAlready gone: team04-dc\n" +
		"Could not remove team03-dc: still locked (lock: backup); remove it in Proxmox.\n"
	if got := out.String(); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	out.Reset()
	printResult(&out, apply.Result{})
	if out.Len() != 0 {
		t.Errorf("empty result printed %q", out.String())
	}
}

// -vms acts on exactly the named VMs, e.g. to retry a job's failed ones.
func TestVMsFlagTargetsExactVMs(t *testing.T) {
	e := newEnv(t, false, "", vm(10101, "team01-dc"), vm(10102, "team01-web"), vm(10201, "team02-dc"), vm(10202, "team02-web"))
	for _, id := range []int{10101, 10102, 10201, 10202} {
		e.api.setSnapshots(id, "initial")
	}
	if code := e.run("reset", "-teams", "1-2", "-vms", "team01-dc, team02-web", "-yes"); code != 0 {
		t.Fatalf("code = %d\n%s%s", code, e.stdout, e.stderr)
	}
	if got := fmt.Sprint(e.api.rolled()); got != "[10101 10202]" && got != "[10202 10101]" {
		t.Errorf("rolled back %v, want exactly 10101 and 10202", got)
	}
	if strings.Contains(e.stdout.String(), "team01-web") || strings.Contains(e.stdout.String(), "team02-dc") {
		t.Errorf("the plan shows VMs outside -vms:\n%s", e.stdout)
	}
}

func TestVMsFlagChecksNames(t *testing.T) {
	e := newEnv(t, false, "", vm(10101, "team01-dc"))
	if code := e.run("teardown", "-teams", "1", "-vms", "team02-dc", "-yes"); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(e.stderr.String(), "team02-dc is not in teams 1") {
		t.Errorf("stderr:\n%s", e.stderr)
	}
	if len(e.api.Deleted) != 0 {
		t.Errorf("deleted %v", e.api.Deleted)
	}
	e = newEnv(t, false, "", vm(10101, "team01-dc"))
	if code := e.run("teardown", "-teams", "1", "-vms", " , ", "-yes"); code != 2 {
		t.Errorf("empty -vms: code = %d, want 2", code)
	}
}

// A stored summary lists interrupted VMs apart from failed ones, so the
// counts match the items' statuses.
func TestPrintSummaryListsInterrupted(t *testing.T) {
	var buf bytes.Buffer
	printSummary(&buf, jobs.Summary{
		Failed:      map[string]string{"team01-dc": "the app's Proxmox token is missing VM.Allocate on /vms/10101"},
		Interrupted: []string{"team01-web", "team02-dc"},
	})
	out := buf.String()
	for _, want := range []string{
		"1 failed:\n  team01-dc: the app's Proxmox token",
		"2 VMs interrupted (job cancelled or stopped early): team01-web, team02-dc; re-run the same command to finish.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	buf.Reset()
	printSummary(&buf, jobs.Summary{Interrupted: []string{"team01-web"}})
	if strings.Contains(buf.String(), "failed") {
		t.Errorf("interrupted-only summary mentions failures:\n%s", buf.String())
	}
}

// A direct run without a database lists the VMs a stop cut off as
// interrupted, as a stored job's summary does, not as failures.
func TestPrintResultListsInterrupted(t *testing.T) {
	var buf bytes.Buffer
	printResult(&buf, apply.Result{
		Failed: map[string]error{"team01-dc": errors.New("the app's Proxmox token is missing VM.Allocate on /vms/10101")},
		Interrupted: map[string]error{
			"team01-web": fmt.Errorf("clone: %w", context.Canceled),
			"team02-dc":  context.Canceled,
		},
	})
	out := buf.String()
	for _, want := range []string{
		"1 failed:\n  team01-dc: the app's Proxmox token",
		"2 VMs interrupted (job cancelled or stopped early): team01-web, team02-dc; re-run the same command to finish.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "context canceled") {
		t.Errorf("interrupted VMs are listed as failures:\n%s", out)
	}
	buf.Reset()
	printResult(&buf, apply.Result{Interrupted: map[string]error{"team01-web": context.Canceled}})
	if strings.Contains(buf.String(), "failed") || !strings.Contains(buf.String(), "1 VM interrupted") {
		t.Errorf("interrupted-only result:\n%s", buf.String())
	}
}

// proxmox.ca_file reaches the client: it verifies Proxmox's certificate
// against the bundle.
func TestProxmoxCAFileVerifiesTheCluster(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "pve-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "battleship.toml")
	toml := "[proxmox]\nurl = \"" + srv.URL + "\"\nca_file = \"" + ca + "\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := asUser(newProxmoxAPI(cfg.Proxmox), func() proxmox.Credential { return proxmox.TokenCredential("u@pve!t", "s") }, nil).ClusterVMs(context.Background()); err != nil {
		t.Errorf("with proxmox.ca_file: %v", err)
	}
}
