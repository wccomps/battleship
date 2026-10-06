package pods

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// notChanges are the API methods a step may call after its run stopped:
// reads, task waits and stopping a task. Every other method changes
// something, so guardedAPI must refuse it then.
var notChanges = map[string]bool{
	"ClusterVMs": true, "OnlineNodes": true, "VMConfig": true, "Snapshots": true,
	"CurrentStatus": true, "StorageContent": true, "VMIDHeld": true, "WaitTask": true, "StopTask": true,
}

// countingAPI counts the calls that reach it.
type countingAPI struct {
	API
	calls int
}

// Every API method a step can call either is in notChanges or is held back
// by guardedAPI once the step's run has stopped, so a method added to API
// can't slip past the guard.
func TestGuardedAPIHoldsBackEveryChange(t *testing.T) {
	run, stop := context.WithCancel(context.Background())
	stop()
	ctx := context.WithValue(context.Background(), stepKey{}, &stepState{run: run})
	apiType := reflect.TypeFor[API]()
	for i := range apiType.NumMethod() {
		m := apiType.Method(i)
		inner := &countingAPI{}
		args := []reflect.Value{reflect.ValueOf(ctx)}
		for j := 1; j < m.Type.NumIn(); j++ {
			args = append(args, reflect.Zero(m.Type.In(j)))
		}
		var out []reflect.Value
		func() {
			// A method guardedAPI passes through reaches the nil API
			// countingAPI embeds, which panics: count it as a call.
			defer func() {
				if recover() != nil {
					inner.calls++
				}
			}()
			out = reflect.ValueOf(guardedAPI{inner}).MethodByName(m.Name).Call(args)
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
