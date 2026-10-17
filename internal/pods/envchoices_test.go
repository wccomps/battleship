package pods

import (
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// The DNS choices a form lists are exactly the names a Set DNS job takes:
// ones that pass its input check (router, or a lower-case host name) and
// that EnvDNS accepts. Hosts' names and notes are anyone's to write.
func TestPropDNSChoices(t *testing.T) {
	names := []string{"teak", "oak", "DC", "router", "Alder"}
	rapid.Check(t, func(t *rapid.T) {
		env := Environment{Domain: "kilo.alpha"}
		for i := range rapid.IntRange(0, len(names)).Draw(t, "hosts") {
			env.Hosts = append(env.Hosts, EnvHost{
				Name:   names[i] + ".kilo.alpha",
				Router: rapid.Bool().Draw(t, "router"),
				Notes: Notes{
					IP: rapid.SampledFrom([]string{"", "192.168.100.10"}).Draw(t, "ip"),
					OS: rapid.SampledFrom([]string{"", ImageWindows, ImageLinux, ImageOther, "WINDOWS"}).Draw(t, "os"),
				},
			})
		}
		got := env.DNSChoices()
		if len(got) == 0 || got[0] != DNSRouter || slices.Index(got[1:], DNSRouter) >= 0 {
			t.Fatalf("DNSChoices = %v, want the router first, once", got)
		}
		for _, n := range append([]string{DNSRouter}, names...) {
			takes := n == DNSRouter || CheckHostname(n) == nil
			if _, err := env.DNSAddress(n); err != nil {
				takes = false
			}
			if listed := slices.Contains(got, n); listed != takes {
				t.Fatalf("%s: listed %v, but a job takes it: %v (choices %v)", n, listed, takes, got)
			}
		}
	})
}

func TestRouterName(t *testing.T) {
	env := Environment{Hosts: []EnvHost{{Name: "teak.kilo.alpha"}, {Name: "gw.kilo.alpha", Router: true}}}
	if env.RouterName() != "gw.kilo.alpha" || !env.HasRouter() {
		t.Errorf("RouterName = %q", env.RouterName())
	}
	if (Environment{Hosts: env.Hosts[:1]}).HasRouter() {
		t.Errorf("an environment without a router has one")
	}
}
