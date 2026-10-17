package pods

import (
	"fmt"
	"slices"
)

// The parts of an environment request an EnvProblem names.
const (
	FieldDomain  = "domain"
	FieldSlot    = "slot"
	FieldName    = "name"
	FieldImage   = "image"
	FieldAddress = "address"
)

// EnvProblem is something the planner refuses or blocks in an environment
// request. Field says which part: the domain, the slot, or a host's name,
// image or address, where Row is the host: 0 the router, n req.Hosts[n-1].
// Why starts with a word, never a name, so a form can capitalise it.
type EnvProblem struct {
	Row   int
	Field string
	Why   string
}

func (p EnvProblem) Error() string { return p.Why }

// CheckEnv checks a create's or add's req as EnvCreate and EnvAdd do, given
// what they read and the slots queued jobs will fill. It asks Proxmox
// nothing; planning may find more. A domain already in its chosen slot, or
// a host already in it, is resumed, not a problem.
func CheckEnv(kind Kind, envs []Environment, queued []Slot, images []BaseImage, req EnvRequest) []EnvProblem {
	var out []EnvProblem
	add := func(err error) {
		if pr, ok := err.(EnvProblem); ok {
			out = append(out, pr)
		}
	}
	reserved := map[string]bool{}
	for _, s := range queued {
		reserved[slotLock(s.Name)] = true
	}
	env, hosts := Environment{Domain: req.Domain}, req.Hosts
	withRouter := req.Router.Host != ""
	switch kind {
	case KindEnvCreate:
		if err := CheckDomain(req.Domain); err != nil {
			out = append(out, EnvProblem{Field: FieldDomain, Why: err.Error()})
		}
		var err error
		if req.Slot != 0 {
			env, _, err = chosenEnv(envs, reserved, req)
		} else if env, err = firstFreeEnv(envs, reserved, req.Domain); err == nil && env.Domain == "" {
			err = EnvProblem{Field: FieldSlot, Why: allSlotsInUse}
		}
		add(err)
		if !withRouter {
			add(errNoRouter)
		}
	case KindEnvAdd:
		e, ok := EnvByDomain(envs, req.Domain)
		if !ok {
			add(noEnv(req.Domain))
			break
		}
		env = e
		if err := oneRouter(env, req); err != nil {
			add(err)
			hosts = req.Hosts
			withRouter = false
		}
	}
	if withRouter {
		hosts = append([]HostSpec{req.Router}, hosts...)
	}
	for i, pr := range checkHosts(env, images, hosts, withRouter) {
		if pr.Why != "" {
			if pr.Row = i; !withRouter {
				pr.Row++
			}
			out = append(out, pr)
		}
	}
	return out
}

// checkHosts is what blocks each of hosts in env, by index, before
// Proxmox is asked anything; Why is "" for a host without a problem. A
// host already in env is resumed. With routerFirst, hosts[0] is the
// request's router, so its image must be a router.
func checkHosts(env Environment, images []BaseImage, hosts []HostSpec, routerFirst bool) []EnvProblem {
	out := make([]EnvProblem, len(hosts))
	_, claimed := claimOctets(env, hosts)
	seen := map[string]bool{}
	router := false // an earlier host is a router this request adds
	for i, h := range hosts {
		im, found := Newest(images, h.Family)
		existing, resumed := env.Host(h.Host + "." + env.Domain)
		mismatch := ""
		if found && resumed {
			mismatch = resumeMismatch(existing, im, h.Family)
		}
		switch {
		case seen[h.Host]:
			out[i] = EnvProblem{Field: FieldName, Why: "named twice"}
		case !found:
			out[i] = EnvProblem{Field: FieldImage, Why: fmt.Sprintf("no usable base image of family %q", h.Family)}
		case mismatch != "":
			out[i] = EnvProblem{Field: FieldImage, Why: "host " + existing.Name + " already exists " + mismatch}
		case i == 0 && routerFirst && im.Kind != ImageRouter:
			out[i] = EnvProblem{Field: FieldImage, Why: fmt.Sprintf("the router needs a router image, not %s (%s)", h.Family, im.Kind)}
		case im.Kind == ImageRouter && (env.HasRouter() && !resumed || router):
			out[i] = EnvProblem{Field: FieldImage, Why: "an environment has one router"}
		case im.Kind == ImageWindows && len(h.Host) > MaxWindowsHostname:
			out[i] = EnvProblem{Field: FieldName, Why: fmt.Sprintf("Windows host names are at most %d characters", MaxWindowsHostname)}
		case (im.Kind == ImageWindows || im.Kind == ImageLinux) && h.Octet != 0 && claimed[h.Octet] != h.Host &&
			!(resumed && LastOctet(existing.Notes.IP) != 0):
			out[i] = EnvProblem{Field: FieldAddress, Why: fmt.Sprintf("address .%d is already taken in %s", h.Octet, env.Domain)}
		}
		seen[h.Host] = true
		router = router || found && im.Kind == ImageRouter && out[i].Why == ""
	}
	return out
}

