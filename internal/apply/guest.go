package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

const (
	agentPoll   = 5 * time.Second // between agent pings, and verify reads
	execPoll    = 2 * time.Second
	execLimit   = 5 * time.Minute // the setup scripts take seconds
	verifyTries = 24              // two minutes at agentPoll: Windows applies an address with a delay
	// A Windows guest can take minutes to finish a rename before it powers off.
	restartShutdown = 10 * time.Minute
)

// waitAgent waits for the guest agent to answer, up to
// environments.agent_timeout. Attempts are counted, not timed.
func (e *Executor) waitAgent(ctx context.Context, it *pods.Item) (bool, error) {
	tries := max(1, int(e.Cfg.Environments.AgentTimeout/agentPoll))
	var err error
	for range tries {
		if err = e.api.AgentPing(ctx, it.Node, it.VMID); err == nil {
			return false, nil
		}
		if ctx.Err() != nil || proxmox.IsForbidden(err) {
			return false, err
		}
		if serr := e.Sleep(ctx, agentPoll); serr != nil {
			return false, serr
		}
	}
	return false, fmt.Errorf("the guest agent didn't answer within %s (is qemu-guest-agent installed and running in the image?): %s",
		e.Cfg.Environments.AgentTimeout, proxmox.Describe(err))
}

// mac is the VM's net0 MAC, which the scripts find the NIC by.
func (e *Executor) mac(ctx context.Context, it *pods.Item) (string, error) {
	var cfg map[string]string
	if err := e.call(ctx, func() (err error) { cfg, err = e.api.VMConfig(ctx, it.Node, it.VMID); return }); err != nil {
		return "", err
	}
	mac, ok := pods.NICMAC(cfg["net0"])
	if !ok {
		return "", fmt.Errorf("net0 has no MAC address (%q)", cfg["net0"])
	}
	return mac, nil
}

// configureGuest runs the setup script, and reboots if it renamed the
// computer. The scripts are idempotent, so a retry runs them again.
func (e *Executor) configureGuest(ctx context.Context, it *pods.Item) (bool, error) {
	mac, err := e.mac(ctx, it)
	if err != nil {
		return false, err
	}
	cmd, input, err := pods.GuestCommand(*it.Guest, mac)
	if err != nil {
		return false, err
	}
	out, err := e.exec(ctx, it, cmd, input)
	if err != nil {
		return true, err
	}
	if pods.NeedsReboot(out) {
		e.emit(it.Name, pods.StepGuest, EventInfo, "restarting it to apply the new host name")
		if err := e.restart(ctx, it); err != nil {
			return true, fmt.Errorf("restarting after the rename: %w", err)
		}
		if _, err := e.waitAgent(ctx, it); err != nil {
			return true, err
		}
	}
	return true, nil
}

// restart shuts the guest down, waiting for it to finish applying the
// rename, and starts it again.
func (e *Executor) restart(ctx context.Context, it *pods.Item) error {
	if err := e.taskOnce(ctx, func() (string, error) { return e.api.Shutdown(ctx, it.Node, it.VMID, restartShutdown, false) }); err != nil {
		return err
	}
	var status string
	if err := e.call(ctx, func() (err error) { status, err = e.api.CurrentStatus(ctx, it.Node, it.VMID); return }); err != nil {
		return err
	}
	if status == "running" {
		return nil
	}
	return e.taskOnce(ctx, func() (string, error) { return e.api.Power(ctx, it.Node, it.VMID, "start") })
}

