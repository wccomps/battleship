package pods

import (
	"encoding/base64"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/guest from the current scripts")

var testGuest = Guest{Hostname: "teak", Domain: "kilo.alpha", IP: "192.168.100.10", Prefix: 24, Gateway: "192.168.100.2", DNS: "192.168.100.2"}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "guest", name)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file; run go test ./internal/pods -run Guest -update-golden and review the diff", name)
	}
}

func TestLinuxGuestScript(t *testing.T) {
	for name, dnsOnly := range map[string]bool{"linux.sh": false, "linux-dns.sh": true} {
		g := testGuest
		g.OS, g.DNSOnly = ImageLinux, dnsOnly
		linuxGuestScript(t, name, g)
	}
}

func linuxGuestScript(t *testing.T, name string, g Guest) {
	cmd, input, err := GuestCommand(g, "bc:24:11:00:00:01")
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" || cmd[2] != linuxLocked {
		t.Errorf("command = %q", cmd)
	}
	golden(t, name, input)
	if sh, err := exec.LookPath("sh"); err == nil {
		c := exec.Command(sh, "-n")
		c.Stdin = strings.NewReader(input)
		if out, err := c.CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v\n%s", err, out)
		}
	}
}

func TestWindowsGuestScript(t *testing.T) {
	for name, dnsOnly := range map[string]bool{"windows.ps1": false, "windows-dns.ps1": true} {
		g := testGuest
		g.OS, g.DNSOnly = ImageWindows, dnsOnly
		windowsGuestScript(t, name, g)
	}
}

func windowsGuestScript(t *testing.T, name string, g Guest) {
	cmd, input, err := GuestCommand(g, "bc:24:11:00:00:01")
	if err != nil {
		t.Fatal(err)
	}
	if input != "" || len(cmd) != 7 || cmd[0] != "powershell.exe" || cmd[5] != "-EncodedCommand" {
		t.Fatalf("command = %q, input %q", cmd, input)
	}
	raw, err := base64.StdEncoding.DecodeString(cmd[6])
	if err != nil || len(raw)%2 != 0 {
		t.Fatalf("not base64 UTF-16: %v", err)
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	golden(t, name, string(utf16.Decode(u)))
}

func TestNeedsReboot(t *testing.T) {
	if !NeedsReboot("x\n"+RebootMarker+"\n") || NeedsReboot("done") {
		t.Error("NeedsReboot")
	}
}

// Values reach scripts run as root or SYSTEM; PowerShell also ends a
// single-quoted string at a curly quote.
func TestGuestCommandRefusesBadValues(t *testing.T) {
	bad := map[string]func(g *Guest){
		"quote in IP":       func(g *Guest) { g.IP = "192.168.100.10'; reboot; '" },
		"curly quote in IP": func(g *Guest) { g.IP = "192.168.100.10\u2019" },
		"gateway":           func(g *Guest) { g.Gateway = "192.168.100.2 x" },
		"DNS":               func(g *Guest) { g.DNS = "" },
		"hostname":          func(g *Guest) { g.Hostname = "te'ak" },
		"domain":            func(g *Guest) { g.Domain = "kilo.al\u2018pha" },
		"prefix":            func(g *Guest) { g.Prefix = 0 },
		"OS":                func(g *Guest) { g.OS = "bsd" },
	}
	for name, mut := range bad {
		for _, os := range []string{ImageLinux, ImageWindows} {
			g := testGuest
			g.OS = os
			mut(&g)
			if _, _, err := GuestCommand(g, "bc:24:11:00:00:01"); err == nil {
				t.Errorf("%s (%s): no error", name, os)
			}
		}
	}
	g := testGuest
	g.OS = ImageWindows
	if _, _, err := GuestCommand(g, "bc:24:11:00:00:0'"); err == nil {
		t.Error("bad MAC: no error")
	}
}

// Set DNS runs on configured hosts: its scripts must not touch the address,
// routes, other network files or the host name.
func TestDNSOnlyScriptsLeaveTheRestAlone(t *testing.T) {
	for _, os := range []string{ImageLinux, ImageWindows} {
		g := testGuest
		g.OS, g.DNSOnly = os, true
		script, err := GuestScript(g, "bc:24:11:00:00:01")
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{g.IP, "Remove-Net", "New-Net", "Rename-Computer", "Dhcp", "battleship-off", "ifdown", "ifup",
			"ipv4.addresses", "ipv4.method", "hostname", "cloud.cfg.d", "addresses: [$IP"} {
			if strings.Contains(script, bad) {
				t.Errorf("%s DNS-only script contains %q", os, bad)
			}
		}
		if !strings.Contains(script, g.DNS) || !strings.Contains(script, g.Domain) {
			t.Errorf("%s DNS-only script lacks the server or domain", os)
		}
	}
}

// lockTools are the guest commands lockedShell needs, or skip.
func lockTools(t *testing.T) (sh string) {
	t.Helper()
	for _, tool := range []string{"sh", "flock", "timeout"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	sh, _ = exec.LookPath("sh")
	return sh
}

// runLocked runs body as the agent would, under lockedShell(lock, wait).
func runLocked(t *testing.T, sh, lock string, wait time.Duration, env []string, body string) ([]byte, error) {
	c := exec.Command(sh, "-c", lockedShell(lock, wait))
	c.Env = append(os.Environ(), env...)
	c.Stdin = strings.NewReader(body)
	return c.CombinedOutput()
}

// Two copies of a Linux script never run at once: the second waits for the
// first's lock (one a stopped job left running), then runs. A daemon the
// first starts doesn't keep the lock.
func TestLinuxScriptsTakeTurns(t *testing.T) {
	sh := lockTools(t)
	dir := t.TempDir()
	lock, log := filepath.Join(dir, "lock"), filepath.Join(dir, "log")
	body := "echo start >> \"$LOG\"; sleep 0.3; echo end >> \"$LOG\"; (sleep 5 </dev/null >/dev/null 2>&1 &)\n"
	run := func(i int) {
		if out, err := runLocked(t, sh, lock, 10*time.Second, []string{"LOG=" + log}, body); err != nil {
			t.Errorf("script %d: %v\n%s", i, err, out)
		}
	}
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			run(i)
		}()
	}
	wg.Wait()
	start := time.Now()
	run(2) // the first two left sleeps running: they must not hold the lock
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a third script waited %v behind the first two's background process", d)
	}
	b, _ := os.ReadFile(log)
	if got := strings.Fields(string(b)); !slices.Equal(got, []string{"start", "end", "start", "end", "start", "end"}) {
		t.Errorf("log = %q, want the runs one after the other", got)
	}
}

