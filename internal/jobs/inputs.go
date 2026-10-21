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
	Domain     string   `json:"domain,omitempty"`
	Slot       string   `json:"slot,omitempty"`        // create, teardown: two digits, e.g. "02"; empty takes the first free, or the domain's
	Router     string   `json:"router,omitempty"`      // create: "name=family"
	EnvHosts   []string `json:"env_hosts,omitempty"`   // create, add: "name=family[:octet]" each
	EnvHost    string   `json:"env_host,omitempty"`    // env-edit, env-delete-host: host label or full VM name
	Octet      int      `json:"octet,omitempty"`       // env-edit: FirstHostOctet..LastHostOctet, ClearOctet, or 0
	EjectCDROM bool     `json:"eject_cdrom,omitempty"` // env-edit: eject attached CD-ROM media
	CPU        string   `json:"cpu,omitempty"`         // env-edit: custom CPU model name
	pods.ImageRequest
}

type inputField uint16

const (
	inTeams inputField = 1 << iota
	inVMs
	inSlot
	inDetails
	inFreeNotes
	inDomain
	inFamily
	inImage
	inVMID
)

// request records a Kind's input and planning rules: which field groups it
// accepts or requires, what a typed confirmation asks for, its kind-specific
// checks, and how BuildPlan calls the planner.
type request struct {
	takes inputField
	needs inputField
	typed func(Inputs) (val, what string)
	check func(Inputs) []error
	plan  func(context.Context, pods.Planner, Inputs, []string) (*pods.Plan, error)
}

