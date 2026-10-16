package jobs

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// finishedJob is a stored job with in's inputs and plan's plan.
func finishedJob(t interface {
	Helper()
	Fatal(...any)
}, status string, in Inputs, plan pods.Plan) store.Job {
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

// Hostless teardown items force a retry to ask every host, narrowed by name.
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

// A reset retry must never roll back VMs that already succeeded, though the
// failed VMs' teams × hosts include them.
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

var (
	retryableItems = []string{store.ItemFailed, store.ItemBlocked, store.ItemInterrupted, store.ItemRemoved, store.ItemNotRun}
	itemStatuses   = append([]string{store.ItemDone, store.ItemPending, store.ItemRunning, "", "weird"}, retryableItems...)
	retryableJobs  = []string{store.StatusCompletedWithFailures, store.StatusCancelled, store.StatusInterrupted}
	jobStatuses    = append([]string{store.StatusSucceeded, store.StatusFailed, store.StatusStale, store.StatusRunning, "weird"}, retryableJobs...)
)

// sortedSet is s sorted without repeats.
func sortedSet(s []string) []string {
	var out []string
	for _, v := range s {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

// checkRetry checks RetryInputs of a job of orig's inputs, plan, status
// and items: refused for a status that can't retry, nothing to retry, or an
// item the plan has no team VM for; else exactly the retryable VMs, their
// teams and hosts, in inputs that validate.
func checkRetry(t interface {
	Helper()
	Fatal(...any)
	Fatalf(string, ...any)
}, job store.Job, orig Inputs, plan pods.Plan, items []store.Item, wantErr bool) {
	var teams, hosts, names []string
	whole := false
	for _, it := range items {
		if !slices.Contains(retryableItems, it.Status) {
			continue
		}
		i := slices.IndexFunc(plan.Items, func(p pods.Item) bool { return p.Name == it.Name })
		switch {
		case i < 0 || it.Team == "" || plan.Items[i].Host == "" && plan.Kind != pods.KindTeardown:
			wantErr = true
		case plan.Items[i].Host == "":
			whole = true
		}
		teams, names = append(teams, it.Team), append(names, it.Name)
		if i >= 0 {
			hosts = append(hosts, plan.Items[i].Host)
		}
	}
	wantErr = wantErr || !slices.Contains(retryableJobs, job.Status) || len(names) == 0
	in, err := RetryInputs(job, items)
	if (err != nil) != wantErr {
		t.Fatalf("RetryInputs(%s job, %+v) = %+v, %v; want error %v", job.Status, items, in, err, wantErr)
	}
	if err != nil {
		return
	}
	wantHosts := sortedSet(hosts)
	if whole {
		wantHosts = nil
	}
	got, perr := pods.ParseTeams(in.Teams)
	if verr := in.Validate(); verr != nil || perr != nil || !slices.Equal(got, sortedSet(teams)) || !slices.Equal(in.VMs, sortedSet(names)) ||
		!slices.Equal(in.Hosts, wantHosts) || in.Rebuild {
		t.Fatalf("RetryInputs = %+v (%v); want teams %v, hosts %v, VMs %v", in, verr, sortedSet(teams), wantHosts, sortedSet(names))
	}
	for _, name := range in.VMs {
		if team, ok := teamVMModel(name); !ok || !slices.Contains(got, team) {
			t.Fatalf("retry VM %s is not a VM of teams %s", name, in.Teams)
		}
	}
	in.Teams, in.Hosts, in.VMs, in.Rebuild = orig.Teams, orig.Hosts, orig.VMs, orig.Rebuild
	if !reflect.DeepEqual(in, orig) {
		t.Fatalf("retry changed the job's other inputs: %+v, was %+v", in, orig)
	}
}

var retryKinds = []pods.Kind{pods.KindDeploy, pods.KindTeardown, pods.KindReset, pods.KindSnapshot, pods.KindPower}

func retryOrig(kind pods.Kind, rebuild bool) Inputs {
	orig := validInputs[slices.IndexFunc(validInputs, func(in Inputs) bool { return in.Kind == kind })]
	orig.Hosts, orig.VMs, orig.Rebuild = nil, nil, kind == pods.KindDeploy && rebuild
	return orig
}

// A retry asks for exactly the job's retryable VMs, their teams and hosts,
// in inputs that validate, or is refused with a reason.
func TestPropRetryInputs(t *testing.T) {
	for _, kind := range retryKinds {
		orig := retryOrig(kind, true)
		for _, status := range itemStatuses {
			for _, it := range []pods.Item{{Team: "01", Host: "dc", Name: "team01-dc"}, {Team: "01", Name: "team01-disks-10121"}} {
				plan := pods.Plan{Kind: kind, Items: []pods.Item{it, {Team: "02", Host: "dc", Name: "team02-dc"}}}
				items := []store.Item{{Name: it.Name, Team: "01", Status: status}, {Name: "team02-dc", Team: "02", Status: store.ItemFailed}}
				for _, job := range jobStatuses {
					checkRetry(t, finishedJob(t, job, orig, plan), orig, plan, items, false)
				}
			}
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.SampledFrom(retryKinds).Draw(t, "kind")
		orig := retryOrig(kind, rapid.Bool().Draw(t, "rebuild"))
		plan := pods.Plan{Kind: kind}
		var items []store.Item
		for _, team := range rapid.SliceOfNDistinct(rapid.SampledFrom([]string{"01", "02", "05", "17", "99"}), 1, 3, rapid.ID).Draw(t, "teams") {
			for _, host := range rapid.SliceOfNDistinct(rapid.SampledFrom([]string{"dc", "web", "ftp", ""}), 1, 3, rapid.ID).Draw(t, team+" hosts") {
				it := pods.Item{Team: team, Host: host, Name: "team" + team + "-" + host}
				if host == "" {
					it.Name = "team" + team + "-disks-1" + team + "21"
				}
				plan.Items = append(plan.Items, it)
				items = append(items, store.Item{Name: it.Name, Team: team, Status: rapid.SampledFrom(itemStatuses).Draw(t, it.Name)})
			}
		}
		if rapid.IntRange(0, 5).Draw(t, "stray") == 0 { // an item the plan lacks, or without a team
			stray := rapid.SampledFrom([]store.Item{{Name: "team09-ghost", Team: "09"}, {Name: items[0].Name}}).Draw(t, "stray item")
			stray.Status = rapid.SampledFrom(itemStatuses).Draw(t, "stray status")
			items = append(items, stray)
		}
		job := finishedJob(t, rapid.SampledFrom(jobStatuses).Draw(t, "status"), orig, plan)
		garbage := false
		switch rapid.IntRange(0, 9).Draw(t, "garbage") {
		case 0:
			job.Inputs, garbage = []byte("x"+rapid.String().Draw(t, "inputs json")), true
		case 1:
			job.Plan, garbage = []byte("x"+rapid.String().Draw(t, "plan json")), true
		}
		checkRetry(t, job, orig, plan, items, garbage)
	})
}
