package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// GuestNIC is a guest network interface as the QEMU guest agent reports it.
type GuestNIC struct {
	Name string
	MAC  string   // lower case, colon-separated
	IPs  []string // "address/prefix"
}

// ExecStatus is the state of a command the guest agent runs.
type ExecStatus struct {
	Exited   bool
	ExitCode int
	Out, Err string
}

// AgentPing fails unless the VM's guest agent answers.
func (c *Client) AgentPing(ctx context.Context, node string, vmid int) error {
	return c.do(ctx, http.MethodPost, vmPath(node, vmid)+"/agent/ping", nil, nil)
}

// AgentExec starts command in the guest with input on its stdin and returns
// its PID.
func (c *Client) AgentExec(ctx context.Context, node string, vmid int, command []string, input string) (int, error) {
	params := url.Values{}
	for _, a := range command {
		params.Add("command", a)
	}
	if input != "" {
		params.Set("input-data", input)
	}
	var raw struct {
		PID json.Number `json:"pid"`
	}
	if err := c.do(ctx, http.MethodPost, vmPath(node, vmid)+"/agent/exec", params, &raw); err != nil {
		return 0, err
	}
	pid, err := raw.PID.Int64()
	if err != nil {
		return 0, fmt.Errorf("agent exec: Proxmox returned no PID (%q)", raw.PID)
	}
	return int(pid), nil
}

// AgentExecStatus reads the state of the guest command pid.
func (c *Client) AgentExecStatus(ctx context.Context, node string, vmid, pid int) (ExecStatus, error) {
	var raw struct {
		Exited   json.RawMessage `json:"exited"`
		ExitCode json.Number     `json:"exitcode"`
		Out      string          `json:"out-data"`
		Err      string          `json:"err-data"`
	}
	params := url.Values{"pid": {strconv.Itoa(pid)}}
	if err := c.do(ctx, http.MethodGet, vmPath(node, vmid)+"/agent/exec-status", params, &raw); err != nil {
		return ExecStatus{}, err
	}
	exited := strings.TrimSpace(string(raw.Exited))
	return ExecStatus{Exited: exited == "1" || exited == "true", ExitCode: int(int64Of(raw.ExitCode)), Out: raw.Out, Err: raw.Err}, nil
}

// maxNICsAnswer bounds an answer to AgentInterfaces, which the guest
// writes: 1 MiB is some 2,000 interfaces with their addresses and
// statistics, far more than any real guest has.
const maxNICsAnswer = 1 << 20

// AgentInterfaces lists the guest's network interfaces. It refuses an
// answer over maxNICsAnswer.
func (c *Client) AgentInterfaces(ctx context.Context, node string, vmid int) ([]GuestNIC, error) {
	var raw struct {
		Result []struct {
			Name string `json:"name"`
			HW   string `json:"hardware-address"`
			IPs  []struct {
				Addr   string      `json:"ip-address"`
				Prefix json.Number `json:"prefix"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if err := c.capped(maxNICsAnswer).do(ctx, http.MethodGet, vmPath(node, vmid)+"/agent/network-get-interfaces", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]GuestNIC, 0, len(raw.Result))
	for _, r := range raw.Result {
		n := GuestNIC{Name: r.Name, MAC: strings.ToLower(r.HW)}
		for _, ip := range r.IPs {
			n.IPs = append(n.IPs, ip.Addr+"/"+ip.Prefix.String())
		}
		out = append(out, n)
	}
	return out, nil
}

// AgentHostname is the guest's host name.
func (c *Client) AgentHostname(ctx context.Context, node string, vmid int) (string, error) {
	var raw struct {
		Result struct {
			Name string `json:"host-name"`
		} `json:"result"`
	}
	err := c.do(ctx, http.MethodGet, vmPath(node, vmid)+"/agent/get-host-name", nil, &raw)
	return raw.Result.Name, err
}