var requests = map[pods.Kind]request{
	pods.KindDeploy: {
		takes: inTeams | inVMs,
		check: func(in Inputs) []error {
			if strings.TrimSpace(in.Pattern) == "" {
				return []error{errors.New("deploy needs a template pattern")}
			}
			return nil
		},
		plan: func(ctx context.Context, p pods.Planner, in Inputs, teams []string) (*pods.Plan, error) {
			return p.Deploy(ctx, pods.DeployRequest{
				Pattern: in.Pattern, Teams: teams, Hosts: in.Hosts, Rebuild: in.Rebuild, Snapshot: !in.NoSnapshot,
			})
		},
	},
	pods.KindTeardown: {
		takes: inTeams | inVMs,
		typed: func(in Inputs) (string, string) { return strings.TrimSpace(in.Teams), "the pod range" },
		check: func(in Inputs) []error {
			if _, err := pods.ParseSet(string(in.Set)); err != nil {
				return []error{err}
			}
			return nil
		},
		plan: func(ctx context.Context, p pods.Planner, in Inputs, teams []string) (*pods.Plan, error) {
			set, _ := pods.ParseSet(string(in.Set))
			return p.Teardown(ctx, teams, in.Hosts, set)
		},
	},
	pods.KindReset: {
		takes: inTeams | inVMs,
		plan: func(ctx context.Context, p pods.Planner, in Inputs, teams []string) (*pods.Plan, error) {
			return p.Reset(ctx, teams, in.Hosts, in.Snapshot)
		},
	},
	pods.KindPower: {
		takes: inTeams | inVMs,
		check: func(in Inputs) []error {
			if !pods.IsPowerAction(in.Action) {
				return []error{fmt.Errorf("power action must be start, shutdown, stop or reboot, not %q", in.Action)}
			}
			return nil
		},
		plan: func(ctx context.Context, p pods.Planner, in Inputs, teams []string) (*pods.Plan, error) {
			return p.Power(ctx, teams, in.Hosts, in.Action)
		},
	},
	pods.KindSnapshot: {
		takes: inTeams | inVMs,
		check: func(in Inputs) []error {
			var errs []error
			if err := pods.CheckSnapshotName(in.Snapshot); err != nil {
				errs = append(errs, err)
			}
			if n := utf8.RuneCountInString(in.Description); n > MaxSnapshotDescription {
				errs = append(errs, fmt.Errorf("the snapshot description is too long: %d characters, at most %d", n, MaxSnapshotDescription))
			}
			return errs
		},
		plan: func(ctx context.Context, p pods.Planner, in Inputs, teams []string) (*pods.Plan, error) {
			return p.Snapshot(ctx, pods.SnapshotRequest{
				Teams: teams, Hosts: in.Hosts, Name: in.Snapshot, Description: in.Description, VMState: in.VMState,
			})
		},
	},
	pods.KindEnvCreate: {
		takes: inDomain | inSlot | inVMs,
		check: func(in Inputs) []error {
			if req, _ := in.EnvRequest(); req.Router.Host == "" {
				return []error{errors.New("a new environment needs a router, e.g. gw=opnsense")}
			}
			return nil
		},
		plan: envPlan(pods.Planner.EnvCreate),
	},
	pods.KindEnvAdd: {
		takes: inDomain | inVMs,
		check: func(in Inputs) []error {
			if req, _ := in.EnvRequest(); req.Router.Host == "" && len(in.EnvHosts) == 0 {
				return []error{errors.New("name at least one host to add")}
			}
			return nil
		},
		plan: envPlan(pods.Planner.EnvAdd),
	},
	pods.KindEnvTeardown: {
		takes: inDomain | inSlot,
		typed: func(in Inputs) (string, string) { return strings.TrimSpace(in.Domain), "the domain" },
		plan:  envPlan(pods.Planner.EnvTeardown),
	},
	pods.KindEnvEdit: {
		takes: inVMID | inDetails | inFreeNotes,
		check: func(in Inputs) []error {
			var errs []error
			if in.VMID < 100 {
				errs = append(errs, errors.New("name the host to edit by its VMID"))
			}
			if h := strings.TrimSpace(in.EnvHost); h != "" {
				if hostPart, domPart, ok := strings.Cut(h, "."); ok {
					if err := pods.CheckHostname(hostPart); err != nil {
						errs = append(errs, err)
					}
					if err := pods.CheckDomain(domPart); err != nil {
						errs = append(errs, err)
					}
				} else if err := pods.CheckHostname(h); err != nil {
					errs = append(errs, err)
				}
			}
			if in.ImageKind != "" && !slices.Contains(pods.ImageKinds, in.ImageKind) {
				errs = append(errs, fmt.Errorf("host kind must be one of %s", strings.Join(pods.ImageKinds, ", ")))
			}
			if in.Octet != 0 && in.Octet != pods.ClearOctet && (in.Octet < pods.FirstHostOctet || in.Octet > pods.LastHostOctet) {
				errs = append(errs, fmt.Errorf("the address must be %d-%d (.%d is the router, .3 the team VPN)", pods.FirstHostOctet, pods.LastHostOctet, pods.GatewayOctet))
			}
			if in.Image != "" && in.Image != pods.NoBaseImage {
				if _, _, ok := pods.ParseImageName(in.Image); !ok {
					errs = append(errs, fmt.Errorf("%q isn't a base image name like debian-12-v1", in.Image))
				}
			}
			if in.CPU != "" {
				if err := pods.CheckCPUModel(in.CPU); err != nil {
					errs = append(errs, err)
				}
			}
			return errs
		},
		plan: func(ctx context.Context, p pods.Planner, in Inputs, _ []string) (*pods.Plan, error) {
			return p.EnvEdit(ctx, pods.HostEditInput{
				VMID:       in.VMID,
				Host:       strings.TrimSpace(in.EnvHost),
				Kind:       in.ImageKind,
				Octet:      in.Octet,
				Image:      in.Image,
				CPU:        in.CPU,
				EjectCDROM: in.EjectCDROM,
				Details:    in.Details,
			})
		},
	},
	pods.KindEnvPower: {
		takes: inVMID,
		check: func(in Inputs) []error {
			var errs []error
			if in.VMID < 100 {
				errs = append(errs, errors.New("name the host by its VMID"))
			}
			if !pods.IsPowerAction(in.Action) {
				errs = append(errs, fmt.Errorf("power action must be start, shutdown, stop or reboot, not %q", in.Action))
			}
			return errs
		},
		plan: func(ctx context.Context, p pods.Planner, in Inputs, _ []string) (*pods.Plan, error) {
			return p.EnvPower(ctx, in.VMID, in.Action)
		},
	},
	pods.KindEnvDeleteHost: {
		takes: inVMID,
		typed: func(in Inputs) (string, string) { return strings.TrimSpace(in.EnvHost), "the VM name" },
		check: func(in Inputs) []error {
			var errs []error
			if in.VMID < 100 {
				errs = append(errs, errors.New("name the host to delete by its VMID"))
			}
			if strings.TrimSpace(in.EnvHost) == "" {
				errs = append(errs, errors.New("name the host to delete"))
			}
			return errs
		},
		plan: func(ctx context.Context, p pods.Planner, in Inputs, _ []string) (*pods.Plan, error) {
			return p.EnvDeleteHost(ctx, in.VMID, strings.TrimSpace(in.EnvHost))
		},
	},
	pods.KindImageRegister: {
		takes: inVMID | inFamily | inDetails,
		check: func(in Inputs) []error {
			var errs []error
			if in.VMID < 100 {
				errs = append(errs, errors.New("name the VM to register by its VMID"))
			}
			if in.ImageKind != "" && !slices.Contains(pods.ImageKinds, in.ImageKind) {
				errs = append(errs, fmt.Errorf("image kind must be one of %s", strings.Join(pods.ImageKinds, ", ")))
			}
			return errs
		},
		plan: imagePlan(pods.Planner.ImageRegister),
	},
	pods.KindImageEdit: {
		takes: inFamily,
		plan:  imagePlan(pods.Planner.ImageEdit),
	},
	pods.KindImageDetails: {
		takes: inFamily | inDetails | inFreeNotes,
		needs: inDetails,
		plan:  imagePlan(pods.Planner.ImageSetDetails),
	},
	pods.KindImageDeprecate: {
		takes: inImage,
		plan:  imagePlan(pods.Planner.ImageDeprecate),
	},
	pods.KindImageDelete: {
		takes: inImage,
		plan:  imagePlan(pods.Planner.ImageDelete),
	},
}

