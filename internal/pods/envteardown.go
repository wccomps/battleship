package pods

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/wccomps/battleship/internal/proxmox"
)

// EnvTeardown plans deleting the environment with req.Domain: each VM in its
// slot's pool (Environment.Hosts), stopped cleanly and then deleted as a team
// teardown does. VMs outside the pool on its networks (Users) are never
// touched; the plan keeps them (Kept). Its half-deleted VMs are listed
// blocked, and disks gone VMs left in the slot's VMIDs are freed. Every
// item carries the slot's pool. req.Slot, if set (a retry's), names the slot, which may by then
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
	plan := p.envPlanIn(KindEnvTeardown, s, req.Domain)
	listed := map[int]proxmox.VM{}
	for _, vm := range vms {
		listed[vm.VMID] = vm
	}
	for _, h := range env.Hosts {
		if Domain(h.Name) != req.Domain {
			continue // teardownEnv refuses a slot of mixed domains
		}
		notes := h.Notes
		it := Item{Name: h.Name, Host: h.Host, VMID: h.VMID, Node: h.Node, Pool: s.Pool, Tags: h.Tags, Notes: &notes,
			Steps: []Step{StepStop, StepDelete}}
		if listed[h.VMID].Template {
			it.Blocked = "is a template, not one of " + req.Domain + "'s VMs; move it out of " + s.Pool + " first"
		}
		plan.Items = append(plan.Items, it)
	}
	for _, h := range env.HalfDeleted {
		it := halfDeletedItem(h.VMID, h.Node)
		it.Pool = s.Pool
		plan.Items = append(plan.Items, it)
	}
	for _, u := range env.Users {
		plan.Kept = append(plan.Kept, KeptVM{Name: u, Why: "outside " + s.Pool + " but has a NIC on its networks"})
	}
	var cands []int
	for id := s.FirstVMID; id <= s.LastVMID(); id++ {
		cands = append(cands, id)
	}
	err = p.orphanedDisks(ctx, plan, vms, cands, func(vmid int, node string) Item {
		return Item{Name: "disks-" + strconv.Itoa(vmid), VMID: vmid, Node: node, Pool: s.Pool, Steps: []Step{StepFreeDisks}}
	})
	if err != nil {
		return nil, err
	}
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
		env, err := slotN(envs, req.Slot)
		switch {
		case err != nil:
			return Environment{}, err
		case env.Mixed:
			return Environment{}, mixed(env)
		case env.Domain != "" && env.Domain != req.Domain:
			return Environment{}, fmt.Errorf("dev slot %s holds %s, not %s", env.Slot.Name, env.Domain, req.Domain)
		}
		return env, nil
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
