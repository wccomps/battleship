package jobs

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// finishedJob is a stored job with in's inputs and plan's plan.
func finishedJob(t *testing.T, status string, in Inputs, plan pods.Plan) store.Job {
	t.Helper()
	rawIn, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	rawPlan, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return store.Job{ID: 7, Kind: string(in.Kind), Status: status, Inputs: rawIn, Plan: rawPlan}
}

func planItems(kind pods.Kind, names ...[2]string) pods.Plan {
	p := pods.Plan{Kind: kind}
	seen := map[string]bool{}
	for _, n := range names {
		team, host := n[0], n[1]
		p.Items = append(p.Items, pods.Item{Team: team, Host: host, Name: "team" + team + "-" + host})
		if !seen[team] {
			seen[team] = true
			p.Teams = append(p.Teams, team)
		}
	}
	return p
}

func TestRetryInputs(t *testing.T) {
	plan := planItems(pods.KindReset,
		[2]string{"01", "dc"}, [2]string{"01", "web"},
		[2]string{"02", "dc"}, [2]string{"02", "web"},
		[2]string{"03", "dc"}, [2]string{"03", "web"}, [2]string{"03", "ftp"},
	)
	orig := Inputs{Kind: pods.KindReset, Teams: "1-3", Snapshot: "initial"}
	items := []store.Item{
		{Name: "team01-dc", Team: "01", Status: store.ItemDone},
		{Name: "team01-web", Team: "01", Status: store.ItemFailed, Error: "rollback: boom"},
		{Name: "team02-dc", Team: "02", Status: store.ItemDone},
		{Name: "team02-web", Team: "02", Status: store.ItemDone},
		{Name: "team03-dc", Team: "03", Status: store.ItemBlocked, Error: "no snapshot"},
		{Name: "team03-web", Team: "03", Status: store.ItemDone},
		{Name: "team03-ftp", Team: "03", Status: store.ItemInterrupted, Step: "stop"},
	}
	r, err := RetryInputs(finishedJob(t, store.StatusCompletedWithFailures, orig, plan), items)
	if err != nil {
		t.Fatal(err)
	}
	want := Inputs{Kind: pods.KindReset, Teams: "1,3", Hosts: []string{"dc", "ftp", "web"}, Snapshot: "initial",
		VMs: []string{"team01-web", "team03-dc", "team03-ftp"}}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("inputs = %+v, want %+v", r, want)
	}
}

func TestRetryInputsCoversEveryUnfinishedStatus(t *testing.T) {
	plan := planItems(pods.KindPower,
		[2]string{"01", "a"}, [2]string{"01", "b"}, [2]string{"01", "c"}, [2]string{"01", "d"},
		[2]string{"01", "e"}, [2]string{"01", "f"}, [2]string{"01", "g"}, [2]string{"01", "h"},
	)
	orig := Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}
	items := []store.Item{
		{Name: "team01-a", Team: "01", Status: store.ItemDone},
		{Name: "team01-b", Team: "01", Status: store.ItemFailed},
		{Name: "team01-c", Team: "01", Status: store.ItemBlocked},
		{Name: "team01-d", Team: "01", Status: store.ItemInterrupted},
		{Name: "team01-e", Team: "01", Status: store.ItemRemoved},
		{Name: "team01-g", Team: "01", Status: store.ItemDone},
		{Name: "team01-h", Team: "01", Status: store.ItemNotRun},
	}
	r, err := RetryInputs(finishedJob(t, store.StatusInterrupted, orig, plan), items)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "c", "d", "e", "h"}; !reflect.DeepEqual(r.Hosts, want) {
		t.Errorf("hosts = %v, want %v", r.Hosts, want)
	}
	if want := []string{"team01-b", "team01-c", "team01-d", "team01-e", "team01-h"}; !reflect.DeepEqual(r.VMs, want) {
		t.Errorf("VMs = %v, want %v", r.VMs, want)
	}
	if r.Teams != "1" || r.Action != "start" {
		t.Errorf("inputs = %+v", r)
	}
}

// Freeing a gone VM's disks and a half-deleted VM have no host and are
// planned only by a whole-team teardown, so a retry that includes one asks
// every host and narrows by name.
func TestRetryInputsOfFreeDisksAsksEveryHost(t *testing.T) {
	plan := planItems(pods.KindTeardown, [2]string{"01", "dc"}, [2]string{"01", "web"})
	plan.Items = append(plan.Items,
		pods.Item{Team: "01", Name: "team01-disks-10121", VMID: 10121, Steps: []pods.Step{pods.StepFreeDisks}},
		pods.Item{Team: "01", Name: "team01-half-deleted-10122", VMID: 10122, Steps: []pods.Step{pods.StepDelete}, Blocked: "half-deleted"})
	orig := Inputs{Kind: pods.KindTeardown, Teams: "1"}
	items := []store.Item{
		{Name: "team01-dc", Team: "01", Status: store.ItemDone},
		{Name: "team01-web", Team: "01", Status: store.ItemFailed},
		{Name: "team01-disks-10121", Team: "01", Status: store.ItemFailed},
		{Name: "team01-half-deleted-10122", Team: "01", Status: store.ItemBlocked},
	}
	r, err := RetryInputs(finishedJob(t, store.StatusCompletedWithFailures, orig, plan), items)
	if err != nil {
		t.Fatal(err)
	}
	want := Inputs{Kind: pods.KindTeardown, Teams: "1", VMs: []string{"team01-disks-10121", "team01-half-deleted-10122", "team01-web"}}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("inputs = %+v, want %+v", r, want)
	}
}