// claimOctets is the octets in use after this request's explicit claims,
// and which host claims each: the first to name a free one. A resumed host
// keeps its own.
func claimOctets(env Environment, hosts []HostSpec) (used map[int]bool, claimed map[int]string) {
	used, claimed = env.UsedOctets(), map[int]string{}
	for _, h := range hosts {
		if existing, ok := env.Host(h.Host + "." + env.Domain); ok && existing.Notes.IP != "" {
			continue
		}
		if h.Octet != 0 && !used[h.Octet] && claimed[h.Octet] == "" {
			claimed[h.Octet] = h.Host
		}
	}
	for o := range claimed {
		used[o] = true
	}
	return used, claimed
}

// chosenEnv is the environment a create of req, which names its slot,
// goes in as far as envs and reserved tell (freeState): a new one in that
// slot, or the domain's own there, to resume (resumed).
func chosenEnv(envs []Environment, reserved map[string]bool, req EnvRequest) (env Environment, resumed bool, err error) {
	env, err = slotN(envs, req.Slot)
	if err != nil {
		return Environment{}, false, EnvProblem{Field: FieldSlot, Why: err.Error()}
	}
	if e, ok := EnvByDomain(envs, req.Domain); ok && e.Slot.N != req.Slot {
		return Environment{}, false, domainElsewhere(req.Domain, e)
	}
	if env.Domain == req.Domain {
		return env, true, nil
	}
	if err := slotRefused(freeState(env, reserved), env.Slot); err != nil {
		return Environment{}, false, err
	}
	return Environment{Slot: env.Slot, Domain: req.Domain, Gateway: env.Gateway}, false, nil
}

// firstFreeEnv is a new environment for domain in the first slot of envs
// that freeState finds open; one without Domain if there is none.
func firstFreeEnv(envs []Environment, reserved map[string]bool, domain string) (Environment, error) {
	if e, ok := EnvByDomain(envs, domain); ok {
		return Environment{}, domainElsewhere(domain, e)
	}
	if i := slices.IndexFunc(envs, func(e Environment) bool { return freeState(e, reserved) == slotOpen }); i >= 0 {
		return Environment{Slot: envs[i].Slot, Domain: domain, Gateway: envs[i].Gateway}, nil
	}
	return Environment{}, nil
}

// slotRefused says why a new environment may not take slot s, in state
// st; nil if it may.
func slotRefused(st slotState, s Slot) error {
	why := map[slotState]string{
		slotInUse:  "dev slot %s is in use; choose a free one",
		slotQueued: "a queued job will fill dev slot %s; choose another",
		slotHidden: "dev slot %s holds VMs you can't see; choose another",
	}[st]
	if why == "" {
		return nil
	}
	return EnvProblem{Field: FieldSlot, Why: fmt.Sprintf(why, s.Name)}
}

func domainElsewhere(domain string, e Environment) error {
	return EnvProblem{Field: FieldDomain, Why: fmt.Sprintf("domain %s is already in dev slot %s; add hosts to it instead", domain, e.Slot.Name)}
}

func noEnv(domain string) error {
	return EnvProblem{Field: FieldDomain, Why: fmt.Sprintf("no dev slot holds %s; create it first", domain)}
}

// oneRouter refuses a router req adds to env beside its own; naming the
// existing one again resumes it.
func oneRouter(env Environment, req EnvRequest) error {
	if _, exists := env.Host(req.Router.Host + "." + env.Domain); req.Router.Host != "" && env.HasRouter() && !exists {
		return EnvProblem{Field: FieldName, Why: "environment " + req.Domain + " already has a router"}
	}
	return nil
}

var errNoRouter = EnvProblem{Field: FieldName, Why: "a new environment needs a router"}

const allSlotsInUse = "every dev slot is in use; archive an environment to free one"
