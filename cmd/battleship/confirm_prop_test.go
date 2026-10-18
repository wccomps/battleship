package main

import (
	"bytes"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// confirmSpace is what the confirmation trims from the typed line: ASCII
// white space and the Unicode spaces people paste.
const confirmSpace = " \t\r\v\f 　"

// confirm approves only a first line that, trimmed, is exactly what it
// asked for: yes, the team range for a teardown or the domain for an
// environment's, and never an empty one.
func TestPropConfirm(t *testing.T) {
	// Nothing typed never confirms a teardown with no team range or domain.
	for _, input := range []string{"", "\n", " \n", "\r\n", "\t"} {
		for _, spec := range []string{"", " ", "\t"} {
			for _, in := range []jobs.Inputs{{Kind: pods.KindTeardown, Teams: spec}, {Kind: pods.KindEnvTeardown, Domain: spec}} {
				if confirm(deps{in: strings.NewReader(input), stdout: &bytes.Buffer{}}, in) {
					t.Errorf("%s of %q confirmed by %q", in.Kind, spec, input)
				}
			}
		}
	}
	typed := []string{"yes", "YES", "Yes", "y", "ye", "yess", "yes yes", "no", "", " ", "1-32", "1-3", "1-32 ", "1,2", "01-32", "all", "ALL", "уes", "yes\x00",
		"kilo.alpha", "kilo", "KILO.ALPHA", "kilo.alpha.", "kiloxalpha", "alpha.kilo"}
	rapid.Check(t, func(t *rapid.T) {
		cmd := rapid.SampledFrom([]string{"power", "reset", "deploy", "snapshot", "teardown", "env-teardown", "env-create"}).Draw(t, "cmd")
		spec := rapid.SampledFrom([]string{"1-32", " 1-32 ", "all", "", " ", "1,2", "yes", "kilo.alpha", " kilo.alpha "}).Draw(t, "spec")
		pad := func(label string) string {
			return strings.Join(rapid.SliceOfN(rapid.SampledFrom(strings.Split(confirmSpace, "")), 0, 2).Draw(t, label), "")
		}
		line := pad("lead") + rapid.SampledFrom(typed).Draw(t, "typed") + pad("trail")
		input := line
		switch rapid.IntRange(0, 3).Draw(t, "ending") {
		case 0:
			input += "\n"
		case 1:
			input += "\r\n"
		case 2:
			input += "\nyes\n" // later lines are never read
		case 3:
			input = "\n" + input // an empty first line
		}
		var out bytes.Buffer
		in := jobs.Inputs{Kind: pods.Kind(cmd), Teams: spec}
		if in.Kind.Teamless() {
			in = jobs.Inputs{Kind: in.Kind, Domain: spec}
		}
		got := confirm(deps{in: strings.NewReader(input), stdout: &out}, in)

		want := "yes"
		if cmd == string(pods.KindTeardown) || cmd == string(pods.KindEnvTeardown) {
			want = strings.Trim(spec, confirmSpace)
		}
		first, _, _ := strings.Cut(input, "\n")
		wantOK := strings.Trim(first, confirmSpace) == want && want != ""
		if got != wantOK {
			t.Fatalf("confirm(%q, spec %q) on %q = %v, want %v", cmd, spec, input, got, wantOK)
		}
		ask := map[string]string{"teardown": "the pod range", "env-teardown": "the domain"}[cmd]
		if prompt := "Type " + ask + " (" + want + ") to delete these VMs: "; ask != "" && !strings.Contains(out.String(), prompt) {
			t.Fatalf("%s asked %q, want %q", cmd, out.String(), prompt)
		}
	})
}
