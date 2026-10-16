package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// terminalTricks are what untrusted text might use to take over or mislead
// a terminal: escape sequences, hyperlinks, CSI, line breaks, bidi overrides.
var terminalTricks = []string{
	"\x1b[2J", "\x1b]8;;http://x\x07", "\x9b", "\u009b31m", "\r", "\n", "\r\n", "\t", "\x00", "\x07", "\x7f",
	"\u202e", "\u2066", "\u200b", "\ufeff", "\u2028", "\xff", "\xc3", `\`, `"`, "team01-dc", "dc", "",
}

var allKinds = []pods.Kind{pods.KindDeploy, pods.KindTeardown, pods.KindReset, pods.KindPower, pods.KindSnapshot,
	pods.KindImageRegister, pods.KindImageEdit, pods.KindImageDeprecate, pods.KindImageDelete,
	pods.KindEnvCreate, pods.KindEnvAdd, pods.KindEnvDNS}

// untrusted draws text a web user, an IdP, Proxmox or a guest could write.
func untrusted(t *rapid.T, label string) string {
	piece := rapid.OneOf(
		rapid.SampledFrom(terminalTricks),
		rapid.String(),
		rapid.Map(rapid.SliceOf(rapid.Byte()), func(b []byte) string { return string(b) }),
	)
	return strings.Join(rapid.SliceOfN(piece, 0, 3).Draw(t, label), "")
}

func untrustedList(t *rapid.T, label string) []string {
	return rapid.SliceOfN(rapid.Custom(func(t *rapid.T) string { return untrusted(t, label) }), 0, 3).Draw(t, label)
}

// terminalSafe is why out could act on a terminal, or "".
func terminalSafe(out string) string {
	if !utf8.ValidString(out) {
		return "invalid UTF-8"
	}
	for _, r := range out {
		if r != '\n' && (r < 0x20 || r >= 0x7f && r <= 0x9f || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029') {
			return fmt.Sprintf("writes %U", r)
		}
	}
	return ""
}

// clean writes anything that isn't graphic as a Go escape, so the text reads
// back as it was; graphic text stays as it is.
func TestPropCleanEscapes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := untrusted(t, "s")
		got := clean(s)
		if why := terminalSafe(got); why != "" || strings.Contains(got, "\n") {
			t.Fatalf("clean(%q) = %q: %s", s, got, why)
		}
		if utf8.ValidString(s) && !strings.ContainsFunc(s, notGraphic) && got != s {
			t.Fatalf("clean(%q) = %q; graphic text must stay", s, got)
		}
		if !strings.ContainsAny(s, `\"`) {
			if back, err := strconv.Unquote(`"` + got + `"`); err != nil || back != s {
				t.Fatalf("clean(%q) = %q, which reads back as %q, %v", s, got, back, err)
			}
		}
	})
}

