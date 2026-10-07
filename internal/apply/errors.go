package apply

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// errExplained marks an error that says itself what to check or do, so
// it is shown as is, with no advice to remove the VM: the VM may not be this
// run's, or removing it may not be the fix.
var errExplained = errors.New("explained")

// explained is an error text that matches errExplained.
type explained string

func (x explained) Error() string        { return string(x) }
func (x explained) Is(target error) bool { return target == errExplained }

// errUnconfirmed marks a VM whose state cleanup could not determine.
var errUnconfirmed error = explained("could not confirm")

type notOursError struct {
	vmid         int
	holder, name string
}

func (n *notOursError) Error() string {
	return fmt.Sprintf("VMID %d now holds %s, which this run did not create; left alone. Check whether %s needs removing.",
		n.vmid, n.holder, n.name)
}

func (n *notOursError) Is(target error) bool { return target == errExplained }

// CleanupAdvice is the line to show for a Result.CleanupFailed entry. It
// advises removing the VM by hand only when the VM is known to be this run's;
// for a VM of unknown identity or ownership the error already says what to
// check, and suggesting deletion could destroy someone else's VM.
func CleanupAdvice(name string, err error) string {
	if errors.Is(err, errExplained) {
		return err.Error()
	}
	return fmt.Sprintf("Could not remove %s: %s; remove it in Proxmox.", name, proxmox.Describe(err))
}

// halfDeletedError is a VM a failed destroy task left half-deleted: some of
// it (usually its disks) gone and its config holding only lock=destroyed,
// so every later destroy is refused. The error says how to finish it.
type halfDeletedError struct {
	vmid  int
	node  string
	cause error // the failed delete, if this run saw it
}

func (h *halfDeletedError) Error() string {
	msg := pods.HalfDeletedText(h.vmid, h.node)
	if h.cause != nil {
		msg += "\nthe delete failed with: " + proxmox.Describe(h.cause)
	}
	return msg
}

// Is matches errExplained. There is no Unwrap: errors.Is and errors.As don't
// reach the cause.
func (h *halfDeletedError) Is(target error) bool { return target == errExplained }

// unconvertedError is a template whose config says template: 1 but one of
// whose disks was never renamed to a base- volume: Proxmox's convert failed
// partway, typically on a storage lock timeout. Linked clones from it fail
// ("Linked clone feature is not supported"), so only a rebuild helps. There
// is no Unwrap: errors.Is and errors.As don't reach the cause.
type unconvertedError struct {
	name     string
	vmid     int
	key, vol string
	cause    error // the convert task's error, if it failed
	reused   bool  // found on an existing template rather than after converting
}

func (u *unconvertedError) Error() string {
	if u.reused {
		return fmt.Sprintf("template %s (%d) has disk %s (%s) unconverted: an earlier conversion failed partway, so linked clones from it would fail; deploy with rebuild to recreate it",
			u.name, u.vmid, u.key, u.vol)
	}
	msg := fmt.Sprintf("converting to template left disk %s (%s) unconverted (Proxmox may have timed out on a storage lock); rebuild the template", u.key, u.vol)
	if u.cause != nil {
		msg += "\nthe convert task failed with: " + proxmox.Describe(u.cause)
	}
	return msg
}

// leftoverDisksError is a VM that is deleted but whose disks could not all
// be freed. The error says how to free them by hand.
type leftoverDisksError struct {
	vmid   int
	node   string
	vols   []string
	causes []string // why freeing failed, per volume
	// holder names the VM that has the VMID now; its disks may be the
	// ones listed, so nothing was freed.
	holder string
}

func (l *leftoverDisksError) Error() string {
	if l.holder != "" {
		return fmt.Sprintf("VM %d is deleted, but VMID %d is now held by %s, so the disks the deleted VM left (%s) were not freed: they may be that VM's own now. Check in Proxmox which VM they belong to.",
			l.vmid, l.vmid, l.holder, strings.Join(l.vols, ", "))
	}
	var cmds []string
	for _, v := range l.vols {
		cmds = append(cmds, "`pvesm free "+v+"`")
	}
	msg := fmt.Sprintf("VM %d is deleted, but its disks %s are still on the storage and could not be freed. "+
		"First check that VMID %d is still free (`qm config %d` fails on every node): a VM created there since gets disks with the same names. Then free them by hand: %s on %s",
		l.vmid, strings.Join(l.vols, ", "), l.vmid, l.vmid, strings.Join(cmds, ", "), l.node)
	if len(l.causes) > 0 {
		msg += "\nfreeing failed with: " + strings.Join(l.causes, "; ")
	}
	return msg
}

func (l *leftoverDisksError) Is(target error) bool { return target == errExplained }
