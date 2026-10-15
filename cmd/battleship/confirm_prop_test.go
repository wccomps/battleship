package main

import (
	"bytes"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
)

// confirmSpace is what the confirmation trims from the typed line: ASCII
// white space and the Unicode spaces people paste.
const confirmSpace = " \t\r\v\f 　"

// confirm approves only a first line that, trimmed, is exactly what it
// asked for: yes, or the team range for a teardown, and never an empty one.
func TestPropConfirm(t *testing.T) {
	// Nothing typed never confirms a teardown with no team range.
	for _, input := range []string{"", "\n", " \n", "\r\n", "\t"} {
		for _, spec := range []string{"", " ", "\t"} {
			if confirm(deps{in: strings.NewReader(input), stdout: &bytes.Buffer{}}, string(pods.KindTeardown), spec) {
				t.Errorf("teardown with team range %q confirmed by %q", spec, input)
			}
		}
	}
	typed := []string{"yes", "YES", "Yes", "y", "ye", "yess", "yes yes", "no", "", " ", "1-32", "1-3", "1-32 ", "1,2", "01-32", "all", "ALL", "уes", "yes\x00"}
	rapid.Check(t, func(t *rapid.T) {
		cmd := rapid.SampledFrom([]string{"power", "reset", "deploy", "snapshot", "teardown"}).Draw(t, "cmd")
		spec := rapid.SampledFrom([]string{"1-32", " 1-32 ", "all", "", " ", "1,2", "yes"}).Draw(t, "spec")
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
		got := confirm(deps{in: strings.NewReader(input), stdout: &out}, cmd, spec)

		want := "yes"
		if cmd == string(pods.KindTeardown) {
			want = strings.Trim(spec, confirmSpace)
		}
		first, _, _ := strings.Cut(input, "\n")
		wantOK := strings.Trim(first, confirmSpace) == want && want != ""
		if got != wantOK {
			t.Fatalf("confirm(%q, spec %q) on %q = %v, want %v", cmd, spec, input, got, wantOK)
		}
	})
}
