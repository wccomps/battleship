package seal

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
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

// A key never prints its bytes, however it is formatted.
func TestKeyIsRedacted(t *testing.T) {
	k := NewKey("a long enough secret for tests", "job credential v1")
	raw := string(k.key)
	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %s %q %x", k, k, k, k, k, k), fmt.Sprintf("%v", &k)} {
		if strings.Contains(s, raw) || strings.Contains(s, fmt.Sprintf("%x", k.key)) || strings.Contains(s, fmt.Sprint(k.key)) {
			t.Fatalf("%q shows the key", s)
		}
	}
}

// Open refuses, without panicking, anything but a box sealed for the
// binding: junk, short or cut boxes, and a box with one character changed.
func TestPropOpenRefusesAllButTheBox(t *testing.T) {
	k := NewKey("a long enough secret for tests", "job credential v1")
	rapid.Check(t, func(t *rapid.T) {
		box := k.Seal("job:7", []byte(rapid.String().Draw(t, "plain")))
		var other string
		switch rapid.IntRange(0, 3).Draw(t, "kind") {
		case 0:
			other = rapid.SampledFrom([]string{"", "v1:", "v2:", "v1"}).Draw(t, "prefix") + rapid.String().Draw(t, "junk")
		case 1:
			other = "v1:" + base64.RawURLEncoding.EncodeToString(rapid.SliceOfN(rapid.Byte(), 0, 40).Draw(t, "raw"))
		case 2:
			other = box[:rapid.IntRange(0, len(box)-1).Draw(t, "cut")]
		case 3:
			i := rapid.IntRange(0, len(box)-1).Draw(t, "at")
			alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_=+/:"
			c := rapid.SampledFrom(strings.Split(alphabet, "")).Filter(func(c string) bool { return c != box[i:i+1] }).Draw(t, "char")
			other = box[:i] + c + box[i+1:]
		}
		if _, err := k.Open("job:7", other); err == nil {
			t.Fatalf("Open(%q) succeeded; sealed %q", other, box)
		}
	})
}

// Every other last character of a box is refused, including the ones that
// decode to the same bytes without Strict, and so are line breaks.
func TestOpenHasOneSpellingPerBox(t *testing.T) {
	k := NewKey("a long enough secret for tests", "job credential v1")
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for n := 0; n < 6; n++ { // each length leaves other spare bits in the last character
		box := k.Seal("job:7", make([]byte, n))
		for _, c := range alphabet {
			other := box[:len(box)-1] + string(c)
			_, err := k.Open("job:7", other)
			if (err == nil) != (other == box) {
				t.Errorf("plain %d bytes: Open(...%c) err = %v, original ends %c", n, c, err, box[len(box)-1])
			}
		}
		for _, lb := range []string{"\n", "\r\n", "\r"} {
			for _, other := range []string{box + lb, box[:5] + lb + box[5:], lb + box} {
				if _, err := k.Open("job:7", other); err == nil {
					t.Errorf("Open(%q) accepted a line break", other)
				}
			}
		}
	}
}
