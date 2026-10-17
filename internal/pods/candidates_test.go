package pods

import (
	"context"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

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

// A family's build VM or edit copy, in any case, suggests that family; any
// name suggests a valid family or none.
func TestPropSuggestFamily(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		f := rapid.StringMatching(`[a-z0-9][a-z0-9-]{0,20}[a-z0-9]`).Filter(func(f string) bool { return CheckFamily(f) == nil }).Draw(t, "family")
		for _, name := range []string{f, f + "-build", strings.ToUpper(f) + "-BUILD", f + EditSuffix} {
			if got := SuggestFamily(name); got != f {
				t.Fatalf("SuggestFamily(%q) = %q, want %q", name, got, f)
			}
		}
		name := rapid.String().Draw(t, "name")
		if got := SuggestFamily(name); got != "" && CheckFamily(got) != nil {
			t.Fatalf("SuggestFamily(%q) = %q, not a valid family", name, got)
		}
	})
}

// Every Proxmox OS type: its Windows ones are windows, its Linux ones linux.
func TestKindForOSType(t *testing.T) {
	for ostype, want := range map[string]string{
		"wxp": ImageWindows, "w2k": ImageWindows, "w2k3": ImageWindows, "w2k8": ImageWindows, "wvista": ImageWindows,
		"win7": ImageWindows, "win8": ImageWindows, "win10": ImageWindows, "win11": ImageWindows,
		"l24": ImageLinux, "l26": ImageLinux, "other": "", "solaris": "", "": "",
	} {
		if got := KindForOSType(ostype); got != want {
			t.Errorf("KindForOSType(%q) = %q, want %q", ostype, got, want)
		}
	}
}

// The pool's VMs that aren't templates, busy or registered already are
// listed, by VMID, each with the family an edit copy's notes give before
// its name does, and the reason a register would refuse it.
func TestRegisterCandidates(t *testing.T) {
	cfg := envConfig()
	pool := cfg.Images.Pool
	f := podstest.New("alder")
	agent := map[string]string{"agent": "1", "ostype": "win11", "description": "Login: admin"}
	f.Add(proxmox.VM{VMID: 301, Name: "teak-build", Node: "alder", Pool: pool, Status: "stopped"}, agent)
	f.Add(proxmox.VM{VMID: 302, Name: "oak-build", Node: "alder", Pool: pool, Status: "running"}, agent)
	f.Add(proxmox.VM{VMID: 303, Name: "cedar-build", Node: "alder", Pool: pool, Status: "stopped"}, map[string]string{"ostype": "l26"})
	f.Add(proxmox.VM{VMID: 304, Name: "copy", Node: "alder", Pool: pool, Status: "stopped"},
		map[string]string{"agent": "1", "description": "battleship: base=alder-v1"})
	f.Add(proxmox.VM{VMID: 305, Name: "birch-v1", Node: "alder", Pool: pool, Status: "stopped"}, map[string]string{"lock": "clone"})
	f.Add(proxmox.VM{VMID: 306, Name: "elm-build", Node: "alder", Pool: "dev-01", Status: "stopped"}, agent)
	vms, _ := f.ClusterVMs(context.Background())
	images := []BaseImage{
		{Name: "server-v2", Family: "server", Version: 2, Kind: ImageWindows, Source: 301},
		{Name: "server-v1", Family: "server", Version: 1, Kind: ImageWindows, Source: 301},
		{Name: "alder-v1", Family: "alder", Version: 1, Kind: ImageLinux},
	}
	got := NewPlanner(f, cfg).RegisterCandidates(context.Background(), vms, images)
	if len(got) != 3 || got[0].VMID != 302 || got[1].VMID != 303 || got[2].VMID != 304 {
		t.Fatalf("RegisterCandidates = %+v, want 302-304 (301 is registered)", got)
	}
	if c := got[0]; c.Family != "oak" || c.Kind != ImageWindows || !strings.Contains(c.Blocked, "running") || c.Notes != "Login: admin" {
		t.Errorf("302 = %+v, want family oak, blocked as running", c)
	}
	if c := got[1]; c.Kind != ImageLinux || !strings.Contains(c.Blocked, "guest agent") {
		t.Errorf("303 = %+v, want linux, blocked for no guest agent", c)
	}
	if c := got[2]; c.Family != "alder" || c.Kind != ImageLinux || c.Blocked != "" {
		t.Errorf("304 = %+v, want family alder from its notes", c)
	}
}
