package web

import (
	"context"
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

// drawTime draws a time within a few days of base, to the nanosecond, in
// any zone.
func drawTime(t *rapid.T, base time.Time, label string) time.Time {
	at := base.Add(time.Duration(rapid.Int64Range(-int64(72*time.Hour), int64(72*time.Hour)).Draw(t, label)))
	offset := rapid.IntRange(-14*60, 14*60).Draw(t, label+" zone") * 60
	return at.In(time.FixedZone("Z"+strconv.Itoa(offset), offset))
}

// The three ways pages write a time agree for any time and any zone it
// comes in: in UTC, the time of day alone on the same UTC day as now and
// with the date otherwise; the short forms are the full one (the title)
// cut short; and the full one reads back as the time, to the second.
func TestPropTimeFormats(t *testing.T) {
	base := time.Date(2026, 12, 31, 22, 30, 0, 0, time.UTC) // years and days roll over nearby
	rapid.Check(t, func(t *rapid.T) {
		now, at := drawTime(t, base, "now"), drawTime(t, base, "at")
		full, short, minute := clockTime(now, at), shortTime(now, at), minuteTime(now, at)
		u := at.UTC()
		sameDay := u.Format(time.DateOnly) == now.UTC().Format(time.DateOnly)
		if again := clockTime(now.UTC(), u); again != full {
			t.Fatalf("clockTime depends on the zone: %q in UTC, %q in %s", again, full, at.Location())
		}
		if !strings.HasSuffix(full, " UTC") || !strings.HasPrefix(full, minute) || !strings.HasPrefix(full, short) {
			t.Fatalf("%v (now %v): full %q, short %q, minute %q", at, now, full, short, minute)
		}
		if sameDay {
			if full != u.Format("15:04:05")+" UTC" || short != u.Format("15:04:05") || minute != u.Format("15:04") {
				t.Fatalf("same day %v (now %v): %q %q %q", at, now, full, short, minute)
			}
			return
		}
		read, err := time.Parse("Jan 2 15:04:05 UTC", full)
		if err != nil {
			t.Fatalf("%v (now %v): full %q doesn't read back: %v", at, now, full, err)
		}
		read = read.AddDate(u.Year(), 0, 0)
		if !read.Equal(u.Truncate(time.Second)) || minute != short || minute != u.Format("Jan 2 15:04") {
			t.Fatalf("%v (now %v): full %q (reads %v), short %q, minute %q", at, now, full, read, short, minute)
		}
	})
}

var tookRE = regexp.MustCompile(`^(?:(\d+) s|(\d+) min|(\d+) h|(\d+) h (\d+) min)$`)

// How long a job took reads as the run's length to the second, minute or
// hour below it, and never longer.
func TestPropTook(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		start := drawTime(t, time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), "start")
		d := time.Duration(rapid.Int64Range(0, int64(30*time.Hour)).Draw(t, "ran"))
		end := start.Add(d).In(time.UTC)
		got := took(store.Job{StartedAt: &start, FinishedAt: &end})
		m := tookRE.FindStringSubmatch(got)
		if m == nil {
			t.Fatalf("took %v = %q", d, got)
		}
		n := func(s string) time.Duration { v, _ := strconv.Atoi(s); return time.Duration(v) }
		var shown, unit time.Duration
		switch {
		case m[1] != "":
			shown, unit = n(m[1])*time.Second, time.Second
		case m[2] != "":
			shown, unit = n(m[2])*time.Minute, time.Minute
		case m[3] != "":
			shown, unit = n(m[3])*time.Hour, time.Minute
		default:
			shown, unit = n(m[4])*time.Hour+n(m[5])*time.Minute, time.Minute
		}
		r := d.Round(time.Second)
		if shown > r || r-shown >= unit {
			t.Fatalf("took %v = %q", d, got)
		}
		if took(store.Job{StartedAt: &start}) != "" || took(store.Job{FinishedAt: &end}) != "" {
			t.Fatal("a job that didn't run or didn't finish took something")
		}
	})
}

