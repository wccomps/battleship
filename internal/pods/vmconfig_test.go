package pods

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/config"
)

func TestNetworkChangesTwoNICsWithCloudInit(t *testing.T) {
	cur := map[string]string{
		"net0":  "virtio=BC:24:11:00:00:01,bridge=vmbr0,firewall=1",
		"net1":  "e1000=BC:24:11:00:00:02,bridge=vmbr1",
		"ide2":  "competitions:vm-10105-cloudinit,media=cdrom",
		"scsi0": "competitions:base-9005-disk-0.qcow2/10105/vm-10105-disk-0.qcow2,size=32G",
	}
	got := NetworkChanges(config.Default().Network, cur, "05", 2)
	want := map[string]string{
		"net0":       "virtio=BC:24:11:00:00:01,bridge=ext05,firewall=1",
		"net1":       "e1000=BC:24:11:00:00:02,bridge=int05",
		"ipconfig0":  "ip=10.50.105.2/30,gw=10.50.105.1",
		"nameserver": "10.50.105.1",
		"cicustom":   "vendor=competitions:snippets/ssh-keys.yaml",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NetworkChanges =\n %v\nwant\n %v", got, want)
	}
}

func TestNetworkChangesOneNICNoCloudInit(t *testing.T) {
	cur := map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0"}
	got := NetworkChanges(config.Default().Network, cur, "12", 1)
	want := map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=int12"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NetworkChanges = %v, want %v", got, want)
	}
}

func TestNetworkChangesConvergedIsEmpty(t *testing.T) {
	cur := map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=int12"}
	if got := NetworkChanges(config.Default().Network, cur, "12", 1); len(got) != 0 {
		t.Errorf("NetworkChanges on converged VM = %v, want none", got)
	}
}

func TestDiskLimitChanges(t *testing.T) {
	cur := map[string]string{
		"scsi0":   "competitions:vm-10105-disk-0.qcow2,size=32G,mbps_rd=100",
		"ide2":    "none,media=cdrom",
		"scsihw":  "virtio-scsi-pci",
		"virtio1": "competitions:vm-10105-disk-1.qcow2,mbps_rd=300,mbps_wr=300",
	}
	got := DiskLimitChanges(cur, 300, 300)
	want := map[string]string{"scsi0": "competitions:vm-10105-disk-0.qcow2,mbps_rd=300,mbps_wr=300,size=32G"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiskLimitChanges = %v, want %v", got, want)
	}
}

func TestCDROMChangesSkipsCloudInit(t *testing.T) {
	cur := map[string]string{
		"ide0": "local:iso/win.iso,media=cdrom",
		"ide1": "none,media=cdrom",
		"ide2": "competitions:vm-10105-cloudinit,media=cdrom",
	}
	got := CDROMChanges(cur)
	want := map[string]string{"ide0": "none,media=cdrom"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CDROMChanges = %v, want %v", got, want)
	}
}

func TestCountInterfaces(t *testing.T) {
	cfg := map[string]string{"net0": "x", "net1": "y", "netfoo": "z", "name": "n"}
	if got := CountInterfaces(cfg); got != 2 {
		t.Errorf("CountInterfaces = %d, want 2", got)
	}
}

func TestDiskLimitsConvergeOnProxmoxFormat(t *testing.T) {
	cur := map[string]string{"scsi0": "competitions:117/vm-117-disk-0.qcow2,iothread=1,size=64G"}
	got := DiskLimitChanges(cur, 300, 300)
	want := map[string]string{"scsi0": "competitions:117/vm-117-disk-0.qcow2,iothread=1,mbps_rd=300,mbps_wr=300,size=64G"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiskLimitChanges = %v, want %v", got, want)
	}
	// Apply the change and verify round-trip converges
	for k, v := range got {
		cur[k] = v
	}
	if len(DiskLimitChanges(cur, 300, 300)) != 0 {
		t.Errorf("DiskLimitChanges not stable after applying changes")
	}
}

