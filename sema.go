package sema

import (
	"context"
	"fmt"
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
//	sem := sema.New(3)
//	sem.Acquire()        // blocks until a slot is available
//	defer sem.Release()  // always release when done
//	// ... do guarded work ...
func (s *semaphore) Acquire() {
	s.mu.Lock()
	for {
		ch := s.channelLocked()
		select {
		case ch <- struct{}{}:
			s.notify(func(o Observer) { o.OnAcquire(len(ch), cap(ch)) })
			s.mu.Unlock()
			return
		default:
		}
		// Channel is full — wait for a Release or SetCap to broadcast.
		s.cond.Wait()
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
	s.mu.Lock()
	for {
		select {
		case <-ctx.Done():
			s.mu.Unlock()
			return ErrAcquireCancelled{Cause: ctx.Err()}
		default:
		}

		ch := s.channelLocked()
		select {
		case ch <- struct{}{}:
			s.notify(func(o Observer) { o.OnAcquire(len(ch), cap(ch)) })
			s.mu.Unlock()
			return nil
		default:
		}

		// Channel is full. We need to wait for a signal while also
		// watching the context. Spawn a goroutine that does a single
		// cond.Wait and then exits, so the outer loop can re-check
		// the (possibly swapped) channel.
		waitDone := make(chan struct{}, 1)
		go func() {
			s.mu.Lock()
			s.cond.Wait()
			s.mu.Unlock()
			waitDone <- struct{}{}
		}()

		s.mu.Unlock()

		select {
		case <-waitDone:
			// Woken by Broadcast (from Release, SetCap, etc).
			// Re-check context before re-acquiring the lock.
			select {
			case <-ctx.Done():
				return ErrAcquireCancelled{Cause: ctx.Err()}
			default:
			}
			s.mu.Lock()
			continue
		case <-ctx.Done():
			// Context cancelled while waiting. Wake the goroutine
			// so it doesn't leak.
			s.cond.Broadcast()
			<-waitDone // wait for goroutine to exit
			return ErrAcquireCancelled{Cause: ctx.Err()}
		}
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
	ch := s.channelLocked()
	select {
	case ch <- struct{}{}:
		s.notify(func(o Observer) { o.OnAcquire(len(ch), cap(ch)) })
		return true
	default:
		return false
	}
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
	ch := s.channelLocked()
	select {
	case ch <- struct{}{}:
		s.notify(func(o Observer) { o.OnAcquire(len(ch), cap(ch)) })
		return nil
	default:
		return ErrNoSlot{Requested: 1, Available: cap(ch) - len(ch)}
	}
}

// Release frees a single slot back to the semaphore, allowing a waiting
// [semaphore.Acquire] call to proceed. It returns [ErrReleaseExceedsCount]
// if no slots are currently held.
//
// Release also updates the EWMA utilization metric and broadcasts to all
// waiters.
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
	ch := s.channelLocked()
	select {
	case <-ch:
		s.notify(func(o Observer) { o.OnRelease(len(ch), cap(ch)) })
		s.updateEWMA(ch)
		s.cond.Broadcast()
		return nil
	default:
		return ErrReleaseExceedsCount{Attempted: 1, Current: len(ch)}
	}
}

// AcquireN acquires n slots from the semaphore. It first attempts a fast
// non-blocking path under a single lock, and falls back to
// [semaphore.AcquireNWith] with a 10-minute timeout if that fails.
//
// It returns [ErrNExceedsCap] if n is greater than the semaphore capacity,
// or an error from [validateN] if n is invalid (e.g. n ≤ 0).
//
// If the acquisition is partially completed and then fails or times out,
// all partially acquired slots are rolled back automatically.
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
	ch := s.channelLocked()
	if n > cap(ch) {
		s.mu.Unlock()
		return ErrNExceedsCap{Requested: n, Cap: cap(ch)}
	}
	// Fast path: try non-blocking acquire under lock.
	if s.tryAcquireNLocked(n) {
		s.notify(func(o Observer) { o.OnAcquire(len(ch), cap(ch)) })
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return s.AcquireNWith(ctx, n)
}

// AcquireNWith acquires n slots from the semaphore, blocking until all n
// are claimed or the context is cancelled. Partial acquisitions are rolled
// back automatically on cancellation or error.
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
	ch := s.channelLocked()
	if n > cap(ch) {
		s.mu.Unlock()
		return ErrNExceedsCap{Requested: n, Cap: cap(ch)}
	}

	acquired := 0

	defer func() {
		// Rollback on any exit where we haven't acquired all n.
		if acquired > 0 && acquired < n {
			rollbackCh := s.channelLocked()
			for j := 0; j < acquired; j++ {
				select {
				case <-rollbackCh:
				default:
				}
			}
			s.cond.Broadcast()
		}
		s.mu.Unlock()
	}()

	for acquired < n {
		select {
		case <-ctx.Done():
			return ErrAcquireCancelled{Cause: ctx.Err()}
		default:
		}

		ch = s.channelLocked()
		select {
		case ch <- struct{}{}:
			acquired++
			continue
		default:
		}

		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				s.cond.Broadcast()
			case <-done:
			}
		}()

		s.cond.Wait()
		close(done)
	}

	ch = s.channelLocked()
	s.notify(func(o Observer) { o.OnAcquire(len(ch), cap(ch)) })
	acquired = n // prevent rollback in defer
	return nil
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
	ch := s.channelLocked()
	if n > cap(ch) {
		return false
	}
	if s.tryAcquireNLocked(n) {
		s.notify(func(o Observer) { o.OnAcquire(len(ch), cap(ch)) })
		return true
	}
	return false
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
	ch := s.channelLocked()
	if n > cap(ch) {
		return ErrNExceedsCap{Requested: n, Cap: cap(ch)}
	}
	if s.tryAcquireNLocked(n) {
		s.notify(func(o Observer) { o.OnAcquire(len(ch), cap(ch)) })
		return nil
	}
	return ErrNoSlot{Requested: n, Available: cap(ch) - len(ch)}
}

