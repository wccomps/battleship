package apply

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func catalogExec(f *podstest.Fake) *Executor {
	cfg := config.Default()
	cfg.Retry.Attempts, cfg.Retry.Rounds = 2, 0
	return &Executor{API: f, Cfg: cfg, Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
}

func TestRegisterCopiesConvertsAndRetires(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 150, Name: "fresh", Node: "alder"}, map[string]string{"tags": "junk", "ide2": "local:iso/x.iso,media=cdrom",
		"scsi0": "competitions:150/vm-150-disk-0.qcow2"})
	f.Add(proxmox.VM{VMID: 8000, Name: "deb-v1", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"tags": "base;linux"})
	plan := &pods.Plan{Kind: pods.KindImageRegister, Items: []pods.Item{{
		Name: "deb-v2", VMID: 8001, Node: "alder", Pool: "dev-templates", Tags: "base;linux",
		Source: &pods.Source{VMID: 150, Node: "alder", Name: "fresh"}, Supersedes: &pods.Source{VMID: 8000, Node: "alder", Name: "deb-v1"},
		Steps: []pods.Step{pods.StepClone, pods.StepLabel, pods.StepCDROM, pods.StepConvert, pods.StepRetire}}}}
	res := catalogExec(f).Run(context.Background(), plan)
	if len(res.Succeeded) != 1 {
		t.Fatalf("result = %+v", res)
	}
	v := f.VMs[8001]
	if !v.Template || v.Pool != "dev-templates" || v.Config["tags"] != "base;linux" || v.Config["ide2"] != "none,media=cdrom" {
		t.Errorf("new version = %+v %v", v.VM, v.Config)
	}
	if f.VMs[8000].Config["tags"] != "base;linux;deprecated" {
		t.Errorf("previous tags = %q", f.VMs[8000].Config["tags"])
	}
}

func TestEnvHostCloneIsRemovedWhenItFails(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 8001, Name: "deb-v1", Node: "alder", Template: true}, map[string]string{"net0": "virtio=BC:24:11:00:80:01,bridge=vmbr0"})
	f.FailOn("setconfig:2101", &proxmox.APIError{Status: 403, Message: "Permission check failed"})
	plan := &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{{
		Name: "teak.kilo.alpha", Host: "teak", VMID: 2101, Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha",
		Source: &pods.Source{VMID: 8001, Node: "alder", Name: "deb-v1"}, NICs: map[string]string{"net0": "devint01"},
		Steps: []pods.Step{pods.StepClone, pods.StepLabel, pods.StepNetwork, pods.StepStart}}}}
	res := catalogExec(f).Run(context.Background(), plan)
	if len(res.Failed) != 1 || len(res.Removed) != 1 || f.VMs[2101] != nil {
		t.Errorf("result = %+v; the half-built clone must be removed", res)
	}
}

func TestEnvHostIsWiredAndLabelled(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 8001, Name: "deb-v1", Node: "alder", Template: true}, map[string]string{"tags": "base;linux", "net0": "virtio=BC:24:11:00:80:01,bridge=vmbr0"})
	plan := &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{{
		Name: "teak.kilo.alpha", Host: "teak", VMID: 2101, Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha",
		Notes:  &pods.Notes{IP: netip.MustParseAddr("192.168.100.10"), OS: "linux", Base: "deb-v1", DNS: "router"},
		Source: &pods.Source{VMID: 8001, Node: "alder", Name: "deb-v1"}, NICs: map[string]string{"net0": "devint01"},
		Steps: []pods.Step{pods.StepClone, pods.StepLabel, pods.StepNetwork, pods.StepStart}}}}
	res := catalogExec(f).Run(context.Background(), plan)
	v := f.VMs[2101]
	if len(res.Succeeded) != 1 || v.Config["tags"] != "dev;kilo.alpha" || !strings.Contains(v.Config["net0"], "bridge=devint01") ||
		v.Config["description"] != "battleship: ip=192.168.100.10 os=linux base=deb-v1 dns=router" || v.Status != "running" {
		t.Errorf("res %+v vm %+v %v", res, v.VM, v.Config)
	}
}

func registerPlan(steps ...pods.Step) *pods.Plan {
	return &pods.Plan{Kind: pods.KindImageRegister, Items: []pods.Item{{
		Name: "deb-v2", VMID: 8001, Node: "alder", Pool: "dev-templates", Tags: "base;linux",
		Source: &pods.Source{VMID: 150, Node: "alder", Name: "fresh"}, Supersedes: &pods.Source{VMID: 8000, Node: "alder", Name: "deb-v1"},
		Steps: steps}}}
}

func registerFake() *podstest.Fake {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 150, Name: "fresh", Node: "alder"}, map[string]string{"scsi0": "competitions:150/vm-150-disk-0.qcow2"})
	f.Add(proxmox.VM{VMID: 8000, Name: "deb-v1", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"tags": "base;linux"})
	return f
}

