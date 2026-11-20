package pods

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// Battleship enforces nothing: Proxmox checks every call with the caller's
// credential. A plan only marks in advance the VMs the user lacks a
// privilege for, so the preview shows them blocked and the job skips them.
// If a privilege is revoked after planning, Proxmox refuses with 403 and the
// item fails as not permitted.

// Access is what the planning user may do: their effective privileges at
// an ACL path, as Proxmox resolves them (inheritance, groups, pools), and
// the datacenter's tag-access rules.
type Access interface {
	Privileges(ctx context.Context, path string) (map[string]bool, error)
	TagAccess(ctx context.Context) (proxmox.TagAccess, error)
}

type PermissionReader interface {
	Permissions(ctx context.Context) (proxmox.Permissions, error)
	PermissionsAt(ctx context.Context, path string) (map[string]bool, error)
	TagAccess(ctx context.Context) (proxmox.TagAccess, error)
}

// UserAccess is an Access read from Proxmox: one /access/permissions for all
// listed paths, and /access/permissions?path= once for any other. Unlisted
// paths are never guessed from parents, since a deeper ACL replaces the
// inherited one. Safe for concurrent use; caches for its whole life.
type UserAccess struct {
	r        PermissionReader
	mu       sync.Mutex
	listed   proxmox.Permissions // nil until read
	listErr  error               // the listing failed: kept, and not asked again
	at       map[string]map[string]bool
	inflight map[string]chan struct{}
	tagsSet  bool
	tags     proxmox.TagAccess
	tagsErr  error
}

// NewAccess reads privileges through r.
func NewAccess(r PermissionReader) *UserAccess {
	return &UserAccess{r: r, at: map[string]map[string]bool{}, inflight: map[string]chan struct{}{}}
}

func (a *UserAccess) TagAccess(ctx context.Context) (proxmox.TagAccess, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tagsSet || a.tagsErr != nil {
		return a.tags, a.tagsErr
	}
	t, err := a.r.TagAccess(ctx)
	if err != nil {
		err = fmt.Errorf("reading Proxmox tag access: %w", err)
		if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			a.tagsErr = err
		}
		return proxmox.TagAccess{}, err
	}
	a.tags, a.tagsSet = t, true
	return t, nil
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
	for {
		a.mu.Lock()
		if p, ok := a.at[path]; ok {
			a.mu.Unlock()
			return p, nil
		}
		if wait, ok := a.inflight[path]; ok {
			a.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		wait := make(chan struct{})
		a.inflight[path] = wait
		a.mu.Unlock()
		p, err := a.r.PermissionsAt(ctx, path)
		a.mu.Lock()
		delete(a.inflight, path)
		if err == nil {
			a.at[path] = p
		}
		close(wait)
		a.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("reading your Proxmox privileges on %s: %w", path, err)
		}
		return p, nil
	}
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

// LiveAddrsPrivilege is what reading a VM's NIC addresses from its guest
// agent (LiveAddrs) needs; the SSH check in LiveAddrs also uses
// VM.GuestAgent.Unrestricted when held.
const LiveAddrsPrivilege = "VM.GuestAgent.Audit"

// OfferPrivileges are the privileges any of which makes k worth offering:
// what its steps need (itemNeeds), or for deploy and teardown, anywhere.
// Previews check exactly.
func OfferPrivileges(k Kind) []string { return slices.Clone(specs[k].offer) }

