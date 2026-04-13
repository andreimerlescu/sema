package sema

import (
	"context"
	"fmt"
	"math"
	"time"
)

// --- single slot ---

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

func (s *semaphore) AcquireTimeout(d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return s.AcquireWith(ctx)
}

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

// --- multi slot ---

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

func (s *semaphore) AcquireNTimeout(n int, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return s.AcquireNWith(ctx, n)
}

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

// --- introspection ---

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

// Drain forcibly empties all held slots from the channel. This does NOT
// coordinate with goroutines that hold slots — they will receive
// ErrReleaseExceedsCount when they subsequently call Release. Use Wait
// to let in-flight work finish gracefully before calling Drain. The EWMA
// is reset to zero after draining.
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

// Reset replaces the internal channel with a fresh one, discarding all
// held slots. Like Drain, goroutines that hold slots from before the
// reset will receive ErrReleaseExceedsCount. Cap is preserved. EWMA is
// reset to zero.
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

func (s *semaphore) Len() int {
	return len(s.channel())
}

func (s *semaphore) Cap() int {
	return cap(s.channel())
}

func (s *semaphore) Utilization() float64 {
	ch := s.channel()
	c := cap(ch)
	if c == 0 {
		return 0
	}
	return float64(len(ch)) / float64(c)
}

func (s *semaphore) UtilizationSmoothed() float64 {
	return math.Float64frombits(s.ewma.Load())
}

func (s *semaphore) IsEmpty() bool {
	return s.Len() == 0
}

func (s *semaphore) IsFull() bool {
	return s.Len() == s.Cap()
}

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
