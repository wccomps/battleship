package status

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// driftCluster has one clean VM and one VM per kind of scan drift.
func driftCluster(h *harness) {
	h.api.add(teamVM("01", "dc", 10101), cleanConfig("01"), "initial", "pre-inject")
	web := cleanConfig("01")
	web["net1"] = "virtio=BC:24:11:00:00:02,bridge=int02" // wired to team 02
	h.api.add(teamVM("01", "web", 10102), web, "initial")
	dc2 := cleanConfig("02")
	dc2["scsi0"] = "competitions:base-9005-disk-0/vm-2-disk-0,mbps_rd=100,mbps_wr=300,size=32G"
	h.api.add(teamVM("02", "dc", 10201), dc2, "initial")
	h.api.add(teamVM("02", "web", 10202), cleanConfig("02")) // no baseline snapshot
}

func TestScanFindsDrift(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.poll(t)
	for _, c := range []string{"01/dc", "01/web", "02/dc", "02/web"} {
		team, host, _ := strings.Cut(c, "/")
		if got := cell(t, h.p.Grid(), team, host); got.State != StateRunning {
			t.Errorf("%s before any scan = %s, want running", c, got.State)
		}
	}
	h.clock.Advance(time.Second)
	sub, cancel := h.hub.SubscribeTopics(TopicGrid)
	defer cancel()
	h.scan(t)
	g := h.p.Grid()
	recv(t, sub)
	if !g.ScannedAt.Equal(h.clock.Now()) || g.ScanErr != "" {
		t.Errorf("ScannedAt %v ScanErr %q, want now and no error", g.ScannedAt, g.ScanErr)
	}
	want := map[string][]Drift{
		"01/dc":  nil,
		"01/web": {{Kind: DriftNetwork, Reason: "network differs from team 01's: net1 should be virtio=BC:24:11:00:00:02,bridge=int01"}},
		"02/dc":  {{Kind: DriftDiskLimits, Reason: "disk limits differ on scsi0 (want 300 MB/s read and 300 MB/s write)"}},
		"02/web": {{Kind: DriftSnapshot, Reason: `no "initial" or fresh_clone_* baseline snapshot`}},
	}
	for key, drift := range want {
		team, host, _ := strings.Cut(key, "/")
		c := cell(t, g, team, host)
		if !reflect.DeepEqual(c.Drift, drift) {
			t.Errorf("%s drift = %+v, want %+v", key, c.Drift, drift)
		}
		wantState := StateRunning
		if drift != nil {
			wantState = StateDrifted
		}
		if c.State != wantState {
			t.Errorf("%s state = %s, want %s", key, c.State, wantState)
		}
	}
	if reads, _, _ := h.api.counts(); reads != 8 {
		t.Errorf("scan made %d reads, want 8 (config and snapshots of 4 VMs)", reads)
	}
}

func TestScanDriftLastsUntilNextScan(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.poll(t)
	h.scan(t)
	first := h.p.Grid().ScannedAt

	// Fixed in Proxmox: the grid still shows the drift until the next scan.
	h.api.setConfig(10102, "net1", "virtio=BC:24:11:00:00:02,bridge=int01")
	h.api.setSnaps(10202, "initial")
	h.clock.Advance(5 * time.Second)
	h.poll(t)
	if c := cell(t, h.p.Grid(), "01", "web"); c.State != StateDrifted {
		t.Errorf("01/web after a poll = %s, want still drifted until the next scan", c.State)
	}
	h.clock.Advance(2 * time.Minute)
	h.scan(t)
	g := h.p.Grid()
	for _, c := range []Cell{cell(t, g, "01", "web"), cell(t, g, "02", "web")} {
		if c.State != StateRunning || c.Drift != nil {
			t.Errorf("%s after the next scan = %s %+v, want running without drift", c.Name, c.State, c.Drift)
		}
	}
	if c := cell(t, g, "02", "dc"); c.State != StateDrifted {
		t.Errorf("02/dc = %s, want still drifted", c.State)
	}
	if !g.ScannedAt.Equal(h.clock.Now()) || !g.ScannedAt.After(first) {
		t.Errorf("ScannedAt = %v, want the second scan's finish %v", g.ScannedAt, h.clock.Now())
	}
}

