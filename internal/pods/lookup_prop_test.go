package pods

import (
	"context"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// accessPaths are ACL paths and near misses: the root, empty, trailing and
// doubled slashes, and shared prefixes that aren't parents.
var accessPaths = []string{"", "/", "//", "/vms", "/vms/", "/vms/1", "/vms/10", "/vms/10101", "/vms//1", "/vms/10101/x", "/pool", "/pool/a", "/pool/a/", "/pool/ab", "/pool/a/b",
	"vms", "vms/1", "/vm", "/vmsx", "/storage/local", "/storage"}

// approxModel is the privileges a listed path's own entry gives, else what
// the nearest listed ancestor propagates: ancestors of p are its prefixes
// before each of its slashes, longest first, then the root.
func approxModel(listed proxmox.Permissions, p string) map[string]bool {
	if own, ok := listed[p]; ok {
		return own
	}
	var ancestors []string
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == '/' {
			ancestors = append(ancestors, p[:i])
		}
	}
	ancestors = append(ancestors, "/")
	for _, a := range ancestors {
		if a == p {
			continue
		}
		if privs, ok := listed[a]; ok {
			out := map[string]bool{}
			for k, propagates := range privs {
				if propagates {
					out[k] = true
				}
			}
			return out
		}
	}
	return map[string]bool{}
}

// Approx follows listed paths only: a path's own entry, else the nearest
// listed ancestor's propagating privileges, and never asks Proxmox beyond
// the one listing.
func TestPropApproxMatchesAncestorModel(t *testing.T) {
	privs := []string{"VM.Audit", "VM.PowerMgmt", "VM.Snapshot"}
	rapid.Check(t, func(t *rapid.T) {
		listed := proxmox.Permissions{}
		for _, p := range rapid.SliceOfN(rapid.SampledFrom(accessPaths), 0, 6).Draw(t, "listed paths") {
			m := map[string]bool{}
			for _, pr := range rapid.SliceOfN(rapid.SampledFrom(privs), 0, 3).Draw(t, "privs") {
				m[pr] = rapid.Bool().Draw(t, "propagates")
			}
			listed[p] = m
		}
		f := &fakePerms{listed: listed}
		acc := NewAccess(f)
		for _, p := range accessPaths {
			got, err := acc.Approx(context.Background(), p)
			want := approxModel(listed, p)
			if err != nil || len(got) != len(want) {
				t.Fatalf("Approx(%q) with %v = %v, %v; want %v", p, listed, got, err, want)
			}
			for k, v := range want {
				if gv, ok := got[k]; !ok || gv != v {
					t.Fatalf("Approx(%q) with %v = %v, want %v", p, listed, got, want)
				}
			}
		}
		if len(f.calls) != 1 {
			t.Fatalf("calls = %v, want only the listing", f.calls)
		}
	})
}

// MatchesHost: no filters match every host; else a host matches when it
// contains a non-blank filter, ignoring case and space around the filter.
func TestPropMatchesHost(t *testing.T) {
	containsFold := func(host, f string) bool {
		lower := func(s string) string {
			b := []byte(s)
			for i, c := range b {
				if c >= 'A' && c <= 'Z' {
					b[i] = c + 32
				}
			}
			return string(b)
		}
		h, f := lower(host), lower(f)
		for i := 0; i+len(f) <= len(h); i++ {
			if h[i:i+len(f)] == f {
				return true
			}
		}
		return false
	}
	model := func(host string, filters []string) bool {
		if len(filters) == 0 {
			return true
		}
		for _, f := range filters {
			if f = strings.Trim(f, " \t\n\r"); f != "" && containsFold(host, f) {
				return true
			}
		}
		return false
	}
	words := []string{"dc", "DC", "Dc", "web", "WEB", "teak", "oak", "d", "c", "dcx", "xdc", "a-b", "a.b", "", " ", "\t", " dc ", "dc\n", "w eb", "ab", "ba", "aab", "dcweb"}
	rapid.Check(t, func(t *rapid.T) {
		host := rapid.SampledFrom(words).Draw(t, "host")
		if rapid.Bool().Draw(t, "joined") {
			host += rapid.SampledFrom(words).Draw(t, "more")
		}
		var filters []string
		if rapid.Bool().Draw(t, "nil") {
			filters = rapid.SliceOfN(rapid.SampledFrom(words), 0, 4).Draw(t, "filters")
		}
		if got, want := MatchesHost(host, filters), model(host, filters); got != want {
			t.Fatalf("MatchesHost(%q, %q) = %v, want %v", host, filters, got, want)
		}
	})
}

