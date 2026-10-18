package pods

import "testing"

func TestRouterName(t *testing.T) {
	env := Environment{Hosts: []EnvHost{{Name: "teak.kilo.alpha"}, {Name: "gw.kilo.alpha", Router: true}}}
	if env.RouterName() != "gw.kilo.alpha" || !env.HasRouter() {
		t.Errorf("RouterName = %q", env.RouterName())
	}
	if (Environment{Hosts: env.Hosts[:1]}).HasRouter() {
		t.Errorf("an environment without a router has one")
	}
}