// The plan, event and summary printers and describeInputs write untrusted
// text escaped: nothing a terminal acts on, every field findable as clean
// gives it, and as many lines as the structure has.
func TestPropPrintersEscapeUntrustedText(t *testing.T) {
	naming := pods.NewNaming(config.Default().Naming)
	rapid.Check(t, func(t *rapid.T) {
		var buf bytes.Buffer
		var lines int
		var fields []string
		switch printer := rapid.SampledFrom([]string{"plan", "event", "summary", "inputs"}).Draw(t, "printer"); printer {
		case "plan":
			plan := &pods.Plan{Kind: rapid.SampledFrom(allKinds).Draw(t, "kind"), Teams: untrustedList(t, "teams")}
			for range rapid.IntRange(0, 3).Draw(t, "templates") {
				plan.Templates = append(plan.Templates, pods.TemplateSpec{Name: untrusted(t, "template"), Node: untrusted(t, "node"),
					Blocked: untrusted(t, "blocked"), MasterName: untrusted(t, "master"), WillStopMaster: rapid.Bool().Draw(t, "stops"),
					GPU: rapid.Bool().Draw(t, "gpu"), VMID: rapid.IntRange(0, 9999).Draw(t, "vmid")})
				fields = append(fields, plan.Templates[len(plan.Templates)-1].Name)
			}
			for range rapid.IntRange(0, 3).Draw(t, "items") {
				it := pods.Item{Name: untrusted(t, "name"), Team: untrusted(t, "team"), Node: untrusted(t, "node"),
					Snapshot: untrusted(t, "snapshot"), Blocked: untrusted(t, "blocked"), Baseline: rapid.Bool().Draw(t, "baseline"),
					VMState: rapid.Bool().Draw(t, "vmstate")}
				for _, s := range untrustedList(t, "steps") {
					it.Steps = append(it.Steps, pods.Step(s))
				}
				plan.Items = append(plan.Items, it)
				fields = append(fields, it.Name)
			}
			printPlan(&buf, naming, plan)
			lines = 1 + 2 + len(plan.Items)
			if len(plan.Templates) > 0 {
				lines += 2 + len(plan.Templates)
			}
			if !plan.Kind.Teamless() {
				fields = append(fields, plan.Teams...)
			}
		case "event":
			e := apply.Event{Time: time.Unix(0, 0), Item: untrusted(t, "item"), Step: pods.Step(untrusted(t, "step")),
				Status:  rapid.SampledFrom([]apply.EventStatus{apply.EventSkipped, apply.EventStatus(untrusted(t, "status"))}).Draw(t, "status"),
				Message: untrusted(t, "message")}
			printEvent(&buf, e)
			if e.Status != apply.EventSkipped || e.Message != "" {
				// A message's own lines are indented under it.
				lines = 1 + strings.Count(e.Message, "\n")
				fields = append(fields, e.Item)
				fields = append(fields, strings.Split(e.Message, "\n")...)
			}
		case "summary":
			sum := jobs.Summary{Failed: map[string]string{}, CleanupFailed: map[string]string{},
				Interrupted: untrustedList(t, "interrupted"), Completed: untrustedList(t, "completed"),
				Removed: untrustedList(t, "removed"), AlreadyGone: untrustedList(t, "gone")}
			for range rapid.IntRange(0, 3).Draw(t, "failed") {
				sum.Failed[untrusted(t, "failed")] = untrusted(t, "error")
			}
			for range rapid.IntRange(0, 2).Draw(t, "cleanup failed") {
				sum.CleanupFailed[untrusted(t, "vm")] = untrusted(t, "advice")
			}
			printSummary(&buf, sum)
			if len(sum.Failed) > 0 {
				lines += 2
			}
			// A failure's own lines are indented under it.
			for name, msg := range sum.Failed {
				lines += 1 + strings.Count(msg, "\n")
				fields = append(fields, name)
				fields = append(fields, strings.Split(msg, "\n")...)
			}
			for _, list := range [][]string{sum.Interrupted, sum.Completed, sum.Removed, sum.AlreadyGone} {
				if len(list) > 0 {
					lines += 2
				}
				fields = append(fields, list...)
			}
			lines += len(sum.CleanupFailed)
		case "inputs":
			in := jobs.Inputs{Kind: rapid.SampledFrom(allKinds).Draw(t, "kind"), Teams: untrusted(t, "teams"), Hosts: untrustedList(t, "hosts"),
				Pattern: untrusted(t, "pattern"), Action: untrusted(t, "action"), Snapshot: untrusted(t, "snapshot"),
				Domain: untrusted(t, "domain"), Family: untrusted(t, "family"), Image: untrusted(t, "image")}
			raw, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			buf.WriteString(describeInputs(raw))
			// JSON carries invalid UTF-8 as U+FFFD.
			if err := json.Unmarshal(raw, &in); err != nil {
				t.Fatal(err)
			}
			if !in.Kind.Teamless() {
				fields = append(fields, in.Teams)
				fields = append(fields, in.Hosts...)
			}
		}
		out := buf.String()
		if why := terminalSafe(out); why != "" {
			t.Fatalf("output %s:\n%q", why, out)
		}
		if got := strings.Count(out, "\n"); got != lines {
			t.Fatalf("%d lines, want %d:\n%s", got, lines, out)
		}
		for _, f := range fields {
			if !strings.Contains(out, clean(f)) {
				t.Fatalf("output lacks %q as %q:\n%s", f, clean(f), out)
			}
		}
	})
}

