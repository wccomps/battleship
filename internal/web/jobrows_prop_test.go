package web

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// allKinds are the operation kinds, and one battleship doesn't know.
var allKinds = []pods.Kind{pods.KindPower, pods.KindReset, pods.KindSnapshot, pods.KindDeploy, pods.KindTeardown,
	pods.KindEnvCreate, pods.KindEnvAdd, pods.KindEnvTeardown, pods.KindEnvEdit, pods.KindEnvPower, pods.KindEnvDeleteHost,
	pods.KindImageRegister, pods.KindImageEdit, pods.KindImageDeprecate, pods.KindImageDelete, pods.KindImageDetails, "x"}

var (
	clockRE = regexp.MustCompile(`^(\d+):(\d\d)$|^(\d+):(\d\d):(\d\d)$`)
	tickRE  = regexp.MustCompile(`<time data-tick="(-?\d+)">([^<]*)</time>`)
)

// clockText is whole seconds as m:ss under an hour, else h:mm:ss, and
// reads back as the time it was given, down to the second.
func TestPropClockText(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d := time.Duration(rapid.Int64Range(-int64(time.Hour), int64(200*time.Hour)).Draw(t, "d"))
		got := clockText(d)
		m := clockRE.FindStringSubmatch(got)
		if m == nil {
			t.Fatalf("clockText(%v) = %q", d, got)
		}
		n := func(s string) int { v, _ := strconv.Atoi(s); return v }
		secs := n(m[1])*60 + n(m[2])
		if m[3] != "" {
			secs = n(m[3])*3600 + n(m[4])*60 + n(m[5])
			if secs < 3600 || n(m[4]) > 59 {
				t.Fatalf("clockText(%v) = %q: hours under an hour, or 60+ minutes", d, got)
			}
		} else if secs >= 3600 {
			t.Fatalf("clockText(%v) = %q: an hour or more as minutes", d, got)
		}
		if want := max(int(d/time.Second), 0); secs != want {
			t.Fatalf("clockText(%v) = %q reads %d s, want %d", d, got, secs, want)
		}
	})
}

