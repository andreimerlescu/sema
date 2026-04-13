package sema

import "math"

// channel returns the current channel without holding the mutex.
// Safe for lock-free reads (Len, Cap, Utilization, IsEmpty, IsFull).
// Must NOT be used in acquire/release paths — use channelLocked instead.
func (s *semaphore) channel() chan struct{} {
	return *s.ch.Load()
}

// channelLocked returns the current channel. Caller must hold s.mu.
func (s *semaphore) channelLocked() chan struct{} {
	return *s.ch.Load()
}

func (s *semaphore) notify(fn func(Observer)) {
	if s.observer != nil {
		fn(s.observer)
	}
}

// updateEWMA updates the exponentially weighted moving average.
// ch must be the channel snapshot taken under the same lock scope
// as the release that triggered this update.
func (s *semaphore) updateEWMA(ch chan struct{}) {
	current := float64(len(ch)) / float64(cap(ch))
	for {
		old := math.Float64frombits(s.ewma.Load())
		next := ewmaAlpha*current + (1-ewmaAlpha)*old
		if s.ewma.CompareAndSwap(
			math.Float64bits(old),
			math.Float64bits(next),
		) {
			return
		}
	}
}

// resetEWMA zeroes the EWMA. Called by Drain and Reset.
func (s *semaphore) resetEWMA() {
	s.ewma.Store(0)
}

func (s *semaphore) drainLocked() {
	ch := s.channelLocked()
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func (s *semaphore) tryAcquireNLocked(n int) bool {
	ch := s.channelLocked()
	if len(ch)+n > cap(ch) {
		return false
	}
	for i := 0; i < n; i++ {
		select {
		case ch <- struct{}{}:
		default:
			for j := 0; j < i; j++ {
				<-ch
			}
			return false
		}
	}
	return true
}
