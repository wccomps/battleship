package pods

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// Battleship enforces nothing: Proxmox checks every call with the caller's
// credential. A plan only marks in advance the VMs the user lacks a
// privilege for, so the preview shows them blocked and the job skips them.
// If a privilege is revoked after planning, Proxmox refuses with 403 and the
// item fails as not permitted.

// Access is what the planning user may do: their effective privileges at
// an ACL path, as Proxmox resolves them (inheritance, groups, pools).
type Access interface {
	Privileges(ctx context.Context, path string) (map[string]bool, error)
}

// PermissionReader reads a caller's privileges from Proxmox;
// *proxmox.Client is one.
type PermissionReader interface {
	Permissions(ctx context.Context) (proxmox.Permissions, error)
	PermissionsAt(ctx context.Context, path string) (map[string]bool, error)
}

// UserAccess is an Access read from Proxmox: one /access/permissions for all
// listed paths, and /access/permissions?path= once for any other. Unlisted
// paths are never guessed from parents, since a deeper ACL replaces the
// inherited one. Safe for concurrent use; caches for its whole life.
type UserAccess struct {
	r       PermissionReader
	mu      sync.Mutex
	listed  proxmox.Permissions // nil until read
	listErr error               // the listing failed: kept, and not asked again
	at      map[string]map[string]bool
}

// NewAccess reads privileges through r.
func NewAccess(r PermissionReader) *UserAccess {
	return &UserAccess{r: r, at: map[string]map[string]bool{}}
}

func (a *UserAccess) list(ctx context.Context) (proxmox.Permissions, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listed != nil || a.listErr != nil {
		return a.listed, a.listErr
	}
	p, err := a.r.Permissions(ctx)
	if err != nil {
		err = fmt.Errorf("reading your Proxmox privileges: %w", err)
		// Failures are cached for the access's life so pages don't re-ask per
		// cell; a cancelled or timed-out read is the caller's, so it isn't.
		if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			a.listErr = err
		}
		return nil, err
	}
	if p == nil {
		p = proxmox.Permissions{}
	}
	a.listed = p
	return p, nil
}

// Privileges returns the privileges at path, mapped to whether they
// propagate.
func (a *UserAccess) Privileges(ctx context.Context, path string) (map[string]bool, error) {
	listed, err := a.list(ctx)
	if err != nil {
		return nil, err
	}
	if p, ok := listed[path]; ok {
		return p, nil
	}
	a.mu.Lock()
	p, ok := a.at[path]
	a.mu.Unlock()
	if ok {
		return p, nil
	}
	p, err = a.r.PermissionsAt(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("reading your Proxmox privileges on %s: %w", path, err)
	}
	a.mu.Lock()
	a.at[path] = p
	a.mu.Unlock()
	return p, nil
}

// Anywhere reports whether any listed path grants priv, which decides
// whether a page offers that kind of action.
func (a *UserAccess) Anywhere(ctx context.Context, priv string) (bool, error) {
	listed, err := a.list(ctx)
	if err != nil {
		return false, err
	}
	for _, privs := range listed {
		if _, ok := privs[priv]; ok {
			return true, nil
		}
	}
	return false, nil
}

// grant is one privilege on one path.
type grant struct{ path, priv string }

// need is satisfied by any one of its grants. Its text names the first
// path, the one battleship acts through.
type need []grant

func (n need) text() string {
	privs := make([]string, len(n))
	same := true
	for i, g := range n {
		privs[i] = g.priv
		same = same && g.path == n[0].path
	}
	if !same {
		return n[0].priv + " on " + n[0].path
	}
	return strings.Join(privs, " or ") + " on " + n[0].path
}

func one(path, priv string) need { return need{{path, priv}} }

// VMPath and PoolPath are the ACL paths of a VM and of a pool.
func VMPath(vmid int) string      { return "/vms/" + strconv.Itoa(vmid) }
func PoolPath(pool string) string { return "/pool/" + pool }

// offerPrivileges are, per operation, the privileges any of which makes it
// worth offering: what its steps need (itemNeeds), or for deploy and
// teardown, anywhere. Previews check exactly.
var offerPrivileges = map[Kind][]string{
	KindPower:    {"VM.PowerMgmt"},
	KindReset:    {"VM.Snapshot", "VM.Snapshot.Rollback"},
	KindSnapshot: {"VM.Snapshot"},
	KindDeploy:   {"VM.Clone"},
	KindTeardown: {"VM.Allocate"},
}