// jobs list and jobs show print stored jobs, written by web users, workers,
// Proxmox and guests, escaped like the other printers.
func TestPropJobsCommandsEscapeStoredText(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	// Postgres text holds neither NUL nor invalid UTF-8.
	storable := func(t *rapid.T, label string) string {
		return strings.ReplaceAll(strings.ToValidUTF8(untrusted(t, label), "?"), "\x00", "?")
	}
	rapid.Check(t, func(t *rapid.T) {
		in := jobs.Inputs{Kind: pods.KindReset, Teams: storable(t, "teams"), Hosts: []string{storable(t, "host")}}
		raw, _ := json.Marshal(in)
		name := "team01-" + storable(t, "host")
		id, err := st.CreateJob(ctx, store.NewJob{Kind: string(in.Kind), Inputs: raw, Plan: []byte(`{}`), LockKeys: []string{"team:01"},
			CreatedBy: "u" + storable(t, "created by"), Items: []store.NewItem{{Name: name, Team: "01", VMID: 10101}}})
		if err != nil {
			t.Fatal(err)
		}
		worker := "w" + storable(t, "worker")
		if _, err := storetest.ClaimJob(st, id, worker, time.Hour); err != nil {
			t.Fatal(err)
		}
		// Errors and messages keep their lines: count them.
		events := rapid.IntRange(0, 3).Draw(t, "events")
		breaks, itemErr := 0, ""
		for range events {
			ev := store.Event{At: time.Now(), Item: name, Step: storable(t, "step"), Status: "failed", Message: storable(t, "message")}
			if err := st.AddEvent(ctx, id, ev); err != nil {
				t.Fatal(err)
			}
			breaks += strings.Count(ev.Message, "\n")
			itemErr = ev.Message // a failed event's message is its item's error
		}
		jobErr := storable(t, "error")
		if err := st.Finish(ctx, id, worker, store.Outcome{Status: store.StatusFailed, Error: jobErr}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		d := deps{stdout: &out, stderr: &out}
		if code := listJobs(ctx, d, st, 1); code != 0 {
			t.Fatalf("list: exit %d: %s", code, out.String())
		}
		if why := terminalSafe(out.String()); why != "" {
			t.Fatalf("jobs list %s:\n%q", why, out.String())
		}
		if got := strings.Count(out.String(), "\n"); got != 2 {
			t.Fatalf("jobs list printed %d lines for one job:\n%s", got, out.String())
		}
		out.Reset()
		if code := showJob(ctx, d, st, id); code != 0 {
			t.Fatalf("show: exit %d: %s", code, out.String())
		}
		if why := terminalSafe(out.String()); why != "" {
			t.Fatalf("jobs show %s:\n%q", why, out.String())
		}
		// Job, created, error if any, blank, VMs header, one VM; then the events.
		want := 5 + strings.Count(itemErr, "\n")
		if jobErr != "" {
			want += 1 + strings.Count(jobErr, "\n")
		}
		if events > 0 {
			want += 2 + events + breaks
		}
		if got := strings.Count(out.String(), "\n"); got != want {
			t.Fatalf("jobs show printed %d lines, want %d:\n%s", got, want, out.String())
		}
		out.Reset()
		if code := reportJob(ctx, d, st, id); code != 1 {
			t.Fatalf("report: exit %d: %s", code, out.String())
		}
		if why := terminalSafe(out.String()); why != "" {
			t.Fatalf("job report %s:\n%q", why, out.String())
		}
		if got, want := strings.Count(out.String(), "\n"), 2+strings.Count(jobErr, "\n"); got != want {
			t.Fatalf("job report printed %d lines, want %d:\n%s", got, want, out.String())
		}
	})
}
