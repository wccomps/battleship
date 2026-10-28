package config

import "testing"

// FuzzParseSSHPublicKey explores keys for one whose canonical form carries
// anything but a key type and plain base64.
func FuzzParseSSHPublicKey(f *testing.F) {
	for _, s := range []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl ca",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5", "ssh-rsa AAAAB3NzaC1yc2E= x\n'$(id)'", "ssh-ed25519  AAAA", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) { checkSSHKey(t, raw) })
}
