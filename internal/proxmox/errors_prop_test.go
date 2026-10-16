package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// classifyModel is Classify as a table of rules, first match wins, written
// from the Meaning docs: which text each rule reads, and for which errors.
func classifyModel(patterns []string, err error) Meaning {
	if err == nil || errors.Is(err, context.Canceled) {
		return Permanent
	}
	if errors.Is(err, ErrLocked) {
		return Locked
	}
	var (
		api    *APIError
		task   *TaskError
		text   string
		status int
		isAPI  = errors.As(err, &api)
	)
	switch {
	case isAPI:
		text, status = api.Message, api.Status
	case errors.As(err, &task):
		text = task.ExitStatus + "\n" + strings.Join(task.LogTail, "\n")
	default:
		text = err.Error()
	}
	text = strings.ToLower(text)
	has := func(s string) bool { return strings.Contains(text, s) }
	destroying := task != nil && strings.HasPrefix(task.UPID, "UPID:") && strings.Split(task.UPID, ":")[min(5, len(strings.Split(task.UPID, ":"))-1)] == "qmdestroy" &&
		len(strings.Split(task.UPID, ":")) > 6
	switch {
	case has("locked (destroyed)"):
		return Destroyed
	case destroying && (has("file-user_cfg") || has("access permissions cleanup")):
		return PartialDestroy
	case has("base volume") && has("linked clone"):
		return InUse
	case isAPI && status == 401:
		return Lapsed
	case isAPI && status == 403:
		return Forbidden
	case isAPI && status == 400:
		return Permanent
	case isAPI && has("does not exist"):
		return NotFound
	case isAPI && has("already exists"):
		return Exists
	case has("is locked"):
		return Locked
	case isAPI && status >= 500:
		return Transient
	}
	// Connection errors, and timeouts such as an expired deadline.
	var ne net.Error
	if errors.As(err, &ne) {
		return Transient
	}
	for _, p := range patterns {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" && has(p) {
			return Transient
		}
	}
	return Permanent
}

// errorPhrases are the phrases Classify reads, near misses of them, and
// noise.
var errorPhrases = []string{
	"locked (destroyed)", "Locked (Destroyed)", "is locked", "IS LOCKED", "VM 101 is locked (clone)", "islocked", "is lock", "locked", "is  locked",
	"file-user_cfg", "cfs-lock 'file-user_cfg' error: got lock request timeout", "access permissions cleanup", "Access Permissions Cleanup", "access permission cleanup",
	"base volume", "linked clone", "linked cloned", "base-volume", "linked  clone", "does not exist", "does not exists", "already exists", "already exist", "File exists",
	"timeout", "got no worker upid", "connection refused", "boom", "Permission check failed (/vms/1, VM.Audit)", "", " ", "\n", "x", "é",
}

var errorPatterns = []string{"timeout", "file exists", "got no worker upid", "Is Locked", " Boom ", "", "  ", "linked", "exist", "é", "x"}

var upids = []string{"", "UPID:", "UPID:n:1:2:3:qmdestroy:101:u:", "UPID:n:1:2:3:qmclone:101:u:", "UPID:n:1:2:3:qmdestroy", "UPID:n:1:2:3:qmdestroy:", "x:n:1:2:3:qmdestroy:101:u:",
	"UPID:n:1:2:3:vzdestroy:101:u:", "UPID:n:1:2:3:qmdestroy2:101:u:"}

func drawText(t *rapid.T, label string) string {
	parts := rapid.SliceOfN(rapid.SampledFrom(errorPhrases), 0, 4).Draw(t, label+" phrases")
	sep := rapid.SampledFrom([]string{" ", "\n", ": ", ""}).Draw(t, label+" sep")
	text := strings.Join(parts, sep)
	if rapid.IntRange(0, 4).Draw(t, label+" noise") == 0 {
		text += rapid.String().Draw(t, label+" noise text")
	}
	return text
}

