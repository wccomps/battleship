package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// parseEventStream reads an event stream as the HTML standard's
// EventSource does: lines end at CRLF, LF or a lone CR; a blank line
// dispatches; data lines are joined with LF.
func parseEventStream(stream string) []sseEvent {
	stream = strings.ReplaceAll(stream, "\r\n", "\n")
	stream = strings.ReplaceAll(stream, "\r", "\n")
	var out []sseEvent
	var ev sseEvent
	var data []string
	for _, line := range strings.Split(stream, "\n") {
		switch {
		case line == "":
			if data != nil {
				ev.Data = strings.Join(data, "\n")
				out = append(out, ev)
			}
			ev, data = sseEvent{}, nil
		case strings.HasPrefix(line, ":"):
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				ev.Event = value
			case "id":
				ev.ID = value
			case "data":
				data = append(data, value)
			}
		}
	}
	return out
}

// An event's data reaches the browser whole, whatever line breaks it
// holds: rendered pieces carry text from VM names, job errors and log
// lines, which html/template leaves CRs in. The browser ends a line at a
// lone CR too, so one sent as is would cut the piece there.
func TestPropSSEEventDataArrivesWhole(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := rapid.SampledFrom([]string{"grid", "patch", "log", "end"}).Draw(t, "name")
		id := rapid.StringMatching(`[0-9]{0,4}`).Draw(t, "id")
		data := hostile(t, "data")
		var buf bytes.Buffer
		sw := &sseWriter{w: &buf, rc: http.NewResponseController(httptest.NewRecorder())}
		if err := sw.event(name, id, data); err != nil {
			t.Fatal(err)
		}
		evs := parseEventStream(buf.String())
		if len(evs) != 1 {
			t.Fatalf("event(%q) arrives as %d events: %q", data, len(evs), buf.String())
		}
		// Line breaks may change kind (the markup doesn't care); nothing
		// else may change.
		want := strings.ReplaceAll(strings.ReplaceAll(data, "\r\n", "\n"), "\r", "\n")
		if got := evs[0]; got.Event != name || got.ID != id || got.Data != want {
			t.Fatalf("event(%q, %q, %q) arrives as %+v", name, id, data, got)
		}
	})
}

// Found by rapid: a lone CR in a job's error cut the piece.
func TestSSEEventLoneCR(t *testing.T) {
	var buf bytes.Buffer
	sw := &sseWriter{w: &buf, rc: http.NewResponseController(httptest.NewRecorder())}
	data := `<template><td class="err">copying 10%` + "\r" + `copying 100%</td></template>`
	if err := sw.event("patch", "", data); err != nil {
		t.Fatal(err)
	}
	evs := parseEventStream(buf.String())
	if len(evs) != 1 || !strings.HasSuffix(evs[0].Data, "copying 100%</td></template>") {
		t.Fatalf("the piece arrives as %q", evs)
	}
}
