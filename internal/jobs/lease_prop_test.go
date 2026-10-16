package jobs

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// leaseWorld is a lease with fake clocks and the real time the database
// reaps by. The machine may sleep (real and wall time pass, monotonic
// doesn't) and NTP may step the wall clock by up to the margin.
type leaseWorld struct {
	l                *lease
	real, mono, wall time.Time
	beatReal         time.Time // when the last successful beat (or the claim) began, in real time
	beatMono         time.Time
	beatWall         time.Time
	lost             bool
}

const leaseStale = time.Minute

func newLeaseWorld() *leaseWorld {
	epoch := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	lw := &leaseWorld{real: epoch, mono: epoch, wall: epoch}
	w := &Worker{now: func() time.Time { return lw.wall }, monoNow: func() time.Time { return lw.mono }}
	w.Cfg.Jobs.StaleAfter = leaseStale
	lw.l = w.newLease()
	lw.beat()
	return lw
}

func (lw *leaseWorld) pass(d time.Duration) {
	lw.real, lw.mono, lw.wall = lw.real.Add(d), lw.mono.Add(d), lw.wall.Add(d)
}

// beat is a successful beat starting now; its stamp is taken later.
func (lw *leaseWorld) beat() {
	lw.l.renew(lw.mono, lw.wall)
	lw.beatReal, lw.beatMono, lw.beatWall = lw.real, lw.mono, lw.wall
}

// check holds the lease to the reap rule: the database can reap the job
// from beatReal + StaleAfter on (a stamp is never before its beat began),
// so a change allowed now must be due to finish, in real time, by then.
// And while both clocks say under StaleAfter/2, changes are allowed.
func (lw *leaseWorld) check(t interface{ Fatalf(string, ...any) }) {
	deadline, err := lw.l.held()
	if err == nil {
		end := lw.real.Add(deadline.Sub(lw.mono)) // the request runs awake, all clocks together
		switch {
		case lw.lost:
			t.Fatalf("held() allowed a change after the claim was lost")
		case !deadline.After(lw.mono):
			t.Fatalf("held() allowed a change with no time left (deadline %v, now %v)", deadline, lw.mono)
		case end.After(lw.beatReal.Add(leaseStale)):
			t.Fatalf("a change allowed %v (real) after the beat may end %v after the earliest reap",
				lw.real.Sub(lw.beatReal), end.Sub(lw.beatReal.Add(leaseStale)))
		}
		return
	}
	if !errors.Is(err, errClaimUnsure) && !errors.Is(err, errLostClaim) {
		t.Fatalf("held() = %v", err)
	}
	if !lw.lost && lw.mono.Sub(lw.beatMono) < leaseStale/2 && lw.wall.Sub(lw.beatWall) < leaseStale/2 {
		t.Fatalf("held() = %v with both clocks under stale_after/2 since the beat", err)
	}
}

func TestPropLeaseNeverOutlastsTheReap(t *testing.T) {
	// Fixed cases: each clock alone reaching StaleAfter/2; a sleep the
	// wall clock shows shortening the deadline.
	for name, f := range map[string]func(*leaseWorld){
		"monotonic":   func(lw *leaseWorld) { lw.pass(29 * time.Second); lw.mono = lw.mono.Add(time.Second) },
		"wall":        func(lw *leaseWorld) { lw.wall = lw.wall.Add(30 * time.Second) },
		"short sleep": func(lw *leaseWorld) { lw.real, lw.wall = lw.real.Add(20*time.Second), lw.wall.Add(20*time.Second) },
		"sleep and awake": func(lw *leaseWorld) {
			lw.real, lw.wall = lw.real.Add(20*time.Second), lw.wall.Add(20*time.Second)
			lw.pass(5 * time.Second)
		},
		"ntp step back": func(lw *leaseWorld) { lw.pass(10 * time.Second); lw.wall = lw.wall.Add(-5 * time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			lw := newLeaseWorld()
			f(lw)
			lw.check(t)
		})
	}
	rapid.Check(t, func(t *rapid.T) {
		lw := newLeaseWorld()
		margin := lw.l.margin()
		for i := range rapid.IntRange(1, 30).Draw(t, "steps") {
			label := fmt.Sprint(i)
			switch rapid.IntRange(0, 5).Draw(t, "op "+label) {
			case 0: // time passes, awake
				lw.pass(time.Duration(rapid.Int64Range(0, int64(leaseStale)).Draw(t, "awake "+label)))
			case 1: // the machine sleeps
				d := time.Duration(rapid.Int64Range(0, int64(2*leaseStale)).Draw(t, "sleep "+label))
				lw.real, lw.wall = lw.real.Add(d), lw.wall.Add(d)
			case 2: // a beat succeeds: it started now and took a while
				lw.beat()
				lw.pass(time.Duration(rapid.Int64Range(0, int64(5*time.Second)).Draw(t, "beat took "+label)))
			case 3: // a beat fails
				lw.pass(time.Duration(rapid.Int64Range(0, int64(5*time.Second)).Draw(t, "failed beat took "+label)))
			case 4: // NTP steps the wall clock, by at most the margin
				lw.wall = lw.wall.Add(time.Duration(rapid.Int64Range(-int64(margin), int64(margin)).Draw(t, "ntp "+label)))
			case 5:
				if rapid.IntRange(0, 4).Draw(t, "lose "+label) == 0 {
					lw.l.lose()
					lw.lost = true
				}
			}
			lw.check(t)
		}
	})
}

// A job stopped with an unsure claim makes no change: execute hands the
// lease to the executor.
func TestExecuteSendsNothingWithoutASureClaim(t *testing.T) {
	st := storetest.New(t)
	f := teamVMs()
	submit(t, st, f, Inputs{Kind: pods.KindTeardown, Teams: "1"})
	w := newWorker(st, f)
	j := claim(t, st)
	unsure := func() (time.Time, error) { return time.Time{}, errClaimUnsure }
	out, _ := w.execute(bg, bg, nil, j, f, unsure)
	if len(f.DeletedIDs()) != 0 {
		t.Errorf("deleted %v without a sure claim", f.DeletedIDs())
	}
	if !strings.Contains(string(out.Summary), "the job may have been reaped") {
		t.Errorf("outcome = %s: %s, want the VMs failed with the claim unsure", out.Status, out.Summary)
	}
}
