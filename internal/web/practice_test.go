package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// updatePracticeExamples rewrites testdata/practice: one request and its
// answer per call of the session interface, for the practice app's client to
// be tested against.
var updatePracticeExamples = flag.Bool("update-practice-examples", false, "rewrite testdata/practice from the current answers")

const (
	practiceTestKey   = "practice-test-key-practice-test-key-0123"
	practicePrincipal = "practice@pve"
)

// practiceHarness is the harness with the session interface on for pods
// 51-59, a principal holding every privilege, and "approved" a registered
// tag.
type practiceHarness struct {
	*harness
	practice http.Handler // PracticeHandler
}

func newPracticeHarness(t *testing.T) *practiceHarness {
	t.Helper()
	h := newHarness(t, func(c *config.Config) {
		c.Practice = config.Practice{Pods: "51-59", Principal: practicePrincipal, Listen: "127.0.0.1:8081", Key: practiceTestKey}
	})
	if err := h.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h.pve.AddUser(practicePrincipal, nil)
	h.pve.Grant(practicePrincipal, "/", podstest.AllPrivileges...)
	h.pve.SetTagAccess(proxmox.TagAccess{Registered: []string{"approved"}, CaseSensitive: true})
	p := &practiceHarness{harness: h, practice: h.srv.PracticeHandler()}
	if p.practice == nil {
		t.Fatal("PracticeHandler is nil with [practice] set")
	}
	return p
}

// addSet adds a set's masters, dc and web, each with the given tags, at
// firstVMID and the next. With built, each has its template (addTemplate):
// dc's of one unit and web's of two, at the VMIDs a deploy would give them.
func (p *practiceHarness) addSet(set string, firstVMID int, dcTags, webTags string, built bool) {
	for i, m := range []struct{ host, tags string }{{"dc", dcTags}, {"web", webTags}} {
		name := m.host + "." + set
		p.api.add(proxmox.VM{VMID: firstVMID + i, Name: name, Node: "n1", Status: "stopped", Tags: m.tags},
			map[string]string{"net0": "virtio=BC:24:11:00:00:0" + itoa(int64(i)) + ",bridge=vmbr0"})
		if built {
			p.addTemplate(name+".tpl", 9000+(firstVMID+i)%100, i+1, "5f1c2a9e-0000-4000-8000-00000000000"+itoa(int64(i)))
		}
	}
}

// addTemplate adds a built template of units x (2 GiB, 1 processor, a 16 GiB
// boot disk). uuid stands for what Proxmox gives every clone anew.
func (p *practiceHarness) addTemplate(name string, vmid, units int, uuid string) {
	size := int64(units) * 2 * gib
	p.api.add(proxmox.VM{VMID: vmid, Name: name, Node: "n1", Template: true, MaxMem: size, MaxCPU: units, MaxDisk: 8 * size},
		map[string]string{
			"net0":    "virtio=BC:24:11:00:00:00,bridge=vmbr0",
			"smbios1": "uuid=" + uuid,
			"scsi0":   fmt.Sprintf("competitions:base-%d-disk-0,size=%dG", vmid, 16*units),
		})
}

// sets reads GET /v1/sets.
func (p *practiceHarness) sets() []practiceSet {
	p.t.Helper()
	rec := p.call(http.MethodGet, "/v1/sets", "")
	if rec.Code != http.StatusOK {
		p.t.Fatalf("GET /v1/sets = %d %s", rec.Code, rec.Body)
	}
	var got struct{ Sets []practiceSet }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		p.t.Fatal(err)
	}
	return got.Sets
}

// deployBody is a deploy of set to podSpec that sends back the digests GET
// /v1/sets lists for the set's templates.
func (p *practiceHarness) deployBody(podSpec, set string) string {
	p.t.Helper()
	digests := map[string]string{}
	for _, s := range p.sets() {
		if s.Name == set {
			for _, t := range s.Templates {
				digests[t.Name] = t.Digest
			}
		}
	}
	b, err := json.Marshal(digests)
	if err != nil {
		p.t.Fatal(err)
	}
	return fmt.Sprintf(`{"kind": "deploy", "pods": %q, "set": %q, "digests": %s}`, podSpec, set, b)
}

// addPod gives a session pod running VMs.
func (p *practiceHarness) addPod(team string, firstVMID int, hosts ...string) {
	for i, host := range hosts {
		p.api.add(teamVM(team, host, firstVMID+i), cleanConfig(team), "initial")
	}
}

// call makes one call with the key and a fresh ticket of the principal;
// edit changes the request first.
func (p *practiceHarness) call(method, target, body string, edit ...func(*http.Request)) *httptest.ResponseRecorder {
	p.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+practiceTestKey)
	req.Header.Set(practiceTicketHeader, p.pve.Ticket(practicePrincipal).Ticket)
	for _, e := range edit {
		e(req)
	}
	rec := httptest.NewRecorder()
	p.practice.ServeHTTP(rec, req)
	return rec
}

// problem reads a refusal's code.
func problem(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p practiceProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Error == "" || p.Message == "" {
		t.Fatalf("answer %d %q is not a refusal with a code and a message", rec.Code, rec.Body)
	}
	return p.Error
}

