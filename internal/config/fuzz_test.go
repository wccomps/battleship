package config

import (
	"slices"
	"strings"
	"testing"
)

// FuzzParseSSHPublicKey explores keys for one whose canonical form carries
// anything but a key type and plain base64. Run with go test -fuzz
// FuzzParseSSHPublicKey ./internal/config.
func FuzzParseSSHPublicKey(f *testing.F) {
	for _, s := range []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl ca",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5", "ssh-rsa AAAAB3NzaC1yc2E= x\n'$(id)'", "ssh-ed25519  AAAA", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		canon, err := ParseSSHPublicKey(raw)
		if err != nil {
			return
		}
		kt, b64, ok := strings.Cut(canon, " ")
		if !ok || !slices.Contains(sshKeyTypes, kt) || b64 == "" || strings.Trim(b64, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=") != "" {
			t.Fatalf("ParseSSHPublicKey(%q) = %q", raw, canon)
		}
		if again, err := ParseSSHPublicKey(canon); err != nil || again != canon {
			t.Fatalf("ParseSSHPublicKey(%q) = %q, %v; not canonical", canon, again, err)
		}
	})
}
