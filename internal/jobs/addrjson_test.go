package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// addrCluster has base images, kilo.alpha in dev slot 01 (a router, hosts
// with and without an address, one whose notes name a DNS host that serves
// none) and dev slot 02 free.
func addrCluster() *podstest.Fake {
	f := podstest.New("alder", "birch")
	f.Add(proxmox.VM{VMID: 8000, Name: "gw-v1", Node: "alder", Template: true, Tags: "base;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:80:00,bridge=vmbr0", "net1": "virtio=BC:24:11:00:80:01,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v1", Node: "alder", Template: true, Tags: "base;linux"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:80:02,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 8002, Name: "win2022-v1", Node: "birch", Template: true, Tags: "base;windows"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:80:03,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 2100, Name: "gw.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha;router", Status: "running"},
		map[string]string{"net0": "virtio=BC:24:11:00:21:00,bridge=devext01", "net1": "virtio=BC:24:11:00:21:01,bridge=devint01",
			"description": "battleship: base=gw-v1 dns=router"})
	f.Add(proxmox.VM{VMID: 2101, Name: "oak.kilo.alpha", Node: "birch", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:21:02,bridge=devint01",
			"description": "battleship: ip=192.168.100.20 os=windows base=win2022-v1 dns=oak"})
	f.Add(proxmox.VM{VMID: 2102, Name: "teak.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:21:03,bridge=devint01",
			"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=oak"})
	f.Add(proxmox.VM{VMID: 2103, Name: "elm.kilo.alpha", Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha", Status: "running"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:21:04,bridge=devint01",
			"description": "battleship: ip=192.168.100.300 os=linux base=debian-12-v1 dns=gone"})
	return f
}