// changes counts the calls made to the fake cluster that change it.
func (p *practiceHarness) changes() int {
	p.api.Mu.Lock()
	defer p.api.Mu.Unlock()
	n := 0
	for _, c := range p.api.Calls {
		for _, prefix := range []string{"clone:", "delete:", "setconfig:", "template:", "cloudinit:", "snapshot:", "rollback:", "power:", "shutdown:", "volume:", "stoptask:"} {
			if strings.HasPrefix(c, prefix) {
				n++
			}
		}
	}
	return n
}

// example checks the exchange against testdata/practice/<name>.json, with
// the answer's times replaced by the fake clock's, or rewrites the file.
func (p *practiceHarness) example(name, method, target, body string, rec *httptest.ResponseRecorder) {
	p.t.Helper()
	type request struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body,omitempty"`
	}
	ex := struct {
		Request request `json:"request"`
		Status  int     `json:"status"`
		Answer  any     `json:"answer,omitempty"`
	}{Request: request{Method: method, Path: target, Headers: map[string]string{
		"Authorization":      "Bearer <practice.key>",
		practiceTicketHeader: "<a Proxmox ticket of practice.principal>",
	}}, Status: rec.Code}
	if body != "" {
		ex.Request.Body = json.RawMessage(body)
	}
	if rec.Body.Len() > 0 {
		var answer map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			p.t.Fatalf("%s: answer %q is not a JSON object: %v", name, rec.Body, err)
		}
		for _, k := range []string{"created_at", "started_at", "finished_at"} {
			if answer[k] != nil {
				answer[k] = p.clock.Now().Format("2006-01-02T15:04:05Z07:00")
			}
		}
		ex.Answer = answer
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ex); err != nil {
		p.t.Fatal(err)
	}
	got := buf.Bytes()
	path := filepath.Join("testdata", "practice", name+".json")
	if *updatePracticeExamples {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			p.t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			p.t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		p.t.Fatalf("%v (run go test ./internal/web -run TestPractice -update-practice-examples)", err)
	}
	if !bytes.Equal(got, want) {
		p.t.Errorf("%s differs from the exchange; it is the practice app's client's fixture, so change both together "+
			"(-update-practice-examples rewrites it)\n got: %s\nwant: %s", path, got, want)
	}
}

func TestPracticeListsApprovedSets(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)
	p.addSet("lima.bravo", 5011, "dev;approved", "dev", true) // a host added after approval
	p.addSet("mike.charlie", 5021, "dev", "dev", true)

	rec := p.call(http.MethodGet, "/v1/sets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/sets = %d %s", rec.Code, rec.Body)
	}
	p.example("sets", http.MethodGet, "/v1/sets", "", rec)
	var got struct{ Sets []practiceSet }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sets) != 1 || got.Sets[0].Name != "kilo.alpha" || !slices.Equal(got.Sets[0].Hosts, []string{"dc", "web"}) {
		t.Errorf("sets = %+v, want kilo.alpha alone with dc and web", got.Sets)
	}
}

// Approval rests on a tag only Sys.Modify may set. Where the tag isn't
// registered, or the principal can't see that it is (Proxmox shows the
// registered tags only to Sys.Audit on /), no set is offered or deployed.
func TestPracticeApprovesNothingWithoutARegisteredTagItCanSee(t *testing.T) {
	const deploy = `{"kind": "deploy", "pods": "51", "set": "kilo.alpha", "digests": {"dc.kilo.alpha.tpl": "0d"}}`
	for name, change := range map[string]func(p *practiceHarness){
		"the tag isn't registered": func(p *practiceHarness) { p.pve.SetTagAccess(proxmox.TagAccess{}) },
		"the principal lacks Sys.Audit on /": func(p *practiceHarness) {
			p.pve.Revoke(practicePrincipal, "/")
			p.pve.Grant(practicePrincipal, "/", slices.DeleteFunc(slices.Clone(podstest.AllPrivileges), func(priv string) bool { return priv == "Sys.Audit" })...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPracticeHarness(t)
			p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)
			change(p)
			for _, c := range []struct{ method, path, body string }{{"GET", "/v1/sets", ""}, {"POST", "/v1/jobs", deploy}} {
				rec := p.call(c.method, c.path, c.body)
				if rec.Code != http.StatusConflict || problem(t, rec) != "not_approved" || !strings.Contains(rec.Body.String(), "Sys.Audit on /") {
					t.Errorf("%s %s = %d %s, want 409 not_approved naming the right to read registered tags", c.method, c.path, rec.Code, rec.Body)
				}
			}
			if n := len(p.jobsInStore()); n != 0 {
				t.Errorf("%d jobs stored", n)
			}
		})
	}
}

