package pods

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

func TestImageNames(t *testing.T) {
	if f, v, ok := ParseImageName("debian-12-v3"); !ok || f != "debian-12" || v != 3 {
		t.Errorf("ParseImageName = %q %d %v", f, v, ok)
	}
	for _, bad := range []string{"debian-12", "-v1", "Debian-v1", "x-v0", "teak.kilo.alpha"} {
		if _, _, ok := ParseImageName(bad); ok {
			t.Errorf("ParseImageName(%q) accepted", bad)
		}
	}
	for _, bad := range []string{"", "Win", "a_b", "x-v2", "-x", "x-"} {
		if CheckFamily(bad) == nil {
			t.Errorf("CheckFamily(%q) accepted", bad)
		}
	}
	if ImageName("win2022", 4) != "win2022-v4" {
		t.Error("ImageName")
	}
}

func TestBaseImagesNewestSkipsDeprecated(t *testing.T) {
	vms := []proxmox.VM{
		{VMID: 8001, Name: "debian-12-v1", Node: "alder", Template: true, Tags: "base;linux;deprecated"},
		{VMID: 8002, Name: "debian-12-v2", Node: "alder", Template: true, Tags: "linux;base"},
		{VMID: 8003, Name: "debian-12-v3", Node: "birch", Template: true, Tags: "base;linux;deprecated"},
		{VMID: 8004, Name: "gw-v1", Node: "alder", Template: true, Tags: "base;router"},
		{VMID: 8005, Name: "odd-v1", Node: "alder", Template: true, Tags: "base;linux;windows"}, // two kinds
		{VMID: 8006, Name: "notpl-v1", Node: "alder", Tags: "base;linux"},                       // not a template
		{VMID: 8007, Name: "named-badly", Node: "alder", Template: true, Tags: "base;linux"},    // no version
	}
	images := BaseImages(vms)
	if len(images) != 4 {
		t.Fatalf("images = %+v", images)
	}
	if n, ok := Newest(images, "debian-12"); !ok || n.Name != "debian-12-v2" {
		t.Errorf("Newest = %+v, %v; want v2 (v3 is deprecated)", n, ok)
	}
	if l, _ := Latest(images, "debian-12"); l.Version != 3 {
		t.Errorf("Latest = %+v; the next version follows v3", l)
	}
	if fs := Families(images, ImageRouter); len(fs) != 1 || fs[0].Name != "gw-v1" {
		t.Errorf("Families(router) = %+v", fs)
	}
	if _, ok := Newest(images, "odd"); ok {
		t.Error("a template with two kinds is a base image")
	}
}

func TestImageTags(t *testing.T) {
	if ImageTags(ImageWindows, true) != "base;windows;deprecated" || ImageTags(ImageLinux, false) != "base;linux" {
		t.Error("ImageTags")
	}
}

// parseImageNameModel splits name at its last "-v" into a family
// safeFamily takes and a version 1-9999 written without leading zeros.
func parseImageNameModel(name string) (string, int, bool) {
	i := strings.LastIndex(name, "-v")
	if i < 0 {
		return "", 0, false
	}
	family, digits := name[:i], name[i+2:]
	if len(digits) < 1 || len(digits) > 4 || digits[0] == '0' || strings.Trim(digits, "0123456789") != "" || !safeFamily(family) {
		return "", 0, false
	}
	v := 0
	for _, c := range digits {
		v = v*10 + int(c-'0')
	}
	return family, v, true
}

// imageFamilies are families and near misses: versions in the name, the
// 40-character limit, case, separators.
var imageFamilies = []string{
	"debian-12", "win2022", "gw", "a", "x-v0", "x-v01", "x-v10000", "a-v1x", "v1", strings.Repeat("a", 40), "debian-12-v1", "x-v9999",
	"Debian-12", "debian_12", "debian 12", "-x", "x-", "", strings.Repeat("a", 41), "deb\u0131an", "debian-12\n",
}

// imageVersions are versions at and around the edges of what a name holds.
var imageVersions = []int{1, 2, 9, 10, 99, 100, 999, 1000, 9998, 9999, 10000, 0, -1, 100000}

func checkImageName(t interface{ Fatalf(string, ...any) }, family string, version int) {
	name := ImageName(family, version)
	if (CheckFamily(family) == nil) != safeFamily(family) {
		t.Fatalf("CheckFamily(%q) = %v", family, CheckFamily(family))
	}
	f, v, ok := ParseImageName(name)
	wf, wv, wok := parseImageNameModel(name)
	if f != wf || v != wv || ok != wok {
		t.Fatalf("ParseImageName(%q) = %q, %d, %v; want %q, %d, %v", name, f, v, ok, wf, wv, wok)
	}
	if safeFamily(family) && version >= 1 && version <= MaxImageVersion && (f != family || v != version) {
		t.Fatalf("ParseImageName(ImageName(%q, %d)) = %q, %d", family, version, f, v)
	}
	if ok && ImageName(f, v) != name {
		t.Fatalf("ImageName(%q, %d) = %q, not %q", f, v, ImageName(f, v), name)
	}
}

