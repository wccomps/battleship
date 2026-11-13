package web

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// updatePracticeExamples rewrites testdata/practice: one request and its
// answer per call of the session interface, which the practice app's client
// is tested against.
var updatePracticeExamples = flag.Bool("update-practice-examples", false, "rewrite testdata/practice from the current answers")

const (
	practiceTestKey   = "practice-test-key-practice-test-key-0123"
	practicePrincipal = "practice@pve"
)

// practiceHarness is the harness with the session interface on for pods
// 51-59 and a principal holding every privilege.
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
	p := &practiceHarness{harness: h, practice: h.srv.PracticeHandler()}
	if p.practice == nil {
		t.Fatal("PracticeHandler is nil with [practice] set")
	}
	return p
}

// addSet adds a set's masters, dc (4 GiB, 2 processors, a 32 GiB disk) and
// web (8 GiB, 4, 64 GiB), each with the given tags.
func (p *practiceHarness) addSet(set string, firstVMID int, dcTags, webTags string) {
	for i, m := range []struct{ host, tags string }{{"dc", dcTags}, {"web", webTags}} {
		size := int64(i+1) * 4 * gib
		p.api.add(proxmox.VM{VMID: firstVMID + i, Name: m.host + "." + set, Node: "n1", Status: "stopped", Tags: m.tags,
			MaxMem: size, MaxCPU: (i + 1) * 2, MaxDisk: 8 * size},
			map[string]string{"net0": "virtio=BC:24:11:00:00:0" + itoa(int64(i)) + ",bridge=vmbr0"})
	}
}

// addTemplate adds a built template of half dc's size. uuid stands for what
// Proxmox gives every clone anew, so a rebuilt template has another.
func (p *practiceHarness) addTemplate(name string, vmid int, uuid string) {
	p.api.add(proxmox.VM{VMID: vmid, Name: name, Node: "n2", Template: true, MaxMem: 2 * gib, MaxCPU: 1, MaxDisk: 16 * gib},
		map[string]string{"net0": "virtio=BC:24:11:00:00:00,bridge=vmbr0", "smbios1": "uuid=" + uuid})
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
		p.t.Errorf("%s differs from the exchange; the practice app's client is tested against it, so change both together "+
			"(-update-practice-examples rewrites it)\n got: %s\nwant: %s", path, got, want)
	}
}

