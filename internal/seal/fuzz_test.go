package seal

import "testing"

// FuzzOpen explores stored values for one Open accepts that wasn't sealed
// for its binding, or one that panics. Run with go test -fuzz FuzzOpen
// ./internal/seal.
func FuzzOpen(f *testing.F) {
	k := NewKey("a long enough secret for tests", "job credential v1")
	f.Add("job:7", []byte("PVE:ticket"), "v1:")
	f.Add("", []byte{}, k.Seal("job:7", []byte("x")))
	f.Add("job:8", []byte{0, 0xff}, "v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	f.Fuzz(func(t *testing.T, binding string, plain []byte, junk string) {
		box := k.Seal(binding, plain)
		if got, err := k.Open(binding, box); err != nil || string(got) != string(plain) {
			t.Fatalf("Open(Seal(%q)) = %q, %v", plain, got, err)
		}
		if _, err := k.Open(binding+"x", box); err == nil {
			t.Fatalf("a box sealed for %q opened for %q", binding, binding+"x")
		}
		if _, err := k.Open(binding, junk); err == nil && junk != box {
			t.Fatalf("Open(%q, %q) accepted a value it never sealed", binding, junk)
		}
	})
}
