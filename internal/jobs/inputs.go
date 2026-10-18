// Package jobs stores confirmed requests as jobs and runs them. A job keeps
// the user's inputs and the confirmed plan's fingerprint; the worker
// re-plans and runs only if it still matches.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wccomps/battleship/internal/pods"
)

// Inputs is what a user asked for; it is replanned when the job runs.
type Inputs struct {
	Kind       pods.Kind `json:"kind"`
	Teams      string    `json:"teams"` // as typed, e.g. "1-32"
	Hosts      []string  `json:"hosts,omitempty"`
	Pattern    string    `json:"pattern,omitempty"`     // deploy
	Rebuild    bool      `json:"rebuild,omitempty"`     // deploy
	NoSnapshot bool      `json:"no_snapshot,omitempty"` // deploy
	// Set is the one template set a teardown takes (pods.ParseSet). A
	// teardown without one, as every one stored before sets, takes
	// everything in its pods (among Hosts).
	Set pods.Set `json:"set,omitempty"`
	// Snapshot is a reset's target (empty: baseline) or a snapshot job's
	// new snapshot.
	Snapshot    string `json:"snapshot,omitempty"`
	Description string `json:"description,omitempty"` // snapshot
	VMState     bool   `json:"vmstate,omitempty"`     // snapshot: also save the RAM of running VMs
	Action      string `json:"action,omitempty"`      // power
	// VMs, if set, narrows the plan to these VMs (e.g. a retry's failures):
	// team VMs, each of one of Teams, or an environment's, each of Domain.
	VMs []string `json:"vms,omitempty"`

	// Environments.
	Domain   string   `json:"domain,omitempty"`
	Slot     string   `json:"slot,omitempty"`      // create, teardown: two digits, e.g. "02"; empty takes the first free, or the domain's
	Router   string   `json:"router,omitempty"`    // create: "name=family"
	EnvHosts []string `json:"env_hosts,omitempty"` // create, add: "name=family[:octet]" each
	DNS      string   `json:"dns,omitempty"`       // env-dns: "router" or a host name
	pods.ImageRequest
}

// Typed is what a person types to confirm in (pods.Kind.TypedConfirm): a
// teardown's pod range or an environment's domain, as given.
func (in Inputs) Typed() string {
	switch in.Kind {
	case pods.KindTeardown:
		return strings.TrimSpace(in.Teams)
	case pods.KindEnvTeardown:
		return strings.TrimSpace(in.Domain)
	}
	return ""
}

// TypedWhat names what Typed is, for asking for it: "the pod range" or
// "the domain"; "" when nothing is typed.
func (in Inputs) TypedWhat() string {
	switch in.Kind {
	case pods.KindTeardown:
		return "the pod range"
	case pods.KindEnvTeardown:
		return "the domain"
	}
	return ""
}

// MaxSnapshotDescription is the longest snapshot description, in characters.
const MaxSnapshotDescription = 500

// Validate checks the inputs without contacting Proxmox.
func (in Inputs) Validate() error {
	if err := in.checkText(); err != nil {
		return err
	}
	if in.Kind.Teamless() {
		return errors.Join(in.validateTeamless()...)
	}
	var errs []error
	switch in.Kind {
	case pods.KindDeploy:
		if strings.TrimSpace(in.Pattern) == "" {
			errs = append(errs, errors.New("deploy needs a template pattern"))
		}
	case pods.KindPower:
		if !pods.IsPowerAction(in.Action) {
			errs = append(errs, fmt.Errorf("power action must be start, shutdown, stop or reboot, not %q", in.Action))
		}
	case pods.KindSnapshot:
		if err := pods.CheckSnapshotName(in.Snapshot); err != nil {
			errs = append(errs, err)
		}
		if n := utf8.RuneCountInString(in.Description); n > MaxSnapshotDescription {
			errs = append(errs, fmt.Errorf("the snapshot description is too long: %d characters, at most %d", n, MaxSnapshotDescription))
		}
	case pods.KindTeardown:
		if _, err := pods.ParseSet(string(in.Set)); err != nil {
			errs = append(errs, err)
		}
	case pods.KindReset:
	default:
		errs = append(errs, fmt.Errorf("unknown job kind %q", in.Kind))
	}
	if _, err := pods.ParseTeams(in.Teams); err != nil {
		errs = append(errs, fmt.Errorf("pods: %w", err))
	}
	for _, h := range in.Hosts {
		if strings.TrimSpace(h) == "" {
			errs = append(errs, errors.New("hosts must not contain blank entries"))
			break
		}
	}
	for _, v := range in.VMs {
		if strings.TrimSpace(v) == "" {
			errs = append(errs, errors.New("vms must not contain blank entries"))
			break
		}
	}
	return errors.Join(errs...)
}