// A set's templates are reported with a digest that follows the template's
// config, so the practice app can tell an environment is no longer the one
// staff published.
func TestPracticeSetsReportTemplates(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", false)
	if got := p.sets(); len(got) != 1 || len(got[0].Templates) != 0 {
		t.Fatalf("before any template is built: %+v", got)
	}

	p.addTemplate("dc.kilo.alpha.tpl", 9001, 1, "5f1c2a9e-0000-4000-8000-000000000001")
	first := p.sets()[0].Templates
	if len(first) != 1 || first[0].Host != "dc" || first[0].Name != "dc.kilo.alpha.tpl" || first[0].VMID != 9001 || first[0].Digest == "" {
		t.Fatalf("templates = %+v, want dc's with a digest", first)
	}
	// Tags and notes aren't the template.
	p.api.Mu.Lock()
	p.api.VMs[9001].Config["tags"], p.api.VMs[9001].Config["description"] = "dev;reviewed", "checked by a lead"
	p.api.Mu.Unlock()
	if again := p.sets()[0].Templates; again[0].Digest != first[0].Digest {
		t.Errorf("the digest changed with the template's tags and notes: %q then %q", first[0].Digest, again[0].Digest)
	}

	// Another clone of the master at the same VMID has its own identifiers.
	p.api.remove("dc.kilo.alpha.tpl")
	p.addTemplate("dc.kilo.alpha.tpl", 9001, 1, "5f1c2a9e-0000-4000-8000-000000000002")
	rebuilt := p.sets()[0].Templates
	if len(rebuilt) != 1 || rebuilt[0].VMID != 9001 || rebuilt[0].Digest == first[0].Digest {
		t.Errorf("a template with another uuid reports digest %q, the same as before", rebuilt[0].Digest)
	}

	// What the planner blocks is listed as blocked with its reason, never as a template.
	blockedBy := func(change func()) string {
		t.Helper()
		change()
		set := p.sets()[0]
		if len(set.Templates) != 1 || set.Templates[0].Host != "web" || len(set.Blocked) == 0 {
			t.Fatalf("set = %+v, want web's template alone and dc's blocked", set)
		}
		return set.Blocked[0].Reason
	}
	p.addTemplate("web.kilo.alpha.tpl", 9002, 2, "5f1c2a9e-0000-4000-8000-000000000009")
	if why := blockedBy(func() { p.api.add(proxmox.VM{VMID: 9102, Name: "dc.kilo.alpha.tpl", Node: "n1"}, nil) }); !strings.Contains(why, "several VMs are named") {
		t.Errorf("two VMs of one template name: %q", why)
	}
	if why := blockedBy(func() {
		p.api.remove("dc.kilo.alpha.tpl")
		p.addTemplate("dc.kilo.alpha.tpl", 7042, 1, "5f1c2a9e-0000-4000-8000-000000000003")
	}); !strings.Contains(why, "is outside") {
		t.Errorf("a template outside the VMID block: %q", why)
	}
	if why := blockedBy(func() {
		p.api.remove("dc.kilo.alpha.tpl")
		p.addTemplate("dc.kilo.alpha.tpl", 9001, 1, "5f1c2a9e-0000-4000-8000-000000000004")
		p.api.add(proxmox.VM{VMID: 5003, Name: "dc.kilo.alpha", Node: "n1", Tags: "dev;approved"}, map[string]string{"net0": "virtio=BC:24:11:00:00:03,bridge=vmbr0"})
	}); !strings.Contains(why, "team51-dc") {
		t.Errorf("two masters of one host: %q", why)
	}
}

// A set that can't be planned, as on a node that is down, carries the error;
// every other set is still answered.
func TestPracticeSetsMarkWhatCannotBeRead(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)
	p.addSet("lima.bravo", 5021, "dev;approved", "dev;approved", true)
	p.api.setReadErr(5021, &proxmox.APIError{Status: 595, Message: "no route to host"})

	sets := p.sets()
	if len(sets) != 2 {
		t.Fatalf("sets = %+v, want both", sets)
	}
	kilo, lima := sets[0], sets[1]
	if lima.Error == "" || len(lima.Templates) != 0 || lima.Pod != (practiceSize{}) {
		t.Errorf("the set that can't be planned = %+v, want an error and nothing else", lima)
	}
	if want := (practiceSize{MemoryBytes: 6 * gib, CPUs: 3, BootDiskBytes: 48 * gib}); kilo.Pod != want || kilo.Error != "" || len(kilo.Templates) != 2 {
		t.Errorf("kilo.alpha = %+v, want both templates and pod %+v", kilo, want)
	}
}

// A template the principal may not clone is listed as blocked with the
// privilege it lacks and is not counted, whether or not the first session pod
// already holds the set.
func TestPracticeSetsBlockWhatThePrincipalCannotClone(t *testing.T) {
	for name, held := range map[string]bool{"an empty pod": false, "a pod that holds the set": true} {
		t.Run(name, func(t *testing.T) {
			p := newPracticeHarness(t)
			p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)
			if held {
				p.addPod("51", 15101, "dc", "web")
			}
			p.pve.Revoke(practicePrincipal, "/")
			p.pve.Grant(practicePrincipal, "/", slices.DeleteFunc(slices.Clone(podstest.AllPrivileges), func(priv string) bool { return priv == "VM.Clone" })...)
			p.pve.Grant(practicePrincipal, "/vms/9002", podstest.AllPrivileges...) // a path's grant replaces what / gives

			set := p.sets()[0]
			if len(set.Templates) != 1 || set.Templates[0].VMID != 9002 {
				t.Errorf("templates = %+v, want web's alone", set.Templates)
			}
			if len(set.Blocked) != 1 || set.Blocked[0].Name != "dc.kilo.alpha.tpl" || !strings.Contains(set.Blocked[0].Reason, "VM.Clone on /vms/9001") {
				t.Errorf("blocked = %+v, want dc's template for want of VM.Clone on /vms/9001", set.Blocked)
			}
			if want := (practiceSize{MemoryBytes: 4 * gib, CPUs: 2, BootDiskBytes: 32 * gib}); set.Pod != want {
				t.Errorf("pod = %+v, want web's alone %+v", set.Pod, want)
			}
		})
	}
}

