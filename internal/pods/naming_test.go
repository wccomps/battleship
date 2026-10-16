package pods

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

func TestNaming(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	if got := n.VMName("01", "teak"); got != "team01-teak" {
		t.Errorf("VMName = %q", got)
	}
	if got := n.Pool("01"); got != "pool-01" {
		t.Errorf("Pool = %q", got)
	}
	if got := n.CloneVMID("01", 9005); got != 10105 {
		t.Errorf("CloneVMID = %d, want 10105", got)
	}
	if got := n.TemplateVMID(1252); got != 9052 {
		t.Errorf("TemplateVMID = %d, want 9052", got)
	}
	if got := n.TemplateName("teak.tango.delta"); got != "teak.tango.delta.tpl" {
		t.Errorf("TemplateName = %q", got)
	}
	if got := Hostname("teak.tango.delta.tpl"); got != "teak" {
		t.Errorf("Hostname = %q", got)
	}
	team, host, ok := n.ParseVMName("team07-pychgynmygytgyn")
	if !ok || team != "07" || host != "pychgynmygytgyn" {
		t.Errorf("ParseVMName = %q %q %v", team, host, ok)
	}
	if _, _, ok := n.ParseVMName("pool-07-vpn"); ok {
		t.Error("ParseVMName matched a non-team VM")
	}
}

func TestParseVMName(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	tests := []struct {
		name     string
		vmName   string
		wantTeam string
		wantHost string
		wantOk   bool
	}{
		{"valid dc-01", "team01-dc-01", "01", "dc-01", true},
		{"valid teak", "team07-teak", "07", "teak", true},
		{"empty host", "team01-", "", "", false},
		{"single digit team", "team7-x", "", "", false},
		{"triple digit team", "team007-x", "", "", false},
		{"pool name", "pool-07-vpn", "", "", false},
		{"case sensitive", "Team01-x", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			team, host, ok := n.ParseVMName(tt.vmName)
			if ok != tt.wantOk || team != tt.wantTeam || host != tt.wantHost {
				t.Errorf("ParseVMName(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.vmName, team, host, ok, tt.wantTeam, tt.wantHost, tt.wantOk)
			}
		})
	}
}

func TestParseVMNameCustomPattern(t *testing.T) {
	n := NewNaming(config.Naming{
		VMName:              "{host}.t{team}+x",
		Pool:                "pool-{team}",
		CloneVMIDBase:       10000,
		CloneVMIDTeamStride: 100,
		TemplateVMIDBase:    9000,
		TemplateSuffix:      ".tpl",
	})
	team, host, ok := n.ParseVMName("dc.t05+x")
	if !ok || team != "05" || host != "dc" {
		t.Errorf("ParseVMName(dc.t05+x) = (%q, %q, %v), want (05, dc, true)", team, host, ok)
	}
	_, _, ok = n.ParseVMName("dc.t5+x")
	if ok {
		t.Error("ParseVMName(dc.t5+x) should not match")
	}
}

func TestVMNameRoundTrip(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	tests := []struct {
		team string
		host string
	}{
		{"00", "teak"},
		{"07", "dc-01"},
		{"99", "a.b"},
	}
	for _, tt := range tests {
		t.Run(tt.team+"-"+tt.host, func(t *testing.T) {
			vmName := n.VMName(tt.team, tt.host)
			team, host, ok := n.ParseVMName(vmName)
			if !ok || team != tt.team || host != tt.host {
				t.Errorf("round trip failed: VMName(%q, %q) -> ParseVMName(%q) = (%q, %q, %v)",
					tt.team, tt.host, vmName, team, host, ok)
			}
		})
	}
}

func TestExpand(t *testing.T) {
	result := Expand("team{team}-{host}", "01", "{team}")
	if result != "team01-{team}" {
		t.Errorf("Expand no re-expansion = %q, want team01-{team}", result)
	}
}

