package sema

import (
	"context"
	"math"
	"time"
)

// Acquire blocks until a slot is available in the semaphore, then claims it.
// It will wait indefinitely if no slots are free. Use [semaphore.AcquireWith]
// or [semaphore.AcquireTimeout] if you need cancellation or a deadline.
//
// Acquire is safe for concurrent use.
//
// Example:
//
//	sem := sema.Must(3)
//	sem.Acquire()        // blocks until a slot is available
//	defer sem.Release()  // always release when done
//	// ... do guarded work ...
func (s *semaphore) Acquire() {
	s.mu.Lock()
	if !s.hasSatisfiableWaiterLocked() && s.held+1 <= s.cap {
		s.held++
		s.syncAtomicsLocked()
		s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
		s.mu.Unlock()
		return
	}
	w := s.enqueueWaiterLocked(1)
	s.mu.Unlock()

	<-w.ready

	if s.observer != nil {
		s.mu.Lock()
		s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
		s.mu.Unlock()
	}
}

// AcquireWith blocks until a slot is available or the provided context is
// cancelled/expired. It returns nil on success, or [ErrAcquireCancelled]
// if the context fires before a slot becomes available.
//
// Example:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	defer cancel()
//
//	if err := sem.AcquireWith(ctx); err != nil {
//	    log.Printf("could not acquire: %v", err)
//	    return
//	}
//	defer sem.Release()
//	// ... do guarded work ...
func (s *semaphore) AcquireWith(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ErrAcquireCancelled{Cause: ctx.Err()}
	default:
	}

	s.mu.Lock()
	if !s.hasSatisfiableWaiterLocked() && s.held+1 <= s.cap {
		s.held++
		s.syncAtomicsLocked()
		s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
		s.mu.Unlock()
		return nil
	}
	w := s.enqueueWaiterLocked(1)
	s.mu.Unlock()

	return s.awaitWaiter(ctx, w)
}

func (s *semaphore) awaitWaiter(ctx context.Context, w *waiter) error {
	select {
	case <-w.ready:
		if s.observer != nil {
			s.mu.Lock()
			s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
			s.mu.Unlock()
		}
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		if w.granted {
			// Slots were handed to w right as ctx fired; return them cleanly
			// unless a Drain/Reset already cleared that generation.
			if w.grantGen == s.gen {
				s.held -= w.n
				if s.held < 0 {
					s.held = 0
				}
				s.syncAtomicsLocked()
				s.notifyEmptyLocked()
				s.dispatchLocked()
			}
			s.mu.Unlock()
			return ErrAcquireCancelled{Cause: ctx.Err()}
		}
		s.removeWaiterLocked(w)
		s.dispatchLocked()
		s.mu.Unlock()
		return ErrAcquireCancelled{Cause: ctx.Err()}
	}
}

// AcquireTimeout is a convenience wrapper around [semaphore.AcquireWith]
// that creates a context with the given timeout duration. It returns nil
// on success, or [ErrAcquireCancelled] if the timeout elapses first.
//
// Example:
//
//	if err := sem.AcquireTimeout(2 * time.Second); err != nil {
//	    log.Println("timed out waiting for semaphore")
//	    return
//	}
//	defer sem.Release()
func (s *semaphore) AcquireTimeout(d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return s.AcquireWith(ctx)
}

// TryAcquire attempts to acquire a single slot without blocking. It returns
// true if a slot was claimed, or false if the semaphore is currently full.
//
// Example:
//
//	if sem.TryAcquire() {
//	    defer sem.Release()
//	    // ... do guarded work ...
//	} else {
//	    log.Println("semaphore full, skipping work")
//	}
func (s *semaphore) TryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasSatisfiableWaiterLocked() || s.held+1 > s.cap {
		return false
	}
	s.held++
	s.syncAtomicsLocked()
	s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
	return true
}

// TryAcquireWith attempts a non-blocking acquire, but first checks whether
// the provided context has already been cancelled. It returns nil on success,
// [ErrAcquireCancelled] if the context is done, or [ErrNoSlot] if no slot
// is available.
//
// Example:
//
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//
//	switch err := sem.TryAcquireWith(ctx); {
//	case err == nil:
//	    defer sem.Release()
//	    // ... do guarded work ...
//	case errors.As(err, &sema.ErrNoSlot{}):
//	    log.Println("no slots available right now")
//	default:
//	    log.Printf("context cancelled: %v", err)
//	}
func (s *semaphore) TryAcquireWith(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ErrAcquireCancelled{Cause: ctx.Err()}
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasSatisfiableWaiterLocked() || s.held+1 > s.cap {
		return ErrNoSlot{Requested: 1, Available: s.availableLocked()}
	}
	s.held++
	s.syncAtomicsLocked()
	s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
	return nil
}