// exec runs a command through the agent and waits for it to exit 0.
//
// The agent can't stop a command, so one that outlasts execLimit keeps
// running in the guest. This run waits for it before starting another,
// while it can read its status; past that, and across jobs, only the guest
// lock (see pods.GuestLockWait) keeps a second copy out, where the guest
// has one.
func (e *Executor) exec(ctx context.Context, it *pods.Item, cmd []string, input string) (string, error) {
	e.mu.Lock()
	prev, running := e.guestRuns[it.VMID]
	e.mu.Unlock()
	if running {
		// One whose status can't be read (a guest restart lost its PID,
		// say) is left to the guest lock.
		switch _, err := e.waitExec(ctx, it, prev); {
		case errors.Is(err, errStillRunning):
			return "", fmt.Errorf("the previous run of the setup script was still running after another %s; not starting a second copy", execLimit)
		case err != nil && ctx.Err() != nil:
			return "", err
		}
	}
	var pid int
	// Sent once: a retry after a lost answer would run a second copy of the
	// script alongside the first. The round retry reruns it instead.
	if err := e.lim.Call(ctx, func() (err error) { pid, err = e.api.AgentExec(ctx, it.Node, it.VMID, cmd, input); return }); err != nil {
		return "", err
	}
	e.mu.Lock()
	e.guestRuns[it.VMID] = pid
	e.mu.Unlock()
	out, err := e.waitExec(ctx, it, pid)
	if errors.Is(err, errStillRunning) {
		return "", fmt.Errorf("the setup script was still running after %s", execLimit)
	}
	return out, err
}

var errStillRunning = errors.New("still running")

// waitExec waits up to execLimit for pid to exit 0, and forgets it once it
// has exited or can't be read (the guest restarted, say).
func (e *Executor) waitExec(ctx context.Context, it *pods.Item, pid int) (string, error) {
	forget := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.guestRuns[it.VMID] == pid {
			delete(e.guestRuns, it.VMID)
		}
	}
	for range int(execLimit / execPoll) {
		var st proxmox.ExecStatus
		if err := e.call(ctx, func() (err error) { st, err = e.api.AgentExecStatus(ctx, it.Node, it.VMID, pid); return }); err != nil {
			if ctx.Err() == nil {
				forget()
			}
			return "", err
		}
		if st.Exited {
			forget()
			if st.ExitCode != 0 {
				return st.Out, fmt.Errorf("the setup script exited %d: %s", st.ExitCode, lastLines(st.Err+"\n"+st.Out, 5))
			}
			return st.Out, nil
		}
		if err := e.Sleep(ctx, execPoll); err != nil {
			return "", err
		}
	}
	return "", errStillRunning
}

// lastLines is the last n non-blank lines of s, each trimmed, joined with
// " / ". A line ends at \n or \r (Windows ends lines with \r\n, and progress
// output rewrites one with a bare \r).
func lastLines(s string, n int) string {
	lines := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	lines = slices.DeleteFunc(lines, func(l string) bool { return l == "" })
	n = min(max(n, 0), len(lines))
	return strings.Join(lines[len(lines)-n:], " / ")
}

// verifyGuest checks, through the agent, that an interface with net0's MAC
// has the address and the guest the host name.
func (e *Executor) verifyGuest(ctx context.Context, it *pods.Item) (bool, error) {
	mac, err := e.mac(ctx, it)
	if err != nil {
		return false, err
	}
	want := it.Guest.IP + "/" + fmt.Sprint(it.Guest.Prefix)
	saw := "nothing"
	for range verifyTries {
		nics, nerr := e.api.AgentInterfaces(ctx, it.Node, it.VMID)
		host, herr := e.api.AgentHostname(ctx, it.Node, it.VMID)
		if proxmox.IsForbidden(nerr) || proxmox.IsForbidden(herr) {
			return false, errors.Join(nerr, herr)
		}
		if nerr == nil && herr == nil {
			// A VLAN subinterface (eth0.10) shares its parent's MAC; any
			// interface with net0's MAC may hold the address.
			var ips []string
			for _, n := range nics {
				if strings.EqualFold(n.MAC, mac) {
					ips = append(ips, n.IPs...)
				}
			}
			short, _, _ := strings.Cut(host, ".")
			if slices.Contains(ips, want) && strings.EqualFold(short, it.Guest.Hostname) {
				return false, nil
			}
			saw = fmt.Sprintf("host name %q with %s", host, strings.Join(ips, ", "))
		} else {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			saw = "an error: " + proxmox.Describe(errors.Join(nerr, herr))
		}
		if err := e.Sleep(ctx, agentPoll); err != nil {
			return false, err
		}
	}
	return false, fmt.Errorf("the guest reports %s; expected %s with %s", saw, it.Guest.Hostname, want)
}