// ReleaseN frees n held slots back to the semaphore. It returns
// [ErrReleaseExceedsCount] if n exceeds the number of currently held
// slots, or an error from [validateN] if n is invalid.
//
// The EWMA utilization metric is updated and all waiters are woken after
// the release completes.
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
	ch := s.channelLocked()
	current := len(ch)
	if n > current {
		return ErrReleaseExceedsCount{Attempted: n, Current: current}
	}
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		default:
			return ErrReleaseExceedsCount{Attempted: n - i, Current: len(ch)}
		}
	}
	s.notify(func(o Observer) { o.OnRelease(len(ch), cap(ch)) })
	s.updateEWMA(ch)
	s.cond.Broadcast()
	return nil
}

// Wait blocks until the semaphore is completely empty (no slots held) or
// the context is cancelled. This is useful for graceful shutdown scenarios
// where you want all in-flight work to finish before proceeding.
//
// It returns nil when the semaphore reaches zero, or [ErrAcquireCancelled]
// if the context fires first.
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
	// If already empty, return immediately without spawning a goroutine.
	if s.IsEmpty() {
		s.mu.Unlock()
		s.notify(func(o Observer) { o.OnWaitEnd(nil) })
		return nil
	}

	// Use a done channel to coordinate between the waiter goroutine
	// and context cancellation. The stop flag tells the goroutine to
	// exit when the context is cancelled, preventing a goroutine leak.
	done := make(chan struct{})
	stop := make(chan struct{})

	go func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for !s.IsEmpty() {
			// Check if we've been told to stop before re-waiting.
			select {
			case <-stop:
				return
			default:
			}
			s.cond.Wait()
		}
		close(done)
	}()

	// We held the lock to check IsEmpty above; release it so the
	// goroutine can acquire it.
	s.mu.Unlock()

	select {
	case <-done:
		s.notify(func(o Observer) { o.OnWaitEnd(nil) })
		return nil
	case <-ctx.Done():
		// Signal the goroutine to stop, then broadcast to wake it.
		close(stop)
		s.cond.Broadcast()
		err := ErrAcquireCancelled{Cause: ctx.Err()}
		s.notify(func(o Observer) { o.OnWaitEnd(err) })
		return err
	}
}

