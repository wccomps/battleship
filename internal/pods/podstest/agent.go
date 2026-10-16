package podstest

import (
	"context"
	"fmt"
	"slices"

	"github.com/wccomps/battleship/internal/proxmox"
)

// Guest is what a VM's QEMU guest agent reports. An agent answers only
// while Up and the VM is running.
type Guest struct {
	Up       bool
	Hostname string
	NICs     []proxmox.GuestNIC
}

// ExecCall is a command run through a guest agent.
type ExecCall struct {
	VMID    int
	Command []string
	Input   string
}

// SetGuest gives vmid a guest agent. It takes Mu.
func (f *Fake) SetGuest(vmid int, g Guest) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Guests == nil {
		f.Guests = map[int]*Guest{}
	}
	f.Guests[vmid] = &g
}

// agentDown is Proxmox's answer when the agent doesn't respond.
func agentDown(vmid int) error {
	return &proxmox.APIError{Status: 500, Message: fmt.Sprintf("VM %d qmp command 'guest-ping' failed - QEMU guest agent is not running", vmid)}
}

// guest finds vmid's answering agent; call with Mu held.
func (f *Fake) guest(node string, vmid int) (*Guest, error) {
	vm, err := f.get(node, vmid)
	if err != nil {
		return nil, err
	}
	g := f.Guests[vmid]
	if g == nil || !g.Up || vm.Status != "running" {
		return nil, agentDown(vmid)
	}
	return g, nil
}

// agentCall runs one agent call under Gate and Mu, recording key.
func (f *Fake) agentCall(ctx context.Context, key, node string, vmid int, fn func(g *Guest) error) error {
	done, err := f.enter(ctx, key)
	defer done()
	if err != nil {
		return err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.record(key); err != nil {
		return err
	}
	g, err := f.guest(node, vmid)
	if err != nil {
		return err
	}
	return fn(g)
}

func (f *Fake) AgentPing(ctx context.Context, node string, vmid int) error {
	return f.agentCall(ctx, fmt.Sprintf("agent-ping:%d", vmid), node, vmid, func(*Guest) error { return nil })
}

func (f *Fake) AgentExec(ctx context.Context, node string, vmid int, command []string, input string) (int, error) {
	var pid int
	err := f.agentCall(ctx, fmt.Sprintf("agent-exec:%d", vmid), node, vmid, func(*Guest) error {
		f.Execs = append(f.Execs, ExecCall{VMID: vmid, Command: slices.Clone(command), Input: input})
		st := proxmox.ExecStatus{Exited: true}
		if f.ExecHook != nil {
			st.ExitCode, st.Out, st.Err = f.ExecHook(vmid, command, input)
		}
		if f.execs == nil {
			f.execs = map[int]proxmox.ExecStatus{}
		}
		f.nextPID++
		pid = f.nextPID
		f.execs[pid] = st
		return nil
	})
	return pid, err
}

// ExecRunning is whether command pid is still running: it exits on the
// status read after its last running one. Call it with f.Mu held, as
// ExecHook is.
func (f *Fake) ExecRunning(pid int) bool {
	return f.ExecRunsFor != nil && f.polls[pid] <= f.ExecRunsFor(pid)
}

func (f *Fake) AgentExecStatus(ctx context.Context, node string, vmid, pid int) (proxmox.ExecStatus, error) {
	var st proxmox.ExecStatus
	err := f.agentCall(ctx, fmt.Sprintf("agent-status:%d", vmid), node, vmid, func(*Guest) error {
		s, ok := f.execs[pid]
		if !ok {
			return &proxmox.APIError{Status: 500, Message: fmt.Sprintf("Agent error: Invalid parameter 'pid' %d", pid)}
		}
		if f.polls == nil {
			f.polls = map[int]int{}
		}
		f.polls[pid]++
		if f.ExecRunning(pid) {
			s = proxmox.ExecStatus{}
		}
		st = s
		return nil
	})
	return st, err
}

func (f *Fake) AgentInterfaces(ctx context.Context, node string, vmid int) ([]proxmox.GuestNIC, error) {
	var out []proxmox.GuestNIC
	err := f.agentCall(ctx, fmt.Sprintf("agent-nics:%d", vmid), node, vmid, func(g *Guest) error {
		out = slices.Clone(g.NICs)
		return nil
	})
	return out, err
}

func (f *Fake) AgentHostname(ctx context.Context, node string, vmid int) (string, error) {
	var out string
	err := f.agentCall(ctx, fmt.Sprintf("agent-host:%d", vmid), node, vmid, func(g *Guest) error {
		out = g.Hostname
		return nil
	})
	return out, err
}
