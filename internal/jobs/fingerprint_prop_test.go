package jobs

import (
	"encoding/json"
	"net/netip"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
)

// fpTexts are strings that stress JSON: quotes, escapes, markup, line
// separators, replacement characters and wide runes. All are valid UTF-8,
// which is what a stored plan can hold.
var fpTexts = []string{"", "a", "team01-dc", "x y", `"q"`, `\`, "<b>&amp;</b>", "\u2028\u2029", "�", "é", "\U0001F600", "\n", "\t", "\x00", "\x7f", "null", "{}", ","}

func drawFPText(t *rapid.T, label string) string {
	return rapid.OneOf(rapid.SampledFrom(fpTexts), rapid.String()).Draw(t, label)
}

// drawFPAddr draws no address, an IPv4, IPv6 or IPv4-in-IPv6 one, or an
// IPv6 one with a zone, the one place an address carries text.
func drawFPAddr(t *rapid.T, label string) netip.Addr {
	b := [16]byte(rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(t, label))
	switch rapid.IntRange(0, 4).Draw(t, label+" kind") {
	case 0:
		return netip.Addr{}
	case 1:
		return netip.AddrFrom4([4]byte(b[:4]))
	case 2:
		return netip.AddrFrom16(b)
	case 3:
		return netip.AddrFrom16(netip.AddrFrom4([4]byte(b[:4])).As16())
	}
	return netip.AddrFrom16(b).WithZone(drawFPText(t, label+" zone"))
}

// drawStrings draws nil or a slice, empty or not.
func drawStrings(t *rapid.T, label string) []string {
	if rapid.Bool().Draw(t, label+" nil") {
		return nil
	}
	return rapid.SliceOfN(rapid.Custom(func(t *rapid.T) string { return drawFPText(t, label) }), 0, 3).Draw(t, label)
}

func drawSource(t *rapid.T, label string) *pods.Source {
	if rapid.Bool().Draw(t, label+" nil") {
		return nil
	}
	return &pods.Source{VMID: rapid.IntRange(0, 99999).Draw(t, label+" vmid"), Node: drawFPText(t, label+" node"), Name: drawFPText(t, label+" name"),
		Bridges: drawStrings(t, label+" bridges")}
}

func drawPlan(t *rapid.T) *pods.Plan {
	p := &pods.Plan{
		Kind:   pods.Kind(drawFPText(t, "kind")),
		Teams:  drawStrings(t, "teams"),
		Config: drawFPText(t, "config"),
		Locks:  drawStrings(t, "locks"),
	}
	if rapid.Bool().Draw(t, "templates?") {
		p.Templates = []pods.TemplateSpec{}
	}
	for range rapid.IntRange(0, 2).Draw(t, "templates") {
		p.Templates = append(p.Templates, pods.TemplateSpec{
			Name: drawFPText(t, "tpl name"), Host: drawFPText(t, "tpl host"), VMID: rapid.IntRange(0, 99999).Draw(t, "tpl vmid"), Node: drawFPText(t, "tpl node"),
			Exists: rapid.Bool().Draw(t, "exists"), Rebuild: rapid.Bool().Draw(t, "rebuild"), OldNode: drawFPText(t, "old node"),
			MasterName: drawFPText(t, "master"), MasterVMID: rapid.IntRange(0, 99999).Draw(t, "master vmid"), MasterNode: drawFPText(t, "master node"),
			Interfaces: rapid.IntRange(0, 8).Draw(t, "ifaces"), GPU: rapid.Bool().Draw(t, "gpu"), Blocked: drawFPText(t, "blocked"),
			WillStopMaster: rapid.Bool().Draw(t, "stop"), CPU: drawFPText(t, "cpu"), CloudInit: rapid.Bool().Draw(t, "ci"), Bridges: drawStrings(t, "bridges"),
		})
	}
	if rapid.Bool().Draw(t, "items?") {
		p.Items = []pods.Item{}
	}
	for range rapid.IntRange(0, 3).Draw(t, "items") {
		it := pods.Item{
			Team: drawFPText(t, "team"), Host: drawFPText(t, "host"), Name: drawFPText(t, "name"), VMID: rapid.IntRange(0, 99999).Draw(t, "vmid"),
			Node: drawFPText(t, "node"), OldNode: drawFPText(t, "item old node"), Template: drawFPText(t, "template"), Snapshot: drawFPText(t, "snapshot"), Baseline: rapid.Bool().Draw(t, "baseline"),
			Action: drawFPText(t, "action"), Blocked: drawFPText(t, "blocked"), Unpermitted: rapid.Bool().Draw(t, "unpermitted"),
			Description: drawFPText(t, "description"), VMState: rapid.Bool().Draw(t, "vmstate"),
			Pool: drawFPText(t, "pool"), Tags: drawFPText(t, "tags"), CPU: drawFPText(t, "item cpu"), Source: drawSource(t, "source"), Supersedes: drawSource(t, "supersedes"),
		}
		for _, s := range drawStrings(t, "steps") {
			it.Steps = append(it.Steps, pods.Step(s))
		}
		if rapid.Bool().Draw(t, "steps?") && it.Steps == nil {
			it.Steps = []pods.Step{}
		}
		if rapid.Bool().Draw(t, "notes") {
			it.Notes = &pods.Notes{IP: drawFPAddr(t, "ip"), OS: drawFPText(t, "os"), Base: drawFPText(t, "base"), DNS: drawFPText(t, "dns")}
		}
		switch rapid.IntRange(0, 2).Draw(t, "nics") {
		case 1:
			it.NICs = map[string]string{}
		case 2:
			it.NICs = map[string]string{}
			for range rapid.IntRange(1, 3).Draw(t, "nic count") {
				it.NICs[rapid.SampledFrom([]string{"net0", "net1", "net2", "net10", ""}).Draw(t, "nic")] = drawFPText(t, "bridge")
			}
		}
		if rapid.Bool().Draw(t, "guest") {
			it.Guest = &pods.Guest{OS: drawFPText(t, "os"), Hostname: drawFPText(t, "hostname"), Domain: drawFPText(t, "domain"), IP: drawFPAddr(t, "gip"),
				Prefix: rapid.IntRange(0, 32).Draw(t, "prefix"), Gateway: drawFPAddr(t, "gw"), DNS: drawFPAddr(t, "gdns")}
		}
		p.Items = append(p.Items, it)
	}
	return p
}

// A plan is stored as JSON and fingerprinted again by the worker that runs
// it, so a plan and what it reads back as must fingerprint alike.
func TestPropFingerprintSurvivesStorage(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		plan := drawPlan(t)
		b, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		var back pods.Plan
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if got, want := Fingerprint(&back), Fingerprint(plan); got != want {
			t.Fatalf("fingerprint changed in storage:\nplan %+v\nstored %s", plan, b)
		}
	})
}
