package web

import (
	"testing"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// A create of no machines plans only its record item, which is no VM.
func TestSummaryLeavesRecordItemsOut(t *testing.T) {
	op, _ := operationFor(pods.KindEnvCreate)
	plan := &pods.Plan{Kind: pods.KindEnvCreate, Items: []pods.Item{{Name: "dev-01", Pool: "dev-01", Record: "kilo.alpha", Steps: []pods.Step{pods.StepRecord}}}}
	if got, want := summaryOf(op, jobs.Inputs{Kind: pods.KindEnvCreate, Domain: "kilo.alpha"}, plan), op.Noun+" kilo.alpha (0 VMs)"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
