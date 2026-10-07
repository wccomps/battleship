package store

import (
	"slices"
	"strings"
	"testing"
)

// TestJobStatusRules pins each job status's rules as the code relied on
// them before they were gathered into jobRules.
func TestJobStatusRules(t *testing.T) {
	type row struct{ active, final, stopped, ranNothing, retry, startAgain, tooLate bool }
	want := map[string]row{
		StatusPending:               {active: true},
		StatusRunning:               {active: true},
		StatusSucceeded:             {final: true, tooLate: true},
		StatusCompletedWithFailures: {final: true, retry: true, tooLate: true},
		StatusFailed:                {final: true, ranNothing: true, startAgain: true, tooLate: true},
		StatusCancelled:             {final: true, stopped: true, retry: true, startAgain: true},
		StatusInterrupted:           {final: true, stopped: true, retry: true},
		StatusStale:                 {final: true, ranNothing: true, startAgain: true},
		"bogus":                     {},
	}
	if len(jobRules) != len(want)-1 {
		t.Errorf("jobRules has %d statuses, want %d", len(jobRules), len(want)-1)
	}
	for st, w := range want {
		s := JobStatus(st)
		got := row{s.Active(), s.Final(), s.Stopped(), s.RanNothing(), s.CanRetry(), s.StartAgain(), s.CancelTooLate()}
		if got != w {
			t.Errorf("%s: got %+v, want %+v", st, got, w)
		}
	}
}

// TestItemStatusRules pins each item status's rules likewise.
func TestItemStatusRules(t *testing.T) {
	type row struct{ unfinished, notRunNoStep, retry, touched, keptOnEvent, keptOnOutcome bool }
	want := map[string]row{
		ItemPending:     {unfinished: true, notRunNoStep: true},
		ItemRunning:     {unfinished: true, notRunNoStep: true},
		ItemDone:        {touched: true},
		ItemFailed:      {retry: true, touched: true},
		ItemBlocked:     {retry: true, keptOnEvent: true, keptOnOutcome: true},
		ItemInterrupted: {notRunNoStep: true, retry: true, touched: true, keptOnEvent: true, keptOnOutcome: true},
		ItemRemoved:     {retry: true, touched: true},
		ItemNotRun:      {retry: true, keptOnEvent: true},
		"bogus":         {},
	}
	if len(itemRules) != len(want)-1 {
		t.Errorf("itemRules has %d statuses, want %d", len(itemRules), len(want)-1)
	}
	for st, w := range want {
		s := ItemStatus(st)
		r := s.rule()
		got := row{s.Unfinished(), r.notRunNoStep, s.Retryable(), s.TouchedVM(), r.keptOnEvent, r.keptOnOutcome}
		if got != w {
			t.Errorf("%s: got %+v, want %+v", st, got, w)
		}
	}
}

// TestStatusSQLLists checks each SQL status list names exactly the
// statuses its table column marks.
func TestStatusSQLLists(t *testing.T) {
	jobs := func(f func(jobRule) bool) []string {
		var out []string
		for s, r := range jobRules {
			if f(r) {
				out = append(out, string(s))
			}
		}
		return out
	}
	items := func(f func(itemRule) bool) []string {
		var out []string
		for s, r := range itemRules {
			if f(r) {
				out = append(out, string(s))
			}
		}
		return out
	}
	for _, c := range []struct {
		name, sql string
		want      []string
	}{
		{"activeJobsSQL", activeJobsSQL, jobs(func(r jobRule) bool { return r.active })},
		{"unfinishedItemsSQL", unfinishedItemsSQL, items(func(r itemRule) bool { return r.unfinished })},
		{"notRunNoStepSQL", notRunNoStepSQL, items(func(r itemRule) bool { return r.notRunNoStep })},
		{"touchedItemsSQL", touchedItemsSQL, items(func(r itemRule) bool { return r.touched })},
		{"keptOnEventSQL", keptOnEventSQL, items(func(r itemRule) bool { return r.keptOnEvent })},
		{"keptOnOutcomeSQL", keptOnOutcomeSQL, items(func(r itemRule) bool { return r.keptOnOutcome })},
	} {
		got := sqlStatuses(t, c.sql)
		slices.Sort(c.want)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s = %s, want %v", c.name, c.sql, c.want)
		}
	}
}

// sqlStatuses reads an SQL list such as ('a', 'b'), sorted.
func sqlStatuses(t *testing.T, list string) []string {
	t.Helper()
	inner, okPrefix := strings.CutPrefix(list, "(")
	inner, okSuffix := strings.CutSuffix(inner, ")")
	if !okPrefix || !okSuffix {
		t.Fatalf("%s is not an SQL list", list)
	}
	var out []string
	for _, q := range strings.Split(inner, ", ") {
		if len(q) < 2 || q[0] != '\'' || q[len(q)-1] != '\'' {
			t.Fatalf("%s: %q is not a quoted status", list, q)
		}
		out = append(out, q[1:len(q)-1])
	}
	slices.Sort(out)
	return out
}
