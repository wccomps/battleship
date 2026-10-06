package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// drawTeamSpec draws a team range as people type it ("1-32", "3,7",
// "12-14", " 07 - 9 ,3") and the teams it means.
func drawTeamSpec(t *rapid.T) (string, []string) {
	var parts []string
	set := map[int]bool{}
	for i := range rapid.IntRange(1, 5).Draw(t, "parts") {
		lo := rapid.IntRange(0, 99).Draw(t, fmt.Sprintf("part %d", i))
		hi := lo
		if rapid.Bool().Draw(t, fmt.Sprintf("part %d range", i)) {
			hi = rapid.IntRange(lo, min(lo+20, 99)).Draw(t, fmt.Sprintf("part %d end", i))
		}
		num := func(n int, label string) string {
			s := strconv.Itoa(n)
			if rapid.Bool().Draw(t, label+" padded") {
				s = pods.FormatTeam(n)
			}
			return s
		}
		pad := func(label string) string { return rapid.SampledFrom([]string{"", " ", "  "}).Draw(t, label) }
		p := num(lo, "lo")
		if hi != lo || rapid.IntRange(0, 4).Draw(t, "n-n") == 0 {
			p += pad("before dash") + "-" + pad("after dash") + num(hi, "hi")
		}
		parts = append(parts, pad("before")+p+pad("after"))
		for n := lo; n <= hi; n++ {
			set[n] = true
		}
	}
	var teams []string
	for n := range set {
		teams = append(teams, pods.FormatTeam(n))
	}
	slices.Sort(teams)
	return strings.Join(parts, ","), teams
}

// A typed team range means the teams it names, and both ways battleship
// writes ranges back (the pages' "01-03, 07" and a retry's "1-3,7") mean
// exactly the same teams again.
func TestPropTeamRangeRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		spec, want := drawTeamSpec(t)
		got, err := pods.ParseTeams(spec)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("ParseTeams(%q) = %v, %v; want %v", spec, got, err, want)
		}
		shown := teamsText(spec)
		if again, err := pods.ParseTeams(shown); err != nil || !slices.Equal(again, want) {
			t.Fatalf("teamsText(%q) = %q, which parses as %v, %v", spec, shown, again, err)
		}
		if teamsText(shown) != shown {
			t.Fatalf("teamsText isn't stable: %q, then %q", shown, teamsText(shown))
		}
		compact := pods.FormatTeams(want)
		if again, err := pods.ParseTeams(compact); err != nil || !slices.Equal(again, want) {
			t.Fatalf("FormatTeams(%v) = %q, which parses as %v, %v", want, compact, again, err)
		}
		if teamsText(compact) != shown {
			t.Fatalf("the same teams show as %q and %q", teamsText(compact), shown)
		}
	})
}

// Any text in the teams field either fails to parse, with an error, or
// means sorted, distinct teams 00-99; the pages show text that doesn't
// parse as it is.
func TestPropTeamRangeGarbage(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		spec := rapid.OneOf(rapid.String(), rapid.StringMatching(`[0-9 ,-]{0,12}`), rapid.StringMatching(`[0-9+x. ,-]{0,12}`)).Draw(t, "spec")
		teams, err := pods.ParseTeams(spec)
		if err != nil {
			if teams != nil || err.Error() == "" {
				t.Fatalf("ParseTeams(%q) = %v, %q", spec, teams, err)
			}
			if teamsText(spec) != spec {
				t.Fatalf("teamsText(%q) = %q for a range that doesn't parse", spec, teamsText(spec))
			}
			return
		}
		if len(teams) == 0 {
			t.Fatalf("ParseTeams(%q) means no teams, without an error", spec)
		}
		for i, team := range teams {
			n, err := strconv.Atoi(team)
			if err != nil || len(team) != 2 || n < 0 || n > 99 || (i > 0 && team <= teams[i-1]) {
				t.Fatalf("ParseTeams(%q) = %v", spec, teams)
			}
		}
	})
}

