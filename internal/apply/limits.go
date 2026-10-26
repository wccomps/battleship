package apply

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/wccomps/battleship/internal/config"
)

// Limits caps concurrent Proxmox work: config calls, clones, template
// builds, deletes and storage operations. Concurrent Executors in one
// process should share one. With Slots (a database), clones, deletes and
// storage operations are capped cluster-wide across processes; the rest are
// per process.
//
// Lock order: builds, clones, deletes, storageOps, config calls; never
// wait for an earlier one while holding a later one. Builds and clone slots
// are never held with deletes or storageOps. A config-call slot covers one
// try of one call (e.g. a read and its POST), never a task wait.
type Limits struct {
	calls  sem
	builds sem
	// clones caps clone tasks when config sets it (see Concurrency.Clones);
	// without a cap, do runs fn at once.
	clones shared
	// deletes caps destroy tasks: each takes Proxmox's cluster-wide user.cfg
	// lock, and too many at once time out on it, leaving VMs half-deleted.
	deletes shared
	// storageOps serializes tasks that hold the clone storage's cluster lock
	// for long: template conversions, template destroys and their disk frees.
	// Concurrent ones time out on the lock ("cfs-lock ... got lock request
	// timeout"), which can leave a template half-converted or a disk behind.
	// Held around those tasks and their config calls, never across a clone.
	// Conversions take priority so finished copies unblock team clones ahead of queued destroys.
	storageOps storageOps
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

// NewClusterLimits is NewLimits, but caps clones, deletes and storage
// operations across every process sharing slots.
func NewClusterLimits(c config.Concurrency, slots Slots) *Limits {
	clones, deletes := c.Clones, max(1, c.Deletes)
	return &Limits{
		calls:      make(sem, c.ConfigCalls),
		builds:     make(sem, max(1, c.TemplateBuilds)),
		clones:     shared{local: make(sem, clones), slots: slots, name: "clones", n: clones},
		deletes:    shared{local: make(sem, deletes), slots: slots, name: "deletes", n: deletes},
		storageOps: storageOps{slots: slots},
	}
}

// Call runs fn holding one config-call slot (see sem.do). The executor and
// the drift scan take one per API call try; planning, the grid listing and
// task/VM polling don't.
func (l *Limits) Call(ctx context.Context, fn func() error) error { return l.calls.do(ctx, fn) }

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
	if s.n == 0 {
		return fn() // no cap
	}
	return s.local.do(ctx, func() error {
		return withClusterSlot(ctx, s.slots, s.name, s.n, fn)
	})
}

func withClusterSlot(ctx context.Context, slots Slots, name string, n int, fn func() error) error {
	if slots == nil {
		return fn()
	}
	release, err := slots.AcquireSlot(ctx, name, n)
	if err != nil {
		return fmt.Errorf("waiting for a cluster-wide %s slot: %w", name, err)
	}
	defer release()
	return fn()
}

type storageOps struct {
	mu     sync.Mutex
	held   bool
	hi, lo []chan struct{}
	slots  Slots
}

func (s *storageOps) do(ctx context.Context, high bool, fn func() error) error {
	if err := s.acquire(ctx, high); err != nil {
		return err
	}
	defer s.release()
	return withClusterSlot(ctx, s.slots, "storage-ops", 1, fn)
}

func (s *storageOps) acquire(ctx context.Context, high bool) error {
	s.mu.Lock()
	if !s.held {
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return err
		}
		s.held = true
		s.mu.Unlock()
		return nil
	}
	ready := make(chan struct{})
	q := &s.lo
	if high {
		q = &s.hi
	}
	*q = append(*q, ready)
	s.mu.Unlock()
	select {
	case <-ready:
		if err := ctx.Err(); err != nil {
			s.release()
			return err
		}
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		i := slices.Index(*q, ready)
		if i >= 0 {
			*q = slices.Delete(*q, i, i+1)
		}
		s.mu.Unlock()
		if i < 0 {
			<-ready
			s.release()
		}
		return ctx.Err()
	}
}

func (s *storageOps) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case len(s.hi) > 0:
		close(s.hi[0])
		s.hi = s.hi[1:]
	case len(s.lo) > 0:
		close(s.lo[0])
		s.lo = s.lo[1:]
	default:
		s.held = false
	}
}