// A job's VM table, whatever its VMs, steps, notes and errors, is safe
// markup with a row per VM; only a running VM of a running job has a time,
// which is its step's age and ticks from when that step began.
func TestPropJobRowsEscapeAndTime(t *testing.T) {
	base := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	stepNames := make([]pods.Step, 0, len(stepDoing))
	for st := range stepDoing {
		stepNames = append(stepNames, st)
	}
	slices.Sort(stepNames)
	itemStatuses := []string{store.ItemPending, store.ItemRunning, store.ItemDone, store.ItemFailed, store.ItemBlocked,
		store.ItemInterrupted, store.ItemRemoved, store.ItemNotRun}
	jobStatuses := []string{store.StatusPending, store.StatusRunning, store.StatusSucceeded, store.StatusCompletedWithFailures,
		store.StatusFailed, store.StatusCancelled, store.StatusInterrupted, store.StatusStale}
	outcomes := []string{"done", "skipped", "failed", "interrupted"}
	rapid.Check(t, func(t *rapid.T) {
		now := drawTime(t, base, "now")
		j := store.Job{ID: 7, Kind: string(rapid.SampledFrom(allKinds).Draw(t, "kind")),
			Status: rapid.SampledFrom(jobStatuses).Draw(t, "job status")}
		var plan pods.Plan
		var items []store.Item
		for i := range rapid.IntRange(0, 20).Draw(t, "n") {
			name := hostile(t, "name")
			if rapid.Bool().Draw(t, "unique") {
				name += "-" + strconv.Itoa(i) // most names differ; some collide
			}
			it := store.Item{Name: name, VMID: rapid.IntRange(-1, 9999).Draw(t, "vmid"), Status: rapid.SampledFrom(append(itemStatuses, "odd")).Draw(t, "status"),
				Error: hostile(t, "error"), Note: hostile(t, "note"), Steps: map[string]string{}}
			if rapid.Bool().Draw(t, "began") {
				it.StepAt = drawTime(t, base, "step at")
			}
			pi := pods.Item{Name: name, Action: rapid.SampledFrom([]string{"start", "stop", "shutdown", "reboot", ""}).Draw(t, "action")}
			for range rapid.IntRange(0, 6).Draw(t, "steps") {
				st := rapid.OneOf(rapid.SampledFrom(stepNames), rapid.Map(rapid.String(), func(s string) pods.Step { return pods.Step(s) })).Draw(t, "step")
				pi.Steps = append(pi.Steps, st)
				if rapid.Bool().Draw(t, "reported") {
					it.Steps[string(st)] = rapid.SampledFrom(append(outcomes, "odd")).Draw(t, "outcome")
				}
			}
			switch rapid.IntRange(0, 2).Draw(t, "address") {
			case 1:
				pi.Guest = &pods.Guest{IP: hostileAddr(t, "ip")}
			case 2:
				pi.Notes = &pods.Notes{IP: hostileAddr(t, "ip")}
			}
			if rapid.Bool().Draw(t, "planned") {
				plan.Items = append(plan.Items, pi)
			}
			items = append(items, it)
		}
		var err error
		if j.Plan, err = json.Marshal(plan); err != nil {
			t.Fatal(err)
		}
		tbl := jobItemsOf(j, items, now)
		h, err := fragment("job", "job-items", jobView{Items: tbl})
		if err != nil {
			t.Fatal(err)
		}
		tags, err := checkMarkup(h)
		if err != nil {
			t.Fatal(err)
		}
		rows := 0
		for _, tag := range tags {
			if tag.Name == "script" {
				t.Fatal("a script in the VM table")
			}
			if tag.Name == "tr" && !strings.Contains(tag.Attrs["class"], "more") {
				rows++
			}
		}
		if len(items) > 0 && rows != len(items)+1 { // and the header
			t.Fatalf("%d rows for %d VMs", rows-1, len(items))
		}
		ticking := 0
		for _, r := range tbl.Rows {
			if r.For == "" {
				continue
			}
			ticking++
			if j.Status != store.StatusRunning || r.status != store.ItemRunning {
				t.Fatalf("a %s VM of a %s job has a time %q", r.status, j.Status, r.For)
			}
			// Since is the step's start cut to the millisecond, so the time
			// on it may read up to a millisecond less.
			began := time.UnixMilli(r.Since)
			if want := clockText(now.Sub(began)); r.For != want && clockText(now.Sub(began)-time.Millisecond) != r.For {
				t.Fatalf("time %q, want %q for a step begun %v at %v", r.For, want, began, now)
			}
		}
		if got := len(tickRE.FindAllString(h, -1)); got != ticking {
			t.Fatalf("%d ticking times in the markup, %d rows with one", got, ticking)
		}
		sum := 0
		for _, p := range tbl.Tally {
			sum += p.N
		}
		if sum != len(items) || len(tbl.Pips) != len(items) {
			t.Fatalf("tally %v and %d pips for %d VMs", tbl.Tally, len(tbl.Pips), len(items))
		}
	})
}

// A finished job's Open button, whatever its inputs, names a valid domain
// or family and stays on this site.
func TestPropOpenStaysOnSite(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := jobs.Inputs{
			Kind:   rapid.SampledFrom(allKinds).Draw(t, "kind"),
			Domain: rapid.OneOf(rapid.Just("kilo.alpha"), rapid.Custom(func(t *rapid.T) string { return hostile(t, "domain") })).Draw(t, "domain"),
			ImageRequest: pods.ImageRequest{
				Family: rapid.OneOf(rapid.Just("debian-12"), rapid.Just(""), rapid.Custom(func(t *rapid.T) string { return hostile(t, "family") })).Draw(t, "family"),
				Image:  rapid.OneOf(rapid.Just("win2022-v3"), rapid.Custom(func(t *rapid.T) string { return hostile(t, "image") })).Draw(t, "image"),
			},
		}
		op, _ := operationFor(in.Kind)
		l := op.result(in)
		if l == nil {
			return
		}
		name, ok := strings.CutPrefix(l.Label, "Open ")
		if !ok || !staysOnSite(l.Href) {
			t.Fatalf("%+v: %+v", in, l)
		}
		switch {
		case l.Href == envHref(name):
			if pods.CheckDomain(name) != nil || name != in.Domain {
				t.Fatalf("%+v opens domain %q", in, name)
			}
		case l.Href == "/images/family/"+name:
			if pods.CheckFamily(name) != nil {
				t.Fatalf("%+v opens family %q", in, name)
			}
		default:
			t.Fatalf("%+v: %+v", in, l)
		}
		h, err := fragment("job", "job-actions", jobView{Open: l})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := checkMarkup(h); err != nil {
			t.Fatal(err)
		}
	})
}