func TestRegisterFailsOnHalfConvertedImage(t *testing.T) {
	f := registerFake()
	f.ConvertLeavesDisk = map[int]bool{8001: true}
	res := catalogExec(f).Run(context.Background(), registerPlan(pods.StepClone, pods.StepConvert, pods.StepRetire))
	if len(res.Failed) != 1 || len(res.Succeeded) != 0 || f.VMs[8001] != nil {
		t.Errorf("result = %+v; a half-converted copy must fail and be removed", res)
	}
	if strings.Contains(f.VMs[8000].Config["tags"], "deprecated") {
		t.Errorf("previous version was retired despite the failure")
	}
}

func TestRegisterKeepsConvertedImageWhenRetireFails(t *testing.T) {
	f := registerFake()
	f.FailOn("setconfig:8000", &proxmox.APIError{Status: 403, Message: "Permission check failed"})
	res := catalogExec(f).Run(context.Background(), registerPlan(pods.StepClone, pods.StepConvert, pods.StepRetire))
	if len(res.Failed) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if v := f.VMs[8001]; v == nil || !v.Template {
		t.Errorf("converted image must survive a failed retire: %+v", v)
	}
}

// The details step rewrites only the notes' details block, or with free
// text the notes less their battleship lines. It refuses details or text
// that would break them, however the plan came by them.
func TestDetailsStepWritesTheBlock(t *testing.T) {
	const desc = "Patched monthly.\nbattleship: source=150"
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 8000, Name: "deb-v1", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"description": desc})
	plan := func(d pods.ImageDetails, free *string) *pods.Plan {
		return &pods.Plan{Kind: pods.KindImageDetails, Items: []pods.Item{{Name: "deb-v1", VMID: 8000, Node: "alder",
			Steps: []pods.Step{pods.StepDetails}, Details: &pods.ImageNotes{ImageDetails: d, Free: free}}}}
	}
	forged := "x\nbattleship: ip=10.50.0.9"
	for _, bad := range []*pods.Plan{plan(pods.ImageDetails{OS: "a\nbattleship: ip=10.50.0.1"}, nil), plan(pods.ImageDetails{OS: "Debian 12"}, &forged)} {
		res := catalogExec(f).Run(context.Background(), bad)
		if len(res.Failed) != 1 || f.VMs[8000].Config["description"] != desc {
			t.Errorf("bad notes: result %+v, notes %q", res, f.VMs[8000].Config["description"])
		}
	}
	res := catalogExec(f).Run(context.Background(), plan(pods.ImageDetails{OS: "Debian 12", User: "root"}, nil))
	if want := "**Details**\n- OS: Debian 12\n- Login: root\n\n" + desc; len(res.Succeeded) != 1 || f.VMs[8000].Config["description"] != want {
		t.Errorf("result %+v, notes %q, want %q", res, f.VMs[8000].Config["description"], want)
	}
	free := "Rebooted."
	res = catalogExec(f).Run(context.Background(), plan(pods.ImageDetails{OS: "Debian 12"}, &free))
	if want := "**Details**\n- OS: Debian 12\n\nRebooted.\n\nbattleship: source=150"; len(res.Succeeded) != 1 || f.VMs[8000].Config["description"] != want {
		t.Errorf("result %+v, notes %q, want %q", res, f.VMs[8000].Config["description"], want)
	}
}

// Every step that writes a VM's notes holds them, as it runs, to what
// Proxmox keeps, counted in characters, however the plan came by them:
// notes of 4100 two-byte characters (8200 bytes) are written, and longer
// ones fail the item with nothing written.
func TestNotesAreBoundAtRun(t *testing.T) {
	fits, long := strings.Repeat("\u00e9", 4100), strings.Repeat("x", 8190)
	for name, it := range map[string]pods.Item{
		"label":   {Name: "deb-v1", VMID: 8000, Node: "alder", Tags: "base;linux", Notes: &pods.Notes{Source: "150"}, Steps: []pods.Step{pods.StepLabel}},
		"details": {Name: "deb-v1", VMID: 8000, Node: "alder", Details: &pods.ImageNotes{ImageDetails: pods.ImageDetails{OS: "Debian 12"}}, Steps: []pods.Step{pods.StepDetails}},
	} {
		for desc, ok := range map[string]bool{fits: true, long: false} {
			f := podstest.New("alder")
			f.Add(proxmox.VM{VMID: 8000, Name: "deb-v1", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"description": desc})
			res := catalogExec(f).Run(context.Background(), &pods.Plan{Kind: pods.KindImageDetails, Items: []pods.Item{it}})
			got := f.VMs[8000].Config["description"]
			switch {
			case ok && (len(res.Succeeded) != 1 || got == desc):
				t.Errorf("%s over %d characters: result %+v, notes unchanged %v", name, len([]rune(desc)), res, got == desc)
			case !ok && (len(res.Failed) != 1 || got != desc || !strings.Contains(fmt.Sprint(res.Failed["deb-v1"]), "characters; Proxmox keeps at most 8192")):
				t.Errorf("%s over %d characters: result %+v, notes changed %v", name, len(desc), res, got != desc)
			}
		}
	}
}