// addrFixtures are environment jobs' inputs, with the plan and fingerprint
// stored for them, as battleship wrote them before addresses were typed.
var addrFixtures = []struct {
	name        string
	in          Inputs
	plan        string
	fingerprint string
}{
	{"create", Inputs{Kind: pods.KindEnvCreate, Domain: "lima.bravo", Router: "gw=gw", EnvHosts: []string{"fir=debian-12:30", "ash=win2022"}},
		`{"kind":"env-create","teams":null,"templates":null,"items":[{"team":"","host":"gw","name":"gw.lima.bravo","vmid":2200,"node":"alder","template":"","steps":["clone","label","network","start"],"snapshot":"","action":"","blocked":"","source":{"vmid":8000,"node":"alder","name":"gw-v1","bridges":["vmbr0"]},"pool":"dev-02","tags":"dev;lima.bravo;router","notes":{"base":"gw-v1","dns":"router"},"nics":{"net0":"devext02","net1":"devint02"}},{"team":"","host":"fir","name":"fir.lima.bravo","vmid":2201,"node":"alder","template":"","steps":["clone","label","network","start","wait-agent","configure-guest","verify"],"snapshot":"","action":"","blocked":"","source":{"vmid":8001,"node":"alder","name":"debian-12-v1","bridges":["vmbr0"]},"pool":"dev-02","tags":"dev;lima.bravo","notes":{"ip":"192.168.100.30","os":"linux","base":"debian-12-v1","dns":"router"},"nics":{"net0":"devint02"},"guest":{"os":"linux","hostname":"fir","domain":"lima.bravo","ip":"192.168.100.30","prefix":24,"gateway":"192.168.100.2","dns":"192.168.100.2"}},{"team":"","host":"ash","name":"ash.lima.bravo","vmid":2202,"node":"birch","template":"","steps":["clone","label","network","start","wait-agent","configure-guest","verify"],"snapshot":"","action":"","blocked":"","source":{"vmid":8002,"node":"birch","name":"win2022-v1","bridges":["vmbr0"]},"pool":"dev-02","tags":"dev;lima.bravo","notes":{"ip":"192.168.100.172","os":"windows","base":"win2022-v1","dns":"router"},"nics":{"net0":"devint02"},"guest":{"os":"windows","hostname":"ash","domain":"lima.bravo","ip":"192.168.100.172","prefix":24,"gateway":"192.168.100.2","dns":"192.168.100.2"}}],"config":"98fe1f400f0bd5f0","locks":["slot:02","domain:lima.bravo"],"slot":"02"}`,
		"992b53ec460af72e4a53949611f3769b00d3a67412523a1268edc58eb8572bcb"},
	{"add", Inputs{Kind: pods.KindEnvAdd, Domain: "kilo.alpha", EnvHosts: []string{"pine=debian-12", "teak=debian-12", "elm=debian-12", "fir=win2022:40"}},
		`{"kind":"env-add","teams":null,"templates":null,"items":[{"team":"","host":"pine","name":"pine.kilo.alpha","vmid":2104,"node":"alder","template":"","steps":["clone","label","network","start","wait-agent","configure-guest","verify"],"snapshot":"","action":"","blocked":"","source":{"vmid":8001,"node":"alder","name":"debian-12-v1","bridges":["vmbr0"]},"pool":"dev-01","tags":"dev;kilo.alpha","notes":{"ip":"192.168.100.227","os":"linux","base":"debian-12-v1","dns":"router"},"nics":{"net0":"devint01"},"guest":{"os":"linux","hostname":"pine","domain":"kilo.alpha","ip":"192.168.100.227","prefix":24,"gateway":"192.168.100.2","dns":"192.168.100.2"}},{"team":"","host":"teak","name":"teak.kilo.alpha","vmid":2102,"node":"alder","template":"","steps":["label","network","start","wait-agent","configure-guest","verify"],"snapshot":"","action":"","blocked":"","source":{"vmid":8001,"node":"alder","name":"debian-12-v1","bridges":["vmbr0"]},"pool":"dev-01","tags":"dev;kilo.alpha","notes":{"ip":"192.168.100.10","os":"linux","base":"debian-12-v1","dns":"router"},"nics":{"net0":"devint01"},"guest":{"os":"linux","hostname":"teak","domain":"kilo.alpha","ip":"192.168.100.10","prefix":24,"gateway":"192.168.100.2","dns":"192.168.100.2"}},{"team":"","host":"elm","name":"elm.kilo.alpha","vmid":2103,"node":"alder","template":"","steps":["label","network","start","wait-agent","configure-guest","verify"],"snapshot":"","action":"","blocked":"","source":{"vmid":8001,"node":"alder","name":"debian-12-v1","bridges":["vmbr0"]},"pool":"dev-01","tags":"dev;kilo.alpha","notes":{"ip":"192.168.100.26","os":"linux","base":"debian-12-v1","dns":"router"},"nics":{"net0":"devint01"},"guest":{"os":"linux","hostname":"elm","domain":"kilo.alpha","ip":"192.168.100.26","prefix":24,"gateway":"192.168.100.2","dns":"192.168.100.2"}},{"team":"","host":"fir","name":"fir.kilo.alpha","vmid":2105,"node":"birch","template":"","steps":["clone","label","network","start","wait-agent","configure-guest","verify"],"snapshot":"","action":"","blocked":"","source":{"vmid":8002,"node":"birch","name":"win2022-v1","bridges":["vmbr0"]},"pool":"dev-01","tags":"dev;kilo.alpha","notes":{"ip":"192.168.100.40","os":"windows","base":"win2022-v1","dns":"router"},"nics":{"net0":"devint01"},"guest":{"os":"windows","hostname":"fir","domain":"kilo.alpha","ip":"192.168.100.40","prefix":24,"gateway":"192.168.100.2","dns":"192.168.100.2"}}],"config":"98fe1f400f0bd5f0","locks":["slot:01","domain:kilo.alpha"],"slot":"01"}`,
		"70eece66bfeafe348c3204128129e65b036b44f122c9d2d17d9cda8f71b4722a"},
	{"dns to a host", Inputs{Kind: pods.KindEnvDNS, Domain: "kilo.alpha", DNS: "oak"},
		`{"kind":"env-dns","teams":null,"templates":null,"items":[{"team":"","host":"elm","name":"elm.kilo.alpha","vmid":2103,"node":"alder","template":"","steps":["label"],"snapshot":"","action":"","blocked":"","tags":"dev;kilo.alpha","notes":{"os":"linux","base":"debian-12-v1","dns":"oak"},"dns_was":{"choice":"gone"}},{"team":"","host":"gw","name":"gw.kilo.alpha","vmid":2100,"node":"alder","template":"","steps":["label"],"snapshot":"","action":"","blocked":"","tags":"dev;kilo.alpha;router","notes":{"base":"gw-v1","dns":"oak"},"dns_was":{"choice":"router","addr":"192.168.100.2"}},{"team":"","host":"oak","name":"oak.kilo.alpha","vmid":2101,"node":"birch","template":"","steps":["configure-guest","verify","label"],"snapshot":"","action":"","blocked":"","tags":"dev;kilo.alpha","notes":{"ip":"192.168.100.20","os":"windows","base":"win2022-v1","dns":"oak"},"guest":{"os":"windows","hostname":"oak","domain":"kilo.alpha","ip":"192.168.100.20","prefix":24,"gateway":"192.168.100.2","dns":"192.168.100.20","dns_only":true},"dns_was":{"choice":"oak","addr":"192.168.100.20"}},{"team":"","host":"teak","name":"teak.kilo.alpha","vmid":2102,"node":"alder","template":"","steps":["configure-guest","verify","label"],"snapshot":"","action":"","blocked":"","tags":"dev;kilo.alpha","notes":{"ip":"192.168.100.10","os":"linux","base":"debian-12-v1","dns":"oak"},"guest":{"os":"linux","hostname":"teak","domain":"kilo.alpha","ip":"192.168.100.10","prefix":24,"gateway":"192.168.100.2","dns":"192.168.100.20","dns_only":true},"dns_was":{"choice":"oak","addr":"192.168.100.20"}}],"config":"98fe1f400f0bd5f0","locks":["slot:01","domain:kilo.alpha"],"slot":"01"}`,
		"a4d3c57e7478a60b4f3083b017348108d70ac7f1c6af31c4495adad06e42def4"},
	{"dns to the router", Inputs{Kind: pods.KindEnvDNS, Domain: "kilo.alpha", DNS: "router"},
		`{"kind":"env-dns","teams":null,"templates":null,"items":[{"team":"","host":"elm","name":"elm.kilo.alpha","vmid":2103,"node":"alder","template":"","steps":["label"],"snapshot":"","action":"","blocked":"","tags":"dev;kilo.alpha","notes":{"os":"linux","base":"debian-12-v1","dns":"router"},"dns_was":{"choice":"gone"}},{"team":"","host":"gw","name":"gw.kilo.alpha","vmid":2100,"node":"alder","template":"","steps":["label"],"snapshot":"","action":"","blocked":"","tags":"dev;kilo.alpha;router","notes":{"base":"gw-v1","dns":"router"},"dns_was":{"choice":"router","addr":"192.168.100.2"}},{"team":"","host":"oak","name":"oak.kilo.alpha","vmid":2101,"node":"birch","template":"","steps":["configure-guest","verify","label"],"snapshot":"","action":"","blocked":"","tags":"dev;kilo.alpha","notes":{"ip":"192.168.100.20","os":"windows","base":"win2022-v1","dns":"router"},"guest":{"os":"windows","hostname":"oak","domain":"kilo.alpha","ip":"192.168.100.20","prefix":24,"gateway":"192.168.100.2","dns":"192.168.100.2","dns_only":true},"dns_was":{"choice":"oak","addr":"192.168.100.20"}},{"team":"","host":"teak","name":"teak.kilo.alpha","vmid":2102,"node":"alder","template":"","steps":["configure-guest","verify","label"],"snapshot":"","action":"","blocked":"","tags":"dev;kilo.alpha","notes":{"ip":"192.168.100.10","os":"linux","base":"debian-12-v1","dns":"router"},"guest":{"os":"linux","hostname":"teak","domain":"kilo.alpha","ip":"192.168.100.10","prefix":24,"gateway":"192.168.100.2","dns":"192.168.100.2","dns_only":true},"dns_was":{"choice":"oak","addr":"192.168.100.20"}}],"config":"98fe1f400f0bd5f0","locks":["slot:01","domain:kilo.alpha"],"slot":"01"}`,
		"65268203a2f4968aaff283fc035cc991367b86abfbc57a8b00df20560f02aefe"},
}

