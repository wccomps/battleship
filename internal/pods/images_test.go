package pods

import (
	"testing"

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
