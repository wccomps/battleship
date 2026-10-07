package proxmox

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePVE serves canned responses keyed by "METHOD /path" and records requests.
type fakePVE struct {
	t        *testing.T
	routes   map[string]func(w http.ResponseWriter, r *http.Request)
	mu       sync.Mutex
	requests []string
	bodies   map[string]string
	rawPaths []string
}

func newFake(t *testing.T) (*fakePVE, *Client) {
	f := &fakePVE{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}, bodies: map[string]string{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=battleship@pve!app=secret" {
			t.Errorf("Authorization = %q", got)
		}
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api2/json")
		f.mu.Lock()
		f.requests = append(f.requests, key+"?"+r.URL.RawQuery)
		body, _ := io.ReadAll(r.Body)
		f.bodies[key] = string(body)
		if r.URL.RawPath != "" {
			f.rawPaths = append(f.rawPaths, r.URL.RawPath)
		} else {
			f.rawPaths = append(f.rawPaths, r.URL.EscapedPath())
		}
		f.mu.Unlock()
		h, ok := f.routes[key]
		if !ok {
			t.Errorf("unexpected request %s", key)
			http.Error(w, "not found", 404)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(Options{URLs: []string{srv.URL}, InsecureSkipVerify: true}).As(TokenCredential("battleship@pve!app", "secret"))
	return f, c
}

func jsonData(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":`+body+`}`)
	}
}