// A template whose deploy lacks another of the principal's rights, here
// VM.Allocate for the clone in the pod's pool, is listed as blocked with it.
func TestPracticeSetsBlockWhatThePrincipalCannotDeploy(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)
	noAllocate := slices.DeleteFunc(slices.Clone(podstest.AllPrivileges), func(priv string) bool { return priv == "VM.Allocate" })
	p.pve.Grant(practicePrincipal, "/vms/15101", noAllocate...)
	p.pve.Grant(practicePrincipal, "/pool/pool-51", noAllocate...)

	set := p.sets()[0]
	if len(set.Blocked) != 1 || set.Blocked[0].Name != "dc.kilo.alpha.tpl" || !strings.Contains(set.Blocked[0].Reason, "VM.Allocate") {
		t.Errorf("blocked = %+v, want dc's template for want of VM.Allocate", set.Blocked)
	}
}

// One pod's size is the sum over the set's built templates, as Proxmox lists
// each one.
func TestPracticeSetsReportPodSize(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", false)
	if got := p.sets()[0].Pod; got != (practiceSize{}) {
		t.Errorf("pod with no template built = %+v, want nothing", got)
	}
	p.addTemplate("dc.kilo.alpha.tpl", 9001, 1, "5f1c2a9e-0000-4000-8000-000000000001")
	if got, want := p.sets()[0].Pod, (practiceSize{MemoryBytes: 2 * gib, CPUs: 1, BootDiskBytes: 16 * gib}); got != want {
		t.Errorf("pod with dc's template built = %+v, want %+v", got, want)
	}
	p.addTemplate("web.kilo.alpha.tpl", 9002, 2, "5f1c2a9e-0000-4000-8000-000000000002")
	if got, want := p.sets()[0].Pod, (practiceSize{MemoryBytes: 6 * gib, CPUs: 3, BootDiskBytes: 48 * gib}); got != want {
		t.Errorf("pod = %+v, want %+v", got, want)
	}
}

