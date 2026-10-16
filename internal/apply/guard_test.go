package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// notChanges are the API methods allowed after a stop: reads, task waits
// and StopTask. guardedAPI must refuse every other method then.
var notChanges = map[string]bool{
	"ClusterVMs": true, "OnlineNodes": true, "VMConfig": true, "Snapshots": true,
	"CurrentStatus": true, "StorageContent": true, "VMIDHeld": true, "WaitTask": true, "StopTask": true,
	"AgentPing": true, "AgentExecStatus": true, "AgentInterfaces": true, "AgentHostname": true,
}

// countingAPI counts the calls that reach it.
type countingAPI struct {
	pods.API
	calls int
}

// Every API method is either in notChanges or held back by guardedAPI, so
// a new API method can't slip past the guard.
func TestGuardedAPIHoldsBackEveryChange(t *testing.T) {
	run, stop := context.WithCancel(context.Background())
	stop()
	ctx := context.WithValue(context.Background(), stepKey{}, &stepState{run: run})
	apiType := reflect.TypeFor[pods.API]()
	for i := range apiType.NumMethod() {
		m := apiType.Method(i)
		inner := &countingAPI{}
		args := []reflect.Value{reflect.ValueOf(ctx)}
		for j := 1; j < m.Type.NumIn(); j++ {
			args = append(args, reflect.Zero(m.Type.In(j)))
		}
		var out []reflect.Value
		func() {
			// A passed-through method hits countingAPI's nil embedded API and panics;
			// count that as a call.
			defer func() {
				if recover() != nil {
					inner.calls++
				}
			}()
			out = reflect.ValueOf(guardedAPI{API: inner}).MethodByName(m.Name).Call(args)
		}()
		passed := inner.calls > 0
		if notChanges[m.Name] != passed {
			t.Errorf("%s: passed through = %v, want %v (a change must be held back after a stop; list a read in notChanges)", m.Name, passed, notChanges[m.Name])
			continue
		}
		if !passed {
			err, _ := out[len(out)-1].Interface().(error)
			if !errors.Is(err, errHeldBack) {
				t.Errorf("%s: err = %v, want errHeldBack", m.Name, err)
			}
		}
	}
}

// Held refuses every change, outside steps too (cleanup, template builds),
// and lets reads through.
func TestHeldHoldsBackEveryChange(t *testing.T) {
	errGone := errors.New("claim uncertain")
	apiType := reflect.TypeFor[pods.API]()
	for i := range apiType.NumMethod() {
		m := apiType.Method(i)
		inner := &countingAPI{}
		args := []reflect.Value{reflect.ValueOf(context.Background())}
		for j := 1; j < m.Type.NumIn(); j++ {
			args = append(args, reflect.Zero(m.Type.In(j)))
		}
		var out []reflect.Value
		func() {
			defer func() {
				if recover() != nil {
					inner.calls++
				}
			}()
			out = reflect.ValueOf(guardedAPI{API: inner, held: func(ctx context.Context) (context.Context, context.CancelFunc, error) { return ctx, func() {}, errGone }}).MethodByName(m.Name).Call(args)
		}()
		passed := inner.calls > 0
		if notChanges[m.Name] != passed {
			t.Errorf("%s: passed through = %v, want %v", m.Name, passed, notChanges[m.Name])
			continue
		}
		if !passed {
			if err, _ := out[len(out)-1].Interface().(error); !errors.Is(err, errGone) {
				t.Errorf("%s: err = %v, want Held's error", m.Name, err)
			}
		}
	}
}

// Every change is sent with Held's context, and done once it returns.
func TestHeldBoundsEveryChange(t *testing.T) {
	type key struct{}
	apiType := reflect.TypeFor[pods.API]()
	for i := range apiType.NumMethod() {
		m := apiType.Method(i)
		if notChanges[m.Name] {
			continue
		}
		var released bool
		inner := &ctxAPI{key: key{}, released: &released}
		g := guardedAPI{API: inner, held: func(ctx context.Context) (context.Context, context.CancelFunc, error) {
			return context.WithValue(ctx, key{}, "bounded"), func() { released = true }, nil
		}}
		args := []reflect.Value{reflect.ValueOf(context.Background())}
		for j := 1; j < m.Type.NumIn(); j++ {
			args = append(args, reflect.Zero(m.Type.In(j)))
		}
		reflect.ValueOf(g).MethodByName(m.Name).Call(args)
		if !inner.bounded {
			t.Errorf("%s: sent without Held's context", m.Name)
		}
		if !released || inner.releasedFirst {
			t.Errorf("%s: done called %v, before the call returned %v", m.Name, released, inner.releasedFirst)
		}
	}
}

// The executor calls Proxmox through e.api, the guarded API, never e.API.
func TestExecutorUsesTheGuardedAPI(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "e.API.") {
				t.Errorf("%s:%d calls e.API directly, bypassing the guard: %s", f, n+1, strings.TrimSpace(line))
			}
		}
	}
}

// ctxAPI records whether each change was sent with Held's context, and
// whether Held's done ran before the change returned.
type ctxAPI struct {
	pods.API
	key           any
	bounded       bool
	releasedFirst bool
	released      *bool
}

func (c *ctxAPI) see(ctx context.Context) {
	c.bounded = ctx.Value(c.key) == "bounded"
	c.releasedFirst = c.released != nil && *c.released
}

func (c *ctxAPI) SetVMConfig(ctx context.Context, node string, vmid int, changes map[string]string) error {
	c.see(ctx)
	return nil
}

func (c *ctxAPI) AgentExec(ctx context.Context, node string, vmid int, command []string, input string) (int, error) {
	c.see(ctx)
	return 0, nil
}

func (c *ctxAPI) Clone(ctx context.Context, r proxmox.CloneRequest) (string, error) {
	c.see(ctx)
	return "", nil
}

func (c *ctxAPI) ConvertToTemplate(ctx context.Context, node string, vmid int) (string, error) {
	c.see(ctx)
	return "", nil
}

func (c *ctxAPI) RegenerateCloudInit(ctx context.Context, node string, vmid int) error {
	c.see(ctx)
	return nil
}

func (c *ctxAPI) CreateSnapshot(ctx context.Context, node string, vmid int, r proxmox.SnapshotRequest) (string, error) {
	c.see(ctx)
	return "", nil
}

func (c *ctxAPI) Rollback(ctx context.Context, node string, vmid int, snapshot string) (string, error) {
	c.see(ctx)
	return "", nil
}

func (c *ctxAPI) Power(ctx context.Context, node string, vmid int, action string) (string, error) {
	c.see(ctx)
	return "", nil
}

func (c *ctxAPI) Shutdown(ctx context.Context, node string, vmid int, timeout time.Duration, forceStop bool) (string, error) {
	c.see(ctx)
	return "", nil
}

func (c *ctxAPI) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	c.see(ctx)
	return "", nil
}

func (c *ctxAPI) DeleteVolume(ctx context.Context, node, storage, volid string) (string, error) {
	c.see(ctx)
	return "", nil
}