func TestClusterVMsParsesQemuOnly(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /cluster/resources"] = jsonData(`[
		{"type":"qemu","vmid":10105,"name":"team01-teak","node":"cedar","status":"running","template":0,"tags":"dev;x","pool":"pool-01","lock":"backup"},
		{"type":"qemu","vmid":9005,"name":"teak.x.tpl","node":"cedar","status":"stopped","template":1},
		{"type":"lxc","vmid":300,"name":"ct","node":"cedar"}]`)

	vms, err := c.ClusterVMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 {
		t.Fatalf("got %d VMs, want 2: %+v", len(vms), vms)
	}
	want := VM{VMID: 10105, Name: "team01-teak", Node: "cedar", Status: "running", Tags: "dev;x", Pool: "pool-01", Lock: "backup"}
	if vms[0] != want {
		t.Errorf("vms[0] = %+v, want %+v", vms[0], want)
	}
	if !vms[1].Template {
		t.Errorf("vms[1].Template = false, want true")
	}
	if len(f.requests) != 1 || f.requests[0] != "GET /cluster/resources?" {
		t.Errorf("request = %q", f.requests[0])
	}
}

func TestVMConfigStringifiesValues(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /nodes/cedar/qemu/101/config"] = jsonData(`{"cores":2,"net0":"virtio=BC:24:11:00:00:01,bridge=vmbr0","onboot":1}`)

	cfg, err := c.VMConfig(context.Background(), "cedar", 101)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["cores"] != "2" || cfg["onboot"] != "1" || cfg["net0"] != "virtio=BC:24:11:00:00:01,bridge=vmbr0" {
		t.Errorf("cfg = %v", cfg)
	}
}

func TestSetVMConfigSendsForm(t *testing.T) {
	f, c := newFake(t)
	f.routes["PUT /nodes/cedar/qemu/101/config"] = jsonData(`null`)

	err := c.SetVMConfig(context.Background(), "cedar", 101, map[string]string{"net0": "virtio,bridge=int01"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.bodies["PUT /nodes/cedar/qemu/101/config"]; got != "net0=virtio%2Cbridge%3Dint01" {
		t.Errorf("body = %q", got)
	}
}

func TestErrorIncludesStatusLineAndParamErrors(t *testing.T) {
	f, c := newFake(t)
	f.routes["POST /nodes/cedar/qemu/9005/clone"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"data":null,"errors":{"newid":"invalid format"}}`)
	}

	_, err := c.Clone(context.Background(), CloneRequest{SourceNode: "cedar", SourceVMID: 9005, NewVMID: 1, Name: "x", TargetNode: "cedar"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || !strings.Contains(apiErr.Message, "newid: invalid format") {
		t.Fatalf("err = %v", err)
	}
}

func TestCloneParams(t *testing.T) {
	f, c := newFake(t)
	f.routes["POST /nodes/cedar/qemu/9005/clone"] = jsonData(`"UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"`)

	upid, err := c.Clone(context.Background(), CloneRequest{
		SourceNode: "cedar", SourceVMID: 9005, NewVMID: 10105, Name: "team01-teak",
		TargetNode: "spruce", Pool: "pool-01", Full: false, Storage: "competitions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if upid != "UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:" {
		t.Errorf("upid = %q", upid)
	}
	// Linked clones must not send storage.
	want := "full=0&name=team01-teak&newid=10105&pool=pool-01&target=spruce"
	if got := f.bodies["POST /nodes/cedar/qemu/9005/clone"]; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestWaitTaskSuccess(t *testing.T) {
	f, c := newFake(t)
	upid := "UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"
	calls := 0
	f.routes["GET /nodes/cedar/tasks/"+upid+"/status"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			jsonData(`{"status":"running"}`)(w, r)
			return
		}
		jsonData(`{"status":"stopped","exitstatus":"OK"}`)(w, r)
	}

	if err := c.WaitTask(context.Background(), upid, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("status polled %d times, want 2", calls)
	}
}

func TestWaitTaskFailureIncludesLogTail(t *testing.T) {
	f, c := newFake(t)
	upid := "UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"
	f.routes["GET /nodes/cedar/tasks/"+upid+"/status"] = jsonData(`{"status":"stopped","exitstatus":"clone failed: File exists"}`)
	f.routes["GET /nodes/cedar/tasks/"+upid+"/log"] = jsonData(`[{"n":1,"t":"create full clone"},{"n":2,"t":"mkdir: File exists"}]`)

	err := c.WaitTask(context.Background(), upid, time.Millisecond)
	var taskErr *TaskError
	if !errors.As(err, &taskErr) {
		t.Fatalf("err = %v, want *TaskError", err)
	}
	if len(taskErr.LogTail) != 2 || taskErr.LogTail[1] != "mkdir: File exists" {
		t.Errorf("LogTail = %v", taskErr.LogTail)
	}
}

func TestParseUPID(t *testing.T) {
	if u, err := ParseUPID("UPID:spruce:0001:02:03:qmstart:101:root@pam:"); err != nil || u != (UPID{Node: "spruce", Type: "qmstart", ID: "101"}) {
		t.Errorf("ParseUPID = %+v, %v", u, err)
	}
	if u, err := ParseUPID("UPID:spruce:0001"); err != nil || u != (UPID{Node: "spruce"}) {
		t.Errorf("ParseUPID(short) = %+v, %v", u, err)
	}
	if _, err := ParseUPID("garbage"); err == nil {
		t.Error("ParseUPID(garbage) succeeded")
	}
}

func TestCloneRejectsNonJSONSuccess(t *testing.T) {
	f, c := newFake(t)
	f.routes["POST /nodes/cedar/qemu/9005/clone"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `<html>proxy</html>`)
	}

	_, err := c.Clone(context.Background(), CloneRequest{SourceNode: "cedar", SourceVMID: 9005, NewVMID: 1, Name: "x", TargetNode: "cedar"})
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("err = %v, want error containing 'invalid JSON'", err)
	}
}

func TestCloneRejectsMissingUPID(t *testing.T) {
	f, c := newFake(t)
	f.routes["POST /nodes/cedar/qemu/9005/clone"] = jsonData(`null`)

	_, err := c.Clone(context.Background(), CloneRequest{SourceNode: "cedar", SourceVMID: 9005, NewVMID: 1, Name: "x", TargetNode: "cedar"})
	if err == nil || !strings.Contains(err.Error(), "no task ID") {
		t.Fatalf("err = %v, want error containing 'no task ID'", err)
	}
}

