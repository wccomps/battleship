package pods

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/proxmox"
)

// checkFake is envFake with kilo.alpha in dev-01: router gw, Linux teak at
// .10 and Windows oak at .20.
func checkFake() Planner {
	f := envFake()
	add := func(vmid int, name, tags, desc string) {
		f.Add(proxmox.VM{VMID: vmid, Name: name, Node: "alder", Pool: "dev-01", Tags: tags, Status: "running"},
			map[string]string{"description": desc})
	}
	add(2100, "gw.kilo.alpha", "dev;kilo.alpha;router", "battleship: base=gw-v1 dns=router")
	add(2101, "teak.kilo.alpha", "dev;kilo.alpha", "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=router")
	add(2102, "oak.kilo.alpha", "dev;kilo.alpha", "battleship: ip=192.168.100.20 os=windows base=win2022-v1 dns=router")
	return NewPlanner(f, envConfig())
}

// What the form is told before planning is what planning then says: each
// problem CheckEnv finds is the refusal EnvCreate or EnvAdd returns, or
// the reason its host's item is blocked, and planning blocks no host
// CheckEnv passed. Host names, images and addresses are typed by anyone.
func TestPropCheckEnvAgreesWithThePlanner(t *testing.T) {
	p := checkFake()
	ctx := context.Background()
	envs, vms, err := p.Environments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.SampledFrom([]Kind{KindEnvCreate, KindEnvAdd}).Draw(t, "kind")
		req := EnvRequest{Domain: rapid.SampledFrom([]string{"kilo.alpha", "kilo.bravo"}).Draw(t, "domain")}
		if kind == KindEnvCreate {
			req.Slot = rapid.IntRange(0, 4).Draw(t, "slot")
		}
		spec := func(label string) HostSpec {
			return HostSpec{
				Host:   rapid.SampledFrom([]string{"gw", "teak", "oak", "dc", "web", "workstation-0001"}).Draw(t, label+" name"),
				Family: rapid.SampledFrom([]string{"gw", "debian-12", "win2022", "nope"}).Draw(t, label+" image"),
				Octet:  rapid.SampledFrom([]int{0, 0, 10, 20, 30}).Draw(t, label+" octet"),
			}
		}
		if kind == KindEnvCreate || rapid.Bool().Draw(t, "router") {
			req.Router = spec("router")
		}
		for i := range rapid.IntRange(0, 4).Draw(t, "hosts") {
			req.Hosts = append(req.Hosts, spec(fmt.Sprint("host ", i)))
		}
		if kind == KindEnvAdd && req.Router.Host == "" && len(req.Hosts) == 0 {
			req.Hosts = append(req.Hosts, spec("host"))
		}
		problems := CheckEnv(kind, envs, nil, BaseImages(vms), req)
		plan, err := p.EnvCreate(ctx, req)
		if kind == KindEnvAdd {
			plan, err = p.EnvAdd(ctx, req)
		}
		if err != nil {
			if !slices.ContainsFunc(problems, func(pr EnvProblem) bool { return pr.Why == err.Error() }) {
				t.Fatalf("planning refused %q; CheckEnv found %+v", err, problems)
			}
			return
		}
		rows := map[int]string{}
		for _, pr := range problems {
			if pr.Field == FieldDomain || pr.Field == FieldSlot {
				t.Fatalf("CheckEnv refused %+v; planning didn't", pr)
			}
			rows[pr.Row] = pr.Why
		}
		first := 1 // the plan's first item's row
		if req.Router.Host != "" {
			first = 0
		}
		for i, it := range plan.Items {
			if it.Blocked != rows[i+first] {
				t.Fatalf("%s: blocked %q, CheckEnv said %q (problems %+v)", it.Name, it.Blocked, rows[i+first], problems)
			}
		}
	})
}