func TestPracticeListsApprovedSets(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved")
	p.addTemplate("dc.kilo.alpha.tpl", 9001, "5f1c2a9e-0000-4000-8000-000000000001")
	p.addSet("lima.bravo", 5011, "dev;approved", "dev") // a host added after approval
	p.addSet("mike.charlie", 5021, "dev", "dev")

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

// A set's templates are reported with a digest that a rebuild changes, so the
// practice app can tell an environment is no longer the one staff published.
func TestPracticeSetsReportTemplates(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved")
	if got := p.sets(); len(got) != 1 || len(got[0].Templates) != 0 {
		t.Fatalf("before any template is built: %+v", got)
	}

	p.addTemplate("dc.kilo.alpha.tpl", 9001, "5f1c2a9e-0000-4000-8000-000000000001")
	first := p.sets()[0].Templates
	if len(first) != 1 || first[0].Host != "dc" || first[0].Name != "dc.kilo.alpha.tpl" || first[0].VMID != 9001 || first[0].Digest == "" {
		t.Fatalf("templates = %+v, want dc's with a digest", first)
	}
	if again := p.sets()[0].Templates; !slices.Equal(again, first) {
		t.Errorf("an unchanged template reads differently: %+v then %+v", first, again)
	}

	// Rebuilt: deleted and cloned from its master again, at the same VMID.
	p.api.remove("dc.kilo.alpha.tpl")
	p.addTemplate("dc.kilo.alpha.tpl", 9001, "5f1c2a9e-0000-4000-8000-000000000002")
	rebuilt := p.sets()[0].Templates
	if len(rebuilt) != 1 || rebuilt[0].VMID != 9001 || rebuilt[0].Digest == first[0].Digest {
		t.Errorf("a rebuilt template reports digest %q, the same as before", rebuilt[0].Digest)
	}

	// Two VMs sharing the template's name: neither is reported, as a deploy would block.
	p.addTemplate("dc.kilo.alpha.tpl", 9002, "5f1c2a9e-0000-4000-8000-000000000003")
	if got := p.sets()[0].Templates; len(got) != 0 {
		t.Errorf("with two templates of one name, reported %+v", got)
	}
}

// One pod's size is the sum over the set's hosts, each sized by its template
// where one is built and by its master where none is.
func TestPracticeSetsReportPodSize(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved")
	if got, want := p.sets()[0].Pod, (practiceSize{MemoryBytes: 12 * gib, CPUs: 6, DiskBytes: 96 * gib}); got != want {
		t.Errorf("pod sized by masters = %+v, want %+v", got, want)
	}
	p.addTemplate("dc.kilo.alpha.tpl", 9001, "5f1c2a9e-0000-4000-8000-000000000001")
	if got, want := p.sets()[0].Pod, (practiceSize{MemoryBytes: 10 * gib, CPUs: 5, DiskBytes: 80 * gib}); got != want {
		t.Errorf("pod with dc's template built = %+v, want %+v", got, want)
	}
	// A set whose name ends another's is sized by its own masters alone,
	// though "*.alpha" matches both.
	p.addSet("alpha", 5031, "dev;approved", "dev;approved")
	for _, set := range p.sets() {
		if want := (practiceSize{MemoryBytes: 12 * gib, CPUs: 6, DiskBytes: 96 * gib}); set.Name == "alpha" && set.Pod != want {
			t.Errorf("alpha's pod = %+v, want its own two masters' %+v", set.Pod, want)
		}
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
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved")

	const body = `{"kind": "deploy", "pods": "51-52", "set": "kilo.alpha"}`
	rec := p.call(http.MethodPost, "/v1/jobs", body)
	j, in := p.submitted(rec)
	p.example("deploy", http.MethodPost, "/v1/jobs", body, rec)

	if j.Kind != "deploy" || j.CreatedBy != practiceActor || j.CreatedAs != practicePrincipal || j.Status != store.StatusPending {
		t.Errorf("job = kind %q by %q as %q, %s", j.Kind, j.CreatedBy, j.CreatedAs, j.Status)
	}
	want := jobs.Inputs{Kind: pods.KindDeploy, Teams: "51-52", Pattern: "*.kilo.alpha", ApprovedOnly: true}
	if in.Kind != want.Kind || in.Teams != want.Teams || in.Pattern != want.Pattern || !in.ApprovedOnly || in.Rebuild || in.NoSnapshot || len(in.Hosts) > 0 {
		t.Errorf("inputs = %+v, want %+v", in, want)
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

func TestPracticeSubmitsATeardown(t *testing.T) {
	p := newPracticeHarness(t)
	p.addPod("51", 15101, "dc", "web")
	p.addPod("52", 15201, "dc")

	const body = `{"kind": "teardown", "pods": "51-52"}`
	rec := p.call(http.MethodPost, "/v1/jobs", body)
	j, in := p.submitted(rec)
	p.example("teardown", http.MethodPost, "/v1/jobs", body, rec)
	if j.Kind != "teardown" || j.CreatedAs != practicePrincipal || in.Teams != "51-52" || in.Set != "" {
		t.Errorf("job = kind %q as %q, inputs %+v", j.Kind, j.CreatedAs, in)
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
	if after, err := p.st.Job(context.Background(), j.ID); err != nil || after.Status != store.StatusCancelled || after.CancelledBy != practiceActor {
		t.Errorf("after the cancel the job is %s, cancelled by %q (%v)", after.Status, after.CancelledBy, err)
	}
	rec = p.call(http.MethodPost, path+"/cancel", "")
	if rec.Code != http.StatusConflict || problem(t, rec) != "not_active" {
		t.Errorf("a second cancel = %d %s", rec.Code, rec.Body)
	}
	p.example("refused", http.MethodPost, path+"/cancel", "", rec)
}

func TestPracticeOffUnlessSet(t *testing.T) {
	h := newHarness(t)
	if h.srv.PracticeHandler() != nil {
		t.Error("PracticeHandler serves without [practice]")
	}
	// Set, it is still no part of the volunteer pages' handler.
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved")
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

// TestPracticeRefusals is every way the interface says no. None stores a job
// or changes the cluster.
func TestPracticeRefusals(t *testing.T) {
	p := newPracticeHarness(t)
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved")
	p.addSet("lima.bravo", 5011, "dev;approved", "dev")
	p.addSet("alpha", 5031, "dev;approved", "dev;approved") // "*.alpha" also matches kilo.alpha's masters
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

	const deploy = `{"kind": "deploy", "pods": "51", "set": "kilo.alpha"}`
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
		{name: "VMs of several pods", method: "GET", path: "/v1/pods/51-52/vms", status: 422, code: "bad_pod"},
		{name: "another kind", method: "POST", path: "/v1/jobs", body: `{"kind": "reset", "pods": "51"}`, status: 422, code: "bad_kind"},
		{name: "an environment kind", method: "POST", path: "/v1/jobs", body: `{"kind": "env-teardown", "pods": "51"}`, status: 422, code: "bad_kind"},
		{name: "no kind", method: "POST", path: "/v1/jobs", body: `{"pods": "51"}`, status: 422, code: "bad_kind"},
		{name: "a field the interface doesn't take", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "51", "set": "kilo.alpha", "rebuild": true}`, status: 400, code: "bad_json"},
		{name: "two requests in one", method: "POST", path: "/v1/jobs", body: deploy + deploy, status: 400, code: "bad_json"},
		{name: "a body over the cap", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "51", "set": "` + strings.Repeat("a", practiceMaxBody) + `"}`, status: 400, code: "bad_json"},
		{name: "deploy of a pattern", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "51", "set": "*"}`, status: 422, code: "bad_set"},
		{name: "deploy of no set", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "51"}`, status: 422, code: "bad_set"},
		{name: "teardown of one set", method: "POST", path: "/v1/jobs", body: `{"kind": "teardown", "pods": "51", "set": "kilo.alpha"}`, status: 422, code: "bad_set"},
		{name: "deploy of an unapproved set", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "51", "set": "lima.bravo"}`, status: 409, code: "not_approved"},
		{name: "deploy of a set that isn't there", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "51", "set": "no.such"}`, status: 409, code: "plan_failed"},
		{name: "deploy of a set whose name ends another's", method: "POST", path: "/v1/jobs", body: `{"kind": "deploy", "pods": "51", "set": "alpha"}`, status: 409, code: "plan_failed"},
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
	p.addSet("kilo.alpha", 5001, "dev;approved", "dev;approved")
	p.pve.Revoke(practicePrincipal, "/")
	p.pve.Grant(practicePrincipal, "/", "VM.Audit", "Sys.Audit", "Datastore.Audit", "Pool.Audit")

	rec := p.call(http.MethodPost, "/v1/jobs", `{"kind": "deploy", "pods": "51", "set": "kilo.alpha"}`)
	if rec.Code != http.StatusConflict || problem(t, rec) != "blocked" {
		t.Fatalf("deploy without the privileges = %d %s", rec.Code, rec.Body)
	}
	var got practiceProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Blocked) == 0 || got.Blocked[0].Name == "" || got.Blocked[0].Reason == "" {
		t.Errorf("the refusal names nothing blocked: %+v", got)
	}
	if n := len(p.jobsInStore()); n != 0 {
		t.Errorf("%d jobs stored", n)
	}
}