func envPlan(fn func(pods.Planner, context.Context, pods.EnvRequest) (*pods.Plan, error)) func(context.Context, pods.Planner, Inputs, []string) (*pods.Plan, error) {
	return func(ctx context.Context, p pods.Planner, in Inputs, _ []string) (*pods.Plan, error) {
		req, err := in.EnvRequest()
		if err != nil {
			return nil, err
		}
		return fn(p, ctx, req)
	}
}

func imagePlan(fn func(pods.Planner, context.Context, pods.ImageRequest) (*pods.Plan, error)) func(context.Context, pods.Planner, Inputs, []string) (*pods.Plan, error) {
	return func(ctx context.Context, p pods.Planner, in Inputs, _ []string) (*pods.Plan, error) {
		return fn(p, ctx, in.ImageRequest)
	}
}

// Typed is what a person types to confirm in (pods.Kind.TypedConfirm): a
// teardown's pod range or an environment's domain, as given.
func (in Inputs) Typed() string {
	if r := requests[in.Kind]; r.typed != nil {
		v, _ := r.typed(in)
		return v
	}
	return ""
}

// TypedWhat names what Typed is, for asking for it: "the pod range" or
// "the domain"; "" when nothing is typed.
func (in Inputs) TypedWhat() string {
	if r := requests[in.Kind]; r.typed != nil {
		_, what := r.typed(in)
		return what
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
	r, ok := requests[in.Kind]
	if !ok {
		return fmt.Errorf("unknown job kind %q", in.Kind)
	}
	var errs []error
	if r.takes&inTeams != 0 {
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
	} else if in.Teams != "" || len(in.Hosts) > 0 {
		errs = append(errs, errors.New("pods and hosts don't apply to images and environments"))
	}
	if r.takes&inTeams == 0 && len(in.VMs) > 0 {
		if r.takes&inVMs == 0 {
			errs = append(errs, errors.New("VMs apply only to creating environments and adding hosts"))
		} else {
			for _, v := range in.VMs {
				if host, domain, _ := strings.Cut(v, "."); pods.CheckHostname(host) != nil || domain != strings.TrimSpace(in.Domain) {
					errs = append(errs, fmt.Errorf("vms: %q isn't a VM of %s", v, strings.TrimSpace(in.Domain)))
					break
				}
			}
		}
	}
	if in.Slot != "" && r.takes&inSlot == 0 {
		errs = append(errs, errors.New("a slot applies only to creating or tearing down an environment"))
	}
	switch {
	case in.Details == nil:
		if r.needs&inDetails != 0 {
			errs = append(errs, errors.New("name the details to write"))
		}
	case r.takes&inDetails == 0:
		errs = append(errs, errors.New("details apply only to registering a base image and editing its notes"))
	case (in.Details.Free != nil || in.Details.From != "") && r.takes&inFreeNotes == 0:
		errs = append(errs, errors.New("free text, and the notes a form opened on, apply only to editing a base image's notes"))
	}
	if r.takes&inDomain != 0 {
		if err := pods.CheckDomain(strings.TrimSpace(in.Domain)); err != nil {
			errs = append(errs, err)
		}
		if _, err := in.EnvRequest(); err != nil {
			errs = append(errs, err)
		}
	}
	if r.takes&inFamily != 0 {
		if err := pods.CheckFamily(in.Family); err != nil {
			errs = append(errs, err)
		}
	}
	if r.takes&inImage != 0 {
		if _, _, ok := pods.ParseImageName(in.Image); !ok {
			errs = append(errs, fmt.Errorf("%q isn't a base image name like debian-12-v1", in.Image))
		}
	}
	if r.check != nil {
		errs = append(errs, r.check(in)...)
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
		{"domain", []string{in.Domain}}, {"slot", []string{in.Slot}}, {"router", []string{in.Router}}, {"env hosts", in.EnvHosts}, {"env host", []string{in.EnvHost}}, {"cpu", []string{in.CPU}},
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
	plan, err := requests[in.Kind].plan(ctx, p, in, teams)
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
	req := pods.EnvRequest{Domain: strings.TrimSpace(in.Domain)}
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
