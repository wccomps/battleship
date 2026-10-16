package jobs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// The claim is sure only while the last successful beat (or the claim)
// began under StaleAfter/2 ago by both clocks, and never after the claim
// was lost. Another worker reaps at StaleAfter, so a sure claim is never
// one another worker can have taken.
func TestPropLeaseIsSureOnlyWhileUnreapable(t *testing.T) {
	// Fixed cases: the wall clock alone, then the monotonic clock alone,
	// reaching StaleAfter/2 after the beat.
	for _, c := range []struct{ mono, wall time.Duration }{{0, 30 * time.Second}, {30 * time.Second, -time.Hour}, {29 * time.Second, 29 * time.Second}} {
		epoch := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
		w := &Worker{now: func() time.Time { return epoch.Add(c.wall) }}
		w.Cfg.Jobs.StaleAfter = time.Minute
		l := w.newLease()
		l.monoNow = func() time.Time { return epoch.Add(c.mono) }
		l.renew(epoch, epoch)
		if sure := c.mono < 30*time.Second && c.wall < 30*time.Second; (l.held() == nil) != sure {
			t.Errorf("mono +%v, wall %+v after the beat: held() = %v, want sure %v", c.mono, c.wall, l.held(), sure)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		const staleAfter = 60 * time.Second
		epoch := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
		mono, wall := epoch, epoch // the fake clocks
		w := &Worker{now: func() time.Time { return wall }}
		w.Cfg.Jobs.StaleAfter = staleAfter
		l := w.newLease()
		l.monoNow = func() time.Time { return mono }
		l.renew(mono, wall)
		beatMono, beatWall, lost := mono, wall, false // the model
		for range rapid.IntRange(1, 30).Draw(t, "steps") {
			switch rapid.IntRange(0, 5).Draw(t, "op") {
			case 0: // time passes on both clocks
				d := time.Duration(rapid.Int64Range(0, int64(staleAfter)).Draw(t, "both"))
				mono, wall = mono.Add(d), wall.Add(d)
			case 1: // the machine sleeps: only the wall clock moves
				wall = wall.Add(time.Duration(rapid.Int64Range(0, int64(2*staleAfter)).Draw(t, "sleep")))
			case 2: // a beat starts now and succeeds, ending a little later
				l.renew(mono, wall)
				beatMono, beatWall = mono, wall
				d := time.Duration(rapid.Int64Range(0, int64(5*time.Second)).Draw(t, "beat took"))
				mono, wall = mono.Add(d), wall.Add(d)
			case 3: // a beat fails: nothing renews
				d := time.Duration(rapid.Int64Range(0, int64(5*time.Second)).Draw(t, "failed beat took"))
				mono, wall = mono.Add(d), wall.Add(d)
			case 5: // NTP steps the wall clock back: only the monotonic one shows time passing
				wall = wall.Add(-time.Duration(rapid.Int64Range(0, int64(2*staleAfter)).Draw(t, "step back")))
			case 4:
				if rapid.IntRange(0, 4).Draw(t, "lose") == 0 {
					l.lose()
					lost = true
				}
			}
			sure := !lost && mono.Sub(beatMono) < staleAfter/2 && wall.Sub(beatWall) < staleAfter/2
			err := l.held()
			if (err == nil) != sure {
				t.Fatalf("held() = %v; since beat: mono %v wall %v, lost %v", err, mono.Sub(beatMono), wall.Sub(beatWall), lost)
			}
			if err != nil && !errors.Is(err, errClaimUnsure) && !errors.Is(err, errLostClaim) {
				t.Fatalf("held() = %v, want errClaimUnsure or errLostClaim", err)
			}
			// Sure means no other worker can have reaped the job yet: real
			// time (the monotonic clock, while awake) is under stale_after.
			if sure && mono.Sub(beatMono) >= staleAfter {
				t.Fatalf("sure %v after the last beat, past stale_after", mono.Sub(beatMono))
			}
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
	out, _ := w.execute(bg, bg, nil, j, f, func() error { return errClaimUnsure })
	if len(f.DeletedIDs()) != 0 {
		t.Errorf("deleted %v without a sure claim", f.DeletedIDs())
	}
	if !strings.Contains(string(out.Summary), "another worker may have taken over the job") {
		t.Errorf("outcome = %s: %s, want the VMs failed with the claim unsure", out.Status, out.Summary)
	}
}
