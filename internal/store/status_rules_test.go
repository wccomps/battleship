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
		StatusInterrupted:           {final: true, stopped: true, retry: true, startAgain: true},
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

type itemRule struct {
	unfinished    bool // pending or running: its job still has to work on it
	notRunNoStep  bool // becomes not run if its job ends before it reaches a step
	retry         bool // a retry of its finished job runs it again
	touched       bool // its job did something to its VM, so it is the VM's last result
	keptOnEvent   bool // an event doesn't make it running again
	keptOnOutcome bool // the worker's outcome doesn't overwrite it
	ended         bool // the run ended it; only a started event makes it running again
	busy          bool // in an active job, still to be worked on, or may be retried by a later round
}

var itemRules = map[ItemStatus]itemRule{
	ItemPending:     {unfinished: true, notRunNoStep: true, busy: true},
	ItemRunning:     {unfinished: true, notRunNoStep: true, busy: true},
	ItemDone:        {touched: true, ended: true},
	ItemFailed:      {retry: true, touched: true, ended: true, busy: true},
	ItemBlocked:     {retry: true, keptOnEvent: true, keptOnOutcome: true},
	ItemInterrupted: {notRunNoStep: true, retry: true, touched: true, keptOnEvent: true, keptOnOutcome: true},
	ItemRemoved:     {retry: true, touched: true},
	ItemNotRun:      {retry: true, keptOnEvent: true},
}

// TestItemStatusRules pins each item status's Retryable rule.
func TestItemStatusRules(t *testing.T) {
	for s, r := range itemRules {
		if got := s.Retryable(); got != r.retry {
			t.Errorf("%s: Retryable() = %t, want %t", s, got, r.retry)
		}
	}
	if ItemStatus("bogus").Retryable() {
		t.Error("bogus: Retryable() = true, want false")
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
		{"endedItemsSQL", endedItemsSQL, items(func(r itemRule) bool { return r.ended })},
		{"busyItemsSQL", busyItemsSQL, items(func(r itemRule) bool { return r.busy })},
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
