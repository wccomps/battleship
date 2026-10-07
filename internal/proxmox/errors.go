package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// APIError is a non-2xx response from the Proxmox API.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// TaskError is a Proxmox task that finished with an exit status other than OK.
type TaskError struct {
	UPID       string
	ExitStatus string
	LogTail    []string
}

func (e *TaskError) Error() string {
	msg := fmt.Sprintf("task %s failed: %s", e.UPID, e.ExitStatus)
	if len(e.LogTail) > 0 {
		msg += "\n  " + strings.Join(e.LogTail, "\n  ")
	}
	return msg
}

// Classifier maps Proxmox errors to a Meaning so callers don't match text.
type Classifier struct {
	patterns []string
}

// NewClassifier makes a Classifier that calls an error Transient if it
// contains a pattern (case-insensitive) and no other Meaning applies.
func NewClassifier(patterns []string) Classifier {
	var lower []string
	for _, p := range patterns {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			lower = append(lower, strings.ToLower(trimmed))
		}
	}
	return Classifier{patterns: lower}
}

// matchText extracts the text portion of an error for pattern matching.
func matchText(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return strings.ToLower(apiErr.Message)
	}
	var taskErr *TaskError
	if errors.As(err, &taskErr) {
		text := taskErr.ExitStatus
		if len(taskErr.LogTail) > 0 {
			text += "\n" + strings.Join(taskErr.LogTail, "\n")
		}
		return strings.ToLower(text)
	}
	return strings.ToLower(err.Error())
}

// Meaning is what an error says about the call that returned it.
type Meaning int

const (
	// Permanent: trying again cannot help; also anything unrecognized.
	Permanent Meaning = iota
	// Transient: a 5xx, connection error or configured pattern.
	Transient
	// Locked: another task holds the VM's lock (ErrLocked, or "is
	// locked"); it may succeed once that task ends.
	Locked
	// Destroyed: a destroy died part way; Proxmox refuses everything on the
	// VM until an admin runs qm destroy --skiplock.
	Destroyed
	// NotFound: the VM's config "does not exist" on the node asked; it may be
	// gone, elsewhere, or a fresh clone pmxcfs hasn't propagated yet.
	NotFound
	// Exists: the VMID "already exists".
	Exists
	// PartialDestroy: a destroy failed on the user.cfg lock after deleting
	// the disks, so retrying the destroy cannot help.
	PartialDestroy
	// InUse: a template's base volume cannot be removed while linked clones
	// still use it.
	InUse
	// Forbidden: 403, a missing privilege. Never retried, and never with
	// another identity.
	Forbidden
	// Lapsed: 401, the credential expired or was revoked; nothing more can
	// be done as the user until they log in again.
	Lapsed
)

// ErrLocked marks an error the caller made for a VM it found locked by
// another task.
var ErrLocked = errors.New("VM is locked")

// Classify says what err means. Only Transient depends on the patterns.
func (c Classifier) Classify(err error) Meaning {
	if err == nil || errors.Is(err, context.Canceled) {
		return Permanent
	}
	if errors.Is(err, ErrLocked) {
		return Locked
	}
	msg := matchText(err)
	var apiErr *APIError
	isAPI := errors.As(err, &apiErr)
	var taskErr *TaskError
	isDestroy := false
	if errors.As(err, &taskErr) {
		u, _ := ParseUPID(taskErr.UPID)
		isDestroy = u.Type == "qmdestroy"
	}
	switch {
	case strings.Contains(msg, "locked (destroyed)"):
		return Destroyed
	case isDestroy && (strings.Contains(msg, "file-user_cfg") || strings.Contains(msg, "access permissions cleanup")):
		return PartialDestroy
	case strings.Contains(msg, "base volume") && strings.Contains(msg, "linked clone"):
		return InUse
	case isAPI && apiErr.Status == 401:
		return Lapsed
	case isAPI && apiErr.Status == 403:
		return Forbidden
	case isAPI && apiErr.Status == 400:
		return Permanent
	case isAPI && strings.Contains(msg, "does not exist"):
		return NotFound
	case isAPI && strings.Contains(msg, "already exists"):
		return Exists
	case strings.Contains(msg, "is locked"):
		return Locked
	case isAPI && apiErr.Status >= 500:
		return Transient
	}
	var urlErr *url.Error
	var netErr net.Error
	if errors.As(err, &urlErr) || errors.As(err, &netErr) {
		return Transient
	}
	for _, p := range c.patterns {
		if strings.Contains(msg, p) {
			return Transient
		}
	}
	return Permanent
}

// IsLapsed reports whether err says Proxmox no longer accepts the
// caller's credential (see Lapsed).
func IsLapsed(err error) bool { return Classifier{}.Classify(err) == Lapsed }

// IsForbidden reports whether err is Proxmox refusing a call for a missing
// privilege (see Forbidden).
func IsForbidden(err error) bool { return Classifier{}.Classify(err) == Forbidden }

// IsNotFound reports whether err says the VM's config does not exist on the
// node asked (see the NotFound meaning).
func IsNotFound(err error) bool { return Classifier{}.Classify(err) == NotFound }

var permissionRE = regexp.MustCompile(`Permission check failed \(([^,]+), ([^)]+)\)`)

// Describe turns known Proxmox errors into an actionable sentence.
func Describe(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == 403 {
		if m := permissionRE.FindStringSubmatch(apiErr.Message); m != nil {
			path := m[1]
			privs := m[2]
			if strings.Contains(privs, "|") {
				privList := strings.Split(privs, "|")
				return fmt.Sprintf("not permitted: you don't have any of %s on %s", strings.Join(privList, ", "), path)
			}
			return fmt.Sprintf("not permitted: you don't have %s on %s", privs, path)
		}
		return "not permitted: " + err.Error()
	}
	if IsLapsed(err) {
		return "authorization lapsed: Proxmox no longer accepts this login (" + err.Error() + ")"
	}
	return err.Error()
}
