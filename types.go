package sema

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Semaphore is a concurrency primitive that limits the number of
// goroutines accessing a shared resource or performing work
// simultaneously. It operates as a counting semaphore backed by a
// channel, with support for context-aware acquisition, bulk
// acquire/release, dynamic capacity changes, graceful draining, and
// real-time utilization metrics.
//
// Create a Semaphore with [New], [NewWithObserver], or [Must].
//
// # Core concept
//
// A Semaphore manages a fixed pool of slots. A goroutine claims a slot
// by acquiring it and returns the slot by releasing it. When all slots
// are held, further acquires block (or fail, for try-variants) until a
// slot is freed. The number of slots is the semaphore's capacity,
// which can be adjusted at runtime via [Semaphore.SetCap].
//
// # Concurrency safety
//
// All methods on Semaphore are safe for concurrent use from multiple
// goroutines. The implementation uses a combination of a mutex,
// condition variable, and atomic operations to coordinate access.
//
// # Method groups
//
// The interface is organized into five groups:
//
// ## Single-slot acquisition
//
// These methods acquire or attempt to acquire exactly one slot:
//
//   - [Semaphore.Acquire] — blocks indefinitely until a slot is available.
//   - [Semaphore.AcquireWith] — blocks until a slot is available or the
//     context is cancelled, returning [ErrAcquireCancelled] on cancellation.
//   - [Semaphore.AcquireTimeout] — convenience wrapper around AcquireWith
//     with a [time.Duration] deadline.
//   - [Semaphore.TryAcquire] — non-blocking; returns true if a slot was
//     claimed, false otherwise.
//   - [Semaphore.TryAcquireWith] — non-blocking with a context check;
//     returns [ErrNoSlot] if no slot is free, or [ErrAcquireCancelled]
//     if the context is already done.
//   - [Semaphore.Release] — frees one held slot; returns
//     [ErrReleaseExceedsCount] if no slots are currently held.
//
// ## Multi-slot (bulk) acquisition
//
// These methods acquire or attempt to acquire n slots at once. Partial
// acquisitions are rolled back automatically on failure or cancellation,
// so the semaphore is never left in a half-acquired state:
//
//   - [Semaphore.AcquireN] — blocks until n slots are available (with a
//     10-minute internal timeout), or returns [ErrNExceedsCap] if n
//     exceeds the capacity.
//   - [Semaphore.AcquireNWith] — blocks until n slots are available or
//     the context fires.
//   - [Semaphore.AcquireNTimeout] — convenience wrapper around
//     AcquireNWith with a [time.Duration] deadline.
//   - [Semaphore.TryAcquireN] — non-blocking; returns true only if all
//     n slots were claimed atomically.
//   - [Semaphore.TryAcquireNWith] — non-blocking with a context check.
//   - [Semaphore.ReleaseN] — frees n held slots; returns
//     [ErrReleaseExceedsCount] if n exceeds the current occupancy.
//
// ## Waiting and draining
//
// These methods support graceful shutdown and lifecycle management:
//
//   - [Semaphore.Wait] — blocks until the semaphore is completely empty
//     (all slots released) or the context is cancelled. Useful for
//     waiting on in-flight work before shutting down.
//   - [Semaphore.Drain] — forcibly removes all held slots from the
//     internal channel. Goroutines that previously acquired slots will
//     receive [ErrReleaseExceedsCount] on their next Release call.
//     Prefer calling Wait first to let in-flight work finish gracefully.
//   - [Semaphore.Reset] — replaces the internal channel with a fresh one
//     of the same capacity, discarding all held slots. Same caveats as
//     Drain regarding in-flight goroutines.
//
// ## Introspection
//
// These methods provide read-only snapshots of the semaphore's state.
// Because the semaphore is concurrent, returned values may be stale by
// the time the caller acts on them — use them for monitoring, logging,
// and heuristics rather than synchronization decisions:
//
//   - [Semaphore.Len] — number of slots currently held.
//   - [Semaphore.Cap] — total capacity (maximum concurrent slots).
//   - [Semaphore.Utilization] — instantaneous utilization as a float64
//     in [0.0, 1.0], computed as Len / Cap.
//   - [Semaphore.UtilizationSmoothed] — exponentially weighted moving
//     average of utilization, updated on each Release/ReleaseN call.
//     Provides a smoothed view of usage over time for adaptive
//     concurrency control or capacity planning.
//   - [Semaphore.IsEmpty] — true when no slots are held (Len == 0).
//   - [Semaphore.IsFull] — true when all slots are held (Len == Cap).
//
// ## Dynamic capacity
//
//   - [Semaphore.SetCap] — adjusts the semaphore's capacity at runtime.
//     Pass -1 to reset to [defaultCap]. If the new capacity is smaller
//     than the current occupancy, existing slots are drained. All
//     blocked acquires are re-evaluated after a capacity change.
//
// # Acquire/release pairing
//
// Every successful acquire (Acquire, AcquireWith, TryAcquire, AcquireN,
// etc.) must be balanced by a corresponding release (Release or
// ReleaseN). The idiomatic pattern uses defer:
//
//	sem.Acquire()
//	defer sem.Release()
//	// ... guarded work ...
//
// For bulk operations:
//
//	if err := sem.AcquireN(5); err != nil {
//	    return err
//	}
//	defer sem.ReleaseN(5)
//	// ... batch work using 5 slots ...
//
// Failing to release a slot permanently reduces the effective capacity
// of the semaphore. Releasing more slots than were acquired returns
// [ErrReleaseExceedsCount].
//
// # Graceful shutdown pattern
//
// A typical shutdown sequence stops accepting new work, waits for
// in-flight operations to complete, then drains any stragglers:
//
//	// 1. Stop dispatching new work (application-specific).
//	close(stopCh)
//
//	// 2. Wait for in-flight work to release its slots.
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//	if err := sem.Wait(ctx); err != nil {
//	    log.Printf("timed out waiting for drain: %v", err)
//	    // 3. Force-drain remaining slots.
//	    _ = sem.Drain()
//	}
//
// # Adaptive concurrency example
//
// The smoothed utilization metric can drive dynamic capacity adjustments:
//
//	go func() {
//	    ticker := time.NewTicker(10 * time.Second)
//	    defer ticker.Stop()
//	    for range ticker.C {
//	        u := sem.UtilizationSmoothed()
//	        switch {
//	        case u > 0.9:
//	            _ = sem.SetCap(sem.Cap() * 2) // scale up
//	        case u < 0.3 && sem.Cap() > minCap:
//	            _ = sem.SetCap(sem.Cap() / 2) // scale down
//	        }
//	    }
//	}()
//
// # HTTP middleware example
//
// Semaphore works well as a request-level concurrency limiter:
//
//	var reqSem = sema.Must(100)
//
//	func LimitMiddleware(next http.Handler) http.Handler {
//	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	        if err := reqSem.AcquireWith(r.Context()); err != nil {
//	            http.Error(w, "service busy", http.StatusServiceUnavailable)
//	            return
//	        }
//	        defer reqSem.Release()
//	        next.ServeHTTP(w, r)
//	    })
//	}
//
// # Worker pool example
//
//	func processItems(ctx context.Context, items []Item) error {
//	    sem := sema.Must(10)
//	    var g errgroup.Group
//
//	    for _, item := range items {
//	        item := item
//	        if err := sem.AcquireWith(ctx); err != nil {
//	            return err
//	        }
//	        g.Go(func() error {
//	            defer sem.Release()
//	            return process(item)
//	        })
//	    }
//
//	    return g.Wait()
//	}
type Semaphore interface {
	// Acquire blocks until a slot is available, then claims it. It waits
	// indefinitely if the semaphore is full. Every successful Acquire
	// must be paired with a [Semaphore.Release] call, typically via defer.
	// Use [Semaphore.AcquireWith] or [Semaphore.AcquireTimeout] when
	// cancellation or a deadline is needed.
	Acquire()

	// AcquireWith blocks until a slot is available or the provided context
	// is cancelled. It returns nil on success, or [ErrAcquireCancelled]
	// wrapping the context error if the context fires first. The underlying
	// cause is accessible via [errors.Unwrap] or [errors.Is] against
	// [context.Canceled] / [context.DeadlineExceeded].
	AcquireWith(ctx context.Context) error

	// AcquireTimeout blocks until a slot is available or the given duration
	// elapses. It is a convenience wrapper around [Semaphore.AcquireWith]
	// with an internally created [context.WithTimeout]. Returns nil on
	// success, or [ErrAcquireCancelled] on timeout.
	AcquireTimeout(d time.Duration) error

	// TryAcquire attempts to claim a slot without blocking. It returns
	// true if a slot was acquired (caller must later call
	// [Semaphore.Release]), or false if the semaphore is currently full.
	// Useful for fast-path checks, load shedding, or fallback logic
	// where blocking is unacceptable.
	TryAcquire() bool

	// TryAcquireWith performs a non-blocking acquire after first checking
	// whether the context is already done. It returns nil on success,
	// [ErrAcquireCancelled] if the context is already cancelled, or
	// [ErrNoSlot] if the semaphore is full. Unlike [Semaphore.AcquireWith],
	// it never blocks waiting for a slot to become available.
	TryAcquireWith(ctx context.Context) error

	// Release frees a single held slot back to the semaphore, waking one
	// or more blocked acquires. It returns [ErrReleaseExceedsCount] if no
	// slots are currently held, indicating a mismatched acquire/release
	// pair or a release after [Semaphore.Drain] / [Semaphore.Reset].
	// The EWMA utilization metric is updated on each successful release.
	Release() error

	// AcquireN acquires n slots from the semaphore, blocking until all n
	// are available. It first attempts a fast non-blocking path and falls
	// back to [Semaphore.AcquireNWith] with a 10-minute internal timeout.
	// Returns [ErrNExceedsCap] if n exceeds the capacity (which would
	// deadlock), or [ErrInvalidN] if n < 1. Partial acquisitions are
	// rolled back automatically on failure.
	AcquireN(n int) error

	// AcquireNWith acquires n slots, blocking until all are claimed or the
	// context is cancelled. If the context fires after some but not all
	// slots have been acquired, the partially acquired slots are released
	// automatically. Returns [ErrAcquireCancelled] on context cancellation,
	// [ErrNExceedsCap] if n exceeds capacity, or [ErrInvalidN] if n < 1.
	AcquireNWith(ctx context.Context, n int) error

	// AcquireNTimeout acquires n slots, blocking until all are claimed or
	// the given duration elapses. It is a convenience wrapper around
	// [Semaphore.AcquireNWith] with an internally created timeout context.
	// Automatic rollback of partial acquisitions applies on timeout.
	AcquireNTimeout(n int, d time.Duration) error

	// TryAcquireN attempts to acquire n slots without blocking. It returns
	// true only if all n slots were claimed atomically; if fewer than n
	// slots are free, no slots are acquired and it returns false. Returns
	// false for invalid n values (n < 1) or if n exceeds the capacity.
	TryAcquireN(n int) bool

	// TryAcquireNWith performs a non-blocking bulk acquire after checking
	// whether the context is already done. It returns nil if all n slots
	// were claimed, [ErrAcquireCancelled] if the context is done,
	// [ErrNExceedsCap] if n exceeds capacity, [ErrInvalidN] if n < 1, or
	// [ErrNoSlot] if not enough slots are currently free.
	TryAcquireNWith(ctx context.Context, n int) error

	// ReleaseN frees n held slots back to the semaphore. It returns
	// [ErrReleaseExceedsCount] if n exceeds the number of currently held
	// slots, or [ErrInvalidN] if n < 1. All blocked acquires are woken
	// after a successful release, and the EWMA utilization metric is
	// updated.
	ReleaseN(n int) error

	// Wait blocks until the semaphore is completely empty (zero slots held)
	// or the context is cancelled. It returns nil when all slots have been
	// released, or [ErrAcquireCancelled] if the context fires first. Wait
	// does not prevent new acquires — coordinate with application-level
	// stop signals to ensure no new work is dispatched while waiting.
	Wait(ctx context.Context) error

	// Drain forcibly removes all held slots from the internal channel.
	// Goroutines that previously acquired slots will receive
	// [ErrReleaseExceedsCount] when they subsequently call
	// [Semaphore.Release]. Use [Semaphore.Wait] to let in-flight work
	// finish gracefully before calling Drain. The EWMA utilization metric
	// is reset to zero. Returns [ErrDrain] if the channel could not be
	// fully emptied.
	Drain() error

	// Reset replaces the internal channel with a fresh one of the same
	// capacity, discarding all held slots and resetting the EWMA metric
	// to zero. Like [Semaphore.Drain], goroutines holding slots from
	// before the reset will receive [ErrReleaseExceedsCount] on their
	// next [Semaphore.Release] call. All blocked acquires are woken and
	// will compete for slots on the new channel.
	Reset() error

	// Len returns the number of slots currently held (acquired but not
	// yet released). This is a point-in-time snapshot and may be stale
	// by the time the caller acts on it. Suitable for monitoring and
	// logging, not for synchronization decisions.
	Len() int

	// Cap returns the total capacity of the semaphore — the maximum
	// number of slots that may be held concurrently. Use
	// [Semaphore.SetCap] to adjust this at runtime.
	Cap() int

	// Utilization returns the instantaneous utilization as a float64 in
	// [0.0, 1.0], computed as Len / Cap. Returns 0 if the capacity is
	// zero. For a temporally smoothed metric, see
	// [Semaphore.UtilizationSmoothed].
	Utilization() float64

	// UtilizationSmoothed returns the exponentially weighted moving
	// average (EWMA) of utilization as a float64 in [0.0, 1.0]. The
	// EWMA is updated on every [Semaphore.Release] and
	// [Semaphore.ReleaseN] call, providing a stable view of usage over
	// time that is useful for adaptive concurrency control and capacity
	// planning decisions.
	UtilizationSmoothed() float64

	// IsEmpty reports whether the semaphore has zero slots currently
	// held (Len == 0). Like [Semaphore.Len], this is a snapshot and
	// may change immediately after the call returns.
	IsEmpty() bool

	// IsFull reports whether all slots are currently held (Len == Cap),
	// meaning any new [Semaphore.Acquire] call would block and any
	// [Semaphore.TryAcquire] call would return false. This is a
	// snapshot and may change immediately after the call returns.
	IsFull() bool

	// SetCap dynamically adjusts the semaphore's capacity to c. Pass -1
	// to reset to [defaultCap]; any other value less than 1 returns
	// [ErrInvalidCap]. If the new capacity is greater than or equal to
	// the current occupancy, all held slots are preserved. If smaller,
	// existing slots are drained (holders will receive
	// [ErrReleaseExceedsCount] on their next release). All blocked
	// acquires are re-evaluated after the resize.
	SetCap(c int) error
}

