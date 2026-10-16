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

// LockKeys names what a plan touches, so overlapping jobs serialize. Every
// VM a plan changes is a vmid: key: the items' VMs, each template, the
// image a register retires, and a master whose template the plan builds
// (the build may stop it). A VM a plan only copies (a base image an
// environment host or new version is cloned from) is a shared read:vmid:
// key: such jobs run together and wait only for one that changes it (see
// store's blocksSQL). So a deploy building from an environment host waits
// for Set DNS on it, an environment waits for its image's delete, and two
// deploys that pick the same free VMID for new templates don't overlap.
func LockKeys(plan *pods.Plan) []string {
	writes, reads := map[string]bool{}, map[string]bool{}
	vmid := func(set map[string]bool, id int) {
		if id != 0 {
			set["vmid:"+strconv.Itoa(id)] = true
		}
	}
	for _, t := range plan.Teams {
		writes["team:"+t] = true
	}
	for _, t := range plan.Templates {
		writes["template:"+t.Name] = true
		vmid(writes, t.VMID)
		if !t.Exists || t.Rebuild {
			vmid(writes, t.MasterVMID)
		}
	}
	for _, it := range plan.Items {
		vmid(writes, it.VMID)
		if it.Source != nil {
			vmid(reads, it.Source.VMID)
		}
		if it.Supersedes != nil {
			vmid(writes, it.Supersedes.VMID)
		}
	}
	for _, k := range plan.Locks {
		writes[k] = true
	}
	for k := range reads {
		if !writes[k] {
			writes["read:"+k] = true
		}
	}
	return slices.Sorted(maps.Keys(writes))
}