// For any history of jobs, the job list is newest first; "cancel
// requested" marks exactly the active jobs being cancelled, "after #N"
// only pending ones, with N an earlier job still active that shares a lock
// key with it (a team, or a template both deploys use), and "nothing ran"
// exactly the stale ones; and each row's time is the short form of its
// title's.
func TestPropJobsList(t *testing.T) {
	h := newHarness(t)
	h.poll()
	addMasters(h)
	_ = h.login(asLead)
	ctx := context.Background()
	rapid.Check(t, func(t *rapid.T) {
		var made []store.Job
		for i := range rapid.IntRange(1, 4).Draw(t, "jobs") {
			_ = i
			made = append(made, drawJob(t, h))
		}
		defer func() {
			for _, j := range made {
				j, _ = h.st.Job(ctx, j.ID)
				endJob(t, h, j)
			}
		}()
		list, err := h.srv.jobsList(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		now := h.srv.now()
		for i, row := range list.Rows {
			if i > 0 && row.ID >= list.Rows[i-1].ID {
				t.Fatalf("row %d is job %d after job %d", i, row.ID, list.Rows[i-1].ID)
			}
			j, err := h.st.Job(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.When != minuteTime(now, j.CreatedAt) || row.At != clockTime(now, j.CreatedAt) || !strings.HasPrefix(row.At, row.When) {
				t.Fatalf("job %d: When %q, At %q", j.ID, row.When, row.At)
			}
			cancelling := j.Active() && j.CancelRequested
			switch {
			case cancelling != (row.Note == "cancel requested"):
				t.Fatalf("job %d (%s, cancel requested %v): note %q", j.ID, j.Status, j.CancelRequested, row.Note)
			case (j.Status == store.StatusStale) != (row.Note == "nothing ran"):
				t.Fatalf("job %d (%s): note %q", j.ID, j.Status, row.Note)
			case strings.HasPrefix(row.Note, "after #"):
				if j.Status != store.StatusPending {
					t.Fatalf("job %d (%s): note %q", j.ID, j.Status, row.Note)
				}
				n, err := strconv.ParseInt(strings.TrimPrefix(row.Note, "after #"), 10, 64)
				if err != nil || n >= j.ID {
					t.Fatalf("job %d waits for %q, not an earlier job", j.ID, row.Note)
				}
				b, err := h.st.Job(ctx, n)
				if err != nil || !b.Active() || !slices.ContainsFunc(j.LockKeys, func(k string) bool { return slices.Contains(b.LockKeys, k) }) {
					t.Fatalf("job %d is after job %d (%s), which isn't active or shares no lock key with it", j.ID, n, b.Status)
				}
			case row.Note != "" && row.Note != "cancel requested" && row.Note != "nothing ran":
				t.Fatalf("job %d: note %q", j.ID, row.Note)
			}
			if j.Status == store.StatusPending && !j.CancelRequested && row.Note == "" {
				// A pending job that waits for nothing could start now.
				if w, err := h.st.Blockers(ctx, j.ID); err != nil || len(w) != 0 {
					t.Fatalf("job %d waits for %v (%v) but its row doesn't say so", j.ID, w, err)
				}
			}
		}
	})
}

// Deploys of different teams share no VM, but a deploy waits for an
// earlier one that uses the same templates.
func TestJobsListDeployWaitsForTemplateUser(t *testing.T) {
	h := newHarness(t)
	h.poll()
	addMasters(h)
	first := h.submitJob(jobs.Inputs{Kind: pods.KindDeploy, Teams: "2-3", Pattern: "*.kilo.alpha"}, byLead)
	next := h.submitJob(jobs.Inputs{Kind: pods.KindDeploy, Teams: "1", Pattern: "*.kilo.alpha"}, byLead)
	list, err := h.srv.jobsList(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range list.Rows {
		if row.ID == next && row.Note != "after #"+itoa(first) {
			t.Errorf("job %d's note = %q, want after #%d", next, row.Note, first)
		}
	}
}