// A register of a VM whose notes are 4100 two-byte characters (8200
// bytes, under the 8192 characters Proxmox keeps) plans clean and runs,
// its details written over the notes it copied.
func TestRegisterKeepsLongNotesProxmoxKeeps(t *testing.T) {
	desc := strings.Repeat("é", 4100)
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 150, Name: "fresh", Node: "alder", Status: "stopped"}, map[string]string{"description": desc, "agent": "1",
		"scsi0": "competitions:150/vm-150-disk-0.qcow2"})
	e := catalogExec(f)
	plan, err := pods.NewPlanner(f, e.Cfg).ImageRegister(context.Background(), pods.ImageRequest{VMID: 150, Family: "deb", Kind: "linux",
		Details: &pods.ImageNotes{ImageDetails: pods.ImageDetails{OS: "Debian 12"}}})
	if err != nil || plan.Items[0].Blocked != "" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	res := e.Run(context.Background(), plan)
	got := f.VMs[plan.Items[0].VMID].Config["description"]
	if want := "**Details**\n- OS: Debian 12\n\n" + desc + "\nbattleship: source=150"; len(res.Succeeded) != 1 || got != want {
		t.Errorf("result %+v, notes %.40q (%d bytes)", res, got, len(got))
	}
}

// A config write carries the digest of the read its change was worked out
// from, so a change made in between isn't written over: the item fails
// saying so, the change is kept, and neither the call nor a later round
// tries again.
func TestConfigWriteRefusedWhenConfigChangedSinceRead(t *testing.T) {
	const desc = "**Details**\n- OS: Debian 12\n\nPatched.\nbattleship: source=150"
	const bob = "**Details**\n- OS: Debian 12\n- Login: admin\n\nPatched.\nbattleship: source=150"
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 8000, Name: "deb-v1", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"description": desc})
	writes := 0
	f.Gate = func(ctx context.Context, key string) (func(), error) {
		if key == "setconfig:8000" {
			f.Mu.Lock()
			if writes++; writes == 1 {
				f.VMs[8000].Config["description"] = bob
			}
			f.Mu.Unlock()
		}
		return nil, nil
	}
	e := catalogExec(f)
	e.Cfg.Retry.Rounds = 2
	plan := &pods.Plan{Kind: pods.KindImageDetails, Items: []pods.Item{{Name: "deb-v1", VMID: 8000, Node: "alder",
		Steps: []pods.Step{pods.StepDetails}, Details: &pods.ImageNotes{ImageDetails: pods.ImageDetails{OS: "Debian 13"}}}}}
	res := e.Run(context.Background(), plan)
	err := res.Failed["deb-v1"]
	if len(res.Failed) != 1 || !proxmox.IsModified(err) || !strings.Contains(err.Error(), "changed since it was read, so nothing was written") || proxmox.Describe(err) != err.Error() {
		t.Errorf("result %+v, describe %q", res, proxmox.Describe(err))
	}
	if got := f.VMs[8000].Config["description"]; got != bob || writes != 1 {
		t.Errorf("notes = %q after %d writes, want the change made in between, after 1", got, writes)
	}
}

// When a SetVMConfig write takes effect on Proxmox before its HTTP response
// is lost, the retry re-reads VMConfig rather than resending the pre-write
// digest and failing with Modified.
func TestConfigWriteRetryRereadsAfterLostResponse(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 8001, Name: "deb-v1", Node: "alder", Template: true}, map[string]string{
		"tags": "base;linux", "net0": "virtio=BC:24:11:00:80:01,bridge=vmbr0",
	})
	setCalls := 0
	f.Gate = func(ctx context.Context, key string) (func(), error) {
		if key == "setconfig:2101" {
			if setCalls++; setCalls == 1 {
				f.Mu.Lock()
				f.VMs[2101].Config["tags"] = "dev;kilo.alpha"
				f.VMs[2101].Config["description"] = "battleship: ip=192.168.100.10 os=linux base=deb-v1 dns=router"
				f.Mu.Unlock()
				return nil, &proxmox.APIError{Status: 502, Message: "Bad Gateway"}
			}
		}
		return nil, nil
	}
	e := catalogExec(f)
	e.Cfg.Retry.Attempts = 3
	plan := &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{{
		Name: "teak.kilo.alpha", Host: "teak", VMID: 2101, Node: "alder", Pool: "dev-01", Tags: "dev;kilo.alpha",
		Notes:  &pods.Notes{IP: netip.MustParseAddr("192.168.100.10"), OS: "linux", Base: "deb-v1", DNS: "router"},
		Source: &pods.Source{VMID: 8001, Node: "alder", Name: "deb-v1"}, NICs: map[string]string{"net0": "devint01"},
		Steps: []pods.Step{pods.StepClone, pods.StepLabel, pods.StepNetwork, pods.StepStart},
	}}}
	res := e.Run(context.Background(), plan)
	if len(res.Succeeded) != 1 || f.VMs[2101] == nil {
		t.Errorf("result %+v, vm %+v", res, f.VMs[2101])
	}
}
