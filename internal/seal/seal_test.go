package seal

import (
	"fmt"
	"strings"
	"testing"
)

func TestSealOpensWithTheSameKeyAndBinding(t *testing.T) {
	k := NewKey("a long enough secret for tests", "job credential v1")
	box := k.Seal("job:7", []byte("PVE:ticket"))
	if strings.Contains(box, "PVE:ticket") || !strings.HasPrefix(box, "v1:") {
		t.Fatalf("box %q", box)
	}
	got, err := k.Open("job:7", box)
	if err != nil || string(got) != "PVE:ticket" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestSealRefusesOtherBindingsKeysAndJunk(t *testing.T) {
	k := NewKey("a long enough secret for tests", "job credential v1")
	box := k.Seal("job:7", []byte("PVE:ticket"))
	if _, err := k.Open("job:8", box); err == nil {
		t.Error("a box opened for another job")
	}
	if _, err := NewKey("another secret entirely", "job credential v1").Open("job:7", box); err == nil {
		t.Error("a box opened with another secret")
	}
	if _, err := NewKey("a long enough secret for tests", "session ticket v1").Open("job:7", box); err == nil {
		t.Error("a box opened for another purpose")
	}
	for _, junk := range []string{"", "v1:", "v1:!!", "PVE:ticket", strings.TrimPrefix(box, "v1:"), box[:len(box)-2]} {
		if _, err := k.Open("job:7", junk); err == nil {
			t.Errorf("junk %q opened", junk)
		}
	}
}

func TestSealIsRandomized(t *testing.T) {
	k := NewKey("s", "p")
	if k.Seal("a", []byte("x")) == k.Seal("a", []byte("x")) {
		t.Fatal("two seals of one value are equal")
	}
}

// Item 15: a key never prints its bytes, however it is formatted.
func TestKeyIsRedacted(t *testing.T) {
	k := NewKey("a long enough secret for tests", "job credential v1")
	raw := string(k.key)
	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %s %q %x", k, k, k, k, k, k), fmt.Sprintf("%v", &k)} {
		if strings.Contains(s, raw) || strings.Contains(s, fmt.Sprintf("%x", k.key)) || strings.Contains(s, fmt.Sprint(k.key)) {
			t.Fatalf("%q shows the key", s)
		}
	}
}
