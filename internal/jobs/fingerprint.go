package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strconv"

	"github.com/wccomps/battleship/internal/pods"
)

// Fingerprint identifies what a plan will do, so a run can't differ from the
// preview it was confirmed from. It leaves out nodes and blocked reasons,
// which change between preview and run; fingerprint_test.go classifies
// every plan field.
func Fingerprint(plan *pods.Plan) string {
	type fpItem struct {
		Name     string
		VMID     int
		Steps    []pods.Step
		Blocked  bool
		Snapshot string
		Action   string
		Template string
		// Left out when empty, so other kinds' plans keep their
		// fingerprints.
		Description string `json:",omitempty"`
		VMState     bool   `json:",omitempty"`
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
	}{Kind: plan.Kind, Teams: plan.Teams, Config: plan.Config}
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
		})
	}
	b, _ := json.Marshal(fp) // plain structs of strings, ints and bools can't fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// LockKeys names what a plan touches, so jobs that overlap run one at a time:
// team:NN for each team, and template:<name> and vmid:<n> for each template
// a deploy uses. The VMID is a key of its own because deploys of different
// masters can pick the same free VMID for their new templates. Keys are
// sorted and unique.
func LockKeys(plan *pods.Plan) []string {
	seen := map[string]bool{}
	for _, t := range plan.Teams {
		seen["team:"+t] = true
	}
	for _, t := range plan.Templates {
		seen["template:"+t.Name] = true
		if t.VMID != 0 {
			seen["vmid:"+strconv.Itoa(t.VMID)] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
