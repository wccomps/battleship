package jobs

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// textPiece draws input text: any runes, raw bytes (so invalid UTF-8),
// control characters and regexp metacharacters.
var textPiece = rapid.OneOf(
	rapid.String(),
	rapid.Map(rapid.SliceOf(rapid.Byte()), func(b []byte) string { return string(b) }),
	rapid.SampledFrom([]string{"*", "\xff", "\xc3", "(", `\`, "[a-", ".", "\x00", "\x1b[2J", "\n", "\r", "\t", "\u0085", "\x7f",
		"dc", "kilo.alpha", "*.kilo.alpha"}),
)

func printable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// A deploy of any pattern and hosts plans or fails, never panics, and
// passes Validate only with printable text. The planner refuses a pattern
// that isn't UTF-8 even without Validate.
func TestPropDeployPatternNeverPanics(t *testing.T) {
	f := twoTeams()
	f.addVM(proxmox.VM{VMID: 5001, Name: "dc.kilo.alpha", Node: "n1", Status: "stopped", Tags: "dev"})
	p := pods.NewPlanner(f, testCfg())
	rapid.Check(t, func(t *rapid.T) {
		in := Inputs{
			Kind:    pods.KindDeploy,
			Teams:   "1",
			Pattern: strings.Join(rapid.SliceOfN(textPiece, 1, 4).Draw(t, "pattern"), ""),
			Hosts:   rapid.SliceOfN(textPiece, 0, 2).Draw(t, "hosts"),
		}
		err := in.Validate()
		if err == nil {
			for _, s := range append([]string{in.Pattern}, in.Hosts...) {
				if !printable(s) {
					t.Fatalf("Validate accepted %q", s)
				}
			}
		}
		if _, berr := BuildPlan(bg, p, in); err != nil && berr == nil {
			t.Fatalf("BuildPlan planned what Validate refused: %v", err)
		}
		_, derr := p.Deploy(bg, pods.DeployRequest{Pattern: in.Pattern, Teams: []string{"01"}, Hosts: in.Hosts})
		if !utf8.ValidString(in.Pattern) && derr == nil {
			t.Fatalf("Deploy planned invalid UTF-8 pattern %q", in.Pattern)
		}
	})
}

// validInputs has one valid Inputs of each kind.
var validInputs = []Inputs{
	{Kind: pods.KindDeploy, Teams: "1", Hosts: []string{"dc"}, VMs: []string{"team01-dc"}, Pattern: "*.x"},
	{Kind: pods.KindTeardown, Teams: "1-2"},
	{Kind: pods.KindReset, Teams: "1", Snapshot: "initial"},
	{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "round2", Description: "before round 2"},
	{Kind: pods.KindPower, Teams: "1", Action: "stop"},
	{Kind: pods.KindEnvCreate, Domain: "kilo.alpha", Router: "gw=opnsense", EnvHosts: []string{"dc=windows"}},
	{Kind: pods.KindEnvAdd, Domain: "kilo.alpha", EnvHosts: []string{"web=debian:20"}},
	{Kind: pods.KindEnvDNS, Domain: "kilo.alpha", DNS: "dc"},
	{Kind: pods.KindImageRegister, VMID: 100, Family: "debian-12", ImageKind: pods.ImageKinds[0]},
	{Kind: pods.KindImageEdit, Family: "debian-12"},
	{Kind: pods.KindImageDeprecate, Image: "debian-12-v1"},
	{Kind: pods.KindImageDelete, Image: "debian-12-v1"},
}

// Validate refuses any string field, of any kind, that holds a control
// character or invalid UTF-8.
func TestPropValidateRefusesUnprintableText(t *testing.T) {
	for _, in := range validInputs {
		if err := in.Validate(); err != nil {
			t.Fatalf("%s: %v", in.Kind, err)
		}
	}
	bad := rapid.SampledFrom([]string{"\x00", "\x1b", "\n", "\r", "\t", "\x7f", "\u0085", "\u009b", "\xff", "\xc3"})
	rapid.Check(t, func(t *rapid.T) {
		in := rapid.SampledFrom(validInputs).Draw(t, "inputs")
		in.Hosts, in.VMs, in.EnvHosts = slices.Clone(in.Hosts), slices.Clone(in.VMs), slices.Clone(in.EnvHosts)
		v := reflect.ValueOf(&in).Elem()
		var fields []reflect.Value
		for i := range v.NumField() {
			f := v.Field(i)
			switch {
			case f.Kind() == reflect.String && f.Type() == reflect.TypeFor[string]():
				fields = append(fields, f)
			case f.Kind() == reflect.Slice:
				if f.Len() == 0 {
					f.Set(reflect.ValueOf([]string{"x"}))
				}
				fields = append(fields, f.Index(rapid.IntRange(0, f.Len()-1).Draw(t, "index")))
			}
		}
		f := fields[rapid.IntRange(0, len(fields)-1).Draw(t, "field")]
		s := f.String()
		at := rapid.IntRange(0, len(s)).Draw(t, "at")
		f.SetString(s[:at] + bad.Draw(t, "bad") + s[at:])
		if err := in.Validate(); err == nil {
			t.Fatalf("Validate accepted %+v", in)
		}
	})
}

// A refused value is quoted only in part, so pages and logs stay short.
func TestValidateBoundsRefusedText(t *testing.T) {
	in := Inputs{Kind: pods.KindSnapshot, Teams: "1", Snapshot: "round2", Description: strings.Repeat("x", 5000) + "\x1b"}
	if err := in.Validate(); err == nil || len(err.Error()) > 200 {
		t.Errorf("Validate = %d bytes: %v", len(fmt.Sprint(err)), err)
	}
}
