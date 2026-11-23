package sema

import (
	"context"
	"fmt"
)

// Priority represents the admission priority tier of a waiter when the
// semaphore is contended. Higher values are admitted before lower values,
// with strict FIFO ordering preserved within each tier.
//
// The zero value of Priority is [PriorityNormal], matching standard
// [Semaphore] methods. Values outside [[PriorityLow], [PriorityCritical]]
// are clamped safely to the nearest valid tier.
type Priority int8

const (
	// PriorityLow is the lowest admission tier, suitable for background
	// telemetry, cleanup, or deferrable batch work.
	PriorityLow Priority = -1

	// PriorityNormal is the default admission tier used by all standard
	// [Semaphore] methods when no priority is attached to the context.
	PriorityNormal Priority = 0

	// PriorityHigh is an elevated admission tier for latency-sensitive
	// work that should preempt queued Normal and Low waiters.
	PriorityHigh Priority = 1

	// PriorityCritical is the highest admission tier, always dispatched
	// before High, Normal, and Low waiters.
	PriorityCritical Priority = 2

	numPriorities = 4
)

// String returns the human-readable name of the priority tier.
func (p Priority) String() string {
	switch clampPriority(p) {
	case PriorityLow:
		return "low"
	case PriorityNormal:
		return "normal"
	case PriorityHigh:
		return "high"
	case PriorityCritical:
		return "critical"
	default:
		return "normal"
	}
}

type priorityContextKey struct{}

// WithPriority returns a derived context carrying admission priority p.
// Standard [Semaphore] context methods ([Semaphore.AcquireWith],
// [Semaphore.AcquireNWith], [Semaphore.TryAcquireWith], and
// [Semaphore.TryAcquireNWith]) honor this priority whenever the semaphore
// is contended, while incurring zero context lookup overhead on the
// uncontended fast path.
func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityContextKey{}, clampPriority(p))
}

// PriorityFromContext returns the [Priority] attached to ctx by
// [WithPriority], defaulting to [PriorityNormal] if none is present or
// ctx is nil.
func PriorityFromContext(ctx context.Context) Priority {
	if ctx == nil {
		return PriorityNormal
	}
	if p, ok := ctx.Value(priorityContextKey{}).(Priority); ok {
		return clampPriority(p)
	}
	return PriorityNormal
}

// PrioritySemaphore extends [Semaphore] with explicit O(1) priority-tier
// admission and a lock-free contention signal for cooperative
// priority-inversion mitigation.
//
// Every [Semaphore] returned by [New], [NewWithObserver], and [Must] also
// implements PrioritySemaphore, so existing code can opt in via type
// assertion (`ps, ok := sem.(sema.PrioritySemaphore)`) or create one
// directly with [NewPriority], [NewPriorityWithObserver], or [MustPriority].
//
// # Go runtime priority-inversion note
//
// Go's runtime scheduler does not expose goroutine CPU priorities or
// OS-level priority inheritance on [sync.Mutex]. PrioritySemaphore
// eliminates queue-level priority inversion via strict O(1) tier dispatch
// and provides [PrioritySemaphore.HighestWaitingPriority] so long-running
// low-priority holders can cooperatively checkpoint and release early
// whenever a higher-priority waiter is blocked.
type PrioritySemaphore interface {
	Semaphore

	// AcquireWithPriority blocks until a slot is available at priority p
	// or ctx is cancelled. Waiters are dispatched strictly in tier order
	// (Critical > High > Normal > Low) and FIFO within each tier.
	AcquireWithPriority(ctx context.Context, p Priority) error

	// AcquireNWithPriority atomically acquires n slots at priority p,
	// blocking until all n slots are available or ctx is cancelled.
	AcquireNWithPriority(ctx context.Context, n int, p Priority) error

	// TryAcquireWithPriority attempts a non-blocking single-slot acquire
	// at priority p. A strictly higher priority caller may succeed ahead
	// of lower-priority queued waiters; same-or-lower priority callers
	// return [ErrNoSlot] when a higher-or-equal waiter is queued.
	TryAcquireWithPriority(ctx context.Context, p Priority) error

	// TryAcquireNWithPriority attempts a non-blocking atomic n-slot
	// acquire at priority p after checking ctx.Done().
	TryAcquireNWithPriority(ctx context.Context, n int, p Priority) error

	// HighestWaitingPriority reports the highest priority tier among
	// currently queued satisfiable waiters, or (PriorityNormal, false) if
	// no satisfiable waiters are queued. It is a lock-free O(1) atomic
	// snapshot suitable for cooperative yielding inside long-running
	// low-priority holders.
	HighestWaitingPriority() (Priority, bool)
}

// NewPriority creates a new [PrioritySemaphore] with capacity c.
// Passing -1 selects [defaultCap]; any other value < 1 returns [ErrInvalidCap].
func NewPriority(c int) (PrioritySemaphore, error) {
	s, err := New(c)
	if err != nil {
		return nil, err
	}
	return s.(*semaphore), nil
}

// NewPriorityWithObserver creates a new [PrioritySemaphore] with capacity c
// and attaches obs to receive lifecycle callbacks.
func NewPriorityWithObserver(c int, obs Observer) (PrioritySemaphore, error) {
	s, err := NewWithObserver(c, obs)
	if err != nil {
		return nil, err
	}
	return s.(*semaphore), nil
}

// MustPriority is like [NewPriority] but panics if c is invalid.
func MustPriority(c int) PrioritySemaphore {
	s, err := NewPriority(c)
	if err != nil {
		panic(fmt.Sprintf("sema.MustPriority: %v", err))
	}
	return s
}

// Compile-time interface assertions.
var (
	_ Semaphore         = (*semaphore)(nil)
	_ PrioritySemaphore = (*semaphore)(nil)
)
