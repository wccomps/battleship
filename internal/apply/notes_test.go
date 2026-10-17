package apply

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// masterNotes are a master's notes as a base image leaves them: the details
// block with a login, free text and an environment's battleship line.
const masterNotes = "**Details**\n" +
	"- OS: Debian 12\n" +
	"- Login: alice / Hunter-2\n" +
	"\n" +
	"Built for teak.tango.delta.\n" +
	"battleship: ip=10.50.1.10 os=linux base=deb-v1 dns=router"

// snapshotDescriptions records each VM's notes when its baseline is taken.
func snapshotDescriptions(f *podstest.Fake) map[int]*string {
	at := map[int]*string{}
	f.WaitGate = func(_ context.Context, upid string) error {
		if !strings.Contains(upid, ":qmsnapshot:") {
			return nil
		}
		var vmid int
		if _, err := fmt.Sscanf(upid[strings.Index(upid, ":qmsnapshot:")+len(":qmsnapshot:"):], "%d", &vmid); err != nil {
			return err
		}
		f.Mu.Lock()
		defer f.Mu.Unlock()
		if d, ok := f.VMs[vmid].Config["description"]; ok {
			at[vmid] = &d
		} else {
			at[vmid] = nil
		}
		return nil
	}
	return at
}

// A team VM comes out of a deploy without the notes its clone copied, before
// its baseline is taken; the master and its template keep theirs.
func TestDeployClearsTeamNotes(t *testing.T) {
	f := newCluster()
	f.VMs[121].Config["description"] = masterNotes
	at := snapshotDescriptions(f)
	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01", "02"))
	if len(res.Failed) != 0 || len(res.Succeeded) != 2 {
		t.Fatalf("result = %+v", res)
	}
	for _, vmid := range []int{10121, 10221} {
		if d, ok := f.VMs[vmid].Config["description"]; ok {
			t.Errorf("VM %d notes = %q, want none", vmid, d)
		}
		if d, ok := at[vmid]; !ok {
			t.Errorf("VM %d: no baseline taken", vmid)
		} else if d != nil {
			t.Errorf("VM %d had notes %q at its baseline, want none", vmid, *d)
		}
	}
	if d := f.VMs[121].Config["description"]; d != masterNotes {
		t.Errorf("master notes = %q, want kept", d)
	}
	if d := f.VMs[9021].Config["description"]; d != masterNotes {
		t.Errorf("template notes = %q, want kept", d)
	}
}

// Notes written on a team VM after its deploy go at the next one, without
// recloning it.
func TestRedeployClearsLaterNotes(t *testing.T) {
	f := newCluster()
	testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	f.VMs[10121].Config["description"] = masterNotes
	res := testExecutor(f, &recorder{}).Run(context.Background(), deployTeak(t, f, "01"))
	if len(res.Failed) != 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if d, ok := f.VMs[10121].Config["description"]; ok || f.Called("clone:10121") != 1 {
		t.Errorf("notes %q (present %t), cloned %d times; want none and one clone", d, ok, f.Called("clone:10121"))
	}
}

// Whatever notes a master has, its team clones have none.
func TestPropDeployClearsAnyMasterNotes(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		f := newCluster()
		desc := rapid.OneOf(rapid.String(), rapid.Just(masterNotes),
			rapid.Map(rapid.String(), func(s string) string { return masterNotes + "\n" + s })).Draw(rt, "notes")
		f.VMs[121].Config["description"] = desc
		plan, err := testPlanner(f).Deploy(context.Background(), pods.DeployRequest{Pattern: "teak.*", Teams: []string{"01"}, Snapshot: true})
		if err != nil {
			rt.Fatal(err)
		}
		res := testExecutor(f, &recorder{}).Run(context.Background(), plan)
		if len(res.Failed) != 0 {
			rt.Fatalf("failed: %v", res.Failed)
		}
		if d, ok := f.VMs[10121].Config["description"]; ok {
			rt.Fatalf("team VM notes = %q, want none", d)
		}
		if f.VMs[121].Config["description"] != desc {
			rt.Fatalf("master notes changed")
		}
	})
}

// An environment's VMs keep their base image's notes, with the battleship
// line added.
func TestEnvCreateKeepsImageNotes(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 8000, Name: "gw-v1", Node: "alder", Template: true, Tags: "base;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:80:00,bridge=vmbr0", "net1": "virtio=BC:24:11:00:80:01,bridge=vmbr0",
			"description": "**Details**\n- Login: alice / Hunter-2"})
	ex := catalogExec(f)
	ex.Cfg.Environments.Slots = "1-1"
	plan, err := pods.NewPlanner(f, ex.Cfg).EnvCreate(context.Background(),
		pods.EnvRequest{Domain: "kilo.alpha", Router: pods.HostSpec{Host: "gw", Family: "gw"}})
	if err != nil {
		t.Fatal(err)
	}
	if res := ex.Run(context.Background(), plan); len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	want := "**Details**\n- Login: alice / Hunter-2\nbattleship: base=gw-v1 dns=router"
	if d := f.VMs[2100].Config["description"]; d != want {
		t.Errorf("environment VM notes = %q, want %q", d, want)
	}
}
