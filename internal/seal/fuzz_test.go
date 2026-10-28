package seal

import "testing"

func FuzzOpen(f *testing.F) {
	k := NewKey("a long enough secret for tests", "job credential v1")
	f.Add("job:7", []byte("PVE:ticket"), "v1:")
	f.Add("", []byte{}, k.Seal("job:7", []byte("x")))
	f.Add("job:8", []byte{0, 0xff}, "v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	f.Fuzz(func(t *testing.T, binding string, plain []byte, junk string) { checkSealed(t, k, binding, plain, junk) })
}
