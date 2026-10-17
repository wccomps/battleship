package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func runCLI(args ...string) (int, string) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, deps{stdout: &stdout, stderr: &stderr})
	return code, stdout.String() + stderr.String()
}

func TestEnvCreateParsesFlags(t *testing.T) {
	code, out := runCLI("env", "create", "-config", "/nonexistent.toml", "-domain", "kilo.alpha",
		"-router", "gw=opnsense", "-host", "teak=debian-12:10", "-host", "oak=win2022")
	if code != 1 || !strings.Contains(out, "nonexistent.toml") {
		t.Errorf("code %d, output %q; flags should parse and fail at loading the config", code, out)
	}
}

func TestEnvCreateNeedsRouter(t *testing.T) {
	if code, out := runCLI("env", "create", "-domain", "kilo.alpha"); code != 2 || !strings.Contains(out, "router") {
		t.Errorf("code %d, output %q", code, out)
	}
}

func TestImageRegisterNeedsVM(t *testing.T) {
	if code, out := runCLI("image", "register", "-family", "deb"); code != 2 || !strings.Contains(out, "VMID") {
		t.Errorf("code %d, output %q", code, out)
	}
}

// -slot takes the slot's number as well as its two-digit name.
func TestEnvCreateSlotFlag(t *testing.T) {
	for _, typed := range []string{"2", "02", " 2 "} {
		c, code, ok := parseEnv([]string{"create", "-domain", "kilo.alpha", "-router", "gw=gw", "-slot", typed}, io.Discard)
		if !ok || code != 0 || c.in.Slot != "02" {
			t.Errorf("-slot %q: slot %q (code %d, ok %v), want 02", typed, c.in.Slot, code, ok)
		}
	}
}