// OfferPrivileges is offerPrivileges[k].
func OfferPrivileges(k Kind) []string { return slices.Clone(offerPrivileges[k]) }

// HoldsOffered reports whether acc grants any of k's offerPrivileges on VM
// vmid or on pool (for a VM a deploy will create; see on). Unreadable paths
// grant nothing.
func HoldsOffered(ctx context.Context, acc Access, k Kind, vmid int, pool string) bool {
	for _, path := range []string{VMPath(vmid), PoolPath(pool)} {
		held, err := acc.Privileges(ctx, path)
		if err == nil && slices.ContainsFunc(offerPrivileges[k], func(p string) bool { _, ok := held[p]; return ok }) {
			return true
		}
	}
	return false
}

// on is a need for priv on a VM: its own path if it exists; for a VM a clone
// will create in pool, the pool's privileges or what its path inherits.
func on(vmid int, pool, priv string) need {
	if pool == "" {
		return one(VMPath(vmid), priv)
	}
	return need{{PoolPath(pool), priv}, {VMPath(vmid), priv}}
}

// cpuMapping is the CPU model mapping a "custom-<name>" cpu needs
// Mapping.Use on, or "".
func cpuMapping(cpu string) string {
	model, _, _ := strings.Cut(cpu, ",")
	if name, ok := strings.CutPrefix(model, "custom-"); ok && name != "" {
		return "/mapping/cpu/" + name
	}
	return ""
}

// templateNeeds is what building t needs; nothing when it is reused.
func templateNeeds(t TemplateSpec, cfg config.Config) []need {
	if t.Exists && !t.Rebuild {
		return nil
	}
	ns := []need{
		one(VMPath(t.MasterVMID), "VM.Clone"),
		one(VMPath(t.VMID), "VM.Allocate"), // the copy, its conversion, and a rebuild's delete
		one("/storage/"+cfg.Deploy.Storage, "Datastore.AllocateSpace"),
	}
	if m := cpuMapping(t.CPU); m != "" {
		ns = append(ns, one(m, "Mapping.Use"))
	}
	if t.WillStopMaster {
		ns = append(ns, one(VMPath(t.MasterVMID), "VM.PowerMgmt"))
	}
	return ns
}

// itemNeeds is what running its steps needs. tpl is its template (deploy).
func itemNeeds(it Item, tpl *TemplateSpec, cfg config.Config) []need {
	pool := ""
	if slices.Contains(it.Steps, StepClone) {
		pool = NewNaming(cfg.Naming).Pool(it.Team)
	}
	var ns []need
	for _, st := range it.Steps {
		switch st {
		case StepClone:
			ns = append(ns,
				one(VMPath(tpl.VMID), "VM.Clone"),
				on(it.VMID, pool, "VM.Allocate"),
				one("/storage/"+cfg.Deploy.Storage, "Datastore.AllocateSpace"))
			if m := cpuMapping(tpl.CPU); m != "" {
				ns = append(ns, one(m, "Mapping.Use"))
			}
		case StepNetwork:
			ns = append(ns, on(it.VMID, pool, "VM.Config.Network"))
			if tpl != nil && tpl.CloudInit {
				ns = append(ns, on(it.VMID, pool, "VM.Config.Cloudinit")) // regenerating the drive
			}
			// Each NIC's bridge is an SDN vnet needing SDN.Use; bridges outside the
			// zone are left to Proxmox.
			bridges := []string{cfg.Network.IntBridge}
			if tpl != nil && tpl.Interfaces >= 2 {
				bridges = []string{cfg.Network.ExtBridge, cfg.Network.IntBridge}
			}
			var joins []string
			for _, b := range bridges {
				joins = append(joins, Expand(b, it.Team, ""))
				ns = append(ns, one("/sdn/zones/"+cfg.Proxmox.SDNZone+"/"+Expand(b, it.Team, ""), "SDN.Use"))
			}
			// A new clone's NICs start on its template's bridges, so SDN.Use is checked
			// on those too when they are team vnets.
			if pool != "" && tpl != nil {
				for _, b := range tpl.Bridges {
					if isTeamBridge(cfg.Network, b) && !slices.Contains(joins, b) {
						ns = append(ns, one("/sdn/zones/"+cfg.Proxmox.SDNZone+"/"+b, "SDN.Use"))
					}
				}
			}
		case StepDiskLimits:
			ns = append(ns, on(it.VMID, pool, "VM.Config.Disk"))
		case StepCDROM:
			ns = append(ns, on(it.VMID, pool, "VM.Config.CDROM"))
		case StepSnapshot:
			ns = append(ns, on(it.VMID, pool, "VM.Snapshot"))
		case StepStart, StepStop, StepPower:
			ns = append(ns, on(it.VMID, pool, "VM.PowerMgmt"))
		case StepRollback:
			ns = append(ns, need{{VMPath(it.VMID), "VM.Snapshot"}, {VMPath(it.VMID), "VM.Snapshot.Rollback"}})
		case StepDelete:
			ns = append(ns, one(VMPath(it.VMID), "VM.Allocate"))
		case StepFreeDisks:
			ns = append(ns, one("/storage/"+cfg.Deploy.Storage, "Datastore.Allocate"))
		}
	}
	return ns
}