// Naming a host already there resumes it, as does creating a domain in its
// own slot: neither is a problem. Another slot, or a slot in use, is.
func TestCheckEnvResumes(t *testing.T) {
	p := checkFake()
	envs, vms, err := p.Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	images := BaseImages(vms)
	teak := HostSpec{Host: "teak", Family: "debian-12"}
	gw := HostSpec{Host: "gw", Family: "gw"}
	for _, c := range []struct {
		kind Kind
		req  EnvRequest
		want []EnvProblem
	}{
		{KindEnvAdd, EnvRequest{Domain: "kilo.alpha", Hosts: []HostSpec{teak}}, nil},
		{KindEnvAdd, EnvRequest{Domain: "kilo.alpha", Hosts: []HostSpec{{Host: "teak", Family: "debian-12", Octet: 20}}}, nil}, // it keeps its .10
		{KindEnvCreate, EnvRequest{Domain: "kilo.alpha", Slot: 1, Router: gw, Hosts: []HostSpec{teak}}, nil},
		{KindEnvCreate, EnvRequest{Domain: "kilo.alpha", Slot: 2, Router: gw}, []EnvProblem{{Field: FieldDomain, Why: "kilo.alpha is already in dev slot 01; add hosts to it instead"}}},
		{KindEnvCreate, EnvRequest{Domain: "kilo.bravo", Slot: 1, Router: gw}, []EnvProblem{{Field: FieldSlot, Why: "dev slot 01 is in use; choose a free one"}}},
		{KindEnvCreate, EnvRequest{Domain: "kilo.bravo", Slot: 9, Router: gw}, []EnvProblem{{Field: FieldSlot, Why: "there's no dev slot 09"}}},
		{KindEnvCreate, EnvRequest{Domain: "kilo.bravo", Slot: 2, Router: gw, Hosts: []HostSpec{teak, {Host: "web", Family: "debian-12", Octet: 10}, teak}},
			[]EnvProblem{{Row: 3, Field: FieldName, Why: "named twice"}}},
		{KindEnvAdd, EnvRequest{Domain: "kilo.alpha", Hosts: []HostSpec{{Host: "web", Family: "debian-12", Octet: 10}, {Host: "oak", Family: "debian-12"}}},
			[]EnvProblem{{Row: 1, Field: FieldAddress, Why: "address .10 is already taken in kilo.alpha"},
				{Row: 2, Field: FieldImage, Why: "oak.kilo.alpha already exists as a different kind of host than debian-12 (linux)"}}},
		{KindEnvAdd, EnvRequest{Domain: "kilo.alpha", Router: HostSpec{Host: "fw", Family: "gw"}, Hosts: []HostSpec{{Host: "workstation-0001", Family: "win2022"}}},
			[]EnvProblem{{Field: FieldName, Why: "kilo.alpha already has a router"},
				{Row: 1, Field: FieldName, Why: "Windows host names are at most 15 characters"}}},
		{KindEnvAdd, EnvRequest{Domain: "kilo.zulu", Hosts: []HostSpec{teak}}, []EnvProblem{{Field: FieldDomain, Why: "no dev slot holds kilo.zulu; create it first"}}},
		{KindEnvAdd, EnvRequest{Domain: "kilo.alpha", Hosts: []HostSpec{{Host: "fw", Family: "gw"}}}, []EnvProblem{{Row: 1, Field: FieldImage, Why: "an environment has one router"}}},
		{KindEnvCreate, EnvRequest{Domain: "kilo.bravo", Slot: 2, Router: gw, Hosts: []HostSpec{{Host: "fw", Family: "gw"}}},
			[]EnvProblem{{Row: 1, Field: FieldImage, Why: "an environment has one router"}}},
	} {
		got := CheckEnv(c.kind, envs, nil, images, c.req)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s %+v:\n got %+v\nwant %+v", c.kind, c.req, got, c.want)
		}
	}
	// A slot a queued job will fill isn't free.
	queued := []Slot{envs[1].Slot}
	if got := CheckEnv(KindEnvCreate, envs, queued, images, EnvRequest{Domain: "kilo.bravo", Slot: 2, Router: gw}); !slices.Equal(got,
		[]EnvProblem{{Field: FieldSlot, Why: "a queued job will fill dev slot 02; choose another"}}) {
		t.Errorf("queued slot: %+v", got)
	}
	queued = []Slot{envs[1].Slot, envs[2].Slot}
	if got := CheckEnv(KindEnvCreate, envs, queued, images, EnvRequest{Domain: "kilo.bravo", Router: gw}); !slices.Equal(got,
		[]EnvProblem{{Field: FieldSlot, Why: "every dev slot is in use; archive an environment to free one"}}) {
		t.Errorf("no free slot: %+v", got)
	}
}
