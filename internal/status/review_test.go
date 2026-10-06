package status

import (
	"testing"
	"time"
)

// Item 20: a stream opening a view during its linger keeps it: the linger
// running out then leaves the view polling, and only the last release's
// linger stops it.
func TestOpenDuringTheLingerGetsALiveView(t *testing.T) {
	h := newViewsHarness(t)
	v, release := h.views.Open("alice@r", ticket("alice@r", "A1"))
	h.waitLists(1)
	release()
	h.clock.BlockUntil(t, 3) // the poll's and scan's waits, and the linger's
	v2, release2 := h.views.Open("alice@r", ticket("alice@r", "A1"))
	if v2 != v {
		t.Fatal("Open during the linger made a new view")
	}
	h.clock.Advance(time.Minute) // the first linger runs out
	select {
	case <-v.Done():
		t.Fatal("the linger stopped a view that was opened again")
	case <-time.After(200 * time.Millisecond):
	}
	release2()
	h.clock.BlockUntilWait(t, time.Minute) // the second linger
	h.clock.Advance(time.Minute)
	select {
	case <-v.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the view didn't stop after its last linger")
	}
}
