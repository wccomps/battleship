package pods

import (
	"encoding/base64"
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
	if strings.Join(cmd, " ") != "/bin/sh -s" {
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

// Two copies of a Linux script never run at once: the second waits for the
// first's lock (one a stopped job left running), then runs.
func TestLinuxScriptsTakeTurns(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	body := linuxLock + "\necho start >> \"$LOG\"; sleep 0.3; echo end >> \"$LOG\"\n"
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			c := exec.Command(sh, "-s")
			c.Env = append(os.Environ(), "BATTLESHIP_LOCK="+filepath.Join(dir, "lock"), "LOG="+log)
			c.Stdin = strings.NewReader(body)
			if out, err := c.CombinedOutput(); err != nil {
				t.Errorf("script %d: %v\n%s", i, err, out)
			}
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(log)
	if got := strings.Fields(string(b)); !slices.Equal(got, []string{"start", "end", "start", "end"}) {
		t.Errorf("log = %q, want the two runs one after the other", got)
	}
}