func TestCloneVMIDPanicsOnBadTeam(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	tests := []struct {
		name      string
		team      string
		wantPanic bool
	}{
		{"non-numeric", "abc", true},
		{"negative", "-1", true},
		{"out of range", "100", true},
		{"valid", "05", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); (r != nil) != tt.wantPanic {
					if tt.wantPanic {
						t.Errorf("CloneVMID(%q, 9005) did not panic", tt.team)
					} else {
						t.Errorf("CloneVMID(%q, 9005) unexpectedly panicked: %v", tt.team, r)
					}
				}
			}()
			n.CloneVMID(tt.team, 9005)
		})
	}
}

func TestIsTemplateName(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	if !n.IsTemplateName("x.tpl") {
		t.Error("IsTemplateName(x.tpl) should be true")
	}
	if n.IsTemplateName("x") {
		t.Error("IsTemplateName(x) should be false")
	}
}

func TestParseVMNameTeamHostPattern(t *testing.T) {
	n := NewNaming(config.Naming{
		VMName:              "{team}{host}",
		Pool:                "pool-{team}",
		CloneVMIDBase:       10000,
		CloneVMIDTeamStride: 100,
		TemplateVMIDBase:    9000,
		TemplateSuffix:      ".tpl",
	})
	team, host, ok := n.ParseVMName(n.VMName("05", "12x"))
	if !ok || team != "05" || host != "12x" {
		t.Errorf("ParseVMName round trip for {team}{host} pattern = (%q, %q, %v), want (05, 12x, true)", team, host, ok)
	}
}

func TestParseVMNameMultiTeamPattern(t *testing.T) {
	n := NewNaming(config.Naming{
		VMName:              "t{team}-{host}-{team}",
		Pool:                "pool-{team}",
		CloneVMIDBase:       10000,
		CloneVMIDTeamStride: 100,
		TemplateVMIDBase:    9000,
		TemplateSuffix:      ".tpl",
	})
	team, host, ok := n.ParseVMName("t05-h-05")
	if !ok || team != "05" || host != "h" {
		t.Errorf("ParseVMName(t05-h-05) = (%q, %q, %v), want (05, h, true)", team, host, ok)
	}
	_, _, ok = n.ParseVMName("t05-h-07")
	if ok {
		t.Error("ParseVMName(t05-h-07) should not match (mismatched team placeholders)")
	}
}

func TestParseVMNameLiteralTeamInHost(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	team, host, ok := n.ParseVMName("team01-{team}")
	if !ok || team != "01" || host != "{team}" {
		t.Errorf("ParseVMName(team01-{team}) = (%q, %q, %v), want (01, {team}, true)", team, host, ok)
	}
}

// namePieces are literal text for naming.vm_name patterns: separators and
// regexp metacharacters.
var namePieces = []string{"", "team", "-", ".", "_", "*", "(", ")", "[", "\\", "$", "^", "+", "?", "|", "{", "}", "{x}", "7", "a.b"}

// vmHosts are host names and near misses: digits that touch the team,
// empty, multi-line, separators.
var vmHosts = []string{"dc", "teak", "web01", "1x", "12", "0", "a-01", "a.b", "", "\n", "a\nb", "dc\r", "{team}", "-", "x\xff", "\u0661\u0662"}

// drawVMNaming draws a vm_name pattern with {team} and {host} once each.
func drawVMNaming(t *rapid.T) Naming {
	piece := rapid.SampledFrom(namePieces)
	first, second := "{team}", "{host}"
	if rapid.Bool().Draw(t, "host first") {
		first, second = second, first
	}
	cfg := config.Default().Naming
	cfg.VMName = piece.Draw(t, "prefix") + first + piece.Draw(t, "middle") + second + piece.Draw(t, "suffix")
	return NewNaming(cfg)
}

func checkVMName(t interface{ Fatalf(string, ...any) }, n Naming, team, host string) {
	name := n.VMName(team, host)
	gt, gh, ok := n.ParseVMName(name)
	if wantOK := host != "" && !strings.Contains(host, "\n"); ok != wantOK || ok && (gt != team || n.VMName(gt, gh) != name) {
		t.Fatalf("pattern %q: ParseVMName(%q) = %q, %q, %v; want team %s, ok %v", n.cfg.VMName, name, gt, gh, ok, team, wantOK)
	}
}

