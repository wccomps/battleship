package pods

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/config"
)

// These functions compute the config changes a VM needs. They return only
// keys whose value differs from the current config, so an empty result means
// the VM already converged. Ported from proxmox_manager.py.

var driveKeyRE = regexp.MustCompile(`^(ide|sata|scsi|virtio)\d+$`)

func isDriveKey(k string) bool {
	return driveKeyRE.MatchString(k)
}

func isNetKey(k string) bool {
	return strings.HasPrefix(k, "net") && len(k) > 3 && strings.Trim(k[3:], "0123456789") == ""
}

// Proxmox never prints bare flags after the volume or model, so every option
// is k=v.

// splitOptions splits "head,k=v,k2=v2" into the head (volume or model=MAC)
// and its options.
func splitOptions(s string) (head string, opts map[string]string) {
	parts := strings.Split(s, ",")
	opts = map[string]string{}
	for _, p := range parts[1:] {
		k, v, _ := strings.Cut(p, "=")
		opts[k] = v
	}
	return parts[0], opts
}

// joinOptions is the inverse of splitOptions, with options in alphabetical
// order as Proxmox prints them, so re-reading a written value compares equal.
func joinOptions(head string, opts map[string]string) string {
	out := head
	for _, k := range slices.Sorted(maps.Keys(opts)) {
		out += "," + k + "=" + opts[k]
	}
	return out
}

// CountInterfaces counts net0, net1, ... keys.
func CountInterfaces(cfg map[string]string) int {
	n := 0
	for k := range cfg {
		if isNetKey(k) {
			n++
		}
	}
	return n
}

// NICBridges lists the bridges of net0 and net1, the NICs a deploy
// rewires, in that order, skipping a missing NIC.
func NICBridges(cfg map[string]string) []string {
	var out []string
	for _, k := range []string{"net0", "net1"} {
		if v, ok := cfg[k]; ok {
			if _, opts := splitOptions(v); opts["bridge"] != "" {
				out = append(out, opts["bridge"])
			}
		}
	}
	return out
}

// HasCloudInit reports whether any drive is a cloud-init drive.
func HasCloudInit(cfg map[string]string) bool {
	for k, v := range cfg {
		if isDriveKey(k) && strings.Contains(v, "cloudinit") && strings.Contains(v, "media=cdrom") {
			return true
		}
	}
	return false
}

func nicSpec(current, bridge string) string {
	head, opts := splitOptions(current)
	if model, mac, ok := strings.Cut(head, "="); ok && len(mac) != 17 {
		head = model
	}
	delete(opts, "tag")
	opts["bridge"] = bridge
	return joinOptions(head, opts)
}

// NetworkChanges wires a VM to its team's networks. VMs with two or more NICs
// get net0 on the external bridge and net1 on the internal bridge; VMs with
// one NIC get net0 on the internal bridge. Cloud-init VMs also get the
// external IP (two-NIC only) and the vendor snippet. Other NIC options such
// as firewall, queues and mtu are kept, tag is dropped, and NICs beyond net1
// are not touched.
func NetworkChanges(net config.Network, cur map[string]string, team string, interfaces int) map[string]string {
	want := map[string]string{}
	ci := HasCloudInit(cur)
	if interfaces >= 2 {
		if cur["net0"] != "" {
			want["net0"] = nicSpec(cur["net0"], Expand(net.ExtBridge, team, ""))
		}
		if cur["net1"] != "" {
			want["net1"] = nicSpec(cur["net1"], Expand(net.IntBridge, team, ""))
		}
		if ci {
			subnet := Expand(net.ExtSubnet, team, "")
			want["ipconfig0"] = fmt.Sprintf("ip=%s.2/30,gw=%s.1", subnet, subnet)
			want["nameserver"] = subnet + ".1"
		}
	} else {
		if cur["net0"] != "" {
			want["net0"] = nicSpec(cur["net0"], Expand(net.IntBridge, team, ""))
		}
	}
	if ci && net.CICustom != "" {
		want["cicustom"] = net.CICustom
	}
	return diff(cur, want)
}

