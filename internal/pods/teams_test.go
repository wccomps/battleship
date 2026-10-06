package pods

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/config"
)

// battleship serve waits web.shutdown_timeout for running jobs to stop, so by
// default it must outlast the longest a stopped Run takes.
func TestDefaultShutdownOutlastsStop(t *testing.T) {
	if got := config.Default().Web.ShutdownTimeout; got <= StopBudget {
		t.Errorf("default web.shutdown_timeout = %s, want more than the %s stop budget", got, StopBudget)
	}
}

func TestParseTeams(t *testing.T) {
	cases := []struct {
		spec    string
		want    []string
		wantErr string
	}{
		{"7", []string{"07"}, ""},
		{"1-3", []string{"01", "02", "03"}, ""},
		{"3,1,2", []string{"01", "02", "03"}, ""},
		{"1-2,2,9", []string{"01", "02", "09"}, ""},
		{"0-0", []string{"00"}, ""},
		{" 1 - 3 ", []string{"01", "02", "03"}, ""},
		{"99", []string{"99"}, ""},
		{"", nil, "no teams given"},
		{"1,", nil, "extra comma"},
		{"+3", nil, "invalid team"},
		{"-5", nil, "invalid team"},
		{"5-", nil, "invalid team"},
		{"1--3", nil, "invalid team"},
		{"99999999999999999999", nil, "team 99999999999999999999 is out of range"},
		{"150", nil, "team 150 is out of range"},
		{"1-100", nil, "team 100 is out of range"},
		{"5-1", nil, "backwards"},
		{"a", nil, "invalid team"},
		{"1,,2", nil, "extra comma"},
		{"1-10,12-", nil, `in "12-"`},
	}
	for _, tc := range cases {
		got, err := ParseTeams(tc.spec)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseTeams(%q) = %v; want %v", tc.spec, got, tc.want)
		}
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("ParseTeams(%q) error = %v; want nil", tc.spec, err)
			}
		} else {
			if err == nil {
				t.Errorf("ParseTeams(%q) error = nil; want error containing %q", tc.spec, tc.wantErr)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseTeams(%q) error = %v; want to contain %q", tc.spec, err, tc.wantErr)
			}
		}
	}
}

func TestFormatTeam(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "00"},
		{7, "07"},
		{42, "42"},
	}
	for _, tc := range cases {
		got := FormatTeam(tc.n)
		if got != tc.want {
			t.Errorf("FormatTeam(%d) = %q; want %q", tc.n, got, tc.want)
		}
	}
}

func TestParseTeamsRange(t *testing.T) {
	got, err := ParseTeams("0-99")
	if err != nil {
		t.Fatalf("ParseTeams(\"0-99\") error = %v", err)
	}
	if len(got) != 100 {
		t.Errorf("ParseTeams(\"0-99\") length = %d; want 100", len(got))
	}
	if got[0] != "00" {
		t.Errorf("ParseTeams(\"0-99\")[0] = %q; want \"00\"", got[0])
	}
	if got[99] != "99" {
		t.Errorf("ParseTeams(\"0-99\")[99] = %q; want \"99\"", got[99])
	}
}

// The shipped Kubernetes config keeps the chain StopBudget <
// web.shutdown_timeout < terminationGracePeriodSeconds, so a rolling update
// never kills a replica while its jobs stop.
func TestKubernetesShutdownChain(t *testing.T) {
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	m := regexp.MustCompile(`terminationGracePeriodSeconds: (\d+)`).FindStringSubmatch(read("../../deploy/k8s/base/deployment.yaml"))
	if m == nil {
		t.Fatal("deployment.yaml sets no terminationGracePeriodSeconds")
	}
	secs, _ := strconv.Atoi(m[1])
	grace := time.Duration(secs) * time.Second
	for _, path := range []string{"../../deploy/k8s/base/battleship.toml", "../../deploy/k8s/overlays/example/battleship.toml"} {
		var file struct {
			Web struct {
				ShutdownTimeout string `toml:"shutdown_timeout"`
			} `toml:"web"`
		}
		if _, err := toml.DecodeFile(path, &file); err != nil {
			t.Fatal(err)
		}
		d, err := time.ParseDuration(file.Web.ShutdownTimeout)
		if err != nil {
			t.Fatalf("%s: web.shutdown_timeout: %v", path, err)
		}
		if d <= StopBudget || d >= grace {
			t.Errorf("%s: shutdown_timeout %s, want between the %s stop budget and the %s grace period", path, d, StopBudget, grace)
		}
	}
}

func TestFormatTeams(t *testing.T) {
	for _, tc := range []struct {
		teams []string
		want  string
	}{
		{[]string{"07"}, "7"},
		{[]string{"00"}, "0"},
		{[]string{"01", "02", "03"}, "1-3"},
		{[]string{"01", "03", "05"}, "1,3,5"},
		{[]string{"01", "02", "03", "07", "09", "10"}, "1-3,7,9-10"},
		{[]string{"03", "01", "02", "02"}, "1-3"},
	} {
		if got := FormatTeams(tc.teams); got != tc.want {
			t.Errorf("FormatTeams(%v) = %q, want %q", tc.teams, got, tc.want)
		}
	}
}

// FormatTeams is the inverse of ParseTeams: any set of teams comes back
// the same.
func TestFormatTeamsRoundTrips(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nums := rapid.SliceOfNDistinct(rapid.IntRange(0, 99), 1, 40, rapid.ID[int]).Draw(t, "teams")
		var teams []string
		for _, n := range nums {
			teams = append(teams, FormatTeam(n))
		}
		spec := FormatTeams(teams)
		got, err := ParseTeams(spec)
		if err != nil {
			t.Fatalf("ParseTeams(%q): %v", spec, err)
		}
		want := slices.Sorted(slices.Values(teams))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("FormatTeams(%v) = %q, parses to %v", teams, spec, got)
		}
	})
}
