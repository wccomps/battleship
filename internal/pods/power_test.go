package pods

import (
	"reflect"
	"testing"
)

func TestPowerActionOf(t *testing.T) {
	for _, a := range PowerActions {
		if got, ok := PowerActionOf(a.Value); !ok || got != a {
			t.Errorf("PowerActionOf(%q) = %+v, %t", a.Value, got, ok)
		}
	}
	if _, ok := PowerActionOf("hibernate"); ok {
		t.Error("PowerActionOf(hibernate) found an action")
	}
}

// The engine says what each action does, not what forms call it: its
// wording is the web app's.
func TestPowerActionHasNoUIWording(t *testing.T) {
	if _, ok := reflect.TypeOf(PowerAction{}).FieldByName("Label"); ok {
		t.Error("PowerAction has a Label; the web app owns power actions' wording")
	}
}