// slotModel is whether bridge is pattern with each {slot} replaced by two
// ASCII digits.
func slotModel(pattern, bridge string) bool {
	parts := strings.Split(pattern, "{slot}")
	if !strings.HasPrefix(bridge, parts[0]) {
		return false
	}
	rest := bridge[len(parts[0]):]
	for _, lit := range parts[1:] {
		if len(rest) < 2 || !strings.HasPrefix(rest[2:], lit) {
			return false
		}
		for _, c := range []byte(rest[:2]) {
			if c < '0' || c > '9' {
				return false
			}
		}
		rest = rest[2+len(lit):]
	}
	return rest == ""
}

// catalogZone knows the bridges the slot patterns make and the edit
// bridge, and nothing else, whatever characters the patterns hold.
func TestPropCatalogZone(t *testing.T) {
	lits := []string{"", "dev", "int", "ext", "-", ".", "*", "(", ")", "[", "\\", "$", "^", "+", "?", "|", "{", "}", "{x}", "7"}
	drawPattern := func(t *rapid.T, label string) string {
		l := rapid.SampledFrom(lits)
		s := l.Draw(t, label+" a") + "{slot}" + l.Draw(t, label+" b")
		if rapid.IntRange(0, 3).Draw(t, label+" twice") == 0 {
			s += "{slot}" + l.Draw(t, label+" c")
		}
		return s
	}
	rapid.Check(t, func(t *rapid.T) {
		cfg := config.Default()
		cfg.Environments.IntBridge, cfg.Environments.ExtBridge = drawPattern(t, "int"), drawPattern(t, "ext")
		cfg.Environments.SDNZone, cfg.Images.EditBridge, cfg.Images.EditZone = "envzone", "editbr", "editzone"
		var bridge string
		switch rapid.IntRange(0, 2).Draw(t, "source") {
		case 0, 1:
			pat := rapid.SampledFrom([]string{cfg.Environments.IntBridge, cfg.Environments.ExtBridge}).Draw(t, "pattern")
			bridge = pat
			for strings.Contains(bridge, "{slot}") {
				bridge = strings.Replace(bridge, "{slot}", rapid.SampledFrom([]string{"01", "10", "99", "00", "1", "001", "ab", "١٢", " 1", "1\n", "{slot}"}).Draw(t, "slot"), 1)
			}
			switch rapid.IntRange(0, 3).Draw(t, "mutate") {
			case 0:
				if bridge != "" {
					i := rapid.IntRange(0, len(bridge)-1).Draw(t, "at")
					bridge = bridge[:i] + bridge[i+1:]
				}
			case 1:
				bridge = "x" + bridge
			case 2:
				bridge += "x"
			}
		default:
			bridge = rapid.SampledFrom([]string{"editbr", "editbr1", "EDITBR", "", "vmbr0", "devint01"}).Draw(t, "other")
		}
		want := ""
		switch {
		case slotModel(cfg.Environments.IntBridge, bridge), slotModel(cfg.Environments.ExtBridge, bridge):
			want = "envzone"
		case bridge == "editbr":
			want = "editzone"
		}
		if got := catalogZone(cfg, bridge); got != want {
			t.Fatalf("catalogZone(%q) with %q, %q = %q, want %q", bridge, cfg.Environments.IntBridge, cfg.Environments.ExtBridge, got, want)
		}
	})
}

// A vm_name pattern that repeats {team} still names VMs that parse back to
// the team and host VMName made them from. (A repeated {host} is refused by
// config validation: its names can't be read back.)
func TestPropVMNamesWithRepeatedTeam(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sep := rapid.SampledFrom([]string{"-", ".", "_", "~"})
		parts := []string{"{team}", "{host}"}
		for range rapid.IntRange(0, 2).Draw(t, "extra") {
			parts = append(parts, "{team}")
		}
		for i := len(parts) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(t, "swap")
			parts[i], parts[j] = parts[j], parts[i]
		}
		cfg := config.Default().Naming
		cfg.VMName = strings.Join(parts, sep.Draw(t, "sep"))
		n := NewNaming(cfg)
		team := rapid.SampledFrom([]string{"00", "05", "42", "99"}).Draw(t, "team")
		host := rapid.SampledFrom([]string{"dc", "teak", "web", "a-b", "a.b", "x_y"}).Draw(t, "host")
		name := n.VMName(team, host)
		gt, gh, ok := n.ParseVMName(name)
		// A second team number that differs from the first isn't a team VM's name.
		if i := strings.LastIndex(name, team); strings.Count(cfg.VMName, "{team}") > 1 && i >= 0 {
			other := name[:i] + "77" + name[i+2:]
			if _, _, ok := n.ParseVMName(other); ok {
				t.Fatalf("pattern %q: ParseVMName(%q) accepted a name with two different teams", cfg.VMName, other)
			}
		}
		if ok && (n.VMName(gt, gh) != name) {
			t.Fatalf("pattern %q: ParseVMName(%q) = %q, %q does not rebuild it", cfg.VMName, name, gt, gh)
		}
		if !ok {
			t.Fatalf("pattern %q: ParseVMName(%q) refused a host it made", cfg.VMName, name)
		}
	})
}
