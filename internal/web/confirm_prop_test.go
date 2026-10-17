package web

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
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
	pods.KindEnvTeardown:    {"domain": {"lab.env"}},
	pods.KindImageRegister:  {"vmid": {"7000"}, "family": {"debian-13"}, "image_kind": {"linux"}},
	pods.KindImageEdit:      {"family": {"debian-12"}},
	pods.KindImageDeprecate: {"image": {"debian-12-v1"}},
	pods.KindImageDelete:    {"image": {"debian-12-v1"}},
}

// confirmFields are every field a confirm form may carry.
var confirmFields = []string{
	fieldTeams, fieldHosts, fieldVMs, fieldPattern, fieldRebuild, fieldBaseline, fieldSnapshot, fieldDescription, fieldVMState,
	fieldAction, fieldNonce, fieldFingerprint, fieldTyped, fieldRetryOf, fieldFrom, fieldDomain, fieldRouter, fieldEnvHosts,
	fieldDNS, fieldVMID, fieldFamily, fieldImageKind, fieldImage, fieldNotes,
}

// operationOf is kind's operation, opened for lab.env if it acts on an
// environment.
func operationOf(kind pods.Kind) operation {
	i := slices.IndexFunc(operations, func(o operation) bool { return o.Kind == kind })
	return operations[i].at("lab.env")
}

func cloneForm(f url.Values) url.Values {
	out := url.Values{}
	for k, v := range f {
		out[k] = slices.Clone(v)
	}
	return out
}

// sameConfirm reports whether a confirm of b is the confirm a: the inputs
// formInputs reads (an environment's domain is its path's), the nonce and
// fingerprint, and a teardown's typed range or domain.
func sameConfirm(kind pods.Kind, a, b url.Values) bool {
	op := operationOf(kind)
	in := op.scope(formInputs(kind, a))
	return reflect.DeepEqual(in, op.scope(formInputs(kind, b))) && a.Get(fieldNonce) == b.Get(fieldNonce) &&
		a.Get(fieldFingerprint) == b.Get(fieldFingerprint) &&
		(!kind.TypedConfirm() || strings.TrimSpace(b.Get(fieldTyped)) == in.Typed())
}

// confirmFieldsOf are the fields a confirm of kind reads: formInputs's, the
// nonce and fingerprint, and a teardown's typed range or domain.
func confirmFieldsOf(kind pods.Kind) []string {
	fs := []string{fieldNonce, fieldFingerprint}
	if kind.TypedConfirm() {
		fs = append(fs, fieldTyped)
	}
	if kind.Teamless() {
		return append(fs, fieldDomain, fieldRouter, fieldEnvHosts, fieldDNS, fieldVMID, fieldFamily, fieldImageKind, fieldImage, fieldNotes)
	}
	fs = append(fs, fieldTeams, fieldHosts, fieldVMs)
	switch kind {
	case pods.KindDeploy:
		fs = append(fs, fieldPattern, fieldRebuild, fieldBaseline)
	case pods.KindReset:
		fs = append(fs, fieldSnapshot)
	case pods.KindSnapshot:
		fs = append(fs, fieldSnapshot, fieldDescription, fieldVMState)
	case pods.KindPower:
		fs = append(fs, fieldAction)
	}
	return fs
}

// confirmTest is a harness with a cluster every confirmPreviews form plans
// against, logged in as a lead.
type confirmTest struct {
	h     *harness
	lead  authtest.Session
	kinds []pods.Kind
	last  int64 // the newest job settled
}

func newConfirmTest(t *testing.T) *confirmTest {
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
	return &confirmTest{h: h, lead: h.login(asLead), kinds: slices.Sorted(maps.Keys(confirmPreviews))}
}

// preview previews kind's form, returning its confirm's action and fields,
// with a teardown's team range or domain typed.
func (c *confirmTest) preview(kind pods.Kind) (string, url.Values) {
	op := operationOf(kind)
	action, form, _ := c.h.preview(&c.lead, op.Path, confirmPreviews[kind])
	if kind.TypedConfirm() {
		form.Set(fieldTyped, op.scope(formInputs(kind, form)).Typed())
	}
	return action, form
}

