package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// upidModel reads a UPID: at least three colon-separated fields, the first
// UPID, a node second; the type and id are fields 6 and 7 when there are
// seven or more.
func upidModel(s string) (UPID, bool) {
	f := strings.Split(s, ":")
	if len(f) < 3 || f[0] != "UPID" || f[1] == "" {
		return UPID{}, false
	}
	if len(f) >= 7 {
		return UPID{Node: f[1], Type: f[5], ID: f[6]}, true
	}
	return UPID{Node: f[1]}, true
}

// ParseUPID reads any text without panicking, and reads real UPIDs back to
// their node, type and id.
func TestPropParseUPID(t *testing.T) {
	for _, s := range []string{"", ":", "UPID", "UPID:", "UPID::", "UPID:n:", "UPID:n:a", "UPID:n:a:b:c:d", "UPID:n:a:b:c:d:e", "UPID:n:a:b:c:d:e:", "upid:n:a",
		"UPID:pve1:0001:0002:0003:qmstart:101:root@pam:", "UPID:pve1:0001:0002:0003:qmstart::root@pam:", "UPID:n:::::::::::"} {
		got, err := ParseUPID(s)
		want, ok := upidModel(s)
		if (err == nil) != ok || (ok && got != want) {
			t.Errorf("ParseUPID(%q) = %+v, %v; want %+v, %v", s, got, err, want, ok)
		}
	}
	field := rapid.SampledFrom([]string{"", "UPID", "upid", "pve1", "0001", "qmstart", "101", "root@pam", "a b", "\n", "é"})
	rapid.Check(t, func(t *rapid.T) {
		s := strings.Join(rapid.SliceOfN(field, 0, 10).Draw(t, "fields"), ":")
		if rapid.Bool().Draw(t, "any") {
			s = rapid.String().Draw(t, "text")
		}
		got, err := ParseUPID(s)
		want, ok := upidModel(s)
		if (err == nil) != ok || (ok && got != want) {
			t.Fatalf("ParseUPID(%q) = %+v, %v; want %+v, %v", s, got, err, want, ok)
		}
	})
}

// numberTexts are JSON numbers and what a number field may hold.
var numberTexts = []string{"0", "-0", "1", "-1", "42", "9223372036854775807", "9223372036854775808", "-9223372036854775808", "-9223372036854775809",
	"1.5", "-1.5", "0.9999", "1e3", "1E3", "1e-3", "1e18", "1e19", "1e999", "-1e999", "123456789012345678901234567890", "1.7976931348623157e308", "4.9e-324", "", "x", "NaN", "1e"}

// int64Of reads whole numbers exactly and others truncated toward zero
// when they fit an int64, else 0.
func TestPropInt64Of(t *testing.T) {
	check := func(t interface{ Fatalf(string, ...any) }, s string) {
		got := int64Of(json.Number(s))
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			if got != i {
				t.Fatalf("int64Of(%q) = %d", s, got)
			}
			return
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f >= 1<<63 || f <= -(1<<63) {
			if err != nil && got != 0 {
				t.Fatalf("int64Of(%q) = %d, want 0", s, got)
			}
			return // out of range: any number, but no panic
		}
		want := int64(f)
		if got != want {
			t.Fatalf("int64Of(%q) = %d, want %d", s, got, want)
		}
	}
	for _, s := range numberTexts {
		check(t, s)
	}
	rapid.Check(t, func(t *rapid.T) {
		check(t, rapid.OneOf(rapid.SampledFrom(numberTexts),
			rapid.Map(rapid.Int64(), func(n int64) string { return strconv.FormatInt(n, 10) }),
			rapid.Map(rapid.Float64(), func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }),
			rapid.String()).Draw(t, "number"))
	})
}

// jsonKeys are the fields the client reads, plus a few it doesn't.
var jsonKeys = []string{"data", "result", "vmid", "name", "node", "status", "tags", "template", "lock", "pool", "maxmem", "maxdisk", "mem", "type", "id",
	"storage", "shared", "online", "snaptime", "snapstate", "vmstate", "parent", "upid", "starttime", "endtime", "exitstatus", "pid", "exitcode", "exited",
	"out-data", "err-data", "hardware-address", "ip-addresses", "ip-address", "prefix", "host-name", "volid", "ticket", "username", "CSRFPreventionToken",
	"user-tag-access", "user-allow", "allowed-tags", "registered-tags", "custom", "ip", "level", "x", ""}

