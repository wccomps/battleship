package jobs

import (
	"encoding/json"
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
	{Kind: pods.KindEnvCreate, Domain: "kilo.alpha", Slot: "02", Router: "gw=opnsense", EnvHosts: []string{"dc=windows"}},
	{Kind: pods.KindEnvAdd, Domain: "kilo.alpha", EnvHosts: []string{"web=debian:20"}},
	{Kind: pods.KindEnvDNS, Domain: "kilo.alpha", DNS: "dc"},
	{Kind: pods.KindEnvTeardown, Domain: "kilo.alpha"},
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

// spoil is a value for one field and whether the kinds that read the field
// accept it.
type spoil struct {
	value any
	ok    bool
}

// spoils are near misses for each field a kind reads, labeled by hand.
var spoils = map[string][]spoil{
	"Teams": {{"1", true}, {" 01 - 02 ", true}, {"0", true}, {"1,2", true}, {"99", true}, {"100", false}, {"", false}, {" ", false},
		{"1-", false}, {"2-1", false}, {"1,,2", false}, {"a", false}, {"-1", false}, {"1\n", false}, {"\u0661", false}},
	"Hosts": {{[]string{"dc"}, true}, {[]string{"dc", "web"}, true}, {[]string(nil), true}, {[]string{""}, false}, {[]string{" "}, false},
		{[]string{"dc", ""}, false}, {[]string{"\x1b[2J"}, false}, {[]string{"d\xffc"}, false}},
	"VMs": {{[]string{"team01-dc"}, true}, {[]string(nil), true}, {[]string{""}, false}, {[]string{" "}, false}, {[]string{"team01-dc\n"}, false}},
	"Pattern": {{"*.kilo.alpha", true}, {"*", true}, {"(", true}, {"[a-", true}, {`\`, true}, {"", false}, {" ", false}, {"\xff", false},
		{"*\x00", false}},
	"Snapshot": {{"round2", true}, {"Ab", true}, {"a_b-c", true}, {strings.Repeat("a", 40), true}, {"", false}, {"a", false}, {"1abc", false},
		{"a b", false}, {strings.Repeat("a", 41), false}, {"current", false}, {"PENDING", false}, {"r\u00e9union", false}, {"ab\n", false}},
	"Description": {{"", true}, {"before round 2", true}, {strings.Repeat("\u00e9", 500), true}, {strings.Repeat("x", 501), false},
		{"a\r\nb", false}, {"a\x00", false}},
	"Rebuild": {{true, true}, {false, true}}, "NoSnapshot": {{true, true}, {false, true}}, "VMState": {{true, true}, {false, true}},
	"Action": {{"start", true}, {"shutdown", true}, {"stop", true}, {"reboot", true}, {"Start", false}, {" stop", false}, {"kill", false},
		{"", false}},
	"Domain": {{"kilo.alpha", true}, {" kilo.alpha ", true}, {"a.b.c", true}, {"kilo", false}, {"Kilo.alpha", false}, {"kilo..alpha", false},
		{"kilo.alpha.", false}, {"", false}, {"ki_lo.alpha", false}},
	"Router": {{"gw=gw", true}, {"gw=opnsense:10", true}, {" gw = gw ", true}, {"gw=WIN 2022", false}, {"gw", false}, {"=gw", false},
		{"gw=gw:3", false}, {"gw=", false}, {"Gw=gw", false}, {"gw=gw-v1", false}},
	"EnvHosts": {{[]string{"dc=debian-12"}, true}, {[]string{"dc=debian-12:20", "web=win2022"}, true}, {[]string{"dc=WIN 2022"}, false},
		{[]string{"dc"}, false}, {[]string{""}, false}, {[]string{"dc=debian-12:255"}, false}, {[]string{"Dc=x"}, false},
		{[]string{"dc=debian-12", "x"}, false}},
	"Slot": {{"02", true}, {"", true}, {"10", true}, {"100", true}, {"2", false}, {"00", false}, {"002", false}, {"dev-02", false},
		{" 02", false}, {"-1", false}, {"+2", false}, {"0x2", false}, {"\u0662", false}},
	// An environment's VMs, by full name; the inputs' domain is kilo.alpha.
	"EnvVMs": {{[]string{"dc.kilo.alpha"}, true}, {[]string{"gw.kilo.alpha", "dc.kilo.alpha"}, true}, {[]string(nil), true},
		{[]string{"team01-dc"}, false}, {[]string{"dc.kilo.bravo"}, false}, {[]string{"dc"}, false}, {[]string{""}, false},
		{[]string{"Dc.kilo.alpha"}, false}, {[]string{" dc.kilo.alpha"}, false}, {[]string{"dc.kilo.alpha", "x.y"}, false}},
	"DNS":  {{"router", true}, {"dc", true}, {"", true}, {" dc ", true}, {"Dc", false}, {"-x", false}, {"a b", false}, {"dc.kilo", false}},
	"VMID": {{100, true}, {150, true}, {1 << 40, true}, {99, false}, {0, false}, {-1, false}},
	"Family": {{"debian-12", true}, {"x-v0", true}, {"WIN 2022", false}, {"debian-12-v1", false}, {"", false}, {" debian-12", false},
		{"Debian-12", false}, {strings.Repeat("a", 41), false}},
	"ImageKind": {{"linux", true}, {"", true}, {"router", true}, {"Linux", false}, {"toaster", false}, {" linux", false}},
	"Image": {{"debian-12-v1", true}, {"x-v9999", true}, {"debian-12-v0", false}, {"debian-12", false}, {"debian-12-v10000", false},
		{" debian-12-v1", false}, {"debian-12-v01", false}, {"Debian-12-v1", false}},
}

// readsFields are the fields each kind reads, beyond a team kind's Teams,
// Hosts and VMs. Some are optional for a kind and labeled for another: a
// reset's snapshot, an env-add's router.
var readsFields = map[pods.Kind][]string{
	pods.KindDeploy: {"Pattern", "Rebuild", "NoSnapshot"}, pods.KindPower: {"Action"}, pods.KindSnapshot: {"Snapshot", "Description", "VMState"},
	pods.KindTeardown: {}, pods.KindReset: {},
	pods.KindEnvCreate: {"Domain", "Slot", "Router", "EnvHosts", "EnvVMs"}, pods.KindEnvAdd: {"Domain", "EnvHosts", "EnvVMs"}, pods.KindEnvDNS: {"Domain", "DNS"},
	pods.KindEnvTeardown:   {"Domain", "Slot"},
	pods.KindImageRegister: {"VMID", "Family", "ImageKind"}, pods.KindImageEdit: {"Family"},
	pods.KindImageDeprecate: {"Image"}, pods.KindImageDelete: {"Image"},
}

// gateCluster has two teams, a master to deploy, a VM to register and base
// images to build environments from.
func gateCluster() pods.Planner {
	f := twoTeams()
	f.addVM(proxmox.VM{VMID: 5001, Name: "dc.kilo.alpha", Node: "n1", Status: "stopped", Tags: "dev"})
	f.Add(proxmox.VM{VMID: 150, Name: "fresh", Node: "n1", Status: "stopped"}, map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:01:50,bridge=vmbr0"})
	f.Add(proxmox.VM{VMID: 8000, Name: "debian-12-v1", Node: "n1", Template: true, Tags: "base;linux"}, map[string]string{"agent": "1"})
	f.Add(proxmox.VM{VMID: 8001, Name: "gw-v1", Node: "n1", Template: true, Tags: "base;router"}, nil)
	return pods.NewPlanner(f, testCfg())
}

func checkGate(t interface{ Fatalf(string, ...any) }, p pods.Planner, in Inputs, wantOK bool) {
	err := in.Validate()
	if (err == nil) != wantOK {
		t.Fatalf("Validate(%#v) = %v, want ok %v", in, err, wantOK)
	}
	if err != nil {
		return
	}
	data, merr := json.Marshal(in)
	var back Inputs
	if uerr := json.Unmarshal(data, &back); merr != nil || uerr != nil || !reflect.DeepEqual(back, in) || back.Validate() != nil {
		t.Fatalf("%#v reads back from its job as %#v (%v, %v)", in, back, merr, uerr)
	}
	if in.Kind == pods.KindEnvCreate || in.Kind == pods.KindEnvAdd || in.Kind == pods.KindEnvDNS || in.Kind == pods.KindEnvTeardown {
		if _, err := in.EnvRequest(); err != nil {
			t.Fatalf("EnvRequest(%#v): %v", in, err)
		}
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("BuildPlan(%#v) panicked: %v", in, r)
			}
		}()
		_, _ = BuildPlan(bg, p, in)
	}()
}

// setField sets in's field to v, cloning slices.
func setField(in *Inputs, field string, v any) {
	if field == "EnvVMs" {
		field = "VMs"
	}
	f := reflect.ValueOf(in).Elem().FieldByName(field)
	if s, ok := v.([]string); ok {
		v = slices.Clone(s)
	}
	f.Set(reflect.ValueOf(v))
}

// Validate is the gate: a valid job of any kind with one field spoilt is
// refused just when the spoilt value is, and what passes reads back from
// the job record the same and plans without panicking.
func TestPropValidateIsTheGate(t *testing.T) {
	p := gateCluster()
	fieldsOf := func(kind pods.Kind) []string {
		if kind.Teamless() {
			return readsFields[kind]
		}
		return append([]string{"Teams", "Hosts", "VMs"}, readsFields[kind]...)
	}
	for _, base := range validInputs {
		for _, field := range fieldsOf(base.Kind) {
			for _, s := range spoils[field] {
				in := base
				setField(&in, field, s.value)
				checkGate(t, p, in, s.ok)
			}
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		in := rapid.SampledFrom(validInputs).Draw(t, "inputs")
		field := rapid.SampledFrom(fieldsOf(in.Kind)).Draw(t, "field")
		s := rapid.SampledFrom(spoils[field]).Draw(t, "spoil")
		setField(&in, field, s.value)
		// Any text around a valid value, in one string field.
		if str, ok := s.value.(string); ok && rapid.IntRange(0, 3).Draw(t, "wrap") == 0 {
			setField(&in, field, textPiece.Draw(t, "before")+str+textPiece.Draw(t, "after"))
			if err := in.Validate(); err == nil {
				checkGate(t, p, in, true)
			}
			return
		}
		checkGate(t, p, in, s.ok)
	})
}

// An environment teardown's domain, typed anyhow, passes Validate just
// when it is printable and, trimmed, a domain; what passes is the text to
// type to confirm, and plans only that domain's VMs.
func TestPropEnvTeardownDomain(t *testing.T) {
	f := twoTeams()
	for _, vm := range []proxmox.VM{
		{VMID: 2100, Name: "gw.kilo.alpha", Node: "n1", Pool: "dev-01", Tags: "dev;kilo.alpha;router"},
		{VMID: 2101, Name: "dc.kilo.alpha", Node: "n1", Pool: "dev-01"},
		{VMID: 2200, Name: "dc.kilo.bravo", Node: "n1", Pool: "dev-02"},
	} {
		f.Add(vm, nil)
	}
	p := pods.NewPlanner(f, testCfg())
	rapid.Check(t, func(t *rapid.T) {
		domain := strings.Join(rapid.SliceOfN(rapid.OneOf(textPiece, rapid.SampledFrom([]string{"kilo.alpha", "kilo.bravo", " ", "KILO", ".alpha"})), 1, 3).Draw(t, "domain"), "")
		in := Inputs{Kind: pods.KindEnvTeardown, Domain: domain}
		err := in.Validate()
		if wantOK := printable(domain) && pods.CheckDomain(strings.TrimSpace(domain)) == nil; (err == nil) != wantOK {
			t.Fatalf("Validate(%q) = %v, want ok %v", domain, err, wantOK)
		}
		plan, berr := BuildPlan(bg, p, in)
		if err != nil {
			if berr == nil {
				t.Fatalf("BuildPlan planned %q, which Validate refused: %v", domain, err)
			}
			return
		}
		if in.Typed() != strings.TrimSpace(domain) {
			t.Fatalf("Typed() = %q for %q", in.Typed(), domain)
		}
		if berr != nil {
			return // no slot holds it
		}
		for _, it := range plan.Items {
			if plan.Kind != pods.KindEnvTeardown || !slices.Contains(it.Steps, pods.StepDelete) || pods.Domain(it.Name) != strings.TrimSpace(domain) {
				t.Fatalf("a teardown of %q plans %s", domain, it.Name)
			}
		}
		if len(plan.Items) == 0 {
			t.Fatalf("a teardown of %q plans nothing", domain)
		}
	})
}
