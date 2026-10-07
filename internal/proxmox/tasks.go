package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxPollFailures = 10

// UPID is what a task's ID says about it:
// "UPID:node:pid:pstart:starttime:type:id:user:".
type UPID struct {
	Node string
	Type string // e.g. qmclone; "" if the UPID is cut short
	ID   string // the VMID for a VM's task; "" if cut short
}

// ParseUPID reads a task's UPID.
func ParseUPID(s string) (UPID, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 3 || parts[0] != "UPID" || parts[1] == "" {
		return UPID{}, fmt.Errorf("malformed UPID %q", s)
	}
	u := UPID{Node: parts[1]}
	if len(parts) > 6 {
		u.Type, u.ID = parts[5], parts[6]
	}
	return u, nil
}

// WaitTask polls a task until it stops, returning a *TaskError with the log
// tail unless it ended OK or with warnings. An empty upid returns nil.
func (c *Client) WaitTask(ctx context.Context, upid string, poll time.Duration) error {
	if upid == "" {
		return nil
	}
	u, err := ParseUPID(upid)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/nodes/%s/tasks/%s", url.PathEscape(u.Node), url.PathEscape(upid))
	failures := 0
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := c.do(ctx, http.MethodGet, path+"/status", nil, &st); err != nil {
			// The task keeps running when a poll fails; only a 4xx is an answer.
			var apiErr *APIError
			switch {
			case ctx.Err() != nil:
				return ctx.Err()
			case errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500:
				return err
			}
			if failures++; failures >= maxPollFailures {
				return fmt.Errorf("polling task %s: %w", upid, err)
			}
		} else {
			failures = 0
			if st.Status == "stopped" {
				if st.ExitStatus == "OK" || strings.HasPrefix(st.ExitStatus, "WARNINGS:") {
					return nil
				}
				return &TaskError{UPID: upid, ExitStatus: st.ExitStatus, LogTail: c.taskLogTail(ctx, path, 5)}
			}
		}
		if err := SleepContext(ctx, poll); err != nil {
			return err
		}
	}
}

// StopTask asks Proxmox to stop a running task; WaitTask says when it has.
func (c *Client) StopTask(ctx context.Context, upid string) error {
	u, err := ParseUPID(upid)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/tasks/%s", url.PathEscape(u.Node), url.PathEscape(upid)), nil, nil)
}

func (c *Client) taskLogTail(ctx context.Context, path string, n int) []string {
	var lines []struct {
		T string `json:"t"`
	}
	if err := c.do(ctx, http.MethodGet, path+"/log", url.Values{"start": {"0"}, "limit": {"5000"}}, &lines); err != nil {
		return nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.T
	}
	return out
}

// Task is one entry of a node's task list.
type Task struct {
	UPID    string
	Type    string // e.g. qmstart, qmclone, qmdestroy
	VMID    int    // the task's ID when it is a VMID, else 0
	Start   time.Time
	Running bool // no end time yet
}

// NodeTasks lists node's tasks started at or after since. Without Sys.Audit
// on /nodes/<node>, Proxmox lists only the caller's own.
func (c *Client) NodeTasks(ctx context.Context, node string, since time.Time) ([]Task, error) {
	var raw []struct {
		UPID      string      `json:"upid"`
		Type      string      `json:"type"`
		ID        string      `json:"id"`
		StartTime json.Number `json:"starttime"`
		EndTime   json.Number `json:"endtime"`
	}
	params := url.Values{"since": {strconv.FormatInt(since.Unix(), 10)}, "source": {"all"}, "limit": {"1000"}}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/tasks", url.PathEscape(node)), params, &raw); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(raw))
	for _, r := range raw {
		vmid, _ := strconv.Atoi(r.ID)
		out = append(out, Task{UPID: r.UPID, Type: r.Type, VMID: vmid,
			Start: time.Unix(int64Of(r.StartTime), 0), Running: r.EndTime == ""})
	}
	return out, nil
}