// settle counts the jobs stored since the last settle, cancelling them so
// their VMs aren't busy for the next preview.
func (c *confirmTest) settle() int {
	ctx := context.Background()
	js, err := c.h.st.Jobs(ctx, 10)
	if err != nil {
		c.h.t.Fatal(err)
	}
	n := 0
	for _, j := range js {
		if j.ID > c.last {
			if err := c.h.st.RequestCancel(ctx, j.ID, "test"); err != nil {
				c.h.t.Fatal(err)
			}
			n++
		}
	}
	if len(js) > 0 {
		c.last = max(c.last, js[0].ID)
	}
	return n
}

// confirm posts form to target as a confirm of the preview (action,
// previewed) of kind, and checks it made a job exactly when it says what was
// previewed. It reports whether it made one.
func (c *confirmTest) confirm(t interface{ Fatalf(string, ...any) }, kind pods.Kind, action string, previewed url.Values, target string, form url.Values) bool {
	same := target == action && sameConfirm(kind, previewed, form)
	rec := c.h.post(&c.lead, target, form)
	if rec.Code >= 500 {
		t.Fatalf("edited confirm = %d\n%s", rec.Code, rec.Body)
	}
	made := c.settle()
	switch {
	case same && (made != 1 || rec.Code != http.StatusSeeOther):
		t.Fatalf("an unchanged confirm (%v) = %d and made %d jobs\n%s", form, rec.Code, made, rec.Body)
	case !same && made != 0:
		t.Fatalf("an edited confirm made %d jobs: %v to %s, previewed %v", made, form, target, previewed)
	}
	return made == 1
}

// Every field a confirm reads, changed, deleted, blanked or repeated, stores
// a job only if the confirm still says exactly what was previewed.
func TestConfirmFieldEdits(t *testing.T) {
	c := newConfirmTest(t)
	for _, kind := range c.kinds {
		action, form := c.preview(kind)
		for _, field := range confirmFieldsOf(kind) {
			// yes flips a checkbox and differs from every text value.
			changed := "yes"
			if form.Get(field) == "yes" {
				changed = "no"
			}
			for _, edit := range []func(url.Values){
				func(f url.Values) { f.Set(field, changed) },
				func(f url.Values) { f.Del(field) },
				func(f url.Values) { f.Set(field, " \t") },
				func(f url.Values) { f.Add(field, f.Get(field)) },
			} {
				mutated := cloneForm(form)
				edit(mutated)
				if c.confirm(t, kind, action, form, action, mutated) {
					action, form = c.preview(kind)
				}
			}
		}
		if !c.confirm(t, kind, action, form, action, form) {
			t.Fatalf("the untouched %s confirm made no job", kind)
		}
	}
}

// A confirm edited in any one field, or sent to another operation, stores a
// job only if it still says exactly what was previewed; either way the
// untouched confirm then makes exactly one job.
func TestPropConfirmMatchesItsPreview(t *testing.T) {
	c := newConfirmTest(t)
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.SampledFrom(c.kinds).Draw(t, "kind")
		action, form := c.preview(kind)
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
		// Mostly a field this kind reads; sometimes any.
		own := rapid.SampledFrom(confirmFieldsOf(kind))
		fields := rapid.OneOf(own, own, own, rapid.SampledFrom(confirmFields))
		switch how := rapid.SampledFrom([]string{"change", "delete", "duplicate", "add", "path"}).Draw(t, "how"); how {
		case "path":
			other := rapid.SampledFrom(slices.DeleteFunc(slices.Clone(c.kinds), func(k pods.Kind) bool { return k == kind })).Draw(t, "other")
			target = operationOf(other).Path + "/confirm"
		default:
			field := fields.Draw(t, "field")
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
		made := c.confirm(t, kind, action, form, target, mutated)
		if rec := c.h.post(&c.lead, action, form); rec.Code != http.StatusSeeOther {
			t.Fatalf("the untouched confirm = %d\n%s", rec.Code, rec.Body)
		}
		if n := c.settle(); made && n != 0 || !made && n != 1 {
			t.Fatalf("the untouched confirm made %d jobs, after the edited one made %v", n, made)
		}
	})
}