type (
	// semaphore is the concrete, unexported implementation of the
	// [Semaphore] interface. It uses a channel as the slot pool, a
	// mutex/cond pair for blocking coordination, and atomic values for
	// lock-free reads of the channel pointer and EWMA metric.
	//
	// Fields:
	//   - mu:       guards all mutable state and is the cond's locker.
	//   - cond:     broadcast-only condition variable; signalled on every
	//               Release, ReleaseN, SetCap, Drain, and Reset so that
	//               blocked acquires and Wait calls can re-evaluate.
	//   - ch:       atomic pointer to the slot channel, enabling lock-free
	//               reads from Len, Cap, Utilization, IsEmpty, and IsFull.
	//               Swapped atomically by SetCap and Reset under mu.
	//   - ewma:     stores the smoothed utilization as a uint64-encoded
	//               float64 (via [math.Float64bits] / [math.Float64frombits]).
	//               Updated on every Release and ReleaseN; read lock-free
	//               by [Semaphore.UtilizationSmoothed].
	//   - observer: optional [Observer] receiving lifecycle callbacks.
	//               Nil when created via [New] or [Must]; set by
	//               [NewWithObserver]. Checked on every acquire/release
	//               but incurs no overhead when nil.
	semaphore struct {
		mu       sync.Mutex
		cond     *sync.Cond
		ch       atomic.Pointer[chan struct{}]
		ewma     atomic.Uint64
		observer Observer
	}

	// ErrInvalidCap indicates that an invalid capacity was passed to
	// [New], [NewWithObserver], [Must], or [Semaphore.SetCap]. Valid
	// capacities are any integer >= 1, or the special value -1 which
	// selects [defaultCap].
	//
	//   - Value: the invalid capacity that was provided.
	ErrInvalidCap struct{ Value int }

	// ErrInvalidN indicates that an invalid slot count (n < 1) was
	// passed to a multi-slot method such as [Semaphore.AcquireN],
	// [Semaphore.ReleaseN], or their variants.
	//
	//   - Value: the invalid n that was provided.
	ErrInvalidN struct{ Value int }

	// ErrReleaseExceedsCount indicates that a Release or ReleaseN call
	// attempted to free more slots than are currently held. This
	// typically signals a mismatched acquire/release pair, a double
	// release, or a release after [Semaphore.Drain] / [Semaphore.Reset]
	// has already cleared the slots.
	//
	//   - Attempted: the number of slots the caller tried to release.
	//   - Current:   the number of slots actually held at call time.
	ErrReleaseExceedsCount struct {
		Attempted int
		Current   int
	}

	// ErrNoSlot indicates that a non-blocking acquire
	// ([Semaphore.TryAcquireWith] or [Semaphore.TryAcquireNWith]) could
	// not claim the requested number of slots because the semaphore did
	// not have enough free capacity at the moment of the attempt.
	//
	//   - Requested: the number of slots the caller asked for.
	//   - Available: the number of free slots at the time of the attempt.
	ErrNoSlot struct {
		Requested int
		Available int
	}

	// ErrAcquireCancelled indicates that a context-aware acquire or wait
	// was terminated because the context was cancelled or its deadline
	// expired before the operation could complete. It wraps the
	// underlying context error and implements [errors.Unwrap], so
	// callers can use [errors.Is](err, [context.Canceled]) or
	// [errors.Is](err, [context.DeadlineExceeded]) to distinguish the
	// cause.
	//
	//   - Cause: the underlying context error ([context.Canceled] or
	//     [context.DeadlineExceeded]).
	ErrAcquireCancelled struct{ Cause error }

	// ErrDrain indicates that [Semaphore.Drain] could not fully empty
	// the internal channel. This is an unexpected internal condition
	// and should not occur under normal usage.
	//
	//   - Cause: a human-readable description of what went wrong.
	ErrDrain struct{ Cause string }

	// ErrRecovered wraps a panic that was caught and converted to an
	// error during a multi-slot acquire operation. It preserves both
	// the raw panic value (which may be any type) and, when possible,
	// an error-typed version for use with [errors.Unwrap],
	// [errors.Is], and [errors.As].
	//
	//   - Cause:   the raw value recovered from the panic (any type).
	//   - AsError: the Cause cast to error, or nil if Cause does not
	//     implement the error interface. Used by the Unwrap method.
	ErrRecovered struct {
		Cause   any
		AsError error
	}

	// ErrNExceedsCap indicates that a multi-slot acquire was rejected
	// because the requested count exceeds the semaphore's capacity.
	// Allowing the acquire would deadlock, since there can never be
	// enough slots to satisfy the request.
	//
	//   - Requested: the number of slots the caller asked for.
	//   - Cap:       the semaphore's capacity at the time of the call.
	ErrNExceedsCap struct {
		Requested int
		Cap       int
	}
)
