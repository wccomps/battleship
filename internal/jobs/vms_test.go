package jobs

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// twoTeams is teams 01 and 02, each with dc and web.
func twoTeams() *fakeAPI {
	return newFake(
		proxmox.VM{VMID: 10101, Name: "team01-dc", Node: "n1", Status: "running"},
		proxmox.VM{VMID: 10102, Name: "team01-web", Node: "n1", Status: "running"},
		proxmox.VM{VMID: 10201, Name: "team02-dc", Node: "n1", Status: "running"},
		proxmox.VM{VMID: 10202, Name: "team02-web", Node: "n1", Status: "running"},
	)
}

func itemNames(p *pods.Plan) []string {
	var out []string
	for _, it := range p.Items {
		out = append(out, it.Name)
	}
	return out
}

func TestBuildPlanKeepsOnlyNamedVMs(t *testing.T) {
	cfg := testCfg()
	p := pods.NewPlanner(twoTeams(), cfg)
	for _, kind := range []pods.Kind{pods.KindReset, pods.KindPower, pods.KindTeardown} {
		in := Inputs{Kind: kind, Teams: "1-2", Action: "stop", VMs: []string{"team02-web", "team01-dc"}}
		plan, err := BuildPlan(bg, p, in)
		if err != nil {
			t.Fatal(err)
		}
		if got := itemNames(plan); !reflect.DeepEqual(got, []string{"team01-dc", "team02-web"}) {
			t.Errorf("%s items = %v, want exactly team01-dc and team02-web", kind, got)
		}
	}
	// A named VM the plan doesn't produce (gone, or outside the hosts) is
	// left out, not an error.
	in := Inputs{Kind: pods.KindReset, Teams: "1-2", Hosts: []string{"dc"}, VMs: []string{"team01-dc", "team02-web", "team02-ftp"}}
	plan, err := BuildPlan(bg, p, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(plan); !reflect.DeepEqual(got, []string{"team01-dc"}) {
		t.Errorf("items = %v, want team01-dc", got)
	}
	if n := MissingVMs(in, plan); !reflect.DeepEqual(n, []string{"team02-ftp", "team02-web"}) {
		t.Errorf("MissingVMs = %v", n)
	}
}

func TestBuildPlanChecksVMNames(t *testing.T) {
	cfg := testCfg()
	p := pods.NewPlanner(twoTeams(), cfg)
	for _, tc := range []struct {
		vms  []string
		want string
	}{
		{[]string{"team03-dc"}, "team03-dc is not in teams 1-2"},
		{[]string{"dc.kilo.alpha"}, `"dc.kilo.alpha" is not a team VM name`},
		{[]string{"team1-dc"}, `"team1-dc" is not a team VM name`},
	} {
		_, err := BuildPlan(bg, p, Inputs{Kind: pods.KindReset, Teams: "1-2", VMs: tc.vms})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("VMs %v: err = %v, want %q", tc.vms, err, tc.want)
		}
	}
	if err := (Inputs{Kind: pods.KindReset, Teams: "1", VMs: []string{"team01-dc", " "}}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "vms must not contain blank entries") {
		t.Errorf("blank VM: err = %v", err)
	}
}

