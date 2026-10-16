package jobs

import "github.com/wccomps/battleship/internal/pods"

// writeKeys and readKeys are LockKeys' two halves.
func writeKeys(plan *pods.Plan) []string { w, _ := plan.LockKeys(); return w }
func readKeys(plan *pods.Plan) []string  { _, r := plan.LockKeys(); return r }