func TestCloneOmitsEmptyTarget(t *testing.T) {
	f, c := newFake(t)
	f.routes["POST /nodes/cedar/qemu/9005/clone"] = jsonData(`"UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"`)

	_, err := c.Clone(context.Background(), CloneRequest{
		SourceNode: "cedar", SourceVMID: 9005, NewVMID: 10105, Name: "team01-teak",
		TargetNode: "", Pool: "pool-01", Full: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := f.bodies["POST /nodes/cedar/qemu/9005/clone"]
	if strings.Contains(got, "target=") {
		t.Errorf("body = %q, should not contain 'target='", got)
	}
}

func TestErrorWithEmptyStatusText(t *testing.T) {
	f, c := newFake(t)
	f.routes["POST /nodes/cedar/qemu/9005/clone"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(596)
		_, _ = io.WriteString(w, "tunnel failed")
	}

	_, err := c.Clone(context.Background(), CloneRequest{SourceNode: "cedar", SourceVMID: 9005, NewVMID: 1, Name: "x", TargetNode: "cedar"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Message != "tunnel failed" {
		t.Errorf("Message = %q, want 'tunnel failed'", apiErr.Message)
	}
}

func TestWaitTaskToleratesPollFailures(t *testing.T) {
	f, c := newFake(t)
	upid := "UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"
	calls := 0
	f.routes["GET /nodes/cedar/tasks/"+upid+"/status"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(502)
			_, _ = io.WriteString(w, `bad gateway`)
			return
		}
		jsonData(`{"status":"stopped","exitstatus":"OK"}`)(w, r)
	}

	if err := c.WaitTask(context.Background(), upid, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("status polled %d times, want 2", calls)
	}
}

func TestWaitTaskGivesUpAfterRepeatedFailures(t *testing.T) {
	f, c := newFake(t)
	upid := "UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"
	calls := 0
	f.routes["GET /nodes/cedar/tasks/"+upid+"/status"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(502)
		_, _ = io.WriteString(w, `bad gateway`)
	}

	err := c.WaitTask(context.Background(), upid, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "polling task") {
		t.Fatalf("err = %v, want error containing 'polling task'", err)
	}
	if calls != 10 {
		t.Errorf("status polled %d times, want 10", calls)
	}
}

func TestWaitTaskReturnsOn4xx(t *testing.T) {
	f, c := newFake(t)
	upid := "UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"
	calls := 0
	f.routes["GET /nodes/cedar/tasks/"+upid+"/status"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `forbidden`)
	}

	err := c.WaitTask(context.Background(), upid, time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("status polled %d times, want 1", calls)
	}
}

func TestWaitTaskAcceptsWarnings(t *testing.T) {
	f, c := newFake(t)
	upid := "UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"
	f.routes["GET /nodes/cedar/tasks/"+upid+"/status"] = jsonData(`{"status":"stopped","exitstatus":"WARNINGS: 1"}`)

	if err := c.WaitTask(context.Background(), upid, time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteVolumeReturnsTaskAndEscapes(t *testing.T) {
	f, c := newFake(t)
	upid := "UPID:cedar:1:2:3:imgdel:10105:battleship@pve!app:"
	f.routes["DELETE /nodes/cedar/storage/competitions/content/competitions:10105/vm-10105-disk-0.qcow2"] = jsonData(`"` + upid + `"`)

	got, err := c.DeleteVolume(context.Background(), "cedar", "competitions", "competitions:10105/vm-10105-disk-0.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	if got != upid {
		t.Errorf("upid = %q, want %q", got, upid)
	}
	// The caller waits for the task: no status poll here.
	if len(f.requests) != 1 {
		t.Errorf("requests = %v, want only the DELETE", f.requests)
	}
	found := false
	for _, p := range f.rawPaths {
		if strings.Contains(p, "%2F") && strings.Contains(p, "competitions:10105") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("rawPaths = %v, want one containing %%2F and 'competitions:10105'", f.rawPaths)
	}
}

// Some storage types free a volume synchronously and return no task.
func TestDeleteVolumeSynchronous(t *testing.T) {
	f, c := newFake(t)
	f.routes["DELETE /nodes/cedar/storage/competitions/content/competitions:10105/vm-10105-disk-0.qcow2"] = jsonData(`null`)

	got, err := c.DeleteVolume(context.Background(), "cedar", "competitions", "competitions:10105/vm-10105-disk-0.qcow2")
	if err != nil || got != "" {
		t.Errorf("DeleteVolume = %q, %v; want no task and no error", got, err)
	}
}

func TestStorageContentListsVolumesOfOneVM(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /nodes/cedar/storage/competitions/content"] = jsonData(`[
		{"volid":"competitions:9008/base-9008-disk-0.qcow2","vmid":9008,"format":"qcow2","size":34359738368,"content":"images"},
		{"volid":"competitions:9008/vm-9008-disk-1.qcow2","vmid":"9008","format":"qcow2","size":34359738368,"content":"images"}
	]`)

	got, err := c.StorageContent(context.Background(), "cedar", "competitions", 9008)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"competitions:9008/base-9008-disk-0.qcow2", "competitions:9008/vm-9008-disk-1.qcow2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("volumes = %q, want %q", got, want)
	}
	if len(f.requests) != 1 || !strings.HasSuffix(f.requests[0], "?vmid=9008") {
		t.Errorf("requests = %v, want one with vmid=9008", f.requests)
	}
}

// Proxmox answers nextid with the VMID when it is free and a 400 "already
// exists" parameter error when any VM holds it, seen or not.
func TestVMIDHeld(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /cluster/nextid"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("vmid") == "10121" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			w.Write([]byte(`{"errors":{"vmid":"VM 10121 already exists"},"data":null}`))
			return
		}
		jsonData(`"10122"`)(w, r)
	}
	for vmid, want := range map[int]bool{10121: true, 10122: false} {
		got, err := c.VMIDHeld(context.Background(), vmid)
		if err != nil || got != want {
			t.Errorf("VMIDHeld(%d) = %v, %v; want %v", vmid, got, err, want)
		}
	}
	f.routes["GET /cluster/nextid"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }
	if _, err := c.VMIDHeld(context.Background(), 10121); err == nil {
		t.Error("VMIDHeld after a 500 = nil error, want it")
	}
}

func TestWaitTaskEscapesUPID(t *testing.T) {
	f, c := newFake(t)
	upid := "UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"
	f.routes["GET /nodes/cedar/tasks/"+upid+"/status"] = jsonData(`{"status":"stopped","exitstatus":"OK"}`)

	if err := c.WaitTask(context.Background(), upid, time.Millisecond); err != nil {
		t.Fatal(err)
	}

	// Check that the UPID with ! is escaped as %21
	found := false
	for _, p := range f.rawPaths {
		if strings.Contains(p, "%21app") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("rawPaths = %v, want one containing %%21app", f.rawPaths)
	}
}

func TestEmptyStatusTextWithJSONBody(t *testing.T) {
	f, c := newFake(t)
	f.routes["POST /nodes/cedar/qemu/9005/clone"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(596)
		_, _ = io.WriteString(w, `{"data":null,"errors":{"vmid":"bad"}}`)
	}

	_, err := c.Clone(context.Background(), CloneRequest{SourceNode: "cedar", SourceVMID: 9005, NewVMID: 1, Name: "x", TargetNode: "cedar"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if strings.Contains(apiErr.Message, `{"data"`) {
		t.Errorf("Message = %q, should not contain JSON envelope", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "vmid: bad") {
		t.Errorf("Message = %q, want to contain 'vmid: bad'", apiErr.Message)
	}
}

func TestWaitTaskReturnsCancelDuringPollBackoff(t *testing.T) {
	f, c := newFake(t)
	upid := "UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"
	calls := 0
	f.routes["GET /nodes/cedar/tasks/"+upid+"/status"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(502)
		_, _ = io.WriteString(w, `bad gateway`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.WaitTask(ctx, upid, 1*time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if calls != 1 {
		t.Errorf("status polled %d times, want 1", calls)
	}
}

// With RootCAs, the client verifies Proxmox's certificate against them (a
// cluster CA) instead of skipping verification.
func TestClientVerifiesWithRootCAs(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	c := New(Options{URLs: []string{srv.URL}, RootCAs: pool}).As(TokenCredential("battleship@pve!app", "secret"))
	if _, err := c.ClusterVMs(context.Background()); err != nil {
		t.Errorf("with the server's CA: %v", err)
	}
	// Without it, the certificate is refused.
	c = New(Options{URLs: []string{srv.URL}}).As(TokenCredential("battleship@pve!app", "secret"))
	var unknown x509.UnknownAuthorityError
	if _, err := c.ClusterVMs(context.Background()); !errors.As(err, &unknown) {
		t.Errorf("without the CA: %v, want an unknown-authority error", err)
	}
}

func TestShutdownSendsTimeoutAndForceStop(t *testing.T) {
	f, c := newFake(t)
	f.routes["POST /nodes/cedar/qemu/10121/status/shutdown"] = jsonData(`"UPID:cedar:1:2:3:qmshutdown:10121:battleship@pve!app:"`)

	upid, err := c.Shutdown(context.Background(), "cedar", 10121, 90*time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if upid != "UPID:cedar:1:2:3:qmshutdown:10121:battleship@pve!app:" {
		t.Errorf("upid = %q", upid)
	}
	want := "forceStop=1&timeout=90"
	if got := f.bodies["POST /nodes/cedar/qemu/10121/status/shutdown"]; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestStopTaskDeletesTheTaskOnItsNode(t *testing.T) {
	f, c := newFake(t)
	upid := "UPID:cedar:1:2:3:qmclone:9005:battleship@pve!app:"
	called := false
	f.routes["DELETE /nodes/cedar/tasks/"+upid] = func(w http.ResponseWriter, r *http.Request) {
		called = true
		jsonData(`null`)(w, r)
	}
	if err := c.StopTask(context.Background(), upid); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("DELETE /nodes/cedar/tasks/<upid> not sent")
	}
}

// A VM pvestatd hasn't reported yet (for about 10s after it is created) is
// listed with no name; its name and template flag come from its config.
func TestClusterVMsNamesVMsTooNewForTheListing(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /cluster/resources"] = jsonData(`[
		{"type":"qemu","vmid":10004,"id":"qemu/10004","node":"birch","status":"unknown"},
		{"type":"qemu","vmid":9004,"id":"qemu/9004","node":"spruce","status":"unknown"},
		{"type":"qemu","vmid":10006,"id":"qemu/10006","node":"birch","status":"unknown"},
		{"type":"qemu","vmid":10105,"name":"team01-teak","node":"cedar","status":"running","template":0}]`)
	f.routes["GET /nodes/birch/qemu/10004/config"] = jsonData(`{"name":"team00-hazel","cores":2}`)
	f.routes["GET /nodes/spruce/qemu/9004/config"] = jsonData(`{"name":"hazel.x.tpl","template":1}`)
	f.routes["GET /nodes/birch/qemu/10006/config"] = pveStatus(t, 500, "Configuration file 'nodes/birch/qemu-server/10006.conf' does not exist")

	vms, err := c.ClusterVMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]VM{}
	for _, vm := range vms {
		got[vm.VMID] = vm
	}
	if len(vms) != 3 {
		t.Fatalf("VMs = %+v, want 10004, 9004 and 10105 (10006 was deleted meanwhile)", vms)
	}
	if v := got[10004]; v.Name != "team00-hazel" || v.Template || v.Node != "birch" {
		t.Errorf("10004 = %+v", v)
	}
	if v := got[9004]; v.Name != "hazel.x.tpl" || !v.Template {
		t.Errorf("9004 = %+v", v)
	}
	if v := got[10105]; v.Name != "team01-teak" {
		t.Errorf("10105 = %+v", v)
	}
}

// A nameless VM on an offline node isn't asked about: it can't answer.
func TestClusterVMsDoesntAskOfflineNodes(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /cluster/resources"] = jsonData(`[
		{"type":"qemu","vmid":10004,"node":"alder","status":"unknown"},
		{"type":"node","node":"alder","status":"offline"},
		{"type":"node","node":"cedar","status":"online"}]`)
	asked := false
	f.routes["GET /nodes/alder/qemu/10004/config"] = func(w http.ResponseWriter, r *http.Request) {
		asked = true
		pveStatus(t, 595, "no route to host")(w, r)
	}
	got, err := c.ClusterVMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if asked || len(got) != 1 || got[0].Name != "" {
		t.Errorf("asked offline node = %v, VMs = %+v, want 10004 kept nameless without asking", asked, got)
	}
	if len(f.requests) != 1 {
		t.Errorf("requests = %v, want the one listing", f.requests)
	}
}

// A VM whose config can't be read for a reason other than being gone fails
// the listing: a plan made without it would silently skip it.
func TestClusterVMsFailsWhenANewVMsNameCannotBeRead(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /cluster/resources"] = jsonData(`[{"type":"qemu","vmid":10004,"node":"birch","status":"unknown"}]`)
	f.routes["GET /nodes/birch/qemu/10004/config"] = pveStatus(t, 403, "Permission check failed (/vms/10004, VM.Audit)")
	_, err := c.ClusterVMs(context.Background())
	if err == nil || !strings.Contains(err.Error(), "10004") {
		t.Fatalf("err = %v, want one naming VM 10004", err)
	}
}

// pveStatus answers as Proxmox does when a call fails: the reason is in the
// status line, which Go's server can't write, so it writes the response raw.
func pveStatus(t *testing.T, code int, reason string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: 13\r\nConnection: close\r\n\r\n{\"data\":null}", code, reason)
		_ = buf.Flush()
	}
}

// A proxy's error page becomes one cut line so it can't split a log line.
func TestNonProxmoxErrorBodyIsOneLine(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /cluster/resources"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(599)
		_, _ = io.WriteString(w, "<html>\n<body>\r\n  upstream\tdown\n"+strings.Repeat("x", 300)+"</body></html>")
	}
	_, err := c.ClusterResources(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an APIError", err)
	}
	if strings.ContainsAny(apiErr.Message, "\r\n\t") || !strings.HasPrefix(apiErr.Message, "<html> <body> upstream down ") || len(apiErr.Message) > 200 {
		t.Errorf("message = %q", apiErr.Message)
	}
}

// A view WhenRefused calls back on each 401, and on nothing else.
func TestWhenRefusedCallsBackOn401(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /cluster/resources"] = pveStatus(t, 401, "authentication failure")
	f.routes["GET /nodes/n1/qemu/1/config"] = pveStatus(t, 500, "boom")
	n := 0
	v := c.WhenRefused(func() { n++ })
	if _, err := v.ClusterResources(context.Background()); !IsLapsed(err) {
		t.Fatalf("err = %v, want a 401", err)
	}
	if _, err := v.VMConfig(context.Background(), "n1", 1); err == nil {
		t.Fatal("want the 500")
	}
	if n != 1 {
		t.Errorf("refused called %d times, want 1 (the 401 only)", n)
	}
	if _, err := c.ClusterResources(context.Background()); !IsLapsed(err) || n != 1 {
		t.Errorf("the view it came from called back: n = %d", n)
	}
}