// A script that can't get the lock in time exits 75 and says why.
func TestLinuxScriptGivesUpOnAHeldLock(t *testing.T) {
	sh := lockTools(t)
	flock, _ := exec.LookPath("flock")
	lock := filepath.Join(t.TempDir(), "lock")
	holder := exec.Command(flock, lock, "sleep", "3")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer holder.Wait() //nolint:errcheck
	time.Sleep(200 * time.Millisecond)
	out, err := runLocked(t, sh, lock, time.Second, nil, "echo ran\n")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 75 || !strings.Contains(string(out), "still running") || strings.Contains(string(out), "ran\n") {
		t.Errorf("err = %v, output %q; want exit 75 saying another script is still running", err, out)
	}
}

// Without flock or a timeout that takes bare seconds, the script still
// runs, unlocked.
func TestLinuxScriptRunsWithoutTheLockTools(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	const works = "#!/bin/sh\nexit 0\n"
	// Busybox before 1.30 takes the time only as -t SECS, and runs a bare
	// number as the command.
	const oldBusybox = "#!/bin/sh\n[ \"$1\" = -t ] || { echo \"timeout: can't execute '$1'\" >&2; exit 127; }\nexit 0\n"
	for name, tools := range map[string]map[string]string{
		"neither":             nil,
		"no timeout":          {"flock": works},
		"no flock":            {"timeout": works},
		"old busybox timeout": {"flock": works, "timeout": oldBusybox},
	} {
		t.Run(name, func(t *testing.T) {
			bin := t.TempDir()
			for tool, script := range tools {
				if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			c := exec.Command(sh, "-c", lockedShell(filepath.Join(t.TempDir(), "lock"), time.Second))
			c.Env = []string{"PATH=" + bin}
			c.Stdin = strings.NewReader("echo ran\n")
			out, err := c.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "ran" {
				t.Errorf("err = %v, output %q; want the script to run", err, out)
			}
		})
	}
}

// The shipped wrapper is lockedShell with the real lock and wait.
func TestLinuxLockedIsTheRealLock(t *testing.T) {
	if linuxLocked != lockedShell("/run/battleship-guest.lock", GuestLockWait) || !strings.Contains(linuxLocked, "timeout 240 flock 9") {
		t.Errorf("linuxLocked = %q", linuxLocked)
	}
}