// The hosts field comes as one value per ticked box, as a typed list, or
// both. However the boxes are ordered, repeated or split, the plan is the
// same: the same VMs, the same steps, the same fingerprint.
func TestPropHostsFieldOrderDoesNotChangeThePlan(t *testing.T) {
	cfg := testConfig()
	api := newFakeAPI()
	hosts := []string{"dc", "web", "ftp", "db"}
	for i, team := range []string{"01", "02", "03"} {
		for j, host := range hosts {
			api.add(teamVM(team, host, 10000+(i+1)*100+j+1), cleanConfig(team), "initial")
		}
	}
	planner := pods.NewPlanner(api, cfg)
	ctx := context.Background()
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.SampledFrom([]pods.Kind{pods.KindPower, pods.KindReset, pods.KindTeardown}).Draw(t, "kind")
		chosen := rapid.SliceOfNDistinct(rapid.SampledFrom(append(hosts, "nosuch")), 1, 5, rapid.ID).Draw(t, "hosts")
		base := url.Values{"teams": {rapid.SampledFrom([]string{"1", "1-3", "2,3"}).Draw(t, "teams")}, "action": {"start"}}
		one := url.Values{}
		for k, v := range base {
			one[k] = v
		}
		one["hosts"] = chosen

		// The same hosts again: shuffled, some repeated, as boxes or
		// typed with spaces, split over several values.
		again := slices.Clone(chosen)
		for i := range again {
			j := rapid.IntRange(0, len(again)-1).Draw(t, "swap")
			again[i], again[j] = again[j], again[i]
		}
		again = append(again, rapid.SliceOfN(rapid.SampledFrom(chosen), 0, 3).Draw(t, "repeats")...)
		var values []string
		for len(again) > 0 {
			n := rapid.IntRange(1, len(again)).Draw(t, "per value")
			values = append(values, " "+strings.Join(again[:n], " , ")+" ")
			again = again[n:]
		}
		two := url.Values{}
		for k, v := range base {
			two[k] = v
		}
		two["hosts"] = values

		a, b := formInputs(kind, one), formInputs(kind, two)
		pa, errA := jobs.BuildPlan(ctx, planner, a)
		pb, errB := jobs.BuildPlan(ctx, planner, b)
		if (errA == nil) != (errB == nil) {
			t.Fatalf("hosts %q plan with %v; %q with %v", one["hosts"], errA, two["hosts"], errB)
		}
		if errA != nil {
			return
		}
		names := func(p *pods.Plan) []string {
			var out []string
			for _, it := range p.Items {
				out = append(out, it.Name+" "+stepsText(it)+" "+it.Blocked)
			}
			return out
		}
		if !slices.Equal(names(pa), names(pb)) {
			t.Fatalf("hosts %q plan %v; %q plan %v", one["hosts"], names(pa), two["hosts"], names(pb))
		}
		if jobs.Fingerprint(pa) != jobs.Fingerprint(pb) {
			t.Fatalf("hosts %q and %q plan the same VMs with different fingerprints", one["hosts"], two["hosts"])
		}
	})
}

// A teardown is confirmed by typing the team range: the confirm takes
// exactly the range as the form had it (spaces around it aside), and
// anything else submits nothing.
func TestPropTypedConfirm(t *testing.T) {
	h := newHarness(t)
	h.poll()
	lead := h.login(asLead)
	rapid.Check(t, func(t *rapid.T) {
		teams := rapid.SampledFrom([]string{"1", "1-3", "2,3", "1-2", "3"}).Draw(t, "teams")
		path := "/teardown"
		form := url.Values{"teams": {teams}}
		rec := h.post(&lead, path+"/preview", form)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s/preview = %d\n%s", path, rec.Code, rec.Body)
		}
		m := confirmFormRE.FindStringSubmatch(rec.Body.String())
		if m == nil {
			t.Fatalf("no confirm form:\n%s", rec.Body)
		}
		if !strings.Contains(m[2], `name="typed"`) {
			t.Fatalf("%s of teams %s doesn't ask for the range to be typed", path, teams)
		}
		_, fields := confirmOf(h.t, rec.Body.String())

		typed := rapid.OneOf(
			rapid.Just(teams),
			rapid.Custom(func(t *rapid.T) string {
				return rapid.SampledFrom([]string{" ", "\t", ""}).Draw(t, "lead") + teams + rapid.SampledFrom([]string{" ", "\n", ""}).Draw(t, "trail")
			}),
			rapid.Just(teamsText(teams)),
			rapid.Just(pods.FormatTeams(mustTeams(t, teams))),
			rapid.SampledFrom([]string{"", "1-3", "01-03", "1,2,3", "all", "yes", strings.ToUpper(teams) + "x"}),
			rapid.StringMatching(`[0-9 ,-]{0,6}`),
		).Draw(t, "typed")
		fields.Set("typed", typed)
		before := len(h.jobsInStore())
		rec = h.post(&lead, m[1], fields)
		after := len(h.jobsInStore())
		ok := strings.TrimSpace(typed) == teams
		switch {
		case ok && (rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/logs/") || after != before+1):
			t.Fatalf("typed %q for %q: %d to %q, %d jobs more", typed, teams, rec.Code, rec.Header().Get("Location"), after-before)
		case !ok && (rec.Code != http.StatusUnprocessableEntity || after != before):
			t.Fatalf("typed %q for %q: %d, %d jobs more; want 422 and none", typed, teams, rec.Code, after-before)
		}
	})
}

func mustTeams(t *rapid.T, spec string) []string {
	teams, err := pods.ParseTeams(spec)
	if err != nil {
		t.Fatal(err)
	}
	return teams
}