// Drain forcibly empties all held slots from the semaphore's internal
// channel. This does NOT coordinate with goroutines that currently hold
// slots — they will receive [ErrReleaseExceedsCount] when they
// subsequently call [semaphore.Release].
//
// Use [semaphore.Wait] to let in-flight work finish gracefully before
// calling Drain. The EWMA utilization metric is reset to zero after
// draining.
//
// It returns [ErrDrain] if the channel could not be fully emptied.
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
	s.drainLocked()
	ch := s.channelLocked()
	if len(ch) != 0 {
		return ErrDrain{Cause: fmt.Sprintf("channel not empty after drain, len=%d", len(ch))}
	}
	s.resetEWMA()
	s.cond.Broadcast()
	return nil
}

// Reset replaces the semaphore's internal channel with a fresh one of the
// same capacity, discarding all held slots. Like [semaphore.Drain],
// goroutines that hold slots from before the reset will receive
// [ErrReleaseExceedsCount] on their next [semaphore.Release] call.
//
// The EWMA utilization metric is reset to zero. All waiters are woken.
//
// Example:
//
//	// Completely reset the semaphore to a clean state.
//	if err := sem.Reset(); err != nil {
//	    log.Printf("reset failed: %v", err)
//	}
//	fmt.Printf("cap=%d, len=%d\n", sem.Cap(), sem.Len()) // cap=N, len=0
func (s *semaphore) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cap(s.channelLocked())
	ch := newChannel(c)
	s.ch.Store(&ch)
	s.resetEWMA()
	s.cond.Broadcast()
	return nil
}

// Len returns the number of slots currently held (acquired but not yet
// released). This is a snapshot and may be stale by the time the caller
// acts on it.
//
// Example:
//
//	fmt.Printf("slots in use: %d\n", sem.Len())
func (s *semaphore) Len() int {
	return len(s.channel())
}

// Cap returns the total capacity of the semaphore — the maximum number of
// slots that may be held concurrently. Use [semaphore.SetCap] to change
// this at runtime.
//
// Example:
//
//	fmt.Printf("max concurrent slots: %d\n", sem.Cap())
func (s *semaphore) Cap() int {
	return cap(s.channel())
}

// Utilization returns the current instantaneous utilization as a float64
// in the range [0.0, 1.0], computed as Len() / Cap(). It returns 0 if the
// capacity is zero.
//
// For a smoothed metric that accounts for historical usage, see
// [semaphore.UtilizationSmoothed].
//
// Example:
//
//	u := sem.Utilization()
//	fmt.Printf("current utilization: %.1f%%\n", u*100)
func (s *semaphore) Utilization() float64 {
	ch := s.channel()
	c := cap(ch)
	if c == 0 {
		return 0
	}
	return float64(len(ch)) / float64(c)
}

// UtilizationSmoothed returns the exponentially weighted moving average
// (EWMA) of utilization as a float64 in [0.0, 1.0]. This value is updated
// on every [semaphore.Release] and [semaphore.ReleaseN] call, providing a
// smoothed view of utilization over time.
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

// IsFull reports whether all slots in the semaphore are currently held,
// meaning any new [semaphore.Acquire] call would block.
//
// Example:
//
//	if sem.IsFull() {
//	    fmt.Println("semaphore at capacity, new acquires will block")
//	}
func (s *semaphore) IsFull() bool {
	return s.Len() == s.Cap()
}

// SetCap dynamically adjusts the semaphore's capacity to c. Passing -1
// resets the capacity to [defaultCap]. Values less than 1 (other than -1)
// return [ErrInvalidCap].
//
// If the new capacity is greater than or equal to the number of currently
// held slots, all held slots are preserved. If the new capacity is smaller,
// the internal channel is drained first (existing holders will receive
// [ErrReleaseExceedsCount] on their next release).
//
// The EWMA metric is updated and all waiters are woken after the resize.
//
// Example:
//
//	// Scale up during peak hours.
//	if err := sem.SetCap(100); err != nil {
//	    log.Printf("set cap failed: %v", err)
//	}
//
//	// Scale back down.
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

	oldCh := s.channelLocked()
	current := len(oldCh)
	newCh := newChannel(c)

	if c >= current {
		for i := 0; i < current; i++ {
			newCh <- struct{}{}
		}
	} else {
		s.drainLocked()
	}

	s.ch.Store(&newCh)
	s.updateEWMA(newCh)
	s.cond.Broadcast()
	return nil
}
