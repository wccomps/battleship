package web

import (
	"testing"

	"github.com/wccomps/battleship/internal/pods"
)

// Every power action the engine knows has its wording here, and the forms
// offer them in the engine's order.
func TestEveryPowerActionHasALabel(t *testing.T) {
	choices := powerChoicesOf(pods.PowerActions)
	if len(choices) != len(pods.PowerActions) {
		t.Fatalf("%d choices for %d actions", len(choices), len(pods.PowerActions))
	}
	for i, a := range pods.PowerActions {
		c := choices[i]
		if c.Value != a.Value || c.Label == "" || c.Label == a.Value {
			t.Errorf("action %q: choice %+v, want its own label", a.Value, c)
		}
		if got := powerLabel(a.Value); got != c.Label {
			t.Errorf("powerLabel(%q) = %q, want %q", a.Value, got, c.Label)
		}
	}
	if got := powerLabel("hibernate"); got != "hibernate" {
		t.Errorf("powerLabel(hibernate) = %q, want the action itself", got)
	}
}
