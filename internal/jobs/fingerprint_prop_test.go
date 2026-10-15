package jobs

import (
	"encoding/json"
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
			Node: drawFPText(t, "node"), Template: drawFPText(t, "template"), Snapshot: drawFPText(t, "snapshot"), Baseline: rapid.Bool().Draw(t, "baseline"),
			Action: drawFPText(t, "action"), Blocked: drawFPText(t, "blocked"), Unpermitted: rapid.Bool().Draw(t, "unpermitted"),
			Description: drawFPText(t, "description"), VMState: rapid.Bool().Draw(t, "vmstate"),
			Pool: drawFPText(t, "pool"), Tags: drawFPText(t, "tags"), Source: drawSource(t, "source"), Supersedes: drawSource(t, "supersedes"),
		}
		for _, s := range drawStrings(t, "steps") {
			it.Steps = append(it.Steps, pods.Step(s))
		}
		if rapid.Bool().Draw(t, "steps?") && it.Steps == nil {
			it.Steps = []pods.Step{}
		}
		if rapid.Bool().Draw(t, "notes") {
			it.Notes = &pods.Notes{IP: drawFPText(t, "ip"), OS: drawFPText(t, "os"), Base: drawFPText(t, "base"), DNS: drawFPText(t, "dns")}
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
			it.Guest = &pods.Guest{OS: drawFPText(t, "os"), Hostname: drawFPText(t, "hostname"), Domain: drawFPText(t, "domain"), IP: drawFPText(t, "gip"),
				Prefix: rapid.IntRange(0, 32).Draw(t, "prefix"), Gateway: drawFPText(t, "gw"), DNS: drawFPText(t, "gdns"), DNSOnly: rapid.Bool().Draw(t, "dnsonly")}
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
		// Rebuilding the NIC map in another order changes nothing.
		for i := range plan.Items {
			if nics := plan.Items[i].NICs; len(nics) > 1 {
				rebuilt := map[string]string{}
				keys := make([]string, 0, len(nics))
				for k := range nics {
					keys = append(keys, k)
				}
				for j := len(keys) - 1; j >= 0; j-- {
					rebuilt[keys[j]] = nics[keys[j]]
				}
				plan.Items[i].NICs = rebuilt
			}
		}
		if got := Fingerprint(plan); got != Fingerprint(&back) {
			t.Fatalf("fingerprint depends on NIC map order")
		}
	})
}