func TestScanDriftSupersededByLaterJob(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.poll(t)
	h.scan(t)
	scanned := h.clock.Now()

	// A power job doesn't touch config: the network drift stays.
	h.clock.Advance(10 * time.Second)
	h.hist.set("team01-web", store.ItemResult{JobID: 3, JobKind: "power", FinishedAt: h.clock.Now(), Status: store.ItemDone, Step: "power"})
	h.poll(t)
	if c := cell(t, h.p.Grid(), "01", "web"); !reflect.DeepEqual(kinds(c), []DriftKind{DriftNetwork}) {
		t.Errorf("01/web after a power job: drift %v, want network", kinds(c))
	}
	// A deploy that finished after the scan read the VM re-converged it.
	h.hist.set("team01-web", store.ItemResult{JobID: 4, JobKind: "deploy", FinishedAt: h.clock.Now(), Status: store.ItemDone, Step: "start"})
	// A reset that finished before the scan read the VM changes nothing.
	h.hist.set("team02-dc", store.ItemResult{JobID: 2, JobKind: "reset", FinishedAt: scanned.Add(-time.Minute), Status: store.ItemDone, Step: "start"})
	h.poll(t)
	g := h.p.Grid()
	if c := cell(t, g, "01", "web"); c.State != StateRunning || c.Drift != nil {
		t.Errorf("01/web after a later deploy = %s %+v, want running", c.State, c.Drift)
	}
	if c := cell(t, g, "02", "dc"); !reflect.DeepEqual(kinds(c), []DriftKind{DriftDiskLimits}) {
		t.Errorf("02/dc after an earlier reset: drift %v, want disk-limits", kinds(c))
	}
}

// The scan's read times are compared with job finish times in the
// database's clock, whatever the skew between the two hosts.
func TestScanComparesInDatabaseTime(t *testing.T) {
	for _, tc := range []struct {
		name       string
		skew       time.Duration // database clock minus the poller's
		finished   time.Duration // the deploy's finish, from the poller's clock at the scan
		superseded bool
	}{
		// The database is 5 minutes ahead: the deploy finished (database
		// time) before the scan read the VM, though after the poller's clock.
		{"database ahead", 5 * time.Minute, time.Minute, false},
		// The database is 5 minutes behind: the deploy finished after the
		// read, though before the poller's clock.
		{"database behind", -5 * time.Minute, -4 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			driftCluster(h)
			h.hist.setSkew(tc.skew)
			h.poll(t)
			scanAt := h.clock.Now()
			h.scan(t)
			h.hist.set("team01-web", store.ItemResult{JobID: 4, JobKind: "deploy", FinishedAt: scanAt.Add(tc.finished), Status: store.ItemDone, Step: "start"})
			h.poll(t)
			c := cell(t, h.p.Grid(), "01", "web")
			if got := len(c.Drift) == 0; got != tc.superseded {
				t.Errorf("01/web drift %v; want superseded %v", kinds(c), tc.superseded)
			}
		})
	}
}

// A cell read uses the offset the last scan measured.
func TestDetailComparesInDatabaseTime(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.hist.setSkew(5 * time.Minute)
	h.poll(t)
	h.scan(t)
	h.clock.Advance(time.Minute)
	readAt := h.clock.Now()
	if _, err := h.p.Detail(ctx, "01", "web"); err != nil {
		t.Fatal(err)
	}
	// Finished 2 minutes after the read by the poller's clock, which is 3
	// minutes before it by the database's.
	h.hist.set("team01-web", store.ItemResult{JobID: 4, JobKind: "deploy", FinishedAt: readAt.Add(2 * time.Minute), Status: store.ItemDone, Step: "start"})
	h.poll(t)
	if c := cell(t, h.p.Grid(), "01", "web"); !reflect.DeepEqual(kinds(c), []DriftKind{DriftNetwork}) {
		t.Errorf("01/web drift %v, want network: the deploy finished before the read", kinds(c))
	}
}