// Narrowing a deploy to some VMs keeps only their templates, so the job
// locks and fingerprints just what it touches.
func TestBuildPlanNarrowsDeployTemplates(t *testing.T) {
	cfg := testCfg()
	f := twoTeams()
	for _, vm := range []proxmox.VM{
		{VMID: 5001, Name: "dc.kilo.alpha", Node: "n1", Status: "stopped", Tags: "dev"},
		{VMID: 5002, Name: "web.kilo.alpha", Node: "n1", Status: "stopped", Tags: "dev"},
	} {
		f.addVM(vm)
	}
	p := pods.NewPlanner(f, cfg)
	all, err := BuildPlan(bg, p, Inputs{Kind: pods.KindDeploy, Teams: "1-2", Pattern: "*.kilo.alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Templates) != 2 {
		t.Fatalf("templates = %+v, want 2", all.Templates)
	}
	some, err := BuildPlan(bg, p, Inputs{Kind: pods.KindDeploy, Teams: "1-2", Pattern: "*.kilo.alpha", VMs: []string{"team02-web"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(some); !reflect.DeepEqual(got, []string{"team02-web"}) {
		t.Errorf("items = %v", got)
	}
	if len(some.Templates) != 1 || some.Templates[0].Name != "web.kilo.alpha.tpl" {
		t.Errorf("templates = %+v, want only web.kilo.alpha.tpl", some.Templates)
	}
	if got := LockKeys(some); !reflect.DeepEqual(got, []string{"team:01", "team:02", "template:web.kilo.alpha.tpl", "vmid:10202", "vmid:5002", "vmid:9002"}) {
		t.Errorf("lock keys = %v", got)
	}
	if Fingerprint(some) == Fingerprint(all) {
		t.Error("narrowed deploy has the full deploy's fingerprint")
	}
}

// Two plans of the same inputs but for different VMs never share a
// fingerprint, so a confirm can't swap one for the other.
func TestFingerprintCoversVMs(t *testing.T) {
	cfg := testCfg()
	p := pods.NewPlanner(twoTeams(), cfg)
	fps := map[string]string{}
	for _, vms := range [][]string{nil, {"team01-dc"}, {"team01-web"}, {"team01-dc", "team01-web", "team02-dc"}} {
		plan, err := BuildPlan(bg, p, Inputs{Kind: pods.KindReset, Teams: "1-2", VMs: vms})
		if err != nil {
			t.Fatal(err)
		}
		fp := Fingerprint(plan)
		if other, ok := fps[fp]; ok {
			t.Errorf("VMs %v and %s have the same fingerprint", vms, other)
		}
		fps[fp] = strings.Join(vms, ",")
	}
}

// teamVMModel reads the default naming, team{team}-{host}: two ASCII
// digits and a host of one line.
func teamVMModel(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "team")
	if !ok || len(rest) < 4 || rest[2] != '-' || strings.Contains(rest, "\n") ||
		rest[0] < '0' || rest[0] > '9' || rest[1] < '0' || rest[1] > '9' {
		return "", false
	}
	return rest[:2], true
}

// narrowNames are team VM names, near misses and names of other teams.
var narrowNames = []string{"team01-dc", "team01-web", "team02-dc", "team02-web", "team03-web", "team03-dc", "team01-nope",
	"team1-dc", "TEAM01-dc", "team01-dc ", " team01-dc", "team100-dc", "team01-", "team01", "dc", "dc.kilo.alpha", "team01-dc,team02-dc",
	"team0\u0661-dc", "team01-dc.kilo", "team01-d", "team02-w", "team01-dcx", "team01-DC"}

// narrowCluster is twoTeams, team 03's web and a master dc.
func narrowCluster() pods.Planner {
	f := twoTeams()
	f.addVM(proxmox.VM{VMID: 10302, Name: "team03-web", Node: "n1", Status: "running"})
	f.addVM(proxmox.VM{VMID: 5001, Name: "dc.kilo.alpha", Node: "n1", Status: "stopped", Tags: "dev"})
	return pods.NewPlanner(f, testCfg())
}

func checkNarrowing(t interface{ Fatalf(string, ...any) }, p pods.Planner, in Inputs, teams []string) {
	wantErr := false
	for _, name := range in.VMs {
		team, ok := teamVMModel(name)
		wantErr = wantErr || !ok || !slices.Contains(teams, team)
	}
	plan, err := BuildPlan(bg, p, in)
	if (err != nil) != wantErr {
		t.Fatalf("BuildPlan(%s, teams %q, vms %q) = %v, want error %v", in.Kind, in.Teams, in.VMs, err, wantErr)
	}
	if err != nil || len(in.VMs) == 0 {
		return
	}
	// What the kind plans without VMs: the teams' VMs, or a deploy's dc.
	var want []string
	for _, name := range in.VMs {
		team, _ := teamVMModel(name)
		exists := slices.Contains([]string{"team01-dc", "team01-web", "team02-dc", "team02-web", "team03-web"}, name)
		if in.Kind == pods.KindDeploy {
			exists = name == "team"+team+"-dc"
		}
		if exists && !slices.Contains(want, name) {
			want = append(want, name)
		}
	}
	got := itemNames(plan)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s of teams %q narrowed to %q: items %q, want %q", in.Kind, in.Teams, in.VMs, got, want)
	}
	var missing []string
	for _, name := range in.VMs {
		if !slices.Contains(want, name) && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	if m := MissingVMs(in, plan); !slices.Equal(m, missing) {
		t.Fatalf("MissingVMs(%q) = %q, want %q", in.VMs, m, missing)
	}
}

// Naming VMs narrows a plan to exactly those VMs of its teams; a name that
// isn't a VM of the teams is refused.
func TestPropVMsNarrowWithinTeams(t *testing.T) {
	p := narrowCluster()
	kinds := []pods.Kind{pods.KindReset, pods.KindPower, pods.KindTeardown, pods.KindSnapshot, pods.KindDeploy}
	inputs := func(kind pods.Kind, teams string, vms []string) Inputs {
		return Inputs{Kind: kind, Teams: teams, VMs: vms, Action: "stop", Snapshot: "round2", Pattern: "dc.kilo.alpha"}
	}
	for _, kind := range kinds {
		for _, name := range narrowNames {
			checkNarrowing(t, p, inputs(kind, "1-2", []string{name}), []string{"01", "02"})
			checkNarrowing(t, p, inputs(kind, "1-2", []string{"team01-dc", name, name}), []string{"01", "02"})
		}
	}
	specs := map[string][]string{"1": {"01"}, "2": {"02"}, "1-2": {"01", "02"}, "1-3": {"01", "02", "03"}, "3,1": {"01", "03"}}
	rapid.Check(t, func(t *rapid.T) {
		spec := rapid.SampledFrom([]string{"1", "2", "1-2", "1-3", "3,1"}).Draw(t, "teams")
		name := rapid.OneOf(rapid.SampledFrom(narrowNames[:7]), rapid.SampledFrom(narrowNames), rapid.StringMatching(`team[0-9]{1,3}-[a-z]{1,3}`))
		vms := rapid.SliceOfN(name, 0, 4).Draw(t, "vms")
		if len(vms) == 0 {
			vms = nil
		}
		checkNarrowing(t, p, inputs(rapid.SampledFrom(kinds).Draw(t, "kind"), spec, vms), specs[spec])
	})
}
