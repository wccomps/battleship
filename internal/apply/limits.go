package apply

import (
	"context"
	"fmt"
	"sync"

	"github.com/wccomps/battleship/internal/config"
)

// Limits caps concurrent Proxmox work: config calls, clones per source
// node, template builds, deletes and storage operations. Concurrent
// Executors in one process should share one. With Slots (a database),
// deletes and storage operations are capped cluster-wide across processes;
// the rest are per process.
//
// Lock order: builds, clone slots, deletes, storageOps, config calls; never
// wait for an earlier one while holding a later one. Clone slots are never
// held with deletes or storageOps. A config-call slot covers one try of one
// call (e.g. a read and its POST), never a task wait.
type Limits struct {
	calls  sem
	builds sem
	// deletes caps destroy tasks: each takes Proxmox's cluster-wide user.cfg
	// lock, and too many at once time out on it, leaving VMs half-deleted.
	deletes shared
	// storageOps serializes tasks that hold the clone storage's cluster lock
	// for long: template conversions, template destroys and their disk frees.
	// Concurrent ones time out on the lock ("cfs-lock ... got lock request
	// timeout"), which can leave a template half-converted or a disk behind.
	// Held around those tasks and their config calls, never across a clone.
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

// NewLimits makes per-process limits from config, as for a direct run
// without a database.
func NewLimits(c config.Concurrency) *Limits { return NewClusterLimits(c, nil) }

// NewClusterLimits is NewLimits, but caps deletes and storage operations
// across every process sharing slots.
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

// Call runs fn holding one config-call slot (see sem.do). The executor and
// the drift scan take one per API call try; planning, the grid listing and
// task/VM polling don't.
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

// do runs fn holding a slot, waiting for one first. It doesn't run fn once
// ctx is done, even if a slot is free.
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

// shared is a cap taken in this process first, then across processes when
// slots are set.
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