func TestPracticeListsAPodsVMs(t *testing.T) {
	p := newPracticeHarness(t)
	p.addPod("51", 15101, "dc", "web")
	p.addPod("52", 15201, "dc")

	rec := p.call(http.MethodGet, "/v1/pods/51/vms", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/pods/51/vms = %d %s", rec.Code, rec.Body)
	}
	p.example("pod-vms", http.MethodGet, "/v1/pods/51/vms", "", rec)
	var got struct {
		Pod string
		VMs []practiceVM
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []practiceVM{
		{Name: "team51-dc", Host: "dc", VMID: 15101, Node: "n1", Status: "running"},
		{Name: "team51-web", Host: "web", VMID: 15102, Node: "n1", Status: "running"},
	}
	if got.Pod != "51" || !slices.Equal(got.VMs, want) {
		t.Errorf("pod %q VMs = %+v, want pod 51's two", got.Pod, got.VMs)
	}
	// An empty pod is an empty list, which a teardown check reads.
	rec = p.call(http.MethodGet, "/v1/pods/59/vms", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"vms":[]`) {
		t.Errorf("empty pod = %d %s", rec.Code, rec.Body)
	}
}

// submitted is a 201 answer's job, read from the store.
func (p *practiceHarness) submitted(rec *httptest.ResponseRecorder) (store.Job, jobs.Inputs) {
	p.t.Helper()
	if rec.Code != http.StatusCreated {
		p.t.Fatalf("submit = %d %s", rec.Code, rec.Body)
	}
	var got struct{ ID int64 }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID == 0 {
		p.t.Fatalf("submit answer %q names no job", rec.Body)
	}
	j, err := p.st.Job(context.Background(), got.ID)
	if err != nil {
		p.t.Fatal(err)
	}
	return j, readInputs(j)
}

func TestPracticeSubmitsADeploy(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)

	body := p.deployBody("51-52", "kilo.alpha")
	rec := p.call(http.MethodPost, "/v1/jobs", body)
	j, in := p.submitted(rec)
	p.example("deploy", http.MethodPost, "/v1/jobs", body, rec)

	if j.Kind != "deploy" || j.Origin != store.OriginSession || j.CreatedBy != practicePrincipal || j.CreatedAs != practicePrincipal || j.Status != store.StatusPending {
		t.Errorf("job = kind %q from %q by %q as %q, %s", j.Kind, j.Origin, j.CreatedBy, j.CreatedAs, j.Status)
	}
	want := jobs.Inputs{Kind: pods.KindDeploy, Teams: "51-52", Pattern: "*.kilo.alpha", ApprovedOnly: true}
	if in.Kind != want.Kind || in.Teams != want.Teams || in.Pattern != want.Pattern || !in.ApprovedOnly || in.Rebuild || in.NoSnapshot || len(in.Hosts) > 0 {
		t.Errorf("inputs = %+v, want %+v", in, want)
	}
	if len(in.Digests) != 2 || in.Digests["dc.kilo.alpha.tpl"] == "" || in.Digests["web.kilo.alpha.tpl"] == "" {
		t.Errorf("the job keeps digests %v, want both templates'", in.Digests)
	}
	items, err := p.st.Items(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Errorf("the deploy has %d VMs, want two pods' 4", len(items))
	}
	// The job runs with a ticket of the principal's that Proxmox takes.
	jc, err := p.st.JobCredential(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := p.creds.Open(jc)
	if err != nil {
		t.Fatal(err)
	}
	if cred.User != practicePrincipal || !p.pve.Accepts(cred) {
		t.Errorf("the job's credential is %v, accepted = %t", cred, p.pve.Accepts(cred))
	}
	if n := p.changes(); n != 0 {
		t.Errorf("submitting made %d changes to the cluster; the worker makes them", n)
	}
}

// A deploy takes the named set's masters and no other's, though one set's
// name ends another's and that other is unapproved.
func TestPracticeDeploysExactlyTheNamedSet(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("alpha", 5031, "dev;approved", "dev;approved", true)
	p.addSet("kilo.alpha", 5001, "dev", "dev", true)

	j, _ := p.submitted(p.call(http.MethodPost, "/v1/jobs", p.deployBody("51", "alpha")))
	items, err := p.st.Items(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	var vmids []int
	for _, it := range items {
		vmids = append(vmids, it.VMID)
	}
	if want := []int{15131, 15132}; !slices.Equal(vmids, want) {
		t.Errorf("the deploy's VMs are %v, want alpha's two %v", vmids, want)
	}
}

func TestPracticeSubmitsATeardown(t *testing.T) {
	p := newPracticeHarness(t)
	p.addPod("51", 15101, "dc", "web")
	p.addPod("52", 15201, "dc")

	const body = `{"kind": "teardown", "pods": "51-52"}`
	rec := p.call(http.MethodPost, "/v1/jobs", body)
	j, in := p.submitted(rec)
	p.example("teardown", http.MethodPost, "/v1/jobs", body, rec)
	if j.Kind != "teardown" || j.Origin != store.OriginSession || j.CreatedAs != practicePrincipal || in.Teams != "51-52" || in.Set != "" {
		t.Errorf("job = kind %q from %q as %q, inputs %+v", j.Kind, j.Origin, j.CreatedAs, in)
	}
	items, err := p.st.Items(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Errorf("the teardown has %d VMs, want the pods' 3", len(items))
	}
}

func TestPracticeReadsAndCancelsItsJob(t *testing.T) {
	p := newPracticeHarness(t)
	p.addPod("51", 15101, "dc", "web")
	j, _ := p.submitted(p.call(http.MethodPost, "/v1/jobs", `{"kind": "teardown", "pods": "51"}`))
	path := "/v1/jobs/" + itoa(j.ID)

	rec := p.call(http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
	}
	p.example("job", http.MethodGet, path, "", rec)
	var got practiceJob
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	wantItems := []practiceItem{
		{Name: "team51-dc", Pod: "51", VMID: 15101, Status: store.ItemPending},
		{Name: "team51-web", Pod: "51", VMID: 15102, Status: store.ItemPending},
	}
	if got.ID != j.ID || got.Kind != "teardown" || got.Pods != "51" || got.Status != store.StatusPending || !slices.Equal(got.Items, wantItems) {
		t.Errorf("job = %+v", got)
	}

	rec = p.call(http.MethodPost, path+"/cancel", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	p.example("cancel", http.MethodPost, path+"/cancel", "", rec)
	if after, err := p.st.Job(context.Background(), j.ID); err != nil || after.Status != store.StatusCancelled || after.CancelledBy != practicePrincipal {
		t.Errorf("after the cancel the job is %s, cancelled by %q (%v)", after.Status, after.CancelledBy, err)
	}
	rec = p.call(http.MethodPost, path+"/cancel", "")
	if rec.Code != http.StatusConflict || problem(t, rec) != "not_active" {
		t.Errorf("a second cancel = %d %s", rec.Code, rec.Body)
	}
	p.example("refused", http.MethodPost, path+"/cancel", "", rec)
}

// A session job's starter is the interface, not whoever the volunteer pages
// know by the same name: only Proxmox privileges let a volunteer cancel it.
func TestVolunteerNamedAsThePrincipalCannotCancelASessionJob(t *testing.T) {
	p := newPracticeHarness(t)
	p.addPod("51", 15101, "dc")
	j, _ := p.submitted(p.call(http.MethodPost, "/v1/jobs", `{"kind": "teardown", "pods": "51"}`))

	cred := p.ticketWith("mallory", []string{"VM.Audit", "Pool.Audit"})
	mallory := authtest.Login(t, p.st, p.cfg, authtest.User{Subject: "mallory", Email: practicePrincipal, At: p.clock.Now(), Proxmox: cred})
	p.openView(cred)
	if rec := p.post(&mallory, "/logs/"+itoa(j.ID)+"/cancel", url.Values{}); rec.Code != http.StatusForbidden {
		t.Errorf("cancel by a volunteer whose email is the principal's name = %d, want 403", rec.Code)
	}
	if after, err := p.st.Job(context.Background(), j.ID); err != nil || after.CancelRequested || after.Status != store.StatusPending {
		t.Errorf("the session job is %s, cancel requested %t (%v)", after.Status, after.CancelRequested, err)
	}
}

func TestPracticeOffUnlessSet(t *testing.T) {
	h := newHarness(t)
	if h.srv.PracticeHandler() != nil {
		t.Error("PracticeHandler serves without [practice]")
	}
	// Set, it is still no part of the volunteer pages' handler.
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)
	for _, target := range []string{"/v1/sets", "/v1/pods/51/vms", "/v1/jobs/1"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer "+practiceTestKey)
		req.Header.Set(practiceTicketHeader, p.pve.Ticket(practicePrincipal).Ticket)
		rec := httptest.NewRecorder()
		p.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusSeeOther && rec.Code != http.StatusFound {
			t.Errorf("the volunteer pages answer GET %s with %d %s", target, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Header().Get("Content-Type"), "json") {
			t.Errorf("the volunteer pages answer GET %s with JSON", target)
		}
	}
}

// No refusal stores a job or changes the cluster.
func TestPracticeRefusals(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)
	p.addSet("lima.bravo", 5011, "dev;approved", "dev", true)
	p.addSet("mike.charlie", 5021, "dev;approved", "dev;approved", false) // approved, never built
	p.addPod("51", 15101, "dc")
	ctx := context.Background()

	// A volunteer's job, which the interface must neither show nor cancel.
	lead := p.ticket("test-lead")
	in := jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "stop"}
	plan, err := jobs.BuildPlan(ctx, pods.NewPlanner(p.as(lead), p.cfg), in)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := jobs.Submit(ctx, p.st, in, plan, jobs.Submitter{User: "lead@example.org", Credential: lead, Seal: p.creds})
	if err != nil {
		t.Fatal(err)
	}
	theirPath := "/v1/jobs/" + itoa(theirs)

	const deploy = `{"kind": "deploy", "pods": "52", "set": "kilo.alpha", "digests": {"dc.kilo.alpha.tpl": "0d"}}`
	header := func(name, value string) func(*http.Request) {
		return func(r *http.Request) {
			if value == "" {
				r.Header.Del(name)
				return
			}
			r.Header.Set(name, value)
		}
	}
	cases := []struct {
		name         string
		method, path string
		body         string
		edit         func(*http.Request)
		status       int
		code         string
		// unasked: Proxmox must not even be asked about a ticket.
		unasked bool
	}{
		{name: "no key", method: "POST", path: "/v1/jobs", body: deploy, edit: header("Authorization", ""), status: 401, code: "bad_key", unasked: true},
		{name: "wrong key", method: "POST", path: "/v1/jobs", body: deploy, edit: header("Authorization", "Bearer "+strings.Repeat("x", 40)), status: 401, code: "bad_key", unasked: true},
		{name: "key without Bearer", method: "GET", path: "/v1/sets", edit: header("Authorization", practiceTestKey+"x"), status: 401, code: "bad_key", unasked: true},
		{name: "no ticket", method: "POST", path: "/v1/jobs", body: deploy, edit: header(practiceTicketHeader, ""), status: 401, code: "no_ticket", unasked: true},
		{name: "another user's ticket", method: "POST", path: "/v1/jobs", body: deploy, edit: header(practiceTicketHeader, lead.Ticket), status: 403, code: "ticket_refused"},
		{name: "another user's ticket reads nothing", method: "GET", path: "/v1/sets", edit: header(practiceTicketHeader, lead.Ticket), status: 403, code: "ticket_refused"},
		{name: "a made-up ticket", method: "GET", path: "/v1/sets", edit: header(practiceTicketHeader, "PVE:"+practicePrincipal+":00000000::forged"), status: 403, code: "ticket_refused"},
		{name: "deploy to a pod outside the range", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "1", "set": "kilo.alpha"}`, status: 403, code: "pod_out_of_range"},
		{name: "deploy to a range reaching outside", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "50-52", "set": "kilo.alpha"}`, status: 403, code: "pod_out_of_range"},
		{name: "teardown of a pod outside the range", method: "POST", path: "/v1/jobs", body: `{"kind": "teardown", "pods": "1-3"}`, status: 403, code: "pod_out_of_range"},
		{name: "VMs of a pod outside the range", method: "GET", path: "/v1/pods/01/vms", status: 403, code: "pod_out_of_range"},
		{name: "deploy to a pod past the range", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "60", "set": "kilo.alpha"}`, status: 403, code: "pod_out_of_range"},
		{name: "teardown reaching past the range", method: "POST", path: "/v1/jobs", body: `{"kind": "teardown", "pods": "51,61"}`, status: 403, code: "pod_out_of_range"},
		{name: "VMs of a pod past the range", method: "GET", path: "/v1/pods/99/vms", status: 403, code: "pod_out_of_range"},
		{name: "VMs of several pods", method: "GET", path: "/v1/pods/51-52/vms", status: 422, code: "bad_pods"},
		{name: "VMs of no pod number", method: "GET", path: "/v1/pods/first/vms", status: 422, code: "bad_pods"},
		{name: "deploy to no pod number", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "all", "set": "kilo.alpha"}`, status: 422, code: "bad_pods"},
		{name: "another kind", method: "POST", path: "/v1/jobs", body: `{"kind": "reset", "pods": "51"}`, status: 422, code: "bad_kind"},
		{name: "an environment kind", method: "POST", path: "/v1/jobs", body: `{"kind": "env-teardown", "pods": "51"}`, status: 422, code: "bad_kind"},
		{name: "no kind", method: "POST", path: "/v1/jobs", body: `{"pods": "51"}`, status: 422, code: "bad_kind"},
		{name: "a field the interface doesn't take", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "52", "set": "kilo.alpha", "digests": {"dc.kilo.alpha.tpl": "0d"}, "rebuild": true}`, status: 400, code: "bad_json"},
		{name: "two requests in one", method: "POST", path: "/v1/jobs", body: deploy + deploy, status: 400, code: "bad_json"},
		{name: "a body over the cap", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "52", "set": "` + strings.Repeat("a", practiceMaxBody) + `"}`, status: 400, code: "bad_json"},
		{name: "deploy of a pattern", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "52", "set": "*"}`, status: 422, code: "bad_set"},
		{name: "deploy of no set", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "52"}`, status: 422, code: "bad_set"},
		{name: "teardown of one set", method: "POST", path: "/v1/jobs", body: `{"kind": "teardown", "pods": "51", "set": "kilo.alpha"}`, status: 422, code: "bad_set"},
		{name: "deploy without digests", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "52", "set": "kilo.alpha"}`, status: 422, code: "bad_digests"},
		{name: "deploy with no digest", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "52", "set": "kilo.alpha", "digests": {}}`, status: 422, code: "bad_digests"},
		{name: "teardown with digests", method: "POST", path: "/v1/jobs", body: `{"kind": "teardown", "pods": "51", "digests": {"dc.kilo.alpha.tpl": "0d"}}`, status: 422, code: "bad_digests"},
		{name: "deploy of an unapproved set", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "52", "set": "lima.bravo", "digests": {"dc.lima.bravo.tpl": "0d"}}`, status: 409, code: "not_approved"},
		{name: "deploy of a set that isn't there", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "52", "set": "no.such", "digests": {"dc.no.such.tpl": "0d"}}`, status: 409, code: "not_approved"},
		{name: "deploy of a set with no template built", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "52", "set": "mike.charlie", "digests": {"dc.mike.charlie.tpl": "0d"}}`, status: 409, code: "blocked"},
		{name: "deploy with a digest that isn't the template's", method: "POST", path: "/v1/jobs", body: deploy, status: 409, code: "blocked"},
		{name: "teardown of an empty pod", method: "POST", path: "/v1/jobs", body: `{"kind": "teardown", "pods": "59"}`, status: 409, code: "nothing_to_run"},
		{name: "read of a volunteer's job", method: "GET", path: theirPath, status: 404, code: "not_found"},
		{name: "cancel of a volunteer's job", method: "POST", path: theirPath + "/cancel", status: 404, code: "not_found"},
		{name: "read of no job", method: "GET", path: "/v1/jobs/999", status: 404, code: "not_found"},
		{name: "a job that isn't a number", method: "GET", path: "/v1/jobs/1e3", status: 404, code: "not_found"},
		{name: "a call that isn't one", method: "GET", path: "/v1/jobs", status: 404, code: "not_found"},
		{name: "a volunteer page", method: "GET", path: "/deploy", status: 404, code: "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobsBefore, changesBefore := len(p.jobsInStore()), p.changes()
			_, renewsBefore, permsBefore := p.pve.Counts()
			var edit []func(*http.Request)
			if tc.edit != nil {
				edit = append(edit, tc.edit)
			}
			rec := p.call(tc.method, tc.path, tc.body, edit...)
			if rec.Code != tc.status || problem(t, rec) != tc.code {
				t.Errorf("%s %s = %d %s, want %d %s", tc.method, tc.path, rec.Code, rec.Body, tc.status, tc.code)
			}
			if n := len(p.jobsInStore()); n != jobsBefore {
				t.Errorf("the refusal stored a job: %d jobs, %d before", n, jobsBefore)
			}
			if n := p.changes(); n != changesBefore {
				t.Errorf("the refusal changed the cluster: %d changes, %d before", n, changesBefore)
			}
			if _, renews, perms := p.pve.Counts(); tc.unasked && (renews != renewsBefore || perms != permsBefore) {
				t.Errorf("Proxmox was asked before the key and ticket were there: %d renewals, %d privilege reads more",
					renews-renewsBefore, perms-permsBefore)
			}
		})
	}
	// The volunteer's job is as it was.
	if j, err := p.st.Job(ctx, theirs); err != nil || j.Status != store.StatusPending || j.CancelRequested {
		t.Errorf("the volunteer's job is %s, cancel requested %t (%v)", j.Status, j.CancelRequested, err)
	}
	// No other user's credential ever reached the cluster through the interface.
	p.credMu.Lock()
	defer p.credMu.Unlock()
	for _, c := range p.used {
		if c.Ticket == lead.Ticket && c.User != lead.User {
			t.Errorf("the lead's ticket was used as %s", c.User)
		}
	}
}