// Stored jobs keep their inputs, plan and fingerprint across deploys: a plan
// and its fingerprint are what they were, and a stored plan reads back to
// the same JSON and fingerprint.
func TestEnvPlanJSONIsStable(t *testing.T) {
	cfg := config.Default()
	cfg.Environments.Slots = "1-2"
	for _, fx := range addrFixtures {
		plan, err := BuildPlan(context.Background(), pods.NewPlanner(addrCluster(), cfg), fx.in)
		if err != nil {
			t.Fatalf("%s: %v", fx.name, err)
		}
		in, _ := json.Marshal(fx.in)
		got, _ := json.Marshal(plan)
		if string(got) != fx.plan || Fingerprint(plan) != fx.fingerprint {
			t.Errorf("%s: plan or fingerprint changed:\n%s\n%s", fx.name, got, Fingerprint(plan))
		}
		var stored pods.Plan
		if err := json.Unmarshal([]byte(fx.plan), &stored); err != nil {
			t.Errorf("%s: reading the stored plan: %v", fx.name, err)
			continue
		}
		again, _ := json.Marshal(&stored)
		if !bytes.Equal(again, []byte(fx.plan)) || Fingerprint(&stored) != fx.fingerprint {
			t.Errorf("%s: the stored plan reads back as\n%s\n%s", fx.name, again, Fingerprint(&stored))
		}
		var back Inputs
		if err := json.Unmarshal(in, &back); err != nil {
			t.Fatalf("%s: reading the inputs: %v", fx.name, err)
		}
		if again, _ := json.Marshal(back); !bytes.Equal(again, in) {
			t.Errorf("%s: inputs read back as %s", fx.name, again)
		}
	}
}
