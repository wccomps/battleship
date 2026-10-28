package config

import (
	"encoding/base64"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// A well-formed key reads as "<type> <base64>" whatever its comment and spacing,
// and anything accepted is canonical.
func TestPropParseSSHPublicKey(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		kt := rapid.SampledFrom(sshKeyTypes).Draw(t, "type")
		blob := binary.BigEndian.AppendUint32(nil, uint32(len(kt)))
		blob = append(append(blob, kt...), rapid.SliceOfN(rapid.Byte(), 0, 40).Draw(t, "key")...)
		b64 := base64.StdEncoding.EncodeToString(blob)
		space := rapid.SampledFrom([]string{" ", "  ", "\t"})
		comment := rapid.SampledFrom([]string{"", " ca", " ops@example", " a b"}).Draw(t, "comment")
		raw := rapid.SampledFrom([]string{"", " "}).Draw(t, "lead") + kt + space.Draw(t, "sep") + b64 + comment
		if got, err := ParseSSHPublicKey(raw); err != nil || got != kt+" "+b64 {
			t.Fatalf("ParseSSHPublicKey(%q) = %q, %v", raw, got, err)
		}
		// Before padding, the last character has spare bits: flipping one
		// decodes to the same key but isn't its canonical spelling.
		spare := b64
		if data := strings.TrimRight(b64, "="); len(data) < len(b64) {
			const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
			i := len(data) - 1
			spare = b64[:i] + string(alphabet[strings.IndexByte(alphabet, b64[i])^1]) + b64[i+1:]
		}
		spoilt := rapid.SampledFrom([]string{
			kt + " " + spare, strings.TrimRight(b64, "="), b64 + "=", kt + "x " + b64, raw + "\n'$(id)'", "\r" + raw,
		}).Draw(t, "spoilt")
		checkSSHKey(t, rapid.OneOf(rapid.Just(spoilt), rapid.String()).Draw(t, "raw"))
	})
}

func checkSSHKey(t interface{ Fatalf(string, ...any) }, raw string) {
	canon, err := ParseSSHPublicKey(raw)
	if err != nil {
		return
	}
	kt, b64, ok := strings.Cut(canon, " ")
	b, err := base64.StdEncoding.DecodeString(b64)
	if !ok || !slices.Contains(sshKeyTypes, kt) || err != nil || len(b) == 0 || base64.StdEncoding.EncodeToString(b) != b64 {
		t.Fatalf("ParseSSHPublicKey(%q) = %q", raw, canon)
	}
	if again, err := ParseSSHPublicKey(canon); err != nil || again != canon {
		t.Fatalf("ParseSSHPublicKey(%q) = %q, %v; not canonical", canon, again, err)
	}
}