// A disabled principal's ticket stops working at once: Proxmox refuses it.
func TestPracticeRefusesADisabledPrincipal(t *testing.T) {
	p := newPracticeHarness(t)
	ticket := p.pve.Ticket(practicePrincipal).Ticket
	p.pve.Disable(practicePrincipal)
	rec := p.call(http.MethodGet, "/v1/sets", "", func(r *http.Request) { r.Header.Set(practiceTicketHeader, ticket) })
	if rec.Code != http.StatusForbidden || problem(t, rec) != "ticket_refused" {
		t.Errorf("a disabled principal's ticket = %d %s", rec.Code, rec.Body)
	}
}

// A deploy is all or nothing: a VM the principal may not create refuses the
// whole request and names it.
func TestPracticeDeployIsAllOrNothing(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)
	body := p.deployBody("51", "kilo.alpha")
	p.pve.Revoke(practicePrincipal, "/")
	p.pve.Grant(practicePrincipal, "/", "VM.Audit", "Sys.Audit", "Datastore.Audit", "Pool.Audit")

	rec := p.call(http.MethodPost, "/v1/jobs", body)
	if rec.Code != http.StatusConflict || problem(t, rec) != "blocked" {
		t.Fatalf("deploy without the privileges = %d %s", rec.Code, rec.Body)
	}
	var got practiceProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Blocked) == 0 || got.Blocked[0].Name == "" || !strings.Contains(got.Blocked[0].Reason, "you don't have VM.Clone") {
		t.Errorf("the refusal doesn't name what the principal may not do: %+v", got)
	}
	if n := len(p.jobsInStore()); n != 0 {
		t.Errorf("%d jobs stored", n)
	}
}

