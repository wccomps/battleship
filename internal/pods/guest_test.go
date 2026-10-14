package pods

import (
	"encoding/base64"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	g := testGuest
	g.OS = ImageLinux
	cmd, input, err := GuestCommand(g, "bc:24:11:00:00:01")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cmd, " ") != "/bin/sh -s" {
		t.Errorf("command = %q", cmd)
	}
	golden(t, "linux.sh", input)
	if sh, err := exec.LookPath("sh"); err == nil {
		c := exec.Command(sh, "-n")
		c.Stdin = strings.NewReader(input)
		if out, err := c.CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v\n%s", err, out)
		}
	}
}

func TestWindowsGuestScript(t *testing.T) {
	g := testGuest
	g.OS = ImageWindows
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
	golden(t, "windows.ps1", string(utf16.Decode(u)))
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
