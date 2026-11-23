package sema

import "context"

// Priority represents the admission priority tier of a waiter when the
// semaphore is contended. Higher tiers are dispatched before lower tiers,
// with strict FIFO order within each tier.
//
// The zero value is [PriorityNormal]. Values outside
// [[PriorityLow], [PriorityCritical]] clamp to the nearest valid tier.
type Priority int8

const (
	PriorityLow      Priority = -1
	PriorityNormal   Priority = 0
	PriorityHigh     Priority = 1
	PriorityCritical Priority = 2

	numPriorities = 4
)

var priorityNames = [numPriorities]string{"low", "normal", "high", "critical"}

// String returns the name of the priority tier.
func (p Priority) String() string {
	return priorityNames[priorityBucket(p)]
}

// PrioritySemaphore extends [Semaphore] with O(1) priority-tier admission
// and a lock-free contention signal.
//
// Every [Semaphore] returned by [New], [NewWithObserver], and [Must] also
// implements PrioritySemaphore via type assertion; [NewPriority] constructs
// one directly.
//
// Because Go's runtime does not provide goroutine CPU priorities or OS
// priority inheritance on [sync.Mutex], PrioritySemaphore eliminates
// queue-level priority inversion via strict tier dispatch and exposes
// [PrioritySemaphore.HighestWaitingPriority] so long-running low-priority
// holders can cooperatively yield early.
type PrioritySemaphore interface {
	Semaphore

	// AcquireWithPriority blocks until a slot is available at priority p
	// or ctx is cancelled.
	AcquireWithPriority(ctx context.Context, p Priority) error

	// AcquireNWithPriority atomically acquires n slots at priority p,
	// blocking until all n slots are available or ctx is cancelled.
	AcquireNWithPriority(ctx context.Context, n int, p Priority) error

	// TryAcquireWithPriority attempts a non-blocking single-slot acquire
	// at priority p after checking ctx.Done().
	TryAcquireWithPriority(ctx context.Context, p Priority) error

	// TryAcquireNWithPriority attempts a non-blocking atomic n-slot
	// acquire at priority p after checking ctx.Done().
	TryAcquireNWithPriority(ctx context.Context, n int, p Priority) error

	// HighestWaitingPriority reports the highest priority among currently
	// queued satisfiable waiters, or (PriorityNormal, false) if none are
	// queued. It is a lock-free O(1) atomic snapshot.
	HighestWaitingPriority() (Priority, bool)
}

// NewPriority creates a new [PrioritySemaphore] with capacity c.
// Passing -1 selects the default capacity (10); any other value < 1
// returns [ErrInvalidCap].
func NewPriority(c int) (PrioritySemaphore, error) {
	s, err := New(c)
	if err != nil {
		return nil, err
	}
	return s.(*semaphore), nil
}

var _ PrioritySemaphore = (*semaphore)(nil)