// checkText rejects invalid UTF-8 and control characters in every string
// field. Each value is one line: the form splits env hosts by line.
func (in Inputs) checkText() error {
	fields := []struct {
		name   string
		values []string
	}{
		{"pods", []string{in.Teams}}, {"hosts", in.Hosts}, {"vms", in.VMs}, {"pattern", []string{in.Pattern}}, {"set", []string{string(in.Set)}},
		{"snapshot", []string{in.Snapshot}}, {"description", []string{in.Description}}, {"action", []string{in.Action}},
		{"domain", []string{in.Domain}}, {"slot", []string{in.Slot}}, {"router", []string{in.Router}}, {"env hosts", in.EnvHosts}, {"dns", []string{in.DNS}},
		{"family", []string{in.Family}}, {"image kind", []string{in.ImageKind}}, {"image", []string{in.Image}},
	}
	if d := in.Details; d != nil {
		fields = append(fields, struct {
			name   string
			values []string
		}{"details", []string{d.OS, d.User, d.Password, d.Built, d.Generalized, d.Installed, d.Licence}})
	}
	var errs []error
	for _, f := range fields {
		for _, v := range f.values {
			if !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) {
				// Bounded: the error reaches pages and logs.
				if len(v) > 100 {
					v = v[:100] + "..."
				}
				errs = append(errs, fmt.Errorf("%s must be printable text, not %q", f.name, v))
				break
			}
		}
	}
	return errors.Join(errs...)
}

// BuildPlan plans the inputs against the cluster. With p.Access it blocks
// what the user lacks privileges for, so preview and job plan agree.
func BuildPlan(ctx context.Context, p pods.Planner, in Inputs) (*pods.Plan, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	var teams []string
	if !in.Kind.Teamless() {
		teams, _ = pods.ParseTeams(in.Teams)
		if err := checkVMs(in, teams, p.Naming); err != nil {
			return nil, err
		}
	}
	plan, err := planFor(ctx, p, in, teams)
	if err != nil {
		return nil, err
	}
	keepVMs(plan, in.VMs)
	if p.Access != nil {
		if err := pods.BlockUnpermitted(ctx, plan, p.Access, p.Cfg); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func checkVMs(in Inputs, teams []string, naming pods.Naming) error {
	var errs []error
	for _, name := range in.VMs {
		team, _, ok := naming.ParseVMName(name)
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("vms: %q is not a pod VM name", name))
		case !slices.Contains(teams, team):
			errs = append(errs, fmt.Errorf("vms: %s is not in pods %s", name, in.Teams))
		}
	}
	return errors.Join(errs...)
}

// keepVMs narrows plan (and a deploy's templates) to vms. Plan teams stay as
// asked, so the job still locks every one.
func keepVMs(plan *pods.Plan, vms []string) {
	if len(vms) == 0 {
		return
	}
	var items []pods.Item
	used := map[string]bool{}
	for _, it := range plan.Items {
		if slices.Contains(vms, it.Name) {
			items = append(items, it)
			used[it.Template] = true
		}
	}
	plan.Items = items
	var templates []pods.TemplateSpec
	for _, t := range plan.Templates {
		if used[t.Name] {
			templates = append(templates, t)
		}
	}
	plan.Templates = templates
}