// isTeamBridge reports whether bridge is some team's ext or int bridge (a
// vnet of proxmox.sdn_zone).
func isTeamBridge(net config.Network, bridge string) bool {
	return patternRE(net.ExtBridge).MatchString(bridge) || patternRE(net.IntBridge).MatchString(bridge)
}

// missing says which of ns acc doesn't satisfy, as "you don't have …"
// sentences joined with "; ", or "".
func missing(ctx context.Context, acc Access, ns []need) (string, error) {
	var out []string
	for _, n := range ns {
		ok := false
		for _, g := range n {
			privs, err := acc.Privileges(ctx, g.path)
			if err != nil {
				return "", err
			}
			if _, has := privs[g.priv]; has {
				ok = true
				break
			}
		}
		if text := "you don't have " + n.text(); !ok && !slices.Contains(out, text) {
			out = append(out, text)
		}
	}
	return strings.Join(out, "; "), nil
}

// BlockUnpermitted blocks each template and item the user lacks a privilege
// for, saying which, plus items cloning from a blocked template. Already
// blocked things keep their reason.
func BlockUnpermitted(ctx context.Context, plan *Plan, acc Access, cfg config.Config) error {
	templates := map[string]*TemplateSpec{}
	unpermitted := map[string]bool{} // templates blocked here
	for i := range plan.Templates {
		t := &plan.Templates[i]
		templates[t.Name] = t
		if t.Blocked != "" {
			continue
		}
		why, err := missing(ctx, acc, templateNeeds(*t, cfg))
		if err != nil {
			return err
		}
		t.Blocked, unpermitted[t.Name] = why, why != ""
	}
	for i := range plan.Items {
		it := &plan.Items[i]
		if it.Blocked != "" {
			continue
		}
		tpl := templates[it.Template]
		if tpl != nil && tpl.Blocked != "" && slices.Contains(it.Steps, StepClone) {
			it.Blocked, it.Unpermitted = "template "+tpl.Name+": "+tpl.Blocked, unpermitted[tpl.Name]
			continue
		}
		if tpl == nil && slices.Contains(it.Steps, StepClone) {
			continue // the planner blocks a clone without a template
		}
		why, err := missing(ctx, acc, itemNeeds(*it, tpl, cfg))
		if err != nil {
			return err
		}
		it.Blocked, it.Unpermitted = why, why != ""
	}
	return nil
}

// Approx is the privileges at path from listed paths alone: its own entry,
// else what the nearest listed parent propagates. Pages use it to decide
// what to offer; plans use Privileges, which asks Proxmox.
func (a *UserAccess) Approx(ctx context.Context, path string) (map[string]bool, error) {
	listed, err := a.list(ctx)
	if err != nil {
		return nil, err
	}
	if p, ok := listed[path]; ok {
		return p, nil
	}
	for p := path; p != "/"; {
		i := strings.LastIndex(p, "/")
		if i <= 0 {
			p = "/"
		} else {
			p = p[:i]
		}
		if privs, ok := listed[p]; ok {
			out := map[string]bool{}
			for k, prop := range privs {
				if prop {
					out[k] = true
				}
			}
			return out, nil
		}
	}
	return map[string]bool{}, nil
}
