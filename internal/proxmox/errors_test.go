package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

var testPatterns = []string{"file exists", "does not exist", "got no worker upid", "is locked", "timeout"}

func TestTransient(t *testing.T) {
	c := NewClassifier(testPatterns)
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// Real messages from Proxmox tasks.
		{"cloud-init file exists", &APIError{Status: 500, Message: "mkdir /mnt/pve/competitions/images/10052: File exists at /usr/share/perl5/PVE/Storage/Plugin.pm line 1055."}, true},
		{"no worker upid", &APIError{Status: 500, Message: "got no worker upid - start worker failed"}, true},
		{"permission denied", &APIError{Status: 403, Message: "Permission check failed (/vms/10126, VM.Config.CDROM)"}, false},
		{"403 whose message matches a pattern", &APIError{Status: 403, Message: "file exists"}, false},
		{"bad parameter", &APIError{Status: 400, Message: "Parameter verification failed."}, false},
		{"404 not found", &APIError{Status: 404, Message: "Configuration file does not exist"}, false},
		{"404 other", &APIError{Status: 404, Message: "no such resource"}, false},
		{"500 unmatched", &APIError{Status: 500, Message: "boom"}, true},
		{"connection error", &url.Error{Op: "Get", URL: "https://pve", Err: errors.New("connection refused")}, true},
		{"canceled in url.Error", &url.Error{Op: "Get", URL: "https://pve", Err: context.Canceled}, false},
		{"bare canceled", context.Canceled, false},
		{"failed task matching pattern", &TaskError{UPID: "UPID:x", ExitStatus: "can't create 'file': File exists"}, true},
		{"failed task other", &TaskError{UPID: "UPID:x", ExitStatus: "unable to find configuration file"}, false},
		{"path text does not match", &APIError{Method: "GET", Path: "/pools/timeout-team", Status: 404, Message: "not found"}, false},
		{"task pattern in log tail", &TaskError{UPID: "UPID:n:1:2:3:x:1:u:", ExitStatus: "error", LogTail: []string{"trying to acquire lock...", "can't lock file '/var/lock/qemu-server/lock-101.conf' - got timeout"}}, true},
		{"wrapped", fmt.Errorf("step network: %w", &APIError{Status: 500, Message: "boom"}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Classify(tc.err) == Transient; got != tc.want {
				t.Errorf("Classify(%v) == Transient is %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	c := NewClassifier(testPatterns)
	notExist := &APIError{Status: 500, Message: "Configuration file 'nodes/a/qemu-server/101.conf' does not exist"}
	cases := []struct {
		name string
		err  error
		want Meaning
	}{
		{"nil", nil, Permanent},
		{"cancel", fmt.Errorf("x: %w", context.Canceled), Permanent},
		{"5xx", &APIError{Status: 500, Message: "boom"}, Transient},
		{"connection", &url.Error{Op: "Get", URL: "https://pve", Err: errors.New("connection refused")}, Transient},
		{"pattern", errors.New("got no worker upid"), Transient},
		{"forbidden", &APIError{Status: 403, Message: "Permission check failed (/vms/1, VM.Audit)"}, Forbidden},
		{"unauthorized", &APIError{Status: 401, Message: "authentication failure"}, Lapsed},
		{"bad request does not exist", &APIError{Status: 400, Message: "storage 'x' does not exist"}, Permanent},
		{"not found despite 5xx and pattern", notExist, NotFound},
		{"not found wrapped", fmt.Errorf("reading: %w", notExist), NotFound},
		{"exists", &APIError{Status: 500, Message: "unable to create VM 101: config file already exists"}, Exists},
		{"locked", &APIError{Status: 500, Message: "VM is locked (clone)"}, Locked},
		{"locked task", &TaskError{UPID: "UPID:x", ExitStatus: "VM 101 is locked (snapshot)"}, Locked},
		{"locked sentinel", fmt.Errorf("VM 101 is locked (clone): %w", ErrLocked), Locked},
		{"destroyed", &APIError{Status: 500, Message: "VM is locked (destroyed)"}, Destroyed},
		{"destroyed task", &TaskError{UPID: "UPID:x", ExitStatus: "error", LogTail: []string{"VM 101 is locked (destroyed)"}}, Destroyed},
		// Proxmox's own wording, typo included.
		{"partial destroy", &TaskError{UPID: "UPID:n:1:2:3:qmdestroy:101:u:", ExitStatus: "access permissions cleanup for VM 101 failed: cfs-lock 'file-user_cfg' error: got lock request timeout"}, PartialDestroy},
		{"partial destroy in pool removal", &TaskError{UPID: "UPID:n:1:2:3:qmdestroy:101:u:", ExitStatus: "error", LogTail: []string{"cfs-lock 'file-user_cfg' error: got lock request timeout"}}, PartialDestroy},
		{"user.cfg timeout of a clone task is transient", &TaskError{UPID: "UPID:n:1:2:3:qmclone:101:u:", ExitStatus: "cfs-lock 'file-user_cfg' error: got lock request timeout"}, Transient},
		{"user.cfg timeout of a clone POST is transient", &APIError{Status: 500, Message: "cfs-lock 'file-user_cfg' error: got lock request timeout"}, Transient},
		{"base volume in use", &APIError{Status: 500, Message: "base volume 'competitions:9021/base-9021-disk-0.qcow2' is still in use by linked cloned"}, InUse},
		{"base volume in use task", &TaskError{UPID: "UPID:x", ExitStatus: "can't remove base volume with linked clones"}, InUse},
		{"base volume in use wrapped", fmt.Errorf("freeing: %s", "base volume 'x' is still in use by linked cloned"), InUse},
		{"unrecognized", errors.New("boom"), Permanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestEmptyPatternsIgnored(t *testing.T) {
	c := NewClassifier([]string{"", "  "})
	if got := c.Classify(errors.New("anything")); got != Permanent {
		t.Errorf("empty patterns: Classify(anything) = %v, want Permanent", got)
	}
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"non-403 APIError", &APIError{Status: 500, Message: "boom", Method: "GET", Path: "/x"}, "GET /x: 500 boom"},
		{"403 unparseable", &APIError{Status: 403, Message: "Permission check failed (user != root@pam)", Method: "GET", Path: "/access"}, "not permitted: GET /access: 403 Permission check failed (user != root@pam)"},
		{"single privilege", fmt.Errorf("wrapped: %w", &APIError{Status: 403, Message: "Permission check failed (/vms/10126, VM.Config.CDROM)"}), "not permitted: you don't have VM.Config.CDROM on /vms/10126"},
		{"alternatives", &APIError{Status: 403, Message: "Permission check failed (/vms/1, VM.Allocate|VM.Clone)"}, "not permitted: you don't have any of VM.Allocate, VM.Clone on /vms/1"},
		{"lapsed", &APIError{Status: 401, Message: "authentication failure", Method: "GET", Path: "/x"}, "authorization lapsed: Proxmox no longer accepts this login (GET /x: 401 authentication failure)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Describe(tc.err); got != tc.want {
				t.Errorf("Describe = %q, want %q", got, tc.want)
			}
		})
	}
}

// transient is a Retrier.Retryable for tests: what Classify calls Transient.
func transient(patterns ...string) func(error) bool {
	c := NewClassifier(patterns)
	return func(err error) bool { return c.Classify(err) == Transient }
}