// Release frees a single slot back to the semaphore, allowing waiting
// acquires to proceed in FIFO order. It returns [ErrReleaseExceedsCount]
// if no slots are currently held.
//
// Release also updates the EWMA utilization metric and wakes Wait callers
// if occupancy reaches zero.
//
// Example:
//
//	sem.Acquire()
//	// ... do work ...
//	if err := sem.Release(); err != nil {
//	    log.Printf("release error: %v", err)
//	}
func (s *semaphore) Release() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held < 1 {
		return ErrReleaseExceedsCount{Attempted: 1, Current: s.held}
	}
	s.held--
	s.syncAtomicsLocked()
	s.notify(func(o Observer) { o.OnRelease(s.held, s.cap) })
	s.updateEWMALocked()
	s.notifyEmptyLocked()
	s.dispatchLocked()
	return nil
}

// AcquireN acquires n slots from the semaphore atomically. It first attempts
// a fast non-blocking path under a single lock, and falls back to
// [semaphore.AcquireNWith] with a 10-minute timeout if that fails.
//
// It returns [ErrNExceedsCap] if n is greater than the semaphore capacity,
// or an error from [validateN] if n is invalid (e.g. n ≤ 0).
//
// Example:
//
//	// Acquire 5 slots for a batch operation.
//	if err := sem.AcquireN(5); err != nil {
//	    log.Printf("batch acquire failed: %v", err)
//	    return
//	}
//	defer sem.ReleaseN(5)
//	// ... do batch work using 5 concurrent slots ...
func (s *semaphore) AcquireN(n int) error {
	if err := validateN(n); err != nil {
		return err
	}
	s.mu.Lock()
	if n > s.cap {
		c := s.cap
		s.mu.Unlock()
		return ErrNExceedsCap{Requested: n, Cap: c}
	}
	if !s.hasSatisfiableWaiterLocked() && s.held+n <= s.cap {
		s.held += n
		s.syncAtomicsLocked()
		s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return s.AcquireNWith(ctx, n)
}

// AcquireNWith acquires n slots from the semaphore atomically, blocking
// until all n are claimed or the context is cancelled. No slots are held
// while waiting, preventing partial-acquisition deadlocks.
//
// It returns [ErrNExceedsCap] if n exceeds the semaphore capacity, or
// [ErrAcquireCancelled] if the context fires before all slots are acquired.
//
// Example:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//
//	if err := sem.AcquireNWith(ctx, 3); err != nil {
//	    log.Printf("could not acquire 3 slots: %v", err)
//	    return
//	}
//	defer sem.ReleaseN(3)
//	// ... do work requiring 3 concurrent slots ...
func (s *semaphore) AcquireNWith(ctx context.Context, n int) error {
	if err := validateN(n); err != nil {
		return err
	}
	s.mu.Lock()
	if n > s.cap {
		c := s.cap
		s.mu.Unlock()
		return ErrNExceedsCap{Requested: n, Cap: c}
	}
	select {
	case <-ctx.Done():
		s.mu.Unlock()
		return ErrAcquireCancelled{Cause: ctx.Err()}
	default:
	}
	if !s.hasSatisfiableWaiterLocked() && s.held+n <= s.cap {
		s.held += n
		s.syncAtomicsLocked()
		s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
		s.mu.Unlock()
		return nil
	}
	w := s.enqueueWaiterLocked(n)
	s.mu.Unlock()

	return s.awaitWaiter(ctx, w)
}

// AcquireNTimeout is a convenience wrapper around [semaphore.AcquireNWith]
// that creates a context with the given timeout duration.
//
// Example:
//
//	if err := sem.AcquireNTimeout(4, 10*time.Second); err != nil {
//	    log.Println("timed out acquiring 4 slots")
//	    return
//	}
//	defer sem.ReleaseN(4)
func (s *semaphore) AcquireNTimeout(n int, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return s.AcquireNWith(ctx, n)
}

// TryAcquireN attempts to acquire n slots without blocking. It returns true
// only if all n slots were claimed atomically; otherwise it returns false
// and no slots are held.
//
// Example:
//
//	if sem.TryAcquireN(3) {
//	    defer sem.ReleaseN(3)
//	    // ... do batch work ...
//	} else {
//	    log.Println("not enough slots available")
//	}
func (s *semaphore) TryAcquireN(n int) bool {
	if err := validateN(n); err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > s.cap || s.hasSatisfiableWaiterLocked() || s.held+n > s.cap {
		return false
	}
	s.held += n
	s.syncAtomicsLocked()
	s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
	return true
}

// TryAcquireNWith performs a non-blocking multi-slot acquire after first
// checking whether the context is already cancelled. It returns nil if all
// n slots were claimed, [ErrAcquireCancelled] if the context is done,
// [ErrNExceedsCap] if n exceeds capacity, or [ErrNoSlot] if not enough
// slots are free.
//
// Example:
//
//	ctx := context.Background()
//	if err := sem.TryAcquireNWith(ctx, 2); err != nil {
//	    log.Printf("try acquire 2 failed: %v", err)
//	    return
//	}
//	defer sem.ReleaseN(2)
func (s *semaphore) TryAcquireNWith(ctx context.Context, n int) error {
	if err := validateN(n); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ErrAcquireCancelled{Cause: ctx.Err()}
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > s.cap {
		return ErrNExceedsCap{Requested: n, Cap: s.cap}
	}
	if s.hasSatisfiableWaiterLocked() || s.held+n > s.cap {
		return ErrNoSlot{Requested: n, Available: s.availableLocked()}
	}
	s.held += n
	s.syncAtomicsLocked()
	s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
	return nil
}

// ReleaseN frees n held slots back to the semaphore. It returns
// [ErrReleaseExceedsCount] if n exceeds the number of currently held
// slots, or an error from [validateN] if n is invalid.
//
// The EWMA utilization metric is updated, Wait callers are woken if
// occupancy reaches zero, and queued acquires are dispatched in FIFO order.
//
// Example:
//
//	sem.AcquireN(5)
//	// ... do batch work ...
//	if err := sem.ReleaseN(5); err != nil {
//	    log.Printf("release error: %v", err)
//	}
func (s *semaphore) ReleaseN(n int) error {
	if err := validateN(n); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > s.held {
		return ErrReleaseExceedsCount{Attempted: n, Current: s.held}
	}
	s.held -= n
	s.syncAtomicsLocked()
	s.notify(func(o Observer) { o.OnRelease(s.held, s.cap) })
	s.updateEWMALocked()
	s.notifyEmptyLocked()
	s.dispatchLocked()
	return nil
}

// Wait blocks until the semaphore is completely empty (no slots held) or
// the context is cancelled. This is useful for graceful shutdown scenarios
// where you want all in-flight work to finish before proceeding.
//
// It returns nil when the semaphore reaches zero, or [ErrAcquireCancelled]
// if the context fires first. Wait spawns no helper goroutines.
//
// Example:
//
//	// Signal workers to stop, then wait for in-flight work to drain.
//	close(stopCh)
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//
//	if err := sem.Wait(ctx); err != nil {
//	    log.Println("timed out waiting for semaphore to drain")
//	} else {
//	    log.Println("all slots released, safe to shut down")
//	}
func (s *semaphore) Wait(ctx context.Context) error {
	s.notify(func(o Observer) { o.OnWaitStart() })

	s.mu.Lock()
	if s.held == 0 {
		s.mu.Unlock()
		s.notify(func(o Observer) { o.OnWaitEnd(nil) })
		return nil
	}
	if s.emptyCh == nil {
		s.emptyCh = make(chan struct{})
	}
	ch := s.emptyCh
	s.mu.Unlock()

	select {
	case <-ch:
		s.notify(func(o Observer) { o.OnWaitEnd(nil) })
		return nil
	case <-ctx.Done():
		err := ErrAcquireCancelled{Cause: ctx.Err()}
		s.notify(func(o Observer) { o.OnWaitEnd(err) })
		return err
	}
}

// Drain forcibly clears all held slots from the semaphore. This does NOT
// coordinate with goroutines that currently hold slots — they will receive
// [ErrReleaseExceedsCount] when they subsequently call [semaphore.Release].
//
// Use [semaphore.Wait] to let in-flight work finish gracefully before
// calling Drain. The EWMA utilization metric is reset to zero, blocked
// [semaphore.Wait] callers are woken first, and queued acquires are then
// dispatched.
//
// Example:
//
//	// Graceful: wait for work to finish, then drain any stragglers.
//	_ = sem.Wait(ctx)
//
//	if err := sem.Drain(); err != nil {
//	    log.Printf("drain failed: %v", err)
//	}
//	log.Printf("semaphore drained, len=%d", sem.Len())
func (s *semaphore) Drain() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = 0
	s.gen++
	s.syncAtomicsLocked()
	s.resetEWMA()
	s.notifyEmptyLocked()
	s.dispatchLocked()
	return nil
}