// MissingVMs lists, sorted, the names in in.VMs that plan has no item for.
func MissingVMs(in Inputs, plan *pods.Plan) []string {
	var out []string
	for _, name := range in.VMs {
		if !slices.ContainsFunc(plan.Items, func(it pods.Item) bool { return it.Name == name }) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func planFor(ctx context.Context, p pods.Planner, in Inputs, teams []string) (*pods.Plan, error) {
	switch in.Kind {
	case pods.KindDeploy:
		return p.Deploy(ctx, pods.DeployRequest{
			Pattern: in.Pattern, Teams: teams, Hosts: in.Hosts, Rebuild: in.Rebuild, Snapshot: !in.NoSnapshot,
		})
	case pods.KindTeardown:
		set, _ := pods.ParseSet(string(in.Set))
		return p.Teardown(ctx, teams, in.Hosts, set)
	case pods.KindReset:
		return p.Reset(ctx, teams, in.Hosts, in.Snapshot)
	case pods.KindSnapshot:
		return p.Snapshot(ctx, pods.SnapshotRequest{
			Teams: teams, Hosts: in.Hosts, Name: in.Snapshot, Description: in.Description, VMState: in.VMState,
		})
	case pods.KindEnvCreate, pods.KindEnvAdd, pods.KindEnvDNS, pods.KindEnvTeardown:
		req, err := in.EnvRequest()
		if err != nil {
			return nil, err
		}
		switch in.Kind {
		case pods.KindEnvCreate:
			return p.EnvCreate(ctx, req)
		case pods.KindEnvAdd:
			return p.EnvAdd(ctx, req)
		case pods.KindEnvTeardown:
			return p.EnvTeardown(ctx, req)
		}
		return p.EnvDNS(ctx, req)
	case pods.KindImageRegister:
		return p.ImageRegister(ctx, in.ImageRequest)
	case pods.KindImageEdit:
		return p.ImageEdit(ctx, in.ImageRequest)
	case pods.KindImageDetails:
		return p.ImageSetDetails(ctx, in.ImageRequest)
	case pods.KindImageDeprecate:
		return p.ImageDeprecate(ctx, in.ImageRequest)
	case pods.KindImageDelete:
		return p.ImageDelete(ctx, in.ImageRequest)
	default: // KindPower; Validate rejected anything else
		return p.Power(ctx, teams, in.Hosts, in.Action)
	}
}

// SplitList splits a comma-separated list, trimming spaces and dropping
// empty entries.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// EnvRequest parses an environment job's inputs.
func (in Inputs) EnvRequest() (pods.EnvRequest, error) {
	req := pods.EnvRequest{Domain: strings.TrimSpace(in.Domain), DNS: strings.TrimSpace(in.DNS)}
	var errs []error
	if in.Slot != "" {
		n, ok := pods.ParseSlotName(in.Slot)
		if !ok {
			errs = append(errs, fmt.Errorf("slot %q must be a dev slot's number, two digits like 02", in.Slot))
		}
		req.Slot = n
	}
	if strings.TrimSpace(in.Router) != "" {
		r, err := pods.ParseHostSpec(in.Router)
		if err != nil {
			errs = append(errs, fmt.Errorf("router: %w", err))
		}
		req.Router = r
	}
	for _, line := range in.EnvHosts {
		h, err := pods.ParseHostSpec(line)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		req.Hosts = append(req.Hosts, h)
	}
	return req, errors.Join(errs...)
}

// validateTeamless checks the inputs of image and environment kinds.
func (in Inputs) validateTeamless() []error {
	var errs []error
	if in.Teams != "" || len(in.Hosts) > 0 {
		errs = append(errs, errors.New("pods and hosts don't apply to images and environments"))
	}
	switch {
	case len(in.VMs) == 0:
	case in.Kind != pods.KindEnvCreate && in.Kind != pods.KindEnvAdd:
		errs = append(errs, errors.New("VMs apply only to creating environments and adding hosts"))
	default:
		for _, v := range in.VMs {
			if host, domain, _ := strings.Cut(v, "."); pods.CheckHostname(host) != nil || domain != strings.TrimSpace(in.Domain) {
				errs = append(errs, fmt.Errorf("vms: %q isn't a VM of %s", v, strings.TrimSpace(in.Domain)))
				break
			}
		}
	}
	if in.Slot != "" && in.Kind != pods.KindEnvCreate && in.Kind != pods.KindEnvTeardown {
		errs = append(errs, errors.New("a slot applies only to creating or tearing down an environment"))
	}
	switch {
	case in.Details == nil:
		if in.Kind == pods.KindImageDetails {
			errs = append(errs, errors.New("name the details to write"))
		}
	case in.Kind != pods.KindImageRegister && in.Kind != pods.KindImageDetails:
		errs = append(errs, errors.New("details apply only to registering a base image and editing its notes"))
	case (in.Details.Free != nil || in.Details.From != "") && in.Kind != pods.KindImageDetails:
		errs = append(errs, errors.New("free text, and the notes a form opened on, apply only to editing a base image's notes"))
	}
	switch in.Kind {
	case pods.KindEnvCreate, pods.KindEnvAdd, pods.KindEnvDNS, pods.KindEnvTeardown:
		if err := pods.CheckDomain(strings.TrimSpace(in.Domain)); err != nil {
			errs = append(errs, err)
		}
		req, err := in.EnvRequest()
		if err != nil {
			errs = append(errs, err)
		}
		switch {
		case in.Kind == pods.KindEnvCreate && req.Router.Host == "":
			errs = append(errs, errors.New("a new environment needs a router, e.g. gw=opnsense"))
		case in.Kind == pods.KindEnvAdd && req.Router.Host == "" && len(in.EnvHosts) == 0:
			errs = append(errs, errors.New("name at least one host to add"))
		case in.Kind == pods.KindEnvDNS && req.DNS != "" && req.DNS != pods.DNSRouter:
			if err := pods.CheckHostname(req.DNS); err != nil {
				errs = append(errs, err)
			}
		}
	case pods.KindImageRegister:
		if in.VMID < 100 {
			errs = append(errs, errors.New("name the VM to register by its VMID"))
		}
		if err := pods.CheckFamily(in.Family); err != nil {
			errs = append(errs, err)
		}
		if in.ImageKind != "" && !slices.Contains(pods.ImageKinds, in.ImageKind) {
			errs = append(errs, fmt.Errorf("image kind must be one of %s", strings.Join(pods.ImageKinds, ", ")))
		}
	case pods.KindImageEdit, pods.KindImageDetails:
		if err := pods.CheckFamily(in.Family); err != nil {
			errs = append(errs, err)
		}
	case pods.KindImageDeprecate, pods.KindImageDelete:
		if _, _, ok := pods.ParseImageName(in.Image); !ok {
			errs = append(errs, fmt.Errorf("%q isn't a base image name like debian-12-v1", in.Image))
		}
	}
	return errs
}