func TestRetryInputsOfADeployDoesNotRebuild(t *testing.T) {
	plan := planItems(pods.KindDeploy, [2]string{"05", "dc"}, [2]string{"05", "web"})
	orig := Inputs{Kind: pods.KindDeploy, Teams: "5", Pattern: "*.kilo.alpha", Rebuild: true, NoSnapshot: true}
	items := []store.Item{
		{Name: "team05-dc", Team: "05", Status: store.ItemDone},
		{Name: "team05-web", Team: "05", Status: store.ItemFailed},
	}
	r, err := RetryInputs(finishedJob(t, store.StatusCompletedWithFailures, orig, plan), items)
	if err != nil {
		t.Fatal(err)
	}
	want := Inputs{Kind: pods.KindDeploy, Teams: "5", Hosts: []string{"web"}, Pattern: "*.kilo.alpha", NoSnapshot: true,
		VMs: []string{"team05-web"}}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("inputs = %+v, want %+v", r, want)
	}
}

// A retry of a reset must never roll back VMs that already succeeded: the
// failed VMs' teams × hosts include finished ones, and the plan leaves
// those out.
func TestRetryOfAResetTargetsOnlyTheFailedVMs(t *testing.T) {
	cfg := testCfg()
	p := pods.NewPlanner(twoTeams(), cfg)
	orig := Inputs{Kind: pods.KindReset, Teams: "1-2", Snapshot: "initial"}
	first, err := BuildPlan(bg, p, orig)
	if err != nil {
		t.Fatal(err)
	}
	items := []store.Item{
		{Name: "team01-dc", Team: "01", Status: store.ItemFailed},
		{Name: "team01-web", Team: "01", Status: store.ItemDone},
		{Name: "team02-dc", Team: "02", Status: store.ItemDone},
		{Name: "team02-web", Team: "02", Status: store.ItemFailed},
	}
	r, err := RetryInputs(finishedJob(t, store.StatusCompletedWithFailures, orig, *first), items)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(bg, p, r)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(plan); !reflect.DeepEqual(got, []string{"team01-dc", "team02-web"}) {
		t.Errorf("retry plan = %v, want exactly team01-dc and team02-web", got)
	}
}

func TestRetryInputsRefuses(t *testing.T) {
	plan := planItems(pods.KindPower, [2]string{"01", "dc"})
	in := Inputs{Kind: pods.KindPower, Teams: "1", Action: "stop"}
	done := []store.Item{{Name: "team01-dc", Team: "01", Status: store.ItemDone}}
	failed := []store.Item{{Name: "team01-dc", Team: "01", Status: store.ItemFailed}}

	for _, status := range []string{store.StatusPending, store.StatusRunning, store.StatusSucceeded, store.StatusStale, store.StatusFailed} {
		if _, err := RetryInputs(finishedJob(t, status, in, plan), failed); !errors.Is(err, ErrNotRetryable) {
			t.Errorf("%s job: err = %v, want ErrNotRetryable", status, err)
		}
	}
	for _, status := range []string{store.StatusCompletedWithFailures, store.StatusInterrupted, store.StatusCancelled} {
		if _, err := RetryInputs(finishedJob(t, status, in, plan), failed); err != nil {
			t.Errorf("%s job: %v", status, err)
		}
		if _, err := RetryInputs(finishedJob(t, status, in, plan), done); !errors.Is(err, ErrNothingToRetry) {
			t.Errorf("%s job with every VM done: err = %v, want ErrNothingToRetry", status, err)
		}
	}
	// A VM the stored plan doesn't have can't be narrowed to.
	odd := []store.Item{{Name: "team01-zz", Team: "01", Status: store.ItemFailed}}
	if _, err := RetryInputs(finishedJob(t, store.StatusCompletedWithFailures, in, plan), odd); err == nil {
		t.Error("item missing from the plan: no error")
	}
	bad := finishedJob(t, store.StatusCompletedWithFailures, in, plan)
	bad.Inputs = json.RawMessage(`{`)
	if _, err := RetryInputs(bad, failed); err == nil {
		t.Error("unreadable inputs: no error")
	}
}