// A deploy sends back the digests the listing gave: a template rebuilt in
// place since, or one the request has no digest for, refuses the whole
// request and is named.
func TestPracticeDeployRefusesTemplatesChangedSinceListed(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(p *practiceHarness, body string) string // after the listing; returns the body to send
	}{
		"dc's template rebuilt in place": {func(p *practiceHarness, body string) string {
			p.api.Mu.Lock()
			p.api.VMs[9001].Config["smbios1"] = "uuid=5f1c2a9e-0000-4000-8000-0000000000ff"
			p.api.Mu.Unlock()
			return body
		}},
		"no digest for dc's template": {func(p *practiceHarness, body string) string {
			var req practiceJobRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				p.t.Fatal(err)
			}
			delete(req.Digests, "dc.kilo.alpha.tpl")
			b, err := json.Marshal(req)
			if err != nil {
				p.t.Fatal(err)
			}
			return string(b)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPracticeHarness(t)
			p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)
			body := tc.change(p, p.deployBody("51", "kilo.alpha"))

			rec := p.call(http.MethodPost, "/v1/jobs", body)
			if rec.Code != http.StatusConflict || problem(t, rec) != "blocked" {
				t.Fatalf("deploy = %d %s, want 409 blocked", rec.Code, rec.Body)
			}
			var got practiceProblem
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(got.Blocked, func(b practiceBlocked) bool {
				return b.Name == "dc.kilo.alpha.tpl" && strings.Contains(b.Reason, "changed since the set was listed")
			}) || slices.ContainsFunc(got.Blocked, func(b practiceBlocked) bool { return b.Name == "web.kilo.alpha.tpl" }) {
				t.Errorf("blocked = %+v, want dc's template alone, changed since the listing", got.Blocked)
			}
			if n := len(p.jobsInStore()); n != 0 {
				t.Errorf("%d jobs stored", n)
			}
		})
	}
}

