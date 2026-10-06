package pods

import (
	"context"
	"fmt"
	"sync"

	"github.com/wccomps/battleship/internal/config"
)

// Limits caps concurrent Proxmox work: the executor's config calls, clone
// tasks per source node, template builds, VM deletes and storage operations.
// Executors that run at the same time, such as several jobs in one process,
// should share one Limits so the load on Proxmox doesn't grow with the
// number of jobs.
//
// Deletes and storage operations are capped cluster-wide when Limits has
// Slots (a database): every process that shares the database, battleship
// serve replicas and direct CLI runs alike, takes the same slots. The other
// caps are per process.
//
// Lock order: a holder of more than one slot takes them in the order builds,
// clone slots, deletes, storageOps, config calls, and never waits for an
// earlier one while holding a later one. Clone slots are never held together
// with deletes or storageOps. A config-call slot is held for one try of one
// call, which for a clone, convert or delete is the read before its POST and
// the POST, never while waiting for a task.
type Limits struct {
	calls  sem
	builds sem
	// deletes caps destroy tasks: each takes Proxmox's cluster-wide user.cfg
	// lock to remove the VM from its pool and ACLs, and too many at once time
	// out on it, leaving VMs half-deleted.
	deletes shared
	// storageOps serializes the tasks that take the clone storage's cluster
	// lock for long: template conversions, template destroys and frees of
	// their disks. Several at once time out on the lock ("cfs-lock
	// 'storage-competitions' error: got lock request timeout"), and Proxmox
	// may then leave a template half-converted or a disk behind. It is held
	// around those tasks, including the config-call slots they take to
	// start and check them, never across a clone.
	storageOps shared
	perNode    int

	mu     sync.Mutex
	clones map[string]sem
}

// Slots are counting semaphores shared by every process that uses the same
// database.
type Slots interface {
	// AcquireSlot takes one of n slots called name, waiting until one is free
	// or ctx ends. Calling release frees it.
	AcquireSlot(ctx context.Context, name string, n int) (release func(), err error)
}

// NewLimits makes the limits for one process from its config, capping
// everything in this process only, as for a direct run without a database.
func NewLimits(c config.Concurrency) *Limits { return NewClusterLimits(c, nil) }

// NewClusterLimits is NewLimits, but caps deletes and storage operations
// across every process that shares slots.
func NewClusterLimits(c config.Concurrency, slots Slots) *Limits {
	deletes := max(1, c.Deletes)
	return &Limits{
		calls:      make(sem, c.ConfigCalls),
		builds:     make(sem, max(1, c.TemplateBuilds)),
		deletes:    shared{local: make(sem, deletes), slots: slots, name: "deletes", n: deletes},
		storageOps: shared{local: make(sem, 1), slots: slots, name: "storage-ops", n: 1},
		perNode:    c.ClonesPerNode,
		clones:     map[string]sem{},
	}
}

// Call runs fn while holding one config-call slot (see sem.do). The
// executor and the status poller's drift scan take one for each try of their
// API calls; planning, the grid's VM listing and polling a task or a VM
// waiting on one don't.
func (l *Limits) Call(ctx context.Context, fn func() error) error { return l.calls.do(ctx, fn) }

// cloneSlot returns the semaphore for clones from node.
func (l *Limits) cloneSlot(node string) sem {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.clones[node]
	if !ok {
		s = make(sem, l.perNode)
		l.clones[node] = s
	}
	return s
}

// sem is a counting semaphore with one slot per unit of capacity.
type sem chan struct{}

// do runs fn while holding a slot, waiting for a free one first, and frees
// the slot when fn returns. It doesn't run fn once ctx is done, even if a
// slot is free.
func (s sem) do(ctx context.Context, fn func() error) error {
	select {
	case s <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s }()
	if err := ctx.Err(); err != nil {
		return err // select may take a free slot over a done ctx
	}
	return fn()
}

// shared is a cap taken first in this process, then, with slots, across
// every process.
type shared struct {
	local sem
	slots Slots
	name  string
	n     int
}

func (s shared) do(ctx context.Context, fn func() error) error {
	return s.local.do(ctx, func() error {
		if s.slots == nil {
			return fn()
		}
		release, err := s.slots.AcquireSlot(ctx, s.name, s.n)
		if err != nil {
			return fmt.Errorf("waiting for a cluster-wide %s slot: %w", s.name, err)
		}
		defer release()
		return fn()
	})
}