// A scan whose database clock read fails keeps the last offset.
func TestScanKeepsOffsetWhenClockReadFails(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.hist.setSkew(5 * time.Minute)
	h.poll(t)
	h.scan(t)
	h.hist.mu.Lock()
	h.hist.nowErr = errors.New("connection refused")
	h.hist.mu.Unlock()
	h.clock.Advance(time.Minute)
	scanAt := h.clock.Now()
	h.scan(t)
	h.hist.set("team01-web", store.ItemResult{JobID: 4, JobKind: "deploy", FinishedAt: scanAt.Add(time.Minute), Status: store.ItemDone, Step: "start"})
	h.poll(t)
	if c := cell(t, h.p.Grid(), "01", "web"); !reflect.DeepEqual(kinds(c), []DriftKind{DriftNetwork}) {
		t.Errorf("01/web drift %v, want network: the deploy finished before the read", kinds(c))
	}
}

// A scan whose database clock read hangs gives up on it after
// dbClockTimeout and goes on with the last offset.
func TestScanClockReadTimesOut(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.hist.setSkew(5 * time.Minute)
	h.poll(t)
	h.scan(t)
	h.p.dbClockTimeout = 50 * time.Millisecond
	h.hist.mu.Lock()
	h.hist.nowHang = true
	h.hist.mu.Unlock()
	h.clock.Advance(time.Minute)
	scanAt := h.clock.Now()
	done := make(chan error, 1)
	go func() { done <- h.p.Scan(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the scan hung on the database clock read")
	}
	if g := h.p.Grid(); !g.ScannedAt.Equal(scanAt) {
		t.Errorf("ScannedAt = %v, want %v", g.ScannedAt, scanAt)
	}
	// The last offset (5 minutes) still dates the scan.
	h.hist.set("team01-web", store.ItemResult{JobID: 4, JobKind: "deploy", FinishedAt: scanAt.Add(time.Minute), Status: store.ItemDone, Step: "start"})
	h.poll(t)
	if c := cell(t, h.p.Grid(), "01", "web"); !reflect.DeepEqual(kinds(c), []DriftKind{DriftNetwork}) {
		t.Errorf("01/web drift %v, want network: the deploy finished before the read", kinds(c))
	}
}

// A one-NIC VM belongs on the team's internal bridge, and a cloud-init VM
// also needs the external IP, nameserver and vendor snippet.
func TestScanOneNICAndCloudInitVMs(t *testing.T) {
	h := newHarness(t, nil)
	oneNIC := func(bridge string) map[string]string {
		c := cleanConfig("01")
		delete(c, "net1")
		c["net0"] = "virtio=BC:24:11:00:00:01,bridge=" + bridge
		return c
	}
	cloudInit := func(ip string) map[string]string {
		c := cleanConfig("02")
		c["ide2"] = "competitions:vm-1-cloudinit,media=cdrom"
		c["ipconfig0"] = ip
		c["nameserver"] = "10.50.102.1"
		c["cicustom"] = "vendor=competitions:snippets/ssh-keys.yaml"
		return c
	}
	h.api.add(teamVM("01", "dc", 10101), oneNIC("int01"), "initial")
	h.api.add(teamVM("01", "web", 10102), oneNIC("ext01"), "initial")
	h.api.add(teamVM("02", "dc", 10201), cloudInit("ip=10.50.102.2/30,gw=10.50.102.1"), "initial")
	h.api.add(teamVM("02", "web", 10202), cloudInit("ip=10.50.101.2/30,gw=10.50.101.1"), "initial")
	h.poll(t)
	h.scan(t)
	g := h.p.Grid()
	want := map[string][]Drift{
		"01/dc":  nil,
		"01/web": {{Kind: DriftNetwork, Reason: "network differs from team 01's: net0 should be virtio=BC:24:11:00:00:01,bridge=int01"}},
		"02/dc":  nil,
		"02/web": {{Kind: DriftNetwork, Reason: "network differs from team 02's: ipconfig0 should be ip=10.50.102.2/30,gw=10.50.102.1"}},
	}
	for key, drift := range want {
		team, host, _ := strings.Cut(key, "/")
		c := cell(t, g, team, host)
		if !reflect.DeepEqual(c.Drift, drift) {
			t.Errorf("%s drift = %+v, want %+v", key, c.Drift, drift)
		}
	}
}

func TestScanResultIsForThatVM(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.poll(t)
	h.scan(t)
	// team01-web is torn down and built again under another VMID.
	h.api.remove(10102)
	h.api.add(proxmox.VM{VMID: 10199, Name: "team01-web", Node: "n1", Status: "running", Pool: "pool-01"}, cleanConfig("01"), "initial")
	h.poll(t)
	if c := cell(t, h.p.Grid(), "01", "web"); c.VMID != 10199 || c.State != StateRunning {
		t.Errorf("rebuilt 01/web = %d %s %+v, want 10199 running without the old VM's drift", c.VMID, c.State, c.Drift)
	}
}

func TestScanKeepsResultWhenReadFails(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.poll(t)
	h.scan(t)
	h.api.setReadErr(10102, &proxmox.APIError{Status: 500, Message: "got timeout"})
	h.api.setReadErr(10201, &proxmox.APIError{Status: 500, Message: "got timeout"})
	h.api.setSnaps(10202, "initial")
	h.clock.Advance(2 * time.Minute)
	h.scan(t)
	g := h.p.Grid()
	if c := cell(t, g, "01", "web"); c.State != StateDrifted {
		t.Errorf("01/web whose read failed = %s, want its last drift kept", c.State)
	}
	if c := cell(t, g, "02", "web"); c.State != StateRunning {
		t.Errorf("02/web = %s, want running (read fine, fixed)", c.State)
	}
	if !strings.Contains(g.ScanErr, "2 of 4 VMs could not be read") || !strings.Contains(g.ScanErr, "got timeout") {
		t.Errorf("ScanErr = %q", g.ScanErr)
	}
	h.api.setReadErr(10102, nil)
	h.api.setReadErr(10201, nil)
	h.api.setConfig(10102, "net1", "virtio=BC:24:11:00:00:02,bridge=int01")
	h.scan(t)
	g = h.p.Grid()
	if c := cell(t, g, "01", "web"); c.State != StateRunning || g.ScanErr != "" {
		t.Errorf("after a good scan: 01/web %s, ScanErr %q", c.State, g.ScanErr)
	}
}

func TestScanNeedsAPoll(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.p.Scan(ctx); err == nil {
		t.Error("Scan before any poll = nil, want an error")
	}
}

func TestScanCancelledKeepsOldResults(t *testing.T) {
	h := newHarness(t, nil)
	driftCluster(h)
	h.poll(t)
	h.scan(t)
	before := h.p.Grid()
	h.api.setConfig(10102, "net1", "virtio=BC:24:11:00:00:02,bridge=int01")
	h.api.gate = make(chan struct{})
	h.api.entered = make(chan int, 16)
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- h.p.Scan(sctx) }()
	<-h.api.entered
	cancel()
	if err := <-done; err == nil {
		t.Error("cancelled Scan = nil, want the context error")
	}
	if g := h.p.Grid(); !g.ScannedAt.Equal(before.ScannedAt) || cell(t, g, "01", "web").State != StateDrifted {
		t.Errorf("a cancelled scan changed the grid: scanned %v, 01/web %s", g.ScannedAt, cell(t, g, "01", "web").State)
	}
}