func TestDiskLimitsSkipCDROMs(t *testing.T) {
	cur := map[string]string{
		"ide2":  "competitions:vm-1-cloudinit,media=cdrom",
		"ide0":  "local:iso/x.iso,media=cdrom",
		"scsi0": "competitions:disk,mbps_rd=100",
	}
	got := DiskLimitChanges(cur, 300, 300)
	want := map[string]string{"scsi0": "competitions:disk,mbps_rd=300,mbps_wr=300"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiskLimitChanges = %v, want %v", got, want)
	}
}

func TestNetworkKeepsOptionsDropsTag(t *testing.T) {
	cur := map[string]string{"net0": "virtio=BC:24:11:00:00:09,bridge=vmbr0,firewall=1,queues=4,tag=20"}
	got := NetworkChanges(config.Default().Network, cur, "03", 1)
	want := map[string]string{"net0": "virtio=BC:24:11:00:00:09,bridge=int03,firewall=1,queues=4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NetworkChanges = %v, want %v", got, want)
	}
	// Recompute on the result to ensure convergence
	if got := NetworkChanges(config.Default().Network, want, "03", 1); len(got) != 0 {
		t.Errorf("NetworkChanges on converged result = %v, want none", got)
	}
}

func TestNetworkTwoNICsWithoutCloudInit(t *testing.T) {
	cur := map[string]string{
		"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0",
		"net1": "e1000=BC:24:11:00:00:02,bridge=vmbr1",
	}
	got := NetworkChanges(config.Default().Network, cur, "05", 2)
	want := map[string]string{
		"net0": "virtio=BC:24:11:00:00:01,bridge=ext05",
		"net1": "e1000=BC:24:11:00:00:02,bridge=int05",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NetworkChanges = %v, want %v", got, want)
	}
}

func TestNetworkDoesNotAddMissingNIC(t *testing.T) {
	cur := map[string]string{"net1": "e1000=BC:24:11:00:00:02,bridge=vmbr1"}
	got := NetworkChanges(config.Default().Network, cur, "05", 2)
	want := map[string]string{"net1": "e1000=BC:24:11:00:00:02,bridge=int05"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NetworkChanges = %v, want %v", got, want)
	}
	if _, hasNet0 := got["net0"]; hasNet0 {
		t.Errorf("NetworkChanges added net0 when it wasn't present")
	}
}

func TestNetworkNoMAC(t *testing.T) {
	cur := map[string]string{"net0": "e1000,bridge=vmbr0"}
	got := NetworkChanges(config.Default().Network, cur, "01", 1)
	want := map[string]string{"net0": "e1000,bridge=int01"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NetworkChanges = %v, want %v", got, want)
	}
}

