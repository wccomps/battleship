package web

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// confirmPreviews are a valid preview form of each operation.
var confirmPreviews = map[pods.Kind]url.Values{
	pods.KindPower:          {"teams": {"1"}, "action": {"stop"}},
	pods.KindReset:          {"teams": {"1"}, "hosts": {"dc"}},
	pods.KindSnapshot:       {"teams": {"1"}, "snapshot": {"round2"}, "description": {"before round 2"}},
	pods.KindDeploy:         {"teams": {"1"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}},
	pods.KindTeardown:       {"teams": {"1-2"}},
	pods.KindEnvCreate:      {"domain": {"new.env"}, "router": {"gw=gw"}, "env_hosts": {"teak=debian-12\noak=debian-12:20"}},
	pods.KindEnvAdd:         {"domain": {"lab.env"}, "env_hosts": {"oak=debian-12"}},
	pods.KindEnvDNS:         {"domain": {"lab.env"}, "dns": {"teak"}},
	pods.KindImageRegister:  {"vmid": {"7000"}, "family": {"debian-13"}, "image_kind": {"linux"}},
	pods.KindImageEdit:      {"family": {"debian-12"}},
	pods.KindImageDeprecate: {"image": {"debian-12-v1"}},
	pods.KindImageDelete:    {"image": {"debian-12-v1"}},
}

// confirmFields are every field a confirm form may carry.
var confirmFields = []string{
	fieldTeams, fieldHosts, fieldVMs, fieldPattern, fieldRebuild, fieldBaseline, fieldSnapshot, fieldDescription, fieldVMState,
	fieldAction, fieldNonce, fieldFingerprint, fieldTyped, fieldRetryOf, fieldFrom, fieldDomain, fieldRouter, fieldEnvHosts,
	fieldDNS, fieldVMID, fieldFamily, fieldImageKind, fieldImage,
}

func operationOf(kind pods.Kind) operation {
	i := slices.IndexFunc(operations, func(o operation) bool { return o.Kind == kind })
	return operations[i]
}

func cloneForm(f url.Values) url.Values {
	out := url.Values{}
	for k, v := range f {
		out[k] = slices.Clone(v)
	}
	return out
}

// sameConfirm reports whether a confirm of b is the confirm a: the inputs
// formInputs reads, the nonce and fingerprint, and a teardown's typed range.
func sameConfirm(kind pods.Kind, a, b url.Values) bool {
	in := formInputs(kind, a)
	return reflect.DeepEqual(in, formInputs(kind, b)) && a.Get(fieldNonce) == b.Get(fieldNonce) &&
		a.Get(fieldFingerprint) == b.Get(fieldFingerprint) &&
		(!kind.TypedConfirm() || strings.TrimSpace(b.Get(fieldTyped)) == in.Teams)
}

// A confirm edited in any one field, or sent to another operation, stores a
// job only if it still says exactly what was previewed; either way the
// untouched confirm then makes exactly one job.
func TestPropConfirmMatchesItsPreview(t *testing.T) {
	h := newHarness(t)
	h.poll()
	addMasters(h)
	addImages(h)
	h.api.add(proxmox.VM{VMID: 7000, Name: "debian-13-build", Node: "n1", Status: "stopped"},
		map[string]string{"agent": "1", "net0": "virtio=BC:24:11:00:70:00,bridge=vmbr0"})
	h.api.add(proxmox.VM{VMID: 2100, Name: "gw.lab.env", Node: "n1", Pool: "dev-01", Status: "running", Tags: "dev;lab.env;router"},
		map[string]string{"net0": "virtio=BC:24:11:00:21:00,bridge=devext01", "net1": "virtio=BC:24:11:00:21:01,bridge=devint01",
			"description": "battleship: base=gw-v1 dns=router"})
	h.api.add(proxmox.VM{VMID: 2101, Name: "teak.lab.env", Node: "n1", Pool: "dev-01", Status: "running", Tags: "dev;lab.env"},
		map[string]string{"net0": "virtio=BC:24:11:00:21:02,bridge=devint01",
			"description": "battleship: ip=192.168.100.10 os=linux base=debian-12-v1 dns=router"})
	lead := h.login(asLead)
	kinds := slices.Sorted(func(yield func(pods.Kind) bool) {
		for k := range confirmPreviews {
			if !yield(k) {
				return
			}
		}
	})
	ctx := context.Background()
	// newJobs lists the jobs stored after job id.
	newJobs := func(id int64) []store.Job {
		js, err := h.st.Jobs(ctx, 10)
		if err != nil {
			h.t.Fatal(err)
		}
		return slices.DeleteFunc(js, func(j store.Job) bool { return j.ID <= id })
	}
	var last int64
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.SampledFrom(kinds).Draw(t, "kind")
		op := operationOf(kind)
		action, form, _ := h.preview(&lead, op.Path, confirmPreviews[kind])
		if kind.TypedConfirm() {
			form.Set(fieldTyped, formInputs(kind, form).Teams)
		}
		mutated, target := cloneForm(form), action
		value := rapid.OneOf(
			rapid.Just(""), rapid.Just(" "), rapid.Just("yes"), rapid.Just("1-3"), rapid.Just("dc"), rapid.Just("*"),
			rapid.Just(" "+form.Get(fieldTeams)+" "), rapid.Just("lab.env"), rapid.Just("debian-12"), rapid.Just("start"),
			rapid.Custom(func(t *rapid.T) string {
				// Another field's value, padded or not.
				v := form.Get(rapid.SampledFrom(confirmFields).Draw(t, "from field"))
				return rapid.SampledFrom([]string{v, " " + v, v + "\t", v + "\n"}).Draw(t, "padded")
			}),
			rapid.String(),
		)
		switch how := rapid.SampledFrom([]string{"change", "delete", "duplicate", "add", "path"}).Draw(t, "how"); how {
		case "path":
			other := rapid.SampledFrom(slices.DeleteFunc(slices.Clone(kinds), func(k pods.Kind) bool { return k == kind })).Draw(t, "other")
			target = operationOf(other).Path + "/confirm"
		default:
			field := rapid.SampledFrom(confirmFields).Draw(t, "field")
			switch how {
			case "change":
				mutated.Set(field, value.Draw(t, "value"))
			case "delete":
				mutated.Del(field)
			case "duplicate":
				mutated.Add(field, mutated.Get(field))
			case "add":
				mutated.Add(field, value.Draw(t, "value"))
			}
		}
		same := target == action && sameConfirm(kind, form, mutated)

		rec := h.post(&lead, target, mutated)
		if rec.Code >= 500 {
			t.Fatalf("edited confirm = %d\n%s", rec.Code, rec.Body)
		}
		made := len(newJobs(last))
		switch {
		case same && (made != 1 || rec.Code != http.StatusSeeOther):
			t.Fatalf("an unchanged confirm (%v) = %d and made %d jobs\n%s", mutated, rec.Code, made, rec.Body)
		case !same && made != 0:
			t.Fatalf("an edited confirm made %d jobs: %v to %s, previewed %v", made, mutated, target, form)
		}
		rec = h.post(&lead, action, form)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("the untouched confirm = %d\n%s", rec.Code, rec.Body)
		}
		js := newJobs(last)
		if len(js) != 1 {
			t.Fatalf("%d jobs from one preview", len(js))
		}
		last = js[0].ID
		// Cancel the job, so its VMs aren't busy for the next preview.
		if err := h.st.RequestCancel(ctx, last, "test"); err != nil {
			t.Fatal(err)
		}
	})
}
