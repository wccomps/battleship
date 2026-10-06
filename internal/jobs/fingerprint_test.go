package jobs

import (
	"reflect"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// fingerprinted says, for every field of the plan types, whether changing it
// must change the fingerprint; TestEveryPlanFieldIsClassified fails on a
// field not listed.
var fingerprinted = map[string]map[string]bool{
	"Plan": {
		"Kind": true, "Teams": true, "Templates": true, "Items": true, "Config": true,
	},
	"TemplateSpec": {
		"Name": true, "VMID": true, "Exists": true, "Rebuild": true, "Blocked": true,
		"WillStopMaster": true, "MasterVMID": true, "Interfaces": true, "GPU": true,
		// Derived from Name, or where things run, which may change with load.
		"Host": false, "MasterName": false, "Node": false, "OldNode": false, "MasterNode": false,
		// Only decide the privileges needed, which Blocked covers.
		"CPU": false, "CloudInit": false, "Bridges": false,
	},
	"Item": {
		"Name": true, "VMID": true, "Steps": true, "Blocked": true, "Snapshot": true, "Action": true, "Template": true,
		"Description": true, "VMState": true, // what a snapshot job writes to Proxmox
		// Derived from Name or Snapshot, or where the VM runs.
		"Team": false, "Host": false, "Node": false, "Baseline": false,
		// Says why Blocked is set, which Blocked already covers.
		"Unpermitted": false,
	},
}

func TestEveryPlanFieldIsClassified(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[pods.Plan](), reflect.TypeFor[pods.TemplateSpec](), reflect.TypeFor[pods.Item]()} {
		classes := fingerprinted[typ.Name()]
		for i := range typ.NumField() {
			f := typ.Field(i)
			if _, ok := classes[f.Name]; !ok {
				t.Errorf("%s.%s is not classified in fingerprinted", typ.Name(), f.Name)
			}
		}
		for name := range classes {
			if _, ok := typ.FieldByName(name); !ok {
				t.Errorf("fingerprinted lists %s.%s, which doesn't exist", typ.Name(), name)
			}
		}
	}
}

func changed(t *testing.T, v reflect.Value) reflect.Value {
	t.Helper()
	out := reflect.New(v.Type()).Elem()
	switch v.Kind() {
	case reflect.String:
		out.SetString(v.String() + "x")
	case reflect.Int:
		out.SetInt(v.Int() + 1)
	case reflect.Bool:
		out.SetBool(!v.Bool())
	case reflect.Slice:
		out = reflect.Append(reflect.MakeSlice(v.Type(), 0, v.Len()+1), reflect.New(v.Type().Elem()).Elem())
		out = reflect.AppendSlice(out, v)
	default:
		t.Fatalf("no way to change a %s", v.Type())
	}
	return out
}

func TestFingerprintFollowsClassification(t *testing.T) {
	base := func() *pods.Plan {
		return &pods.Plan{
			Kind: pods.KindDeploy, Teams: []string{"01"},
			Templates: []pods.TemplateSpec{{Name: "teak.x.tpl", Host: "teak", VMID: 9021, Node: "n1", MasterName: "teak.x",
				MasterVMID: 121, MasterNode: "n1", Interfaces: 2}},
			Items: []pods.Item{{Team: "01", Host: "teak", Name: "team01-teak", VMID: 10121, Node: "n1",
				Template: "teak.x.tpl", Steps: []pods.Step{pods.StepClone, pods.StepStart}}},
		}
	}
	fp := Fingerprint(base())
	check := func(where, field string, mut func(p *pods.Plan)) {
		p := base()
		mut(p)
		if got := Fingerprint(p) != fp; got != fingerprinted[where][field] {
			t.Errorf("changing %s.%s changed the fingerprint: %v, want %v", where, field, got, fingerprinted[where][field])
		}
	}
	for field := range fingerprinted["Plan"] {
		check("Plan", field, func(p *pods.Plan) {
			f := reflect.ValueOf(p).Elem().FieldByName(field)
			f.Set(changed(t, f))
		})
	}
	for field := range fingerprinted["TemplateSpec"] {
		check("TemplateSpec", field, func(p *pods.Plan) {
			f := reflect.ValueOf(&p.Templates[0]).Elem().FieldByName(field)
			f.Set(changed(t, f))
		})
	}
	for field := range fingerprinted["Item"] {
		check("Item", field, func(p *pods.Plan) {
			f := reflect.ValueOf(&p.Items[0]).Elem().FieldByName(field)
			f.Set(changed(t, f))
		})
	}
}

// A plan made under other settings for what a run does to VMs (see
// pods.ConfigHash) must not match.
func TestFingerprintCoversExecutionConfig(t *testing.T) {
	f := teamVMs()
	planUnder := func(cfg config.Config, in Inputs) string {
		p, err := BuildPlan(bg, pods.NewPlanner(f, cfg), in)
		if err != nil {
			t.Fatal(err)
		}
		return Fingerprint(p)
	}
	teardown := Inputs{Kind: pods.KindTeardown, Teams: "1"}
	base := planUnder(testCfg(), teardown)
	for name, mut := range map[string]func(*config.Config){
		"shutdown timeout": func(c *config.Config) { c.Teardown.ShutdownTimeout = 5 * time.Minute },
	} {
		cfg := testCfg()
		mut(&cfg)
		if planUnder(cfg, teardown) == base {
			t.Errorf("teardown: changing the %s didn't change the fingerprint", name)
		}
	}
	cfg := testCfg()
	cfg.Retry.Attempts = 9
	cfg.Concurrency.Workers = 3
	if planUnder(cfg, teardown) != base {
		t.Error("teardown: retry and concurrency settings changed the fingerprint")
	}

	power := Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}
	cfg = testCfg()
	cfg.Teardown.ShutdownTimeout = 5 * time.Minute
	if planUnder(testCfg(), power) != planUnder(cfg, power) {
		t.Error("power: the teardown shutdown timeout changed the fingerprint")
	}

	cfg = testCfg()
	cfg.Naming.Pool = "team-{team}"
	if Fingerprint(deployPlan(t, cfg)) == Fingerprint(deployPlan(t, testCfg())) {
		t.Error("deploy: changing the pool didn't change the fingerprint")
	}
}

func deployPlan(t *testing.T, cfg config.Config) *pods.Plan {
	t.Helper()
	f := twoTeams()
	f.vms[5001] = &proxmox.VM{VMID: 5001, Name: "dc.kilo.alpha", Node: "n1", Status: "stopped", Tags: "dev"}
	p, err := BuildPlan(bg, pods.NewPlanner(f, cfg), Inputs{Kind: pods.KindDeploy, Teams: "1", Pattern: "*.kilo.alpha"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