// drawJSON draws JSON text of any shape, mostly objects and lists of the
// keys above holding values of the wrong type, plus the occasional broken
// text.
func drawJSON(t *rapid.T, depth int) string {
	scalar := rapid.SampledFrom([]string{"null", "true", "false", "0", "1", "-1", "1.5", "1e999", "123456789012345678901234567890",
		`""`, `"1"`, `"x"`, `"qemu"`, `"online"`, `"base;linux"`, `"UPID:n:1:2:3:t:4:u:"`, `"a:b"`, `[]`, `{}`, `{`, `[1,`, `nul`, `"\ud800"`})
	if depth <= 0 {
		return scalar.Draw(t, "scalar")
	}
	switch rapid.IntRange(0, 5).Draw(t, "shape") {
	case 0, 1:
		return scalar.Draw(t, "scalar")
	case 2, 3:
		var parts []string
		for range rapid.IntRange(0, 4).Draw(t, "keys") {
			k, _ := json.Marshal(rapid.SampledFrom(jsonKeys).Draw(t, "key"))
			parts = append(parts, string(k)+":"+drawJSON(t, depth-1))
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		var parts []string
		for range rapid.IntRange(0, 3).Draw(t, "len") {
			parts = append(parts, drawJSON(t, depth-1))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
}

// No answer from Proxmox, however malformed or oddly typed, panics a client
// method: each returns what it can or an error.
func TestPropClientSurvivesAnyAnswer(t *testing.T) {
	var (
		mu     sync.Mutex
		status int
		body   string
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		st, b := status, body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		fmt.Fprint(w, b)
	}))
	t.Cleanup(srv.Close)
	c := New(Options{URLs: []string{srv.URL}, InsecureSkipVerify: true}).As(TokenCredential("battleship@pve!app", "secret"))
	calls := map[string]func(ctx context.Context){
		"ClusterResources": func(ctx context.Context) { c.ClusterResources(ctx) },
		"ClusterVMs":       func(ctx context.Context) { c.ClusterVMs(ctx) },
		"ClusterNodes":     func(ctx context.Context) { c.ClusterNodes(ctx) },
		"OnlineNodes":      func(ctx context.Context) { c.OnlineNodes(ctx) },
		"CustomCPUModels":  func(ctx context.Context) { c.CustomCPUModels(ctx, "n1") },
		"VMConfig":         func(ctx context.Context) { c.VMConfig(ctx, "n1", 101) },
		"Snapshots":        func(ctx context.Context) { c.Snapshots(ctx, "n1", 101) },
		"CurrentStatus":    func(ctx context.Context) { c.CurrentStatus(ctx, "n1", 101) },
		"StorageContent":   func(ctx context.Context) { c.StorageContent(ctx, "n1", "local", 101) },
		"VMIDHeld":         func(ctx context.Context) { c.VMIDHeld(ctx, 101) },
		"Permissions":      func(ctx context.Context) { c.Permissions(ctx) },
		"PermissionsAt":    func(ctx context.Context) { c.PermissionsAt(ctx, "/vms/101") },
		"TagAccess":        func(ctx context.Context) { c.TagAccess(ctx) },
		"NodeTasks":        func(ctx context.Context) { c.NodeTasks(ctx, "n1", time.Unix(0, 0)) },
		"AgentInterfaces":  func(ctx context.Context) { c.AgentInterfaces(ctx, "n1", 101) },
		"AgentHostname":    func(ctx context.Context) { c.AgentHostname(ctx, "n1", 101) },
		"AgentRead":        func(ctx context.Context) { c.AgentRead(ctx, "n1", 101, []string{"id"}) },
		"AgentExec":        func(ctx context.Context) { c.AgentExec(ctx, "n1", 101, []string{"id"}, "") },
		"AgentExecStatus":  func(ctx context.Context) { c.AgentExecStatus(ctx, "n1", 101, 7, 1<<20) },
		"AgentPing":        func(ctx context.Context) { c.AgentPing(ctx, "n1", 101) },
		"Clone": func(ctx context.Context) {
			c.Clone(ctx, CloneRequest{SourceNode: "n1", SourceVMID: 100, NewVMID: 101, Name: "x"})
		},
		"CreateSnapshot":    func(ctx context.Context) { c.CreateSnapshot(ctx, "n1", 101, SnapshotRequest{Name: "s"}) },
		"Rollback":          func(ctx context.Context) { c.Rollback(ctx, "n1", 101, "s") },
		"Power":             func(ctx context.Context) { c.Power(ctx, "n1", 101, "start") },
		"DeleteVM":          func(ctx context.Context) { c.DeleteVM(ctx, "n1", 101) },
		"DeleteVolume":      func(ctx context.Context) { c.DeleteVolume(ctx, "n1", "local", "local:101/x") },
		"ConvertToTemplate": func(ctx context.Context) { c.ConvertToTemplate(ctx, "n1", 101) },
	}
	names := make([]string, 0, len(calls))
	for n := range calls {
		names = append(names, n)
	}
	rapid.Check(t, func(t *rapid.T) {
		b := drawJSON(t, 4)
		switch rapid.IntRange(0, 4).Draw(t, "wrap") {
		case 0:
			b = `{"data":` + b + `}`
		case 1:
			b = `{"data":{"result":` + b + `}}`
		case 2:
			b = rapid.String().Draw(t, "garbage")
		}
		mu.Lock()
		status, body = rapid.SampledFrom([]int{200, 200, 200, 400, 401, 403, 404, 500}).Draw(t, "status"), b
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, n := range names {
			calls[n](ctx)
		}
	})
}