// An outage is not a refusal: while Proxmox can't be reached, a submission is
// answered as Proxmox's failure, so the practice app tries again later.
func TestPracticeAnswersAnOutageAsOne(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved", true)
	deploy := p.deployBody("51", "kilo.alpha")
	for name, down := range map[string]error{
		"no connection":  &url.Error{Op: "Get", URL: "https://pve.example:8006/api2/json/cluster/resources", Err: errors.New("connect: connection refused")},
		"a node error":   &proxmox.APIError{Method: "GET", Path: "/cluster/resources", Status: 595, Message: "no route to host"},
		"a dropped body": fmt.Errorf("GET /cluster/resources: %w", io.ErrUnexpectedEOF),
	} {
		p.api.setListErr(down)
		for _, c := range []struct{ method, path, body string }{{"POST", "/v1/jobs", deploy}, {"GET", "/v1/sets", ""}, {"GET", "/v1/pods/51/vms", ""}} {
			rec := p.call(c.method, c.path, c.body)
			if rec.Code != http.StatusBadGateway || problem(t, rec) != "proxmox_failed" {
				t.Errorf("%s: %s %s = %d %s, want 502 proxmox_failed", name, c.method, c.path, rec.Code, rec.Body)
			}
		}
	}
	p.api.setListErr(nil)
	p.submitted(p.call(http.MethodPost, "/v1/jobs", deploy))
}
