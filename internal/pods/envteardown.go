package pods

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"

	"github.com/wccomps/battleship/internal/proxmox"
)

// EnvTeardown plans deleting the environment with req.Domain: each VM in its
// slot's pool (Environment.Hosts), stopped cleanly and then deleted as a team
// teardown does. VMs outside the pool on its networks (Users) are never
// touched; the plan's notices name them. A VM a failed destroy left locked as
// destroyed is listed blocked, and disks gone VMs left in the slot's VMIDs
// are freed. req.Slot, if set (a retry's), names the slot, which may by then
// hold nothing of the domain but what a failed delete left.
func (p Planner) EnvTeardown(ctx context.Context, req EnvRequest) (*Plan, error) {
	if err := CheckDomain(req.Domain); err != nil {
		return nil, err
	}
	envs, vms, err := p.Environments(ctx)
	if err != nil {
		return nil, err
	}
	env, err := teardownEnv(envs, req)
	if err != nil {
		return nil, err
	}
	s := env.Slot
	plan := &Plan{Kind: KindEnvTeardown, Config: ConfigHash(KindEnvTeardown, p.Cfg),
		Locks: []string{"slot:" + s.Name, "domain:" + req.Domain}}
	listed := map[int]proxmox.VM{}
	for _, vm := range vms {
		listed[vm.VMID] = vm
	}
	itemized := map[int]bool{}
	for _, h := range env.Hosts {
		switch {
		case h.HalfDeleted:
			plan.Items = append(plan.Items, halfDeletedItem(h.VMID, h.Node))
		case Domain(h.Name) == req.Domain:
			notes := h.Notes
			it := Item{Name: h.Name, Host: h.Host, VMID: h.VMID, Node: h.Node, Tags: h.Tags, Notes: &notes,
				Steps: []Step{StepStop, StepDelete}}
			if listed[h.VMID].Template {
				it.Blocked = "is a template, not one of " + req.Domain + "'s VMs; move it out of " + s.Pool + " first"
			}
			plan.Items = append(plan.Items, it)
		default:
			continue // teardownEnv refuses a slot of mixed domains
		}
		itemized[h.VMID] = true
	}
	// A half-deleted VM may also have left the pool.
	for _, vm := range vms {
		if vm.Lock == "destroyed" && vm.VMID >= s.FirstVMID && vm.VMID <= s.LastVMID() && !itemized[vm.VMID] {
			plan.Items = append(plan.Items, halfDeletedItem(vm.VMID, vm.Node))
		}
	}
	for _, u := range env.Users {
		plan.Notices = append(plan.Notices, fmt.Sprintf("Not touched: %s is outside %s but has a NIC on its networks.", u, s.Pool))
	}
	err = p.orphanedDisks(ctx, plan, vms, func(held map[int]bool) []int {
		var out []int
		for id := s.FirstVMID; id <= s.LastVMID(); id++ {
			if !held[id] {
				out = append(out, id)
			}
		}
		return out
	}, func(vmid int, node string) (Item, bool) {
		return Item{Name: "disks-" + strconv.Itoa(vmid), VMID: vmid, Node: node, Steps: []Step{StepFreeDisks}}, true
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(plan.Items, func(i, j int) bool { return plan.Items[i].Name < plan.Items[j].Name })
	return plan, nil
}

// halfDeletedItem is a blocked delete of a VM a failed destroy left locked
// as destroyed; only an admin can finish it.
func halfDeletedItem(vmid int, node string) Item {
	return Item{Name: "half-deleted-" + strconv.Itoa(vmid), VMID: vmid, Node: node, Steps: []Step{StepDelete},
		Blocked: HalfDeletedText(vmid, node)}
}

// teardownEnv is the environment req tears down: the slot req.Slot names,
// else the one holding req.Domain. A slot of several domains is refused:
// tearing it down by one would leave the others half there.
func teardownEnv(envs []Environment, req EnvRequest) (Environment, error) {
	mixed := func(e Environment) error {
		return fmt.Errorf("dev slot %s holds VMs of several domains, so it can't be torn down as %s; rename them to one domain or move the others out of %s first",
			e.Slot.Name, req.Domain, e.Slot.Pool)
	}
	if req.Slot != 0 {
		i := slices.IndexFunc(envs, func(e Environment) bool { return e.Slot.N == req.Slot })
		switch {
		case i < 0:
			return Environment{}, fmt.Errorf("there's no dev slot %s", SlotName(req.Slot))
		case envs[i].Mixed:
			return Environment{}, mixed(envs[i])
		case envs[i].Domain != "" && envs[i].Domain != req.Domain:
			return Environment{}, fmt.Errorf("dev slot %s holds %s, not %s", envs[i].Slot.Name, envs[i].Domain, req.Domain)
		}
		return envs[i], nil
	}
	if e, ok := EnvByDomain(envs, req.Domain); ok {
		return e, nil
	}
	for _, e := range envs {
		if e.Mixed && slices.ContainsFunc(e.Hosts, func(h EnvHost) bool { return Domain(h.Name) == req.Domain }) {
			return Environment{}, mixed(e)
		}
	}
	return Environment{}, fmt.Errorf("no dev slot holds %s", req.Domain)
}