// DiskLimitChanges sets mbps_rd/mbps_wr on every drive backed by storage
// (any value containing ':'), replacing existing limits. Skips drives whose
// value contains media=cdrom (cloud-init drives and ISOs; limits there need
// VM.Config.CDROM and are pointless anyway).
func DiskLimitChanges(cur map[string]string, rd, wr int) map[string]string {
	want := map[string]string{}
	rdStr := strconv.Itoa(rd)
	wrStr := strconv.Itoa(wr)
	for k, v := range cur {
		if !isDriveKey(k) || !strings.Contains(v, ":") || strings.Contains(v, "media=cdrom") {
			continue
		}
		head, opts := splitOptions(v)
		if opts["mbps_rd"] == rdStr && opts["mbps_wr"] == wrStr {
			continue
		}
		opts["mbps_rd"] = rdStr
		opts["mbps_wr"] = wrStr
		want[k] = joinOptions(head, opts)
	}
	return diff(cur, want)
}

// CDROMChanges ejects media from CD-ROM drives. Cloud-init drives are also
// media=cdrom but are left alone.
func CDROMChanges(cur map[string]string) map[string]string {
	want := map[string]string{}
	for k, v := range cur {
		if isDriveKey(k) && strings.Contains(v, "media=cdrom") && !strings.Contains(v, "cloudinit") {
			want[k] = "none,media=cdrom"
		}
	}
	return diff(cur, want)
}

func diff(cur, want map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range want {
		if cur[k] != v {
			out[k] = v
		}
	}
	return out
}

// diskKeyRE matches the config keys that hold a VM's disks.
var diskKeyRE = regexp.MustCompile(`^((ide|sata|scsi|virtio)\d+|efidisk0|tpmstate0)$`)

// DiskVolumes maps each of a VM's disk keys to its volume ("storage:name",
// the part before the first comma). CD-ROMs, including cloud-init drives,
// and empty drives are left out.
func DiskVolumes(cfg map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range cfg {
		if !diskKeyRE.MatchString(k) {
			continue
		}
		head, opts := splitOptions(v)
		if opts["media"] == "cdrom" || head == "none" || head == "" {
			continue
		}
		out[k] = head
	}
	return out
}

// VolumeFile is the last path segment of a volume's name, e.g.
// "vm-9008-disk-1.qcow2" for "competitions:9008/vm-9008-disk-1.qcow2", or
// "vm-10105-disk-0.qcow2" for a linked clone's
// "competitions:base-9005-disk-0.qcow2/10105/vm-10105-disk-0.qcow2".
func VolumeFile(vol string) string {
	_, name, ok := strings.Cut(vol, ":")
	if !ok {
		name = vol
	}
	return name[strings.LastIndex(name, "/")+1:]
}

// VolumeOwner is the VMID a volume's file name says owns it
// (vm-<vmid>-… or base-<vmid>-…), and whether it is a base volume; 0 for
// any other file, such as an ISO or an import.
func VolumeOwner(vol string) (vmid int, base bool) {
	file := VolumeFile(vol)
	rest, base := strings.CutPrefix(file, "base-")
	if !base {
		var ok bool
		if rest, ok = strings.CutPrefix(file, "vm-"); !ok {
			return 0, false
		}
	}
	digits, _, ok := strings.Cut(rest, "-")
	n, err := strconv.Atoi(digits)
	if !ok || err != nil {
		return 0, false
	}
	return n, base
}

// UnconvertedDisk returns the first disk (by key) of a template's config
// whose volume is not a base- volume, or "", "" if all are. Converting to a
// template renames every disk to base-*, and linked clones need that.
func UnconvertedDisk(cfg map[string]string) (key, vol string) {
	disks := DiskVolumes(cfg)
	for _, k := range slices.Sorted(maps.Keys(disks)) {
		if !strings.HasPrefix(VolumeFile(disks[k]), "base-") {
			return k, disks[k]
		}
	}
	return "", ""
}