// scanCluster adds teams × hosts clean VMs.
func scanCluster(h *harness, teams, hosts int) {
	for t := 1; t <= teams; t++ {
		team := fmt.Sprintf("%02d", t)
		for i := 0; i < hosts; i++ {
			h.api.add(teamVM(team, fmt.Sprintf("h%d", i), 10000+t*100+i), cleanConfig(team), "initial")
		}
	}
}

// gatedScan holds scan reads until want are in flight, releases them, and
// returns the peak in-flight count.
func gatedScan(t *testing.T, h *harness, want int) int {
	t.Helper()
	h.api.gate = make(chan struct{})
	h.api.entered = make(chan int, 1000)
	done := make(chan error, 1)
	go func() { done <- h.p.Scan(ctx) }()
	for i := 0; i < want; i++ {
		<-h.api.entered
	}
	close(h.api.gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, _, max := h.api.counts()
	return max
}

func TestScanWorkersDefaultToHalfTheCallCap(t *testing.T) {
	for calls, want := range map[int]int{1: 1, 2: 1, 5: 3, 8: 4} {
		h := newHarness(t, func(c *config.Config, _ *Options) { c.Concurrency.ConfigCalls = calls })
		if h.p.scanWorkers != want {
			t.Errorf("config_calls %d: %d scan workers, want %d", calls, h.p.scanWorkers, want)
		}
	}
}

func TestScanTakesOneSlotPerWorker(t *testing.T) {
	h := newHarness(t, nil) // config_calls 5: 3 scan workers
	scanCluster(h, 3, 4)
	h.poll(t)
	if max := gatedScan(t, h, 3); max != 3 {
		t.Errorf("%d reads in flight at once, want 3 (one per scan worker)", max)
	}
	if reads, _, _ := h.api.counts(); reads != 24 {
		t.Errorf("reads = %d, want 24", reads)
	}
}

func TestScanSharesTheJobsCallCap(t *testing.T) {
	h := newHarness(t, nil) // config_calls 5: 3 scan workers
	scanCluster(h, 3, 4)
	h.poll(t)
	// A job holds 4 of the 5 slots for the whole scan.
	release := make(chan struct{})
	var held, jobs sync.WaitGroup
	for i := 0; i < 4; i++ {
		held.Add(1)
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			h.lim.Call(context.Background(), func() error { held.Done(); <-release; return nil }) //nolint:errcheck
		}()
	}
	held.Wait()
	if max := gatedScan(t, h, 1); max != 1 {
		t.Errorf("%d scan reads in flight while jobs held 4 of 5 slots, want 1", max)
	}
	close(release)
	jobs.Wait()
}