// Reset forcibly clears all held slots while preserving capacity. Like
// [semaphore.Drain], goroutines that hold slots from before the reset will
// receive [ErrReleaseExceedsCount] on their next [semaphore.Release] call.
//
// The EWMA utilization metric is reset to zero, blocked [semaphore.Wait]
// callers are woken, and queued acquires are dispatched.
//
// Example:
//
//	// Completely reset the semaphore to a clean state.
//	if err := sem.Reset(); err != nil {
//	    log.Printf("reset failed: %v", err)
//	}
//	fmt.Printf("cap=%d, len=%d\n", sem.Cap(), sem.Len()) // cap=N, len=0
func (s *semaphore) Reset() error {
	return s.Drain()
}

// Len returns the number of slots currently held (acquired but not yet
// released). This is a snapshot and may be stale by the time the caller
// acts on it.
//
// Example:
//
//	fmt.Printf("slots in use: %d\n", sem.Len())
func (s *semaphore) Len() int {
	return int(s.heldAtom.Load())
}

// Cap returns the total capacity of the semaphore — the maximum number of
// slots that may be held concurrently. Use [semaphore.SetCap] to change
// this at runtime.
//
// Example:
//
//	fmt.Printf("max concurrent slots: %d\n", sem.Cap())
func (s *semaphore) Cap() int {
	return int(s.capAtom.Load())
}