// HoldsOffered reports whether acc grants any of k's OfferPrivileges on VM
// vmid or on pool (for a VM a clone will create; see on). Unreadable paths
// grant nothing.
func HoldsOffered(ctx context.Context, acc Access, k Kind, vmid int, pool string) bool {
	paths := []string{VMPath(vmid)}
	if pool != "" {
		paths = append(paths, PoolPath(pool))
	}
	for _, path := range paths {
		held, err := acc.Privileges(ctx, path)
		if err == nil && slices.ContainsFunc(specs[k].offer, func(p string) bool { _, ok := held[p]; return ok }) {
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

// cloneNeeds is what cloning VM src as vmid into pool needs; bridges are
// src's NICs' paths (sourceBridges), which Proxmox's clone checks
// (sdnUse), and cpu its CPU model.
func cloneNeeds(cfg config.Config, src, vmid int, pool string, bridges []string, cpu string) []need {
	ns := []need{
		one(VMPath(src), "VM.Clone"),
		on(vmid, pool, "VM.Allocate"),
		one("/storage/"+cfg.Deploy.Storage, "Datastore.AllocateSpace"),
	}
	ns = append(ns, sdnUse(cfg, bridges)...)
	if m := cpuMapping(cpu); m != "" {
		ns = append(ns, one(m, "Mapping.Use"))
	}
	return ns
}

// vnetPaths are the paths below /sdn/zones/<zone>/ that pve-guest-common's
// check_vnet_access checks SDN.Use on for NIC value nic, on a clone or a
// netN write: <bridge>/<tag> when VLAN-tagged, <bridge>/<vlan> for each VLAN
// in trunks (a-b ranges expanded), and <bridge> only with neither.
func vnetPaths(nic string) []string {
	_, opts := splitOptions(nic)
	b, tag, trunks := opts["bridge"], opts["tag"], opts["trunks"]
	switch {
	case b == "":
		return nil
	case tag == "" && trunks == "":
		return []string{b}
	}
	var out []string
	if tag != "" {
		out = append(out, b+"/"+tag)
	}
	for _, r := range strings.FieldsFunc(trunks, isTagSeparator) {
		lo, hi, isRange := strings.Cut(r, "-")
		if !isRange {
			hi = lo
		}
		first, errLo := strconv.Atoi(lo)
		last, errHi := strconv.Atoi(hi)
		if errLo != nil || errHi != nil {
			continue // Proxmox refuses such a config
		}
		for v := first; v <= last; v++ {
			out = append(out, b+"/"+strconv.Itoa(v))
		}
	}
	return out
}

// sourceBridges are the vnetPaths of every NIC in cfg, distinct and sorted:
// what a clone of that VM is checked on.
func sourceBridges(cfg map[string]string) []string {
	var out []string
	forEachNIC(cfg, func(_, v string) { out = append(out, vnetPaths(v)...) })
	slices.Sort(out)
	return slices.Compact(out)
}

// sdnUse is SDN.Use on /sdn/zones/<zone>/<path> for each of paths
// (vnetPaths), as Proxmox asks it of every user but root@pam. Proxmox takes
// the zone from its SDN config (localnetwork for a bridge that isn't a
// vnet), which Battleship doesn't read: it asks only bridges whose zone it
// knows (bridgeZone) and leaves any other to Proxmox when the step runs.
func sdnUse(cfg config.Config, paths []string) []need {
	var ns []need
	for _, path := range paths {
		b, _, _ := strings.Cut(path, "/")
		if zone := bridgeZone(cfg, b); zone != "" {
			ns = append(ns, one("/sdn/zones/"+zone+"/"+path, "SDN.Use"))
		}
	}
	return ns
}

// Uncloneable is why acc may not clone tpl into team's pod, or "" when it
// may: what a deploy's clone step asks, whether or not the pod already
// holds a clone.
func Uncloneable(ctx context.Context, acc Access, cfg config.Config, tpl TemplateSpec, team string) (string, error) {
	n := NewNaming(cfg.Naming)
	return missing(ctx, acc, cloneNeeds(cfg, tpl.VMID, n.CloneVMID(team, tpl.VMID), n.Pool(team), tpl.Bridges, tpl.CPU))
}

// templateNeeds is what building t needs, a clone of its master into the
// images pool and the clone's conversion; nothing when it is reused.
func templateNeeds(t TemplateSpec, cfg config.Config) []need {
	if !t.Builds() {
		return nil
	}
	ns := cloneNeeds(cfg, t.MasterVMID, t.VMID, cfg.Images.Pool, t.Bridges, t.CPU)
	if t.Rebuild {
		ns = append(ns, one(VMPath(t.VMID), "VM.Allocate")) // deleting the old template, wherever it is
	}
	if t.WillStopMaster {
		ns = append(ns, one(VMPath(t.MasterVMID), "VM.PowerMgmt"))
	}
	return ns
}

func itemNeeds(it Item, tpl *TemplateSpec, cfg config.Config, tags proxmox.TagAccess) []need {
	pool := ""
	var ns []need
	for _, st := range it.Steps {
		switch st {
		case StepClone:
			pool = it.TargetPool(NewNaming(cfg.Naming))
			src, bridges, cpu := 0, []string(nil), ""
			if it.Source != nil {
				src, bridges = it.Source.VMID, it.Source.Bridges
			} else if tpl != nil {
				src, bridges, cpu = tpl.VMID, tpl.Bridges, tpl.CPU
			}
			ns = append(ns, cloneNeeds(cfg, src, it.VMID, pool, bridges, cpu)...)
		case StepNetwork:
			// Proxmox checks every netN value the step writes, rewritten from
			// NICsWas.
			ns = append(ns, on(it.VMID, pool, "VM.Config.Network"))
			var written map[string]string
			switch {
			case it.NICs != nil:
				written = NICChanges(it.NICsWas, it.NICs)
			case tpl != nil:
				if tpl.CloudInit {
					ns = append(ns, on(it.VMID, pool, "VM.Config.Cloudinit")) // regenerating the drive
				}
				written = NetworkChanges(cfg.Network, it.NICsWas, it.Team, tpl.Interfaces)
			}
			for _, k := range slices.Sorted(maps.Keys(written)) {
				if isNetKey(k) {
					ns = append(ns, sdnUse(cfg, vnetPaths(written[k]))...)
				}
			}
		case StepDiskLimits:
			ns = append(ns, on(it.VMID, pool, "VM.Config.Disk"))
		case StepCDROM:
			ns = append(ns, on(it.VMID, pool, "VM.Config.CDROM"))
		case StepCPU:
			ns = append(ns, on(it.VMID, pool, "VM.Config.CPU"))
			if m := cpuMapping(it.CPU); m != "" {
				ns = append(ns, one(m, "Mapping.Use"))
			}
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
		case StepRecord, StepClearRecord:
			// The step reads the comment it rewrites, so it keeps the rest.
			ns = append(ns, one(PoolPath(it.Pool), "Pool.Allocate"), one(PoolPath(it.Pool), "Pool.Audit"))
		case StepDetails, StepNotes:
			ns = append(ns, on(it.VMID, pool, "VM.Config.Options"))
		case StepLabel, StepSetTag:
			ns = append(ns, on(it.VMID, pool, "VM.Config.Options"))
			ns = append(ns, tagNeeds(changedTags(it, st), tags, cfg)...)
		case StepConvert:
			ns = append(ns, one(VMPath(it.VMID), "VM.Allocate"))
		case StepRetire:
			if it.Supersedes != nil {
				ns = append(ns, one(VMPath(it.Supersedes.VMID), "VM.Config.Options"))
				ns = append(ns, tagNeeds(changedTags(it, st), tags, cfg)...)
			}
		case StepWaitAgent, StepVerify:
			ns = append(ns, on(it.VMID, pool, "VM.GuestAgent.Audit"))
		case StepGuest:
			ns = append(ns, on(it.VMID, pool, "VM.GuestAgent.Unrestricted"))
			if it.Guest != nil && it.Guest.OS == ImageWindows {
				ns = append(ns, on(it.VMID, pool, "VM.PowerMgmt")) // the reboot
			}
		}
	}
	return ns
}

// changedTags are the tags step st of it adds to or removes from a VM's tag
// list: the label and tag-set steps' TagsChanged, and the deprecated tag the
// retire step adds to the version it supersedes (Newest, so not yet
// deprecated).
func changedTags(it Item, st Step) []string {
	switch {
	case st == StepLabel, st == StepSetTag:
		return it.TagsChanged
	case st == StepRetire && it.Supersedes != nil:
		return []string{DeprecatedTag}
	}
	return nil
}

// tagNeeds is what Proxmox's assert_tag_permissions asks, beyond
// VM.Config.Options, of a tag edit that adds or removes changed: Sys.Modify
// on / when user-tag-access refuses one of them, a registered tag always.
// deploy.approved_tag counts as registered whatever tags shows, since
// approval only works registered and a reader without Sys.Audit on / is
// shown no registered tags.
func tagNeeds(changed []string, tags proxmox.TagAccess, cfg config.Config) []need {
	approved := cfg.Deploy.ApprovedTag
	if slices.ContainsFunc(changed, func(t string) bool { return !tags.Allows(t) || approved != "" && t == approved }) {
		return []need{one("/", "Sys.Modify")}
	}
	return nil
}

// bridgeZone is the SDN zone of a bridge Battleship knows as a vnet: a slot
// bridge (environments.sdn_zone), the edit bridge (images.edit_zone) or a
// team bridge (proxmox.sdn_zone); "" for any other.
func bridgeZone(cfg config.Config, bridge string) string {
	slotRE := func(p string) *regexp.Regexp {
		return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(p), `\{slot\}`, `\d{2}`) + "$")
	}
	switch {
	case slotRE(cfg.Environments.IntBridge).MatchString(bridge), slotRE(cfg.Environments.ExtBridge).MatchString(bridge):
		return cfg.Environments.SDNZone
	case bridge == cfg.Images.EditBridge:
		return cfg.Images.EditZone
	case patternRE(cfg.Network.ExtBridge).MatchString(bridge), patternRE(cfg.Network.IntBridge).MatchString(bridge):
		return cfg.Proxmox.SDNZone
	}
	return ""
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

func BlockUnpermitted(ctx context.Context, plan *Plan, acc Access, cfg config.Config) error {
	if acc == nil {
		return errors.New("planning without the user's privileges")
	}
	templates := map[string]*TemplateSpec{}
	unpermitted := make([]bool, len(plan.Templates)) // templates blocked here
	var g errgroup.Group
	g.SetLimit(batchSize)
	for i := range plan.Templates {
		t := &plan.Templates[i]
		templates[t.Name] = t
		if t.Blocked != "" {
			continue
		}
		g.Go(func() error {
			why, err := missing(ctx, acc, templateNeeds(*t, cfg))
			if err != nil {
				return err
			}
			if why != "" {
				t.Blocked, t.WillStopMaster, unpermitted[i] = why, false, true
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	unpermByName := map[string]bool{}
	for i, t := range plan.Templates {
		unpermByName[t.Name] = unpermitted[i]
	}
	var tags proxmox.TagAccess
	for _, it := range plan.Items {
		if it.Blocked == "" && slices.ContainsFunc(it.Steps, func(st Step) bool { return len(changedTags(it, st)) > 0 }) {
			var err error
			tags, err = acc.TagAccess(ctx)
			if err != nil {
				return err
			}
			break
		}
	}
	for i := range plan.Items {
		it := &plan.Items[i]
		if it.Blocked != "" {
			continue
		}
		tpl := templates[it.Template]
		if tpl != nil && tpl.Blocked != "" && slices.Contains(it.Steps, StepClone) {
			it.Node = it.OldNode
			it.Blocked, it.Unpermitted = "template "+tpl.Name+": "+tpl.Blocked, unpermByName[tpl.Name]
			continue
		}
		if tpl == nil && it.Source == nil && slices.Contains(it.Steps, StepClone) {
			continue // the planner blocks a clone without a template
		}
		g.Go(func() error {
			why, err := missing(ctx, acc, itemNeeds(*it, tpl, cfg, tags))
			if err != nil {
				return err
			}
			it.Blocked, it.Unpermitted = why, why != ""
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	plan.BlockUnrebuildable()
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