func TestIsDriveKey(t *testing.T) {
	tests := map[string]bool{
		"scsi0":     true,
		"ide2":      true,
		"sata1":     true,
		"virtio0":   true,
		"scsihw":    false,
		"efidisk0":  false,
		"tpmstate0": false,
		"virtiofs0": false,
		"unused0":   false,
	}
	for k, want := range tests {
		if got := isDriveKey(k); got != want {
			t.Errorf("isDriveKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestHasCloudInitNeedsCDROM(t *testing.T) {
	cfg := map[string]string{"ide2": "competitions:vm-1-disk-cloudinit-data.qcow2,size=1G"}
	if HasCloudInit(cfg) {
		t.Errorf("HasCloudInit = true for cloudinit without media=cdrom, want false")
	}
}

func TestDiskLimitChangesUnequalValues(t *testing.T) {
	// Unequal rd/wr values are set, in alphabetical order.
	config := map[string]string{
		"scsi0": "competitions:vm-1-disk-0.qcow2,size=8G",
	}
	want := map[string]string{
		"scsi0": "competitions:vm-1-disk-0.qcow2,mbps_rd=250,mbps_wr=150,size=8G",
	}
	if got := DiskLimitChanges(config, 250, 150); !reflect.DeepEqual(got, want) {
		t.Errorf("DiskLimitChanges(rd=250, wr=150) = %v; want %v", got, want)
	}
}

func TestDiskVolumes(t *testing.T) {
	cfg := map[string]string{
		"scsi0":     "competitions:9008/vm-9008-disk-1.qcow2,size=32G",
		"virtio1":   "competitions:base-9005-disk-0.qcow2/10105/vm-10105-disk-1.qcow2,iothread=1",
		"efidisk0":  "competitions:9008/vm-9008-disk-0.qcow2,efitype=4m",
		"tpmstate0": "competitions:9008/vm-9008-disk-2.raw,version=v2.0",
		"ide0":      "local:iso/win.iso,media=cdrom",
		"ide1":      "none,media=cdrom",
		"ide2":      "competitions:vm-9008-cloudinit,media=cdrom",
		"sata3":     "none",
		"net0":      "virtio=BC:24:11:00:00:01,bridge=vmbr0",
		"unused0":   "competitions:9008/vm-9008-disk-3.qcow2",
	}
	want := map[string]string{
		"scsi0":     "competitions:9008/vm-9008-disk-1.qcow2",
		"virtio1":   "competitions:base-9005-disk-0.qcow2/10105/vm-10105-disk-1.qcow2",
		"efidisk0":  "competitions:9008/vm-9008-disk-0.qcow2",
		"tpmstate0": "competitions:9008/vm-9008-disk-2.raw",
	}
	if got := DiskVolumes(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("DiskVolumes = %v, want %v", got, want)
	}
}

func TestUnconvertedDisk(t *testing.T) {
	for _, c := range []struct {
		cfg      map[string]string
		key, vol string
	}{
		{map[string]string{"template": "1", "scsi0": "competitions:9008/base-9008-disk-0.qcow2,size=32G",
			"ide2": "competitions:vm-9008-cloudinit,media=cdrom"}, "", ""},
		{map[string]string{"template": "1", "scsi0": "competitions:base-9005-disk-0.qcow2/9010/base-9010-disk-0.qcow2"}, "", ""},
		// Half-conversion: template: 1 but scsi0 never renamed.
		{map[string]string{"template": "1", "scsi0": "competitions:9008/vm-9008-disk-1.qcow2,size=32G"},
			"scsi0", "competitions:9008/vm-9008-disk-1.qcow2"},
		{map[string]string{"template": "1", "scsi0": "competitions:9008/base-9008-disk-0.qcow2",
			"efidisk0": "competitions:9008/vm-9008-disk-1.qcow2,efitype=4m"}, "efidisk0", "competitions:9008/vm-9008-disk-1.qcow2"},
		{map[string]string{"template": "1", "virtio0": "competitions:base-9005-disk-0.qcow2/10105/vm-10105-disk-0.qcow2"},
			"virtio0", "competitions:base-9005-disk-0.qcow2/10105/vm-10105-disk-0.qcow2"},
	} {
		key, vol := UnconvertedDisk(c.cfg)
		if key != c.key || vol != c.vol {
			t.Errorf("UnconvertedDisk(%v) = %q, %q; want %q, %q", c.cfg, key, vol, c.key, c.vol)
		}
	}
}

func TestNICChanges(t *testing.T) {
	cur := map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=5", "net1": "virtio=BC:24:11:00:00:02,bridge=devint01"}
	got := NICChanges(cur, map[string]string{"net0": "devext02", "net1": "devint01"})
	if len(got) != 1 || got["net0"] != "virtio=BC:24:11:00:00:01,bridge=devext02" {
		t.Errorf("NICChanges = %v", got)
	}
}

func TestLabelChanges(t *testing.T) {
	cur := map[string]string{"tags": "kilo.alpha;dev", "description": "hi"}
	if got := LabelChanges(cur, "dev;kilo.alpha", nil); len(got) != 0 {
		t.Errorf("same tags in another order changed: %v", got)
	}
	got := LabelChanges(cur, "", &Notes{DNS: "router"})
	if got["delete"] != "tags" || got["description"] != "hi\nbattleship: dns=router" {
		t.Errorf("LabelChanges = %v", got)
	}
	if AddTag("base;linux", "deprecated") != "base;linux;deprecated" || AddTag("base;deprecated", "deprecated") != "base;deprecated" {
		t.Error("AddTag")
	}
}

// nicHeads are NIC models and MACs, some a character off.
var nicHeads = []string{"virtio=BC:24:11:00:00:01", "e1000=bc:24:11:aa:bb:cc", "virtio", "virtio=", "virtio=BC:24:11:00:00:0",
	"virtio=BC:24:11:00:00:011", "=BC:24:11:00:00:01", "virtio=BC-24-11-00-00-01", ""}

// nicOptions are NIC options as Proxmox prints them, and malformed ones.
var nicOptions = []string{"bridge=vmbr0", "tag=10", "firewall=1", "rate=10", "queues=4", "mtu=1500", "link_down=1",
	"", "=", "=x", "tag=", "bridge=", "x=a=b", "firewall", "Tag=5", " tag=1"}

// checkNIC checks that got is cur put on bridge: cur's model, its MAC if
// it has a whole one, its options but tag, and the bridge, each option a
// key=value with a key.
func checkNIC(t interface{ Fatalf(string, ...any) }, cur, bridge, got string) {
	curParts, parts := strings.Split(cur, ","), strings.Split(got, ",")
	head := curParts[0]
	if model, mac, ok := strings.Cut(head, "="); ok && len(mac) != 17 {
		head = model
	}
	if parts[0] != head {
		t.Fatalf("%q on %s: head %q, want %q", cur, bridge, parts[0], head)
	}
	want := map[string]string{}
	for _, p := range curParts[1:] {
		if k, v, _ := strings.Cut(p, "="); k != "" && k != "tag" {
			want[k] = v
		}
	}
	want["bridge"] = bridge
	opts := map[string]string{}
	var keys []string
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			t.Fatalf("%q on %s = %q: option %q has no key", cur, bridge, got, p)
		}
		opts[k] = v
		keys = append(keys, k)
	}
	if !slices.IsSorted(keys) || !reflect.DeepEqual(opts, want) || len(keys) != len(want) {
		t.Fatalf("%q on %s = %q, want options %v", cur, bridge, got, want)
	}
}

func checkNICChanges(t interface{ Fatalf(string, ...any) }, cur map[string]string, team string, interfaces int) {
	nics := map[string]string{"net0": "vmbr9", "net1": "vmbr8", "net2": "vmbr7"}
	got := NICChanges(cur, nics)
	for k, v := range got {
		checkNIC(t, cur[k], nics[k], v)
	}
	if again := NICChanges(applied(cur, got), nics); len(again) != 0 {
		t.Fatalf("NICChanges(%q) = %q, then %q", cur, got, again)
	}
	net := config.Default().Network
	got = NetworkChanges(net, cur, team, interfaces)
	for _, k := range []string{"net0", "net1"} {
		if v, ok := got[k]; ok {
			bridge := Expand(net.IntBridge, team, "")
			if interfaces >= 2 && k == "net0" {
				bridge = Expand(net.ExtBridge, team, "")
			}
			checkNIC(t, cur[k], bridge, v)
		}
	}
	if again := NetworkChanges(net, applied(cur, got), team, interfaces); len(again) != 0 {
		t.Fatalf("NetworkChanges(%q) = %q, then %q", cur, got, again)
	}
}

// Rewiring keeps a NIC's model, whole MAC and options but its VLAN tag,
// writes only options with keys, and converges.
func TestPropNICChanges(t *testing.T) {
	for _, h := range nicHeads {
		for _, o := range nicOptions {
			checkNICChanges(t, map[string]string{"net0": h + "," + o + ",bridge=vmbr1", "net1": h + "," + o + "," + o}, "05", 2)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		cur := map[string]string{}
		for _, k := range []string{"net0", "net1", "net2"} {
			if rapid.IntRange(0, 4).Draw(t, k+" missing") == 0 {
				continue
			}
			head := rapid.OneOf(rapid.SampledFrom(nicHeads[:2]), rapid.SampledFrom(nicHeads)).Draw(t, k+" head")
			opts := rapid.SliceOfN(rapid.OneOf(rapid.SampledFrom(nicOptions), rapid.StringMatching(`[a-z=_ ]{0,6}`)), 0, 5).Draw(t, k+" options")
			cur[k] = strings.Join(append([]string{head}, opts...), ",")
		}
		checkNICChanges(t, cur, rapid.SampledFrom([]string{"01", "05", "99"}).Draw(t, "team"), rapid.IntRange(1, 3).Draw(t, "interfaces"))
	})
}