// Any pattern names each team's VMs so that the name parses back to the
// team, and only names a team VM could have parse.
func TestPropVMNames(t *testing.T) {
	for _, pattern := range []string{"team{team}-{host}", "{host}{team}", "{team}{host}", "{host}.{team}.*", "({team})[{host}]", "{host}-{team}$"} {
		cfg := config.Default().Naming
		cfg.VMName = pattern
		n := NewNaming(cfg)
		for _, host := range vmHosts {
			for _, team := range []string{"00", "05", "10", "99"} {
				checkVMName(t, n, team, host)
			}
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		n := drawVMNaming(t)
		team := fmt.Sprintf("%02d", rapid.IntRange(0, 99).Draw(t, "team"))
		host := rapid.OneOf(rapid.SampledFrom(vmHosts), rapid.StringMatching(`[a-z0-9.-]{1,8}`), rapid.String()).Draw(t, "host")
		checkVMName(t, n, team, host)
		// A name near one: any that parses is one VMName gives.
		name := n.VMName(team, host)
		i := rapid.IntRange(0, len(name)).Draw(t, "at")
		near := rapid.SampledFrom([]string{name[:i] + name[min(i+1, len(name)):], name[:i] + "1" + name[i:], name[:i] + "x" + name[i:],
			strings.Replace(name, team, team[1:], 1), strings.Replace(name, team, "1"+team, 1), name + "\n"}).Draw(t, "near")
		if gt, gh, ok := n.ParseVMName(near); ok && (len(gt) != 2 || gt[0] < '0' || gt[0] > '9' || gt[1] < '0' || gt[1] > '9' || n.VMName(gt, gh) != near) {
			t.Fatalf("pattern %q: ParseVMName(%q) = %q, %q", n.cfg.VMName, near, gt, gh)
		}
		// Discovery finds exactly the team VMs that parse, of the teams asked,
		// in the team's own clone VMID block.
		vmid := func(label string) int { return rapid.SampledFrom(blockVMIDs(n.cfg, team)).Draw(t, label) }
		vms := []proxmox.VM{{VMID: vmid("vmid 1"), Name: name}, {VMID: vmid("vmid 2"), Name: near},
			{VMID: vmid("vmid 3"), Name: name, Template: true}, {VMID: vmid("vmid 4"), Name: name + n.cfg.TemplateSuffix}}
		for i := range vms {
			vms[i].Node = strconv.Itoa(i) // tells them apart; VMIDs may repeat
		}
		var want []proxmox.VM
		for _, vm := range vms {
			if gt, _, ok := n.ParseVMName(vm.Name); ok && gt == team && !vm.Template && !strings.HasSuffix(vm.Name, n.cfg.TemplateSuffix) && inTeamBlock(n.cfg, team, vm.VMID) {
				want = append(want, vm)
			}
		}
		got := findTeamVMs(vms, n, []string{team}, nil)
		slices.SortFunc(want, func(a, b proxmox.VM) int { return strings.Compare(a.Name, b.Name) })
		if !slices.EqualFunc(got, want, func(a, b proxmox.VM) bool { return a.Node == b.Node }) {
			t.Fatalf("pattern %q: findTeamVMs(%+v) = %+v, want %+v", n.cfg.VMName, vms, got, want)
		}
	})
}

// inTeamBlock is whether vmid is one of team's clone VMIDs: the first 100
// of its stride from clone_vmid_base.
func inTeamBlock(cfg config.Naming, team string, vmid int) bool {
	t, _ := strconv.Atoi(team)
	start := cfg.CloneVMIDBase + t*cfg.CloneVMIDTeamStride
	return vmid >= start && vmid < start+100
}

// blockVMIDs are VMIDs in, at the edges of, and just outside team's clone
// block, in the neighbouring teams' blocks, and far from any block.
func blockVMIDs(cfg config.Naming, team string) []int {
	t, _ := strconv.Atoi(team)
	start := cfg.CloneVMIDBase + t*cfg.CloneVMIDTeamStride
	return []int{start, start + 1, start + 50, start + 99, start + 100, start - 1, start + cfg.CloneVMIDTeamStride,
		start - cfg.CloneVMIDTeamStride, 100, 8000, cfg.CloneVMIDBase - 1}
}
