package store

import (
	"encoding/json"
	"testing"
)

func TestStorableJSON(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{`{"a\u0000":1,"a�":2,"b":"\ud800"}`, `{"a�":1,"b":"�"}`}, // "a\x00" sorts first
		{`[1e400, 12345678901234567890, "\u0000"]`, `[1e400,12345678901234567890,"�"]`},
		{`"x"`, `"x"`},
		{`null`, `null`},
	} {
		got, err := storableJSON(json.RawMessage(c.raw))
		if err != nil || string(got) != c.want {
			t.Errorf("storableJSON(%s) = %s, %v; want %s", c.raw, got, err, c.want)
		}
	}
	for _, raw := range []string{`{} {}`, `{"a":1} x`, `{`, ``} {
		if got, err := storableJSON(json.RawMessage(raw)); err == nil {
			t.Errorf("storableJSON(%q) = %s, want an error", raw, got)
		}
	}
	if got, err := storableJSON(nil); got != nil || err != nil {
		t.Errorf("storableJSON(nil) = %s, %v; want nil", got, err)
	}
}
