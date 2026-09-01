// Package capacity coordinates process-wide instance capacity.
package capacity

import (
	"context"
	"sync"
)

// Broker arbitrates runner capacity across all registered scale sets. Capacity
// needed to satisfy another scale set's minimum is reserved before surplus is
// handed out, and surplus waiters are served in FIFO order.
//
// A non-positive limit is unlimited.
type Broker struct {
	mu      sync.Mutex
	limit   int
	inUse   int
	groups  map[string]*group
	waiters []*waiter
}

type group struct {
	name    string
	minimum int
	inUse   int
}

type waiter struct {
	group     *group
	ready     chan struct{}
	lease     *Lease
	granted   bool
	cancelled bool
}

// Allocator is one scale set's view of a process-wide Broker.
type Allocator struct {
	broker *Broker
	group  *group
}

// NewBroker creates a process-wide capacity broker.
func NewBroker(limit int) *Broker {
	return &Broker{limit: limit, groups: make(map[string]*group)}
}

// Register creates an allocator for a scale set. Callers must register every
// scale set before any of them starts acquiring capacity so all minimums are
// reserved from the beginning.
func (b *Broker) Register(name string, minimum int) *Allocator {
	if b == nil {
		return &Allocator{}
	}
	if minimum < 0 {
		minimum = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.groups[name]; exists {
		panic("capacity: duplicate allocator name " + name)
	}
	g := &group{name: name, minimum: minimum}
	b.groups[name] = g
	return &Allocator{broker: b, group: g}
}

// Acquire waits until this scale set may use one instance slot or ctx is
// cancelled. Requests below a scale set's minimum take priority over surplus.
func (a *Allocator) Acquire(ctx context.Context) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || a.broker == nil || a.group == nil {
		return &Lease{}, nil
	}

	b := a.broker
	w := &waiter{group: a.group, ready: make(chan struct{})}
	b.mu.Lock()
	b.waiters = append(b.waiters, w)
	b.dispatchLocked()
	if w.granted {
		lease := w.lease
		b.mu.Unlock()
		return lease, nil
	}
	b.mu.Unlock()

	select {
	case <-w.ready:
		return w.lease, nil
	case <-ctx.Done():
		b.mu.Lock()
		if w.granted {
			lease := w.lease
			b.mu.Unlock()
			lease.Release()
			return nil, ctx.Err()
		}
		w.cancelled = true
		b.dispatchLocked()
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Limit returns the configured process-wide limit. A non-positive value means
// unlimited.
func (b *Broker) Limit() int {
	if b == nil {
		return 0
	}
	return b.limit
}

// InUse returns the number of currently leased instance slots.
func (b *Broker) InUse() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inUse
}

func (b *Broker) dispatchLocked() {
	for b.limit <= 0 || b.inUse < b.limit {
		idx := b.nextWaiterLocked(true)
		if idx < 0 {
			idx = b.nextWaiterLocked(false)
		}
		if idx < 0 {
			break
		}
		w := b.waiters[idx]
		b.waiters = append(b.waiters[:idx], b.waiters[idx+1:]...)
		b.inUse++
		w.group.inUse++
		w.lease = &Lease{broker: b, group: w.group}
		w.granted = true
		close(w.ready)
	}
	b.compactWaitersLocked()
}

// nextWaiterLocked returns the first eligible waiter in the requested class.
// Minimum waiters are dispatched before surplus waiters; FIFO is preserved
// within each class.
func (b *Broker) nextWaiterLocked(minimum bool) int {
	for i, w := range b.waiters {
		if w.cancelled {
			continue
		}
		belowMinimum := w.group.inUse < w.group.minimum
		if belowMinimum != minimum {
			continue
		}
		if belowMinimum || b.surplusAvailableLocked(w.group) {
			return i
		}
	}
	return -1
}

func (b *Broker) surplusAvailableLocked(requesting *group) bool {
	if b.limit <= 0 {
		return true
	}
	availableAfterGrant := b.limit - b.inUse - 1
	reservedForOthers := 0
	for _, g := range b.groups {
		if g == requesting || g.inUse >= g.minimum {
			continue
		}
		reservedForOthers += g.minimum - g.inUse
	}
	return availableAfterGrant >= reservedForOthers
}

func (b *Broker) compactWaitersLocked() {
	kept := b.waiters[:0]
	for _, w := range b.waiters {
		if !w.cancelled {
			kept = append(kept, w)
		}
	}
	b.waiters = kept
}

// Lease owns one Broker slot. Release is idempotent so lifecycle cleanup can
// safely converge from job-complete, crash, drain, and shutdown paths.
type Lease struct {
	broker *Broker
	group  *group
	once   sync.Once
}

// Release returns the slot to the broker.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.broker == nil || l.group == nil {
			return
		}
		b := l.broker
		b.mu.Lock()
		b.inUse--
		l.group.inUse--
		b.dispatchLocked()
		b.mu.Unlock()
	})
}
