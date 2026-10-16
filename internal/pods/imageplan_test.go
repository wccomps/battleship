package pods

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func imageFake() *podstest.Fake {
	f := podstest.New("alder", "birch")
	f.Add(proxmox.VM{VMID: 150, Name: "fresh-debian", Node: "birch"}, map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:01:50,bridge=vmbr0",
		"ide2": "local:iso/debian.iso,media=cdrom"})
	f.Add(proxmox.VM{VMID: 151, Name: "fresh-win", Node: "birch"}, map[string]string{"net0": "virtio=BC:24:11:00:01:51,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 8000, Name: "debian-12-v1", Node: "alder", Template: true, Tags: "base;linux"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:80:00,bridge=vmbr1"})
	return f
}

func TestImageRegisterNewVersionRetiresPrevious(t *testing.T) {
	p := NewPlanner(imageFake(), envConfig())
	plan, err := p.ImageRegister(context.Background(), ImageRequest{VMID: 150, Family: "debian-12"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("items = %+v", plan.Items)
	}
	it := plan.Items[0]
	want := []Step{StepClone, StepLabel, StepCDROM, StepConvert, StepRetire}
	if it.Name != "debian-12-v2" || it.VMID != 8001 || it.Blocked != "" || !slices.Equal(it.Steps, want) ||
		it.Source.VMID != 150 || !slices.Equal(it.Source.Bridges, []string{"vmbr0"}) || it.Pool != "dev-templates" || it.Tags != "base;linux" || it.Supersedes.VMID != 8000 ||
		it.Notes == nil || it.Notes.Source != "150" {
		t.Errorf("item = %+v", it)
	}
	// The VMs it changes are its items'; Plan.LockKeys locks those.
	if !slices.Equal(plan.Locks, []string{"family:debian-12"}) {
		t.Errorf("locks = %v", plan.Locks)
	}
}

func TestImageRegisterBlocks(t *testing.T) {
	ctx := context.Background()
	f := imageFake()
	p := NewPlanner(f, envConfig())
	plan, err := p.ImageRegister(ctx, ImageRequest{VMID: 151, Family: "win2022", Kind: ImageWindows})
	if err != nil || !strings.Contains(plan.Items[0].Blocked, "guest agent") {
		t.Errorf("no agent: %+v, %v", plan, err)
	}
	f.VMs[150].Status = "running"
	plan, _ = p.ImageRegister(ctx, ImageRequest{VMID: 150, Family: "deb", Kind: ImageLinux})
	if !strings.Contains(plan.Items[0].Blocked, "running") {
		t.Errorf("running source: %+v", plan.Items[0])
	}
	if _, err := p.ImageRegister(ctx, ImageRequest{VMID: 150, Family: "debian-12", Kind: ImageWindows}); err == nil {
		t.Error("a new version may change the family's kind")
	}
	if _, err := p.ImageRegister(ctx, ImageRequest{VMID: 150, Family: "new", Kind: "toaster"}); err == nil {
		t.Error("unknown kind accepted")
	}
	// Team-named sources are refused, each saying what it is.
	f.Add(proxmox.VM{VMID: 10121, Name: "team01-teak", Node: "alder", Status: "stopped"}, nil)
	f.Add(proxmox.VM{VMID: 152, Name: "team01-oak", Node: "alder", Status: "stopped"}, nil)
	for vmid, want := range map[int]string{10121: "team01-teak is a team VM",
		152: "team01-oak has a team 01 name but is VMID 152, outside the team's VMIDs, so it isn't a team VM; rename or remove it in Proxmox"} {
		plan, err := p.ImageRegister(ctx, ImageRequest{VMID: vmid, Family: "deb", Kind: ImageLinux})
		if err != nil || plan.Items[0].Blocked != want {
			t.Errorf("register %d: %+v, %v; want blocked %q", vmid, plan, err, want)
		}
	}
}

func TestImageEditMakesWorkingCopy(t *testing.T) {
	p := NewPlanner(imageFake(), envConfig())
	plan, err := p.ImageEdit(context.Background(), ImageRequest{Family: "debian-12"})
	if err != nil {
		t.Fatal(err)
	}
	it := plan.Items[0]
	if it.Name != "debian-12-edit" || it.Tags != "" || it.NICs["net0"] != "vmbr0" || it.Source.VMID != 8000 || !slices.Equal(it.Source.Bridges, []string{"vmbr1"}) ||
		!slices.Equal(it.Steps, []Step{StepClone, StepLabel, StepNetwork}) {
		t.Errorf("item = %+v", it)
	}
}

func TestImageDeprecateAndDelete(t *testing.T) {
	ctx := context.Background()
	p := NewPlanner(imageFake(), envConfig())
	plan, err := p.ImageDeprecate(ctx, ImageRequest{Image: "debian-12-v1"})
	if err != nil || plan.Items[0].Tags != "base;linux;deprecated" || !slices.Equal(plan.Items[0].Steps, []Step{StepLabel}) {
		t.Errorf("deprecate = %+v, %v", plan, err)
	}
	plan, err = p.ImageDelete(ctx, ImageRequest{Image: "debian-12-v1"})
	if err != nil || plan.Items[0].VMID != 8000 || !slices.Equal(plan.Items[0].Steps, []Step{StepDelete}) {
		t.Errorf("delete = %+v, %v", plan, err)
	}
	if _, err := p.ImageDelete(ctx, ImageRequest{Image: "nope-v1"}); err == nil {
		t.Error("deleting a missing image")
	}
}

func TestSourceVMID(t *testing.T) {
	for desc, want := range map[string]int{
		"Login: root\nbattleship: source=8920": 8920,
		"battleship: source=0150":              0,
		"battleship: source=+150":              0,
		"battleship: source=99":                0,
		"battleship: source=x":                 0,
		"source=8920":                          0,
		"":                                     0,
	} {
		if got := sourceVMID(desc); got != want {
			t.Errorf("sourceVMID(%q) = %d, want %d", desc, got, want)
		}
	}
}

// A router's working copy has its LAN on the edit bridge too, never on the
// slot bridge it was registered from.
func TestImageEditMovesEveryNIC(t *testing.T) {
	f := imageFake()
	f.Add(proxmox.VM{VMID: 8010, Name: "gw-v1", Node: "alder", Template: true, Tags: "base;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:80:10,bridge=devext01", "net1": "virtio=BC:24:11:00:80:11,bridge=devint01"})
	plan, err := NewPlanner(f, envConfig()).ImageEdit(context.Background(), ImageRequest{Family: "gw"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"net0": "vmbr0", "net1": "vmbr0"}; !maps.Equal(plan.Items[0].NICs, want) {
		t.Errorf("NICs = %v, want %v", plan.Items[0].NICs, want)
	}
}
