package pods

import (
	"testing"

	"github.com/wccomps/battleship/internal/config"
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
