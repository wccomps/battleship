package apply

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
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
	probePolls      = 15 // status reads of a setup state check, execPoll apart
	guestReruns     = 2  // of a setup script the guest restarted under
)

// errRestarted is a setup script the guest, or its agent, restarted under.
var errRestarted = errors.New("the guest restarted while the setup script ran")

// waitAgent waits for the guest agent to answer (see waitGuest).
func (e *Executor) waitAgent(ctx context.Context, it *pods.Item) (bool, error) {
	return false, e.waitGuest(ctx, it, false)
}

// waitGuest waits for the guest agent to answer and, with setup on a
// Windows guest, for first-boot setup to finish (see pods.SetupComplete),
// up to environments.agent_timeout. The time is the sum of the waits, not
// the clock. A restart can cut the setup check short; that counts as not
// finished.
func (e *Executor) waitGuest(ctx context.Context, it *pods.Item, setup bool) error {
	setup = setup && it.Guest != nil && it.Guest.OS == pods.ImageWindows
	var waited time.Duration
	var err error
	state, told := "", false
	for {
		if err = e.api.AgentPing(ctx, it.Node, it.VMID); err == nil {
			if !setup {
				return nil
			}
			if state, err = e.setupState(ctx, it, &waited); err == nil {
				if pods.SetupComplete(state) {
					return nil
				}
				if !told {
					e.emit(it.Name, pods.StepGuest, EventInfo, "waiting for Windows to finish first-boot setup")
					told = true
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if proxmox.IsForbidden(err) {
			return err
		}
		if waited >= e.Cfg.Environments.AgentTimeout {
			break
		}
		if serr := e.Sleep(ctx, agentPoll); serr != nil {
			return serr
		}
		waited += agentPoll
	}
	if err == nil {
		return fmt.Errorf("Windows didn't finish first-boot setup within %s (its ImageState is %s)",
			e.Cfg.Environments.AgentTimeout, strings.TrimSpace(state))
	}
	return fmt.Errorf("the guest agent didn't answer within %s (is qemu-guest-agent installed and running in the image?): %s",
		e.Cfg.Environments.AgentTimeout, proxmox.Describe(err))
}

// setupState runs pods.SetupStateCommand and returns its output, adding
// the time it waits to waited.
func (e *Executor) setupState(ctx context.Context, it *pods.Item, waited *time.Duration) (string, error) {
	var pid int
	if err := e.lim.Call(ctx, func() (err error) {
		pid, err = e.api.AgentRead(ctx, it.Node, it.VMID, pods.SetupStateCommand)
		return
	}); err != nil {
		return "", err
	}
	for range probePolls {
		var st proxmox.ExecStatus
		if err := e.lim.Call(ctx, func() (err error) { st, err = e.api.AgentExecStatus(ctx, it.Node, it.VMID, pid, maxExecStatus); return }); err != nil {
			return "", err
		}
		if st.Exited {
			if st.ExitCode != 0 {
				return "", fmt.Errorf("reading the setup state exited %d: %s", st.ExitCode, lastLines(st.Err+"\n"+st.Out, 3))
			}
			return st.Out, nil
		}
		if err := e.Sleep(ctx, execPoll); err != nil {
			return "", err
		}
		*waited += execPoll
	}
	return "", fmt.Errorf("reading the setup state took over %s", probePolls*execPoll)
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

// configureGuest runs the setup script once the guest is ready (see
// waitReady), and reboots if it renamed the computer. The scripts are
// idempotent, so a retry runs them again, and so does a restart under one,
// up to guestReruns times.
func (e *Executor) configureGuest(ctx context.Context, it *pods.Item) (bool, error) {
	mac, err := e.mac(ctx, it)
	if err != nil {
		return false, err
	}
	cmd, input, err := pods.GuestCommand(*it.Guest, mac)
	if err != nil {
		return false, err
	}
	var out string
	for run := 0; ; run++ {
		if err := e.waitReady(ctx, it); err != nil {
			return run > 0, err
		}
		out, err = e.exec(ctx, it, cmd, input)
		if !errors.Is(err, errRestarted) || run == guestReruns {
			break
		}
		e.emit(it.Name, pods.StepGuest, EventInfo, "the guest restarted while the setup script ran; running it again once the guest is ready")
	}
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

// waitReady is waitGuest with setup, ended by a stop at once: no command
// is running for a cancel's grace to see end.
func (e *Executor) waitReady(ctx context.Context, it *pods.Item) error {
	ctx, cancel := ungraced(ctx)
	defer cancel()
	return e.waitGuest(ctx, it, true)
}

// restart shuts the guest down and starts it again so its rename takes effect.
func (e *Executor) restart(ctx context.Context, it *pods.Item) error {
	if err := e.taskOnce(ctx, func() (string, error) { return e.api.Shutdown(ctx, it.Node, it.VMID, restartShutdown) }); err != nil {
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

// maxExecStatus bounds an answer to a status read of a guest command, which
// carries its output. pods' guest commands print a few lines, or one error
// record (their PowerShell stops at the first), and SetupStateCommand one
// word; 1 MiB is hundreds of times that.
const maxExecStatus = 1 << 20

// exec runs a command through the agent and waits for it to exit 0.
//
// The agent can't stop a command, so one that outlasts execLimit keeps
// running in the guest; a retry runs the script again. Where the guest
// lock works (see pods.GuestLockWait), the new copy waits up to that long
// for the one still running, then exits 75.
//
// A status read the agent answers with proxmox.ExecGone means it restarted
// since it started the command: the error is errRestarted. Unlike a lost
// answer to the exec, that leaves nothing to follow: the restart killed the
// command, or, if only the agent restarted, the guest lock makes a rerun
// wait for it.
func (e *Executor) exec(ctx context.Context, it *pods.Item, cmd []string, input string) (string, error) {
	var pid int
	// Sent once: a retry after a lost answer would start a second copy at
	// once. The round retry reruns it instead.
	if err := e.lim.Call(ctx, func() (err error) { pid, err = e.api.AgentExec(ctx, it.Node, it.VMID, cmd, input); return }); err != nil {
		return "", err
	}
	for range int(execLimit / execPoll) {
		var st proxmox.ExecStatus
		if err := e.call(ctx, func() (err error) { st, err = e.api.AgentExecStatus(ctx, it.Node, it.VMID, pid, maxExecStatus); return }); err != nil {
			if proxmox.IsExecGone(err) {
				return "", fmt.Errorf("%w (%s)", errRestarted, proxmox.Describe(err))
			}
			return "", err
		}
		if st.Exited {
			if st.ExitCode != 0 {
				return st.Out, fmt.Errorf("the setup script exited %d: %s", st.ExitCode, lastLines(st.Err+"\n"+st.Out, 5))
			}
			return st.Out, nil
		}
		if err := e.Sleep(ctx, execPoll); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("the setup script was still running after %s", execLimit)
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

// addrList is ips, each written "address/prefix", or as its bare address
// without a valid prefix.
func addrList(ips []netip.Prefix) string {
	s := make([]string, len(ips))
	for i, p := range ips {
		s[i] = p.Addr().String()
		if p.IsValid() {
			s[i] = p.String()
		}
	}
	return strings.Join(s, ", ")
}

// verifyGuest checks, through the agent, that an interface with net0's MAC
// has the address and the guest the host name.
func (e *Executor) verifyGuest(ctx context.Context, it *pods.Item) (bool, error) {
	mac, err := e.mac(ctx, it)
	if err != nil {
		return false, err
	}
	want := netip.PrefixFrom(it.Guest.IP, it.Guest.Prefix)
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
			var ips []netip.Prefix
			for _, n := range nics {
				if strings.EqualFold(n.MAC, mac) {
					ips = append(ips, n.IPs...)
				}
			}
			short, _, _ := strings.Cut(host, ".")
			if slices.Contains(ips, want) && strings.EqualFold(short, it.Guest.Hostname) {
				return false, nil
			}
			saw = fmt.Sprintf("host name %q with %s", host, addrList(ips))
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
