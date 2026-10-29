package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
)

// storable makes s fit a PostgreSQL text column: each NUL, and each run of
// invalid UTF-8, becomes U+FFFD.
func storable(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\ufffd"), "\x00", "\ufffd")
}

// storableJSON is raw, one JSON value, re-encoded with storable on every
// string and key, as jsonb refuses \u0000 and lone surrogate escapes. Of keys
// that become equal, the first in sorted order is kept.
func storableJSON(raw json.RawMessage) (json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("JSON has data after its value")
	}
	return json.Marshal(storableValue(v))
}

func storableValue(v any) any {
	switch v := v.(type) {
	case string:
		return storable(v)
	case []any:
		for i, e := range v {
			v[i] = storableValue(e)
		}
	case map[string]any:
		out := make(map[string]any, len(v))
		for _, k := range slices.Sorted(maps.Keys(v)) {
			sk := storable(k)
			if _, ok := out[sk]; !ok {
				out[sk] = storableValue(v[k])
			}
		}
		return out
	}
	return v
}
