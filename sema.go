package sema

import (
	"context"
	"math"
	"math/bits"
	"time"
)

func (s *semaphore) Acquire() {
	s.mu.Lock()
	if !s.hasHigherOrEqualWaiterLocked(PriorityNormal) && s.canFitLocked(1) {
		s.grantLocked(1)
		s.mu.Unlock()
		return
	}
	w := s.enqueueWaiterLocked(1, PriorityNormal)
	s.mu.Unlock()

	<-w.ready

	if s.observer != nil {
		s.mu.Lock()
		s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
		s.mu.Unlock()
	}
}

func (s *semaphore) AcquireWith(ctx context.Context) error {
	return s.AcquireWithPriority(ctx, PriorityNormal)
}

func (s *semaphore) AcquireWithPriority(ctx context.Context, p Priority) error {
	select {
	case <-ctx.Done():
		return ErrAcquireCancelled{Cause: ctx.Err()}
	default:
	}

	p = clampPriority(p)

	s.mu.Lock()
	if !s.hasHigherOrEqualWaiterLocked(p) && s.canFitLocked(1) {
		s.grantLocked(1)
		s.mu.Unlock()
		return nil
	}
	w := s.enqueueWaiterLocked(1, p)
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
			// Return slots granted just as ctx fired unless Drain/Reset already
			// cleared that generation.
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

func (s *semaphore) AcquireTimeout(d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return s.AcquireWithPriority(ctx, PriorityNormal)
}

func (s *semaphore) TryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasHigherOrEqualWaiterLocked(PriorityNormal) || !s.canFitLocked(1) {
		return false
	}
	s.grantLocked(1)
	return true
}

func (s *semaphore) TryAcquireWith(ctx context.Context) error {
	return s.TryAcquireWithPriority(ctx, PriorityNormal)
}

func (s *semaphore) TryAcquireWithPriority(ctx context.Context, p Priority) error {
	select {
	case <-ctx.Done():
		return ErrAcquireCancelled{Cause: ctx.Err()}
	default:
	}
	p = clampPriority(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasHigherOrEqualWaiterLocked(p) || !s.canFitLocked(1) {
		return ErrNoSlot{Requested: 1, Available: s.availableLocked()}
	}
	s.grantLocked(1)
	return nil
}

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
	if !s.hasHigherOrEqualWaiterLocked(PriorityNormal) && s.canFitLocked(n) {
		s.grantLocked(n)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return s.AcquireNWithPriority(ctx, n, PriorityNormal)
}

func (s *semaphore) AcquireNWith(ctx context.Context, n int) error {
	return s.AcquireNWithPriority(ctx, n, PriorityNormal)
}

func (s *semaphore) AcquireNWithPriority(ctx context.Context, n int, p Priority) error {
	if err := validateN(n); err != nil {
		return err
	}
	p = clampPriority(p)
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
	if !s.hasHigherOrEqualWaiterLocked(p) && s.canFitLocked(n) {
		s.grantLocked(n)
		s.mu.Unlock()
		return nil
	}
	w := s.enqueueWaiterLocked(n, p)
	s.mu.Unlock()

	return s.awaitWaiter(ctx, w)
}

func (s *semaphore) AcquireNTimeout(n int, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return s.AcquireNWithPriority(ctx, n, PriorityNormal)
}

func (s *semaphore) TryAcquireN(n int) bool {
	if err := validateN(n); err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > s.cap || s.hasHigherOrEqualWaiterLocked(PriorityNormal) || !s.canFitLocked(n) {
		return false
	}
	s.grantLocked(n)
	return true
}

func (s *semaphore) TryAcquireNWith(ctx context.Context, n int) error {
	return s.TryAcquireNWithPriority(ctx, n, PriorityNormal)
}

func (s *semaphore) TryAcquireNWithPriority(ctx context.Context, n int, p Priority) error {
	if err := validateN(n); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ErrAcquireCancelled{Cause: ctx.Err()}
	default:
	}
	p = clampPriority(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > s.cap {
		return ErrNExceedsCap{Requested: n, Cap: s.cap}
	}
	if s.hasHigherOrEqualWaiterLocked(p) || !s.canFitLocked(n) {
		return ErrNoSlot{Requested: n, Available: s.availableLocked()}
	}
	s.grantLocked(n)
	return nil
}

func (s *semaphore) HighestWaitingPriority() (Priority, bool) {
	mask := uint8(s.maskAtom.Load())
	if mask == 0 {
		return PriorityNormal, false
	}
	return bucketPriority(31 - bits.LeadingZeros32(uint32(mask))), true
}

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

func (s *semaphore) Reset() error {
	return s.Drain()
}

func (s *semaphore) Len() int {
	return int(s.heldAtom.Load())
}

func (s *semaphore) Cap() int {
	return int(s.capAtom.Load())
}

func (s *semaphore) Utilization() float64 {
	return clampUtilization(s.Len(), s.Cap())
}

func (s *semaphore) UtilizationSmoothed() float64 {
	return math.Float64frombits(s.ewma.Load())
}

func (s *semaphore) IsEmpty() bool {
	return s.Len() == 0
}

func (s *semaphore) IsFull() bool {
	return s.Len() >= s.Cap()
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
	oldCap := s.cap
	s.cap = c
	if c < oldCap {
		s.parkOversizedLocked()
	} else if c > oldCap {
		s.unparkEligibleLocked()
	}
	s.syncAtomicsLocked()
	s.updateEWMALocked()
	s.dispatchLocked()
	return nil
}
