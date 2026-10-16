package pods

import (
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// The DNS choices a form lists are exactly what EnvDNS accepts: the router,
// and each Windows or Linux host with an address. Hosts' notes are anyone's
// to write.
func TestPropDNSChoices(t *testing.T) {
	p := NewPlanner(nil, envConfig())
	rapid.Check(t, func(t *rapid.T) {
		env := Environment{Domain: "kilo.alpha"}
		names := []string{"teak", "oak", "alder", "birch", "cedar"}
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
		if len(got) == 0 || got[0] != DNSRouter {
			t.Fatalf("DNSChoices = %v, want the router first", got)
		}
		for _, n := range append([]string{DNSRouter}, names...) {
			_, err := p.dnsAddress(env, n)
			if listed := slices.Contains(got, n); listed != (err == nil) {
				t.Fatalf("%s: listed %v, but EnvDNS's check says %v (choices %v)", n, listed, err, got)
			}
		}
	})
}

func TestRouterName(t *testing.T) {
	env := Environment{Hosts: []EnvHost{{Name: "teak.kilo.alpha"}, {Name: "gw.kilo.alpha", Router: true}}}
	if env.RouterName() != "gw.kilo.alpha" || !env.HasRouter() {
		t.Errorf("RouterName = %q", env.RouterName())
	}
	if (Environment{Hosts: env.Hosts[:1]}).RouterName() != "" {
		t.Errorf("an environment without a router has one")
	}
}
