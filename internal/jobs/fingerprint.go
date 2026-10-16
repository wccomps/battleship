package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/wccomps/battleship/internal/pods"
)

// Fingerprint identifies what a plan will do, so a run can't differ from its
// confirmed preview. Nodes and blocked reasons are left out;
// fingerprint_test.go classifies every plan field.
func Fingerprint(plan *pods.Plan) string {
	type fpItem struct {
		Name     string
		VMID     int
		Steps    []pods.Step
		Blocked  bool
		Snapshot string
		Action   string
		Template string
		// omitempty keeps other kinds' fingerprints unchanged.
		Description string `json:",omitempty"`
		VMState     bool   `json:",omitempty"`

		Source     *fpSource         `json:",omitempty"`
		Pool       string            `json:",omitempty"`
		Tags       string            `json:",omitempty"`
		Notes      *pods.Notes       `json:",omitempty"`
		NICs       map[string]string `json:",omitempty"`
		Guest      *pods.Guest       `json:",omitempty"`
		Supersedes *fpSource         `json:",omitempty"`
	}
	type fpTemplate struct {
		Name           string
		VMID           int
		Exists         bool
		Rebuild        bool
		Blocked        bool
		WillStopMaster bool
		MasterVMID     int
		Interfaces     int
		GPU            bool
	}
	fp := struct {
		Kind      pods.Kind
		Teams     []string
		Templates []fpTemplate
		Items     []fpItem
		Config    string
		Locks     []string `json:",omitempty"`
	}{Kind: plan.Kind, Teams: plan.Teams, Config: plan.Config, Locks: plan.Locks}
	for _, t := range plan.Templates {
		fp.Templates = append(fp.Templates, fpTemplate{
			Name:           t.Name,
			VMID:           t.VMID,
			Exists:         t.Exists,
			Rebuild:        t.Rebuild,
			Blocked:        t.Blocked != "",
			WillStopMaster: t.WillStopMaster,
			MasterVMID:     t.MasterVMID,
			Interfaces:     t.Interfaces,
			GPU:            t.GPU,
		})
	}
	for _, it := range plan.Items {
		fp.Items = append(fp.Items, fpItem{
			Name:     it.Name,
			VMID:     it.VMID,
			Steps:    it.Steps,
			Blocked:  it.Blocked != "",
			Snapshot: it.Snapshot,
			Action:   it.Action,
			Template: it.Template,

			Description: it.Description,
			VMState:     it.VMState,

			Source: sourceFP(it.Source), Pool: it.Pool, Tags: it.Tags, Notes: it.Notes,
			NICs: it.NICs, Guest: it.Guest, Supersedes: sourceFP(it.Supersedes),
		})
	}
	b, _ := json.Marshal(fp) // plain structs of strings, ints and bools can't fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fpSource is a Source without Bridges, which only decide the privileges
// needed, as a template's do.
type fpSource struct {
	VMID int    `json:"vmid"`
	Node string `json:"node"`
	Name string `json:"name"`
}

func sourceFP(s *pods.Source) *fpSource {
	if s == nil {
		return nil
	}
	return &fpSource{VMID: s.VMID, Node: s.Node, Name: s.Name}
}