func drawError(t *rapid.T) error {
	var err error
	switch rapid.IntRange(0, 6).Draw(t, "kind") {
	case 0, 1:
		err = &APIError{Method: "POST", Path: "/nodes/n/qemu", Status: rapid.SampledFrom([]int{200, 400, 401, 403, 404, 409, 499, 500, 501, 503, 599}).Draw(t, "status"), Message: drawText(t, "api")}
	case 2, 3:
		var tail []string
		for range rapid.IntRange(0, 2).Draw(t, "tail len") {
			tail = append(tail, drawText(t, "tail"))
		}
		err = &TaskError{UPID: rapid.SampledFrom(upids).Draw(t, "upid"), ExitStatus: drawText(t, "exit"), LogTail: tail}
	case 4:
		err = errors.New(drawText(t, "plain"))
	case 5:
		err = &url.Error{Op: "Get", URL: "https://pve", Err: errors.New(drawText(t, "url"))}
	default:
		err = rapid.SampledFrom([]error{nil, context.Canceled, ErrLocked, context.DeadlineExceeded}).Draw(t, "sentinel")
	}
	if err != nil && rapid.Bool().Draw(t, "wrap") {
		err = fmt.Errorf("step: %w", err)
		if rapid.Bool().Draw(t, "wrap text") {
			err = fmt.Errorf("%s: %w", drawText(t, "wrapper"), err)
		}
	}
	return err
}

// Classify follows its rule table for any error, including hostile text in
// tasks' logs and answers' messages.
func TestPropClassifyMatchesRuleTable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		patterns := rapid.SliceOfN(rapid.SampledFrom(errorPatterns), 0, 4).Draw(t, "patterns")
		err := drawError(t)
		if got, want := NewClassifier(patterns).Classify(err), classifyModel(patterns, err); got != want {
			t.Fatalf("Classify(%#v) with %q = %v, want %v", err, patterns, got, want)
		}
	})
}

// Text that has none of the phrases or patterns can't make an answer or a
// failed task anything but permanent (or transient for a 5xx), and a
// 401/403 means that whatever the text says.
func TestPropHostileTextCannotChangeTheMeaning(t *testing.T) {
	phrases := []string{"locked", "linked clone", "base volume", "file-user_cfg", "access permissions cleanup", "does not exist", "already exists", "timeout", "exist"}
	clean := func(s string) bool {
		s = strings.ToLower(s)
		for _, p := range phrases {
			if strings.Contains(s, p) {
				return false
			}
		}
		return true
	}
	c := NewClassifier([]string{"timeout", "file exists", "got no worker upid"})
	rapid.Check(t, func(t *rapid.T) {
		text := rapid.OneOf(rapid.String(), rapid.StringMatching(`[ -~\n\t]{0,60}`),
			rapid.Map(rapid.SliceOfN(rapid.SampledFrom([]string{"lock", "ed", " is ", "base", " volume", "linked", " clone", "file", "-user", "_cfg", "exist", "s", " not "}), 0, 6),
				func(p []string) string { return strings.Join(p, "") })).Draw(t, "text")
		if !clean(text) {
			t.Skip()
		}
		status := rapid.SampledFrom([]int{200, 400, 404, 409, 422}).Draw(t, "status")
		for _, err := range []error{
			&APIError{Status: status, Message: text},
			&TaskError{UPID: rapid.SampledFrom(upids).Draw(t, "upid"), ExitStatus: text, LogTail: []string{text, "x"}},
			errors.New(text),
			fmt.Errorf("%s: %w", text, errors.New(text)),
		} {
			if got := c.Classify(err); got != Permanent {
				t.Fatalf("Classify(%#v) = %v, want Permanent", err, got)
			}
		}
		if got := c.Classify(&APIError{Status: 503, Message: text}); got != Transient {
			t.Fatalf("503 with %q = %v", text, got)
		}
		if got := c.Classify(&APIError{Status: 401, Message: text + " locked (destroyed) is locked"}); got != Destroyed {
			t.Fatalf("401 destroyed = %v", got)
		}
		if got := c.Classify(&APIError{Status: 401, Message: text}); got != Lapsed {
			t.Fatalf("401 with %q = %v", text, got)
		}
		if got := c.Classify(&APIError{Status: 403, Message: text}); got != Forbidden {
			t.Fatalf("403 with %q = %v", text, got)
		}
	})
}