// Utilization returns the current instantaneous utilization as a float64
// in the range [0.0, 1.0], computed as Len() / Cap() and clamped to 1.0
// while oversubscribed during a shrink drain. It returns 0 if the capacity
// is zero.
//
// For a smoothed metric that accounts for historical usage, see
// [semaphore.UtilizationSmoothed].
//
// Example:
//
//	u := sem.Utilization()
//	fmt.Printf("current utilization: %.1f%%\n", u*100)
func (s *semaphore) Utilization() float64 {
	return clampUtilization(s.Len(), s.Cap())
}

// UtilizationSmoothed returns the exponentially weighted moving average
// (EWMA) of utilization as a float64 clamped to [0.0, 1.0]. This value is
// updated on every [semaphore.Release], [semaphore.ReleaseN], and
// [semaphore.SetCap] call, providing a smoothed view of utilization over time.
//
// Example:
//
//	smoothed := sem.UtilizationSmoothed()
//	fmt.Printf("smoothed utilization: %.1f%%\n", smoothed*100)
func (s *semaphore) UtilizationSmoothed() float64 {
	return math.Float64frombits(s.ewma.Load())
}

// IsEmpty reports whether the semaphore has zero slots currently held.
//
// Example:
//
//	if sem.IsEmpty() {
//	    fmt.Println("no work in progress")
//	}
func (s *semaphore) IsEmpty() bool {
	return s.Len() == 0
}

// IsFull reports whether occupancy is at or above capacity (Len >= Cap),
// meaning any new [semaphore.Acquire] call would block.
//
// Example:
//
//	if sem.IsFull() {
//	    fmt.Println("semaphore at capacity, new acquires will block")
//	}
func (s *semaphore) IsFull() bool {
	return s.Len() >= s.Cap()
}

// SetCap dynamically adjusts the semaphore's capacity to c. Passing -1
// resets the capacity to [defaultCap]. Values less than 1 (other than -1)
// return [ErrInvalidCap].
//
// All currently held slots are preserved regardless of whether capacity
// expands or shrinks. When shrinking below current occupancy, no new slots
// are granted until occupancy drops below c, and existing holders release
// normally without [ErrReleaseExceedsCount]. While oversubscribed,
// [semaphore.IsFull] reports true and utilization metrics remain clamped
// to [0.0, 1.0].
//
// The EWMA metric is updated and queued waiters are dispatched in FIFO
// order as capacity allows.
//
// Example:
//
//	// Scale up during peak hours.
//	if err := sem.SetCap(100); err != nil {
//	    log.Printf("set cap failed: %v", err)
//	}
//
//	// Scale back down without interrupting in-flight holders.
//	if err := sem.SetCap(10); err != nil {
//	    log.Printf("set cap failed: %v", err)
//	}
//
//	// Reset to default capacity.
//	_ = sem.SetCap(-1)
func (s *semaphore) SetCap(c int) error {
	if c == -1 {
		c = defaultCap
	}
	if c < 1 {
		return ErrInvalidCap{Value: c}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cap = c
	s.syncAtomicsLocked()
	s.updateEWMALocked()
	s.dispatchLocked()
	return nil
}
