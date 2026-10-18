package main

import (
	"regexp"
	"strings"
	"testing"
)

// -pods names the pods; -teams, the old name, still works and must agree.
func TestPodsFlagAndTeamsAlias(t *testing.T) {
	for _, flags := range [][]string{{"-pods", "2"}, {"-teams", "2"}, {"-pods", "2", "-teams", "2"}} {
		e := newEnv(t, false, "", twoSetPods()...)
		if code := e.run(append(append([]string{"power"}, flags...), "-action", "start", "-yes")...); code != 0 {
			t.Fatalf("%v: code = %d\n%s%s", flags, code, e.stdout, e.stderr)
		}
		if !strings.Contains(e.stdout.String(), "Plan: power, pods 02\n") {
			t.Errorf("%v: output:\n%s", flags, e.stdout)
		}
	}
	for _, tc := range []struct {
		flags []string
		want  string
	}{
		{[]string{"-pods", "1", "-teams", "2"}, "-pods and -teams (the same as -pods) differ; give one"},
		{nil, "-pods: no pods given"},
		{[]string{"-teams", ""}, "-pods: no pods given"},
		{[]string{"-pods", "x"}, `-pods: invalid pod "x"`},
	} {
		e := newEnv(t, false, "", twoSetPods()...)
		if code := e.run(append(append([]string{"power"}, tc.flags...), "-action", "start", "-yes")...); code != 2 || !strings.Contains(e.stderr.String(), tc.want) {
			t.Errorf("%v: code %d, stderr %q; want 2 and %q", tc.flags, code, e.stderr, tc.want)
		}
	}
	// -h documents -pods and -set.
	e := newEnv(t, false, "")
	e.run("teardown", "-h")
	for _, want := range []string{"-pods string\n    \tpods, e.g. 7, 1-32 or 1,3,5, or all: every pod with VMs (required)",
		"-set string\n    \tthe one template set to tear down, by name, e.g. kilo.alpha", "-teams string\n    \tthe same as -pods"} {
		if !strings.Contains(e.stderr.String(), want) {
			t.Errorf("teardown -h lacks %q:\n%s", want, e.stderr)
		}
	}
}

// The usage and each operation's help call them pods: "team" is left only
// in the -teams alias and VM names (naming.vm_name).
func TestHelpSaysPods(t *testing.T) {
	teamRE := regexp.MustCompile(`(?i)\bteams?\b`)
	texts := map[string]string{"usage": usage}
	for _, cmd := range []string{"deploy", "teardown", "reset", "power", "snapshot"} {
		e := newEnv(t, false, "")
		e.run(cmd, "-h")
		texts[cmd+" -h"] = e.stderr.String()
	}
	for name, text := range texts {
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "-teams string") || strings.Contains(line, "the same as -pods") {
				continue
			}
			line = regexp.MustCompile(`team\d{2}-\w+`).ReplaceAllString(line, "VM")
			if teamRE.MatchString(strings.ReplaceAll(line, "-teams", "")) {
				t.Errorf("%s line %d says %q", name, i+1, line)
			}
		}
	}
	if !strings.Contains(usage, "-pods is required") || !strings.Contains(texts["teardown -h"], "-set string") {
		t.Errorf("usage or teardown -h lacks -pods or -set")
	}
}