// A family and version make a name that parses back to them, and only names
// that parse come from one.
func TestPropImageNames(t *testing.T) {
	for _, f := range imageFamilies {
		for _, v := range imageVersions {
			checkImageName(t, f, v)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		family := rapid.OneOf(rapid.SampledFrom(imageFamilies), rapid.SampledFrom(imageFamilies), rapid.StringMatching(`[a-z0-9v-]{0,12}`),
			rapid.String()).Draw(t, "family")
		version := rapid.OneOf(rapid.SampledFrom(imageVersions), rapid.IntRange(-10, 20000)).Draw(t, "version")
		checkImageName(t, family, version)
		// Any text in name form.
		name := family + rapid.SampledFrom([]string{"-v", "-V", "v", "-v0", "-v-", "-v\u0661", "-v+"}).Draw(t, "sep") +
			rapid.SampledFrom([]string{"", "1", "01", "9999", "10000", "1 ", " 1"}).Draw(t, "digits")
		f, v, ok := ParseImageName(name)
		wf, wv, wok := parseImageNameModel(name)
		if f != wf || v != wv || ok != wok {
			t.Fatalf("ParseImageName(%q) = %q, %d, %v; want %q, %d, %v", name, f, v, ok, wf, wv, wok)
		}
	})
}

// registerFake has a stopped VM 150 with an agent and family fam's versions
// at the given versions, the newest ones possibly deprecated.
func registerFake(fam string, versions []int, deprecated []bool) *podstest.Fake {
	f := podstest.New("alder")
	f.Add(proxmox.VM{VMID: 150, Name: "fresh", Node: "alder", Status: "stopped"}, map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:01:50,bridge=vmbr0"})
	for i, v := range versions {
		tags := "base;linux"
		if deprecated[i] {
			tags += ";deprecated"
		}
		f.Add(proxmox.VM{VMID: 8000 + i, Name: ImageName(fam, v), Node: "alder", Template: true, Tags: tags}, nil)
	}
	return f
}

func checkRegister(t interface{ Fatalf(string, ...any) }, fam string, versions []int, deprecated []bool) {
	plan, err := NewPlanner(registerFake(fam, versions, deprecated), envConfig()).ImageRegister(context.Background(), ImageRequest{VMID: 150, Family: fam})
	if err != nil {
		if len(versions) == 0 {
			return // a new family needs a kind
		}
		t.Fatalf("ImageRegister(%s after %v): %v", fam, versions, err)
	}
	latest := 0
	for _, v := range versions {
		latest = max(latest, v)
	}
	it := plan.Items[0]
	if latest >= MaxImageVersion {
		if it.Blocked == "" || !strings.Contains(it.Blocked, strconv.Itoa(MaxImageVersion)) {
			t.Fatalf("after v%d, planned %s (blocked %q)", latest, it.Name, it.Blocked)
		}
		return
	}
	if f, v, ok := parseImageNameModel(it.Name); it.Blocked != "" || !ok || f != fam || v != latest+1 {
		t.Fatalf("after %v, planned %s (blocked %q)", versions, it.Name, it.Blocked)
	}
}

// Registering plans the family's next version, which BaseImages will see,
// or is blocked when the family has run out of versions.
func TestPropImageRegisterPlansAParsableName(t *testing.T) {
	for _, latest := range []int{1, 9998, 9999} {
		checkRegister(t, "debian-12", []int{latest}, []bool{false})
		checkRegister(t, "debian-12", []int{1, latest}, []bool{false, true})
	}
	rapid.Check(t, func(t *rapid.T) {
		fam := rapid.SampledFrom([]string{"debian-12", "win2022", "x-v0", "a"}).Draw(t, "family")
		version := rapid.OneOf(rapid.SampledFrom([]int{1, 9997, 9998, 9999}), rapid.IntRange(1, 9999))
		versions := rapid.SliceOfNDistinct(version, 1, 3, rapid.ID).Draw(t, "versions")
		deprecated := rapid.SliceOfN(rapid.Bool(), len(versions), len(versions)).Draw(t, "deprecated")
		checkRegister(t, fam, versions, deprecated)
	})
}