func TestFullScanAtCompetitionScale(t *testing.T) {
	// 32 teams × 10 hosts is 640 reads. With config_calls 5 the scan runs 3 at
	// a time, so at 100ms a read it takes about 21s of the 2m cadence.
	h := newHarness(t, nil)
	scanCluster(h, 32, 10)
	h.poll(t)
	h.scan(t)
	reads, _, max := h.api.counts()
	if reads != 640 || max > 3 {
		t.Errorf("reads %d, most in flight %d; want 640 and at most 3", reads, max)
	}
	g := h.p.Grid()
	if len(g.Rows) != 32 || len(g.Hosts) != 10 {
		t.Errorf("grid is %d × %d, want 32 × 10", len(g.Rows), len(g.Hosts))
	}
}

// The scan, the task follower and cell details read VMs concurrently, so
// whichever read started last stands, whatever order they are recorded in.
func TestScanResultsKeepTheLatestRead(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := scanResults{}
	s.put("team01-web", scanResult{vmid: 10102, readAt: t0.Add(time.Minute)})
	s.put("team01-web", scanResult{vmid: 10102, readAt: t0})
	if got := s["team01-web"].readAt; !got.Equal(t0.Add(time.Minute)) {
		t.Errorf("an earlier read replaced a later one: readAt %v", got)
	}
	s.put("team01-web", scanResult{vmid: 10199, readAt: t0.Add(time.Minute)})
	if got := s["team01-web"].vmid; got != 10199 {
		t.Errorf("a read as late as the recorded one = VMID %d, want it recorded (10199)", got)
	}
}
