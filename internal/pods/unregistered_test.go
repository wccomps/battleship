package pods

import (
	"context"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// suggestFamilyModel is the rule: lower-case the name, drop one -build or
// -edit suffix, and keep it only if it is a valid family.
func suggestFamilyModel(name string) string {
	f := strings.ToLower(name)
	switch {
	case strings.HasSuffix(f, "-build"):
		f = f[:len(f)-len("-build")]
	case strings.HasSuffix(f, "-edit"):
		f = f[:len(f)-len("-edit")]
	}
	if CheckFamily(f) != nil {
		return ""
	}
	return f
}

func TestSuggestFamily(t *testing.T) {
	for name, want := range map[string]string{
		"win2022-build": "win2022", "debian-13-build": "debian-13", "teak-edit": "teak", "Oak-Build": "oak",
		"alder": "alder", "-build": "", "a-v2-build": "", "x-edit-build": "x-edit", "teak.kilo.alpha": "",
	} {
		if got := SuggestFamily(name); got != want {
			t.Errorf("SuggestFamily(%q) = %q, want %q", name, got, want)
		}
	}
}

// A suggestion follows the rule, and is always a family a register accepts.
func TestPropSuggestFamily(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := rapid.OneOf(
			rapid.StringMatching(`[a-zA-Z0-9.-]{0,20}(-build|-edit|-BUILD|-Edit)?`),
			rapid.StringMatching(`[a-z0-9-]{0,45}-build`),
			rapid.String(),
		).Draw(t, "name")
		got := SuggestFamily(name)
		if want := suggestFamilyModel(name); got != want {
			t.Fatalf("SuggestFamily(%q) = %q, want %q", name, got, want)
		}
		if got != "" {
			if f, v, ok := ParseImageName(ImageName(got, 1)); !ok || f != got || v != 1 {
				t.Fatalf("SuggestFamily(%q) = %q, which doesn't name a version 1", name, got)
			}
		}
	})
}

// The OS type decides the kind only for Windows and Linux.
func TestPropKindForOSType(t *testing.T) {
	known := []string{"other", "wxp", "w2k", "w2k3", "w2k8", "wvista", "win7", "win8", "win10", "win11", "l24", "l26", "solaris"}
	rapid.Check(t, func(t *rapid.T) {
		ostype := rapid.OneOf(rapid.SampledFrom(known), rapid.String()).Draw(t, "ostype")
		want := ""
		if len(ostype) >= 3 && ostype[:3] == "win" {
			want = ImageWindows
		} else if ostype == "l24" || ostype == "l26" {
			want = ImageLinux
		}
		if got := KindForOSType(ostype); got != want {
			t.Fatalf("KindForOSType(%q) = %q, want %q", ostype, got, want)
		}
	})
}

// Only stopped VMs in the pool that aren't templates are listed, by VMID,
// each with the newest image registered from it and its family's kind.
func TestUnregisteredVMs(t *testing.T) {
	vms := []proxmox.VM{
		{VMID: 302, Name: "oak-build", Node: "alder", Pool: "images", Status: "stopped"},
		{VMID: 301, Name: "teak-build", Node: "alder", Pool: "images", Status: "stopped"},
		{VMID: 303, Name: "cedar-build", Node: "alder", Pool: "images", Status: "running"},
		{VMID: 304, Name: "birch-build", Node: "alder", Pool: "dev-01", Status: "stopped"},
		{VMID: 8000, Name: "teak-v1", Node: "alder", Pool: "images", Status: "stopped", Template: true, Tags: "base;linux"},
	}
	images := []BaseImage{
		{Name: "teak-v2", Family: "teak", Version: 2, Kind: ImageLinux, Source: 301},
		{Name: "teak-v1", Family: "teak", Version: 1, Kind: ImageLinux, Source: 301},
		{Name: "teak-v3", Family: "teak", Version: 3, Kind: ImageLinux, Source: 999},
	}
	got := UnregisteredVMs(vms, "images", images)
	if len(got) != 2 || got[0].VMID != 301 || got[1].VMID != 302 {
		t.Fatalf("UnregisteredVMs = %+v, want 301 and 302", got)
	}
	if got[0].Family != "teak" || got[0].Kind != ImageLinux || got[0].Registered != "teak-v2" {
		t.Errorf("301 = %+v, want family teak, its kind, registered as teak-v2", got[0])
	}
	if got[1].Family != "oak" || got[1].Kind != "" || got[1].Registered != "" {
		t.Errorf("302 = %+v, want family oak, no kind yet, never registered", got[1])
	}
}

// Reading fills the notes, and the kind from the OS type unless the
// family already decided it.
func TestReadUnregistered(t *testing.T) {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 301, Name: "teak-build", Node: "alder", Status: "stopped"}, map[string]string{"ostype": "win11", "description": "Login: admin"})
	f.Add(proxmox.VM{VMID: 302, Name: "oak-build", Node: "alder", Status: "stopped"}, map[string]string{"ostype": "win11"})
	got := NewPlanner(f, envConfig()).ReadUnregistered(context.Background(), []Unregistered{
		{VMID: 301, Node: "alder"}, {VMID: 302, Node: "alder", Kind: ImageLinux}})
	if got[0].Kind != ImageWindows || got[0].Notes != "Login: admin" || got[1].Kind != ImageLinux {
		t.Errorf("ReadUnregistered = %+v", got)
	}
}
