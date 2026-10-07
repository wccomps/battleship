package pods

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// keys returns the sorted top-level keys of a JSON object.
func keys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Stored plans outlive the code that wrote them, so their JSON keys are
// fixed. Changing a key here breaks reading older jobs.
func TestPlanJSONKeysAreStable(t *testing.T) {
	plan := Plan{
		Kind:      KindDeploy,
		Teams:     []string{"01"},
		Templates: []TemplateSpec{{Name: "teak.x.tpl"}},
		Items:     []Item{{Name: "team01-teak", Steps: []Step{StepClone}}},
	}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Templates []json.RawMessage `json:"templates"`
		Items     []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"plan": {"items", "kind", "teams", "templates"},
		"template": {"blocked", "exists", "gpu", "host", "interfaces", "master_name", "master_node", "master_vmid",
			"name", "node", "old_node", "rebuild", "vmid", "will_stop_master"},
		"item": {"action", "blocked", "host", "name", "node", "snapshot", "steps", "team", "template", "vmid"},
	}
	got := map[string][]string{"plan": keys(t, b)}
	if len(top.Templates) == 1 && len(top.Items) == 1 {
		got["template"] = keys(t, top.Templates[0])
		got["item"] = keys(t, top.Items[0])
	}
	for name, w := range want {
		if !reflect.DeepEqual(got[name], w) {
			t.Errorf("%s keys = %v, want %v", name, got[name], w)
		}
	}

	var back Plan
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, plan) {
		t.Errorf("round trip = %+v, %v; want %+v", back, err, plan)
	}
}

// Snapshot items store their description and RAM flag; other kinds' items
// omit them.
func TestSnapshotItemJSONKeys(t *testing.T) {
	b, err := json.Marshal(Item{Name: "team01-teak", Steps: []Step{StepSnapshot}, Snapshot: "before-scoring", Description: "round 2", VMState: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"action", "blocked", "description", "host", "name", "node", "snapshot", "steps", "team", "template", "vmid", "vmstate"}
	if got := keys(t, b); !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot item keys = %v, want %v", got, want)
	}
}
