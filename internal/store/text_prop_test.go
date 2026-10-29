package store_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// kept is s's valid characters other than NUL and U+FFFD.
func kept(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError && r != 0 {
			b.WriteRune(r)
		}
		i += n
	}
	return b.String()
}

// checkStored fails unless got, read back for wrote, is valid UTF-8 without
// NUL that keeps wrote's other characters and has a U+FFFD for each NUL, and
// is wrote itself when wrote already was storable.
func checkStored(t *rapid.T, what, wrote, got string) {
	clean := utf8.ValidString(wrote) && !strings.ContainsRune(wrote, 0)
	if !utf8.ValidString(got) || strings.ContainsRune(got, 0) || kept(got) != kept(wrote) ||
		strings.Count(got, "\ufffd") < strings.Count(wrote, "\x00")+strings.Count(wrote, "\ufffd") ||
		!clean && !strings.ContainsRune(got, utf8.RuneError) || clean && got != wrote {
		t.Fatalf("%s: wrote %q, read %q", what, wrote, got)
	}
}

// jsonPieces are JSON string text: escapes jsonb refuses, raw invalid UTF-8,
// and \\u0000, which is not an escape.
var jsonPieces = rapid.SampledFrom([]string{"a", "é", `\u0000`, `\ud800`, `\udc00x`, `\\u0000`, "\xff", "\x80", `\"`, `\n`})

func decoded(t *rapid.T, text string) string {
	var s string
	if err := json.Unmarshal([]byte(`"`+text+`"`), &s); err != nil {
		t.Fatalf("decoding %q: %v", text, err)
	}
	return s
}

// Text a job records from outside, in its columns and its plan and summary JSON, is stored.
func TestPropJobTextIsStored(t *testing.T) {
	s := storetest.New(t)
	pieces := rapid.SampledFrom([]string{"a", "é", "\x00", "\xff", "\x80", "\xc3", `\u0000`, `\`, `"`, "\u2028", "\ufffd", " "})
	round := 0
	rapid.Check(t, func(t *rapid.T) {
		round++
		text := func(label string) string {
			return strings.Join(rapid.SliceOfN(pieces, 0, 6).Draw(t, label), "")
		}
		blocked, note, msg, itemErr, jobErr := text("blocked"), text("note"), text("message"), text("item error"), text("job error")
		inJSON := strings.Join(rapid.SliceOfN(jsonPieces, 0, 6).Draw(t, "in JSON"), "")
		raw := json.RawMessage(`{"x":"` + inJSON + `","` + inJSON + `":"key","n":[1.50,null]}`)
		nj := newJob(fmt.Sprintf("text:%d", round))
		nj.Plan = raw
		nj.Items[1].Blocked = blocked
		id, err := s.CreateJob(ctx, nj)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		if _, err := s.Claim(ctx, "w", time.Hour); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		at := time.Now()
		item := "team01-teak"
		for _, ev := range []store.Event{{At: at, Item: item, Step: "clone", Status: "started"}, {At: at, Item: item, Step: "clone", Status: "info", Message: note}} {
			if err := s.AddEvent(ctx, id, ev); err != nil {
				t.Fatalf("AddEvent(%q): %v", ev.Message, err)
			}
		}
		items, err := s.Items(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		checkStored(t, "note", note, items[0].Note)
		if err := s.AddEvent(ctx, id, store.Event{At: at, Item: item, Step: "clone", Status: "failed", Message: msg}); err != nil {
			t.Fatalf("AddEvent(%q): %v", msg, err)
		}
		if err := s.Finish(ctx, id, "w", store.Outcome{Status: "failed", Summary: raw, Error: jobErr,
			Items: map[string]store.ItemOutcome{item: {Status: "failed", Error: itemErr}}}); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		j, err := s.Job(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		checkStored(t, "job error", jobErr, j.Error)
		want := decoded(t, inJSON)
		for what, got := range map[string]json.RawMessage{"plan": j.Plan, "summary": j.Summary} {
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("%s %s: %v", what, got, err)
			}
			if len(m) != 3 {
				t.Fatalf("%s %s: want 3 keys", what, got)
			}
			if n, _ := json.Marshal(m["n"]); string(n) != "[1.5,null]" {
				t.Fatalf("%s %s: n = %s", what, got, n)
			}
			for k, v := range m {
				switch {
				case k == "x":
					checkStored(t, what, want, v.(string))
				case k != "n":
					checkStored(t, what+" key", want, k)
				}
			}
		}
		if items, err = s.Items(ctx, id); err != nil {
			t.Fatal(err)
		}
		checkStored(t, "item error", itemErr, items[0].Error)
		checkStored(t, "blocked", blocked, items[1].Error)
		evs, err := s.Events(ctx, id, 0, 10)
		if err != nil || len(evs) != 3 {
			t.Fatalf("events %+v: %v", evs, err)
		}
		checkStored(t, "note event", note, evs[1].Message)
		checkStored(t, "event", msg, evs[2].Message)
	})
}
