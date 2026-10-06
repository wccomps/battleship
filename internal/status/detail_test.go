package status

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

func TestDetailReadsLive(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.poll(t)
	h.scan(t) // 01/dc is clean
	h.api.setConfig(10101, "net0", "virtio=BC:24:11:00:00:01,bridge=vmbr0")
	h.clock.Advance(20 * time.Second)
	sub, cancel := h.hub.SubscribeTopics(TopicGrid)
	defer cancel()

	d, err := h.p.Detail(ctx, "01", "dc")
	if err != nil {
		t.Fatal(err)
	}
	if d.Config["net0"] != "virtio=BC:24:11:00:00:01,bridge=vmbr0" || d.Config["scsi0"] == "" {
		t.Errorf("config = %v, want the live config", d.Config)
	}
	if !reflect.DeepEqual(d.Snapshots, []string{"initial", "pre-inject"}) || d.Baseline != "initial" {
		t.Errorf("snapshots %v baseline %q, want [initial pre-inject] and initial", d.Snapshots, d.Baseline)
	}
	want := []Drift{{Kind: DriftNetwork, Reason: "network differs from team 01's: net0 should be virtio=BC:24:11:00:00:01,bridge=ext01"}}
	if d.Cell.State != StateDrifted || !reflect.DeepEqual(d.Cell.Drift, want) || d.Cell.VMID != 10101 {
		t.Errorf("cell = %+v, want drifted with %+v", d.Cell, want)
	}
	// The live read counts as a scan of that VM: the grid shows it at once.
	recv(t, sub)
	if c := cell(t, h.p.Grid(), "01", "dc"); !reflect.DeepEqual(c.Drift, want) {
		t.Errorf("grid cell after Detail = %+v, want the live drift", c)
	}
}

func TestDetailClearsFixedDrift(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.poll(t)
	h.scan(t)
	h.api.setSnaps(10202, "initial")
	d, err := h.p.Detail(ctx, "02", "web")
	if err != nil {
		t.Fatal(err)
	}
	if d.Cell.State != StateRunning || d.Cell.Drift != nil {
		t.Errorf("detail of a fixed VM = %s %+v, want running", d.Cell.State, d.Cell.Drift)
	}
	if c := cell(t, h.p.Grid(), "02", "web"); c.State != StateRunning {
		t.Errorf("grid cell = %s, want running", c.State)
	}
}

func TestDetailKeepsPoolAndJobDrift(t *testing.T) {
	h := newHarness(t, nil)
	vm := teamVM("01", "dc", 10101)
	vm.Pool = "pool-02"
	vm.Status = "stopped"
	h.api.add(vm, cleanConfig("01")) // and no snapshot
	h.hist.set("team01-dc", store.ItemResult{JobID: 5, JobKind: "deploy", FinishedAt: h.clock.Now(), Status: store.ItemFailed, Step: "snapshot", Error: "snapshot: VM is locked"})
	h.poll(t)
	d, err := h.p.Detail(ctx, "01", "dc")
	if err != nil {
		t.Fatal(err)
	}
	if want := []DriftKind{DriftPool, DriftJob, DriftSnapshot}; !reflect.DeepEqual(kinds(d.Cell), want) {
		t.Errorf("drift kinds = %v, want %v", kinds(d.Cell), want)
	}
	if d.Cell.Power != "stopped" || d.Cell.State != StateDrifted {
		t.Errorf("cell = %+v", d.Cell)
	}
}

func TestDetailOfMissingVM(t *testing.T) {
	h := newHarness(t, nil)
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	h.api.add(teamVM("02", "web", 10202), cleanConfig("02"), "initial")
	h.poll(t)
	d, err := h.p.Detail(ctx, "02", "dc")
	if err != nil {
		t.Fatal(err)
	}
	if d.Cell.State != StateMissing || d.Cell.Name != "team02-dc" || d.Config != nil || d.Snapshots != nil || d.Baseline != "" {
		t.Errorf("detail of a missing VM = %+v", d)
	}
	if reads, _, _ := h.api.counts(); reads != 0 {
		t.Errorf("%d reads for a missing VM, want 0", reads)
	}
}

func TestDetailUnknownCell(t *testing.T) {
	h := newHarness(t, nil)
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	h.poll(t)
	for _, c := range [][2]string{{"09", "dc"}, {"01", "nope"}, {"1", "dc"}} {
		if _, err := h.p.Detail(ctx, c[0], c[1]); !errors.Is(err, ErrUnknownCell) {
			t.Errorf("Detail(%s, %s) = %v, want ErrUnknownCell", c[0], c[1], err)
		}
	}
}

func TestDetailReadError(t *testing.T) {
	h := newHarness(t, nil)
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial")
	h.poll(t)
	h.api.setReadErr(10101, &proxmox.APIError{Status: 500, Message: "got timeout"})
	d, err := h.p.Detail(ctx, "01", "dc")
	if err == nil || !strings.Contains(err.Error(), "team01-dc") || !strings.Contains(err.Error(), "got timeout") {
		t.Errorf("Detail with a failing read = %v, want an error naming the VM and the reason", err)
	}
	if d.Cell.Name != "team01-dc" || d.Config != nil {
		t.Errorf("detail = %+v, want the grid cell and no config", d)
	}
}
