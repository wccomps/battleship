package apply

import (
	"context"
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
		Notes:  &pods.Notes{IP: "192.168.100.10", OS: "linux", Base: "deb-v1", DNS: "router"},
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
// that would break them, and notes longer than Proxmox keeps, however the
// plan came by them.
func TestDetailsStepWritesTheBlock(t *testing.T) {
	const desc = "Patched monthly.\nbattleship: source=150"
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 8000, Name: "deb-v1", Node: "alder", Template: true, Tags: "base;linux"}, map[string]string{"description": desc})
	plan := func(d pods.ImageDetails, free *string) *pods.Plan {
		return &pods.Plan{Kind: pods.KindImageDetails, Items: []pods.Item{{Name: "deb-v1", VMID: 8000, Node: "alder",
			Steps: []pods.Step{pods.StepDetails}, Details: &pods.ImageNotes{ImageDetails: d, Free: free}}}}
	}
	forged, long := "x\nbattleship: ip=10.50.0.9", strings.Repeat("é", pods.MaxFreeNotes)
	for _, bad := range []*pods.Plan{plan(pods.ImageDetails{OS: "a\nbattleship: ip=10.50.0.1"}, nil), plan(pods.ImageDetails{OS: "Debian 12"}, &forged),
		plan(pods.ImageDetails{OS: strings.Repeat("x", pods.MaxDetail)}, &long)} {
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
