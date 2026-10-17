package jobs

import (
	"testing"

	"github.com/wccomps/battleship/internal/pods"
)

// Exactly the kinds with a typed confirm name what is typed.
func TestTypedWhat(t *testing.T) {
	want := map[pods.Kind]string{pods.KindTeardown: "the team range", pods.KindEnvTeardown: "the domain"}
	for _, k := range []pods.Kind{pods.KindDeploy, pods.KindTeardown, pods.KindReset, pods.KindSnapshot, pods.KindPower,
		pods.KindImageRegister, pods.KindImageEdit, pods.KindImageDeprecate, pods.KindImageDelete,
		pods.KindEnvCreate, pods.KindEnvAdd, pods.KindEnvDNS, pods.KindEnvTeardown, "unknown"} {
		got := Inputs{Kind: k}.TypedWhat()
		if got != want[k] || (got != "") != k.TypedConfirm() {
			t.Errorf("%s: TypedWhat %q, want %q (TypedConfirm %v)", k, got, want[k], k.TypedConfirm())
		}
		if k.RetriedWhole() != (k == pods.KindEnvTeardown) {
			t.Errorf("%s: RetriedWhole %v", k, k.RetriedWhole())
		}
	}
}
