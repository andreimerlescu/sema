package sema

import "math"

func (s *semaphore) notify(fn func(Observer)) {
	if s.observer != nil {
		fn(s.observer)
	}
}

func (s *semaphore) syncAtomicsLocked() {
	s.heldAtom.Store(int64(s.held))
	s.capAtom.Store(int64(s.cap))
}

func clampUtilization(held, capacity int) float64 {
	if capacity <= 0 || held <= 0 {
		return 0
	}
	u := float64(held) / float64(capacity)
	if u > 1.0 {
		return 1.0
	}
	return u
}

func (s *semaphore) updateEWMALocked() {
	current := clampUtilization(s.held, s.cap)
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

func (s *semaphore) resetEWMA() {
	s.ewma.Store(0)
}

func (s *semaphore) enqueueWaiterLocked(n int) *waiter {
	w := &waiter{
		n:     n,
		ready: make(chan struct{}),
	}
	if s.tail == nil {
		s.head = w
		s.tail = w
	} else {
		w.prev = s.tail
		s.tail.next = w
		s.tail = w
	}
	return w
}

func (s *semaphore) removeWaiterLocked(w *waiter) {
	if w.prev != nil {
		w.prev.next = w.next
	} else if s.head == w {
		s.head = w.next
	}
	if w.next != nil {
		w.next.prev = w.prev
	} else if s.tail == w {
		s.tail = w.prev
	}
	w.prev = nil
	w.next = nil
}

// hasSatisfiableWaiterLocked reports whether any queued waiter can fit within
// the current capacity (ignoring waiters temporarily larger than s.cap).
func (s *semaphore) hasSatisfiableWaiterLocked() bool {
	for w := s.head; w != nil; w = w.next {
		if w.n <= s.cap {
			return true
		}
	}
	return false
}

// dispatchLocked wakes queued waiters in FIFO order whose slot demands fit.
// Waiters whose request exceeds s.cap (after a shrink) are skipped so they
// cannot deadlock smaller waiters; the first satisfiable waiter that does not
// yet have enough free slots stops dispatch to preserve FIFO fairness.
func (s *semaphore) dispatchLocked() {
	for w := s.head; w != nil; {
		next := w.next
		if w.n > s.cap {
			w = next
			continue
		}
		if s.held+w.n > s.cap {
			return
		}
		s.removeWaiterLocked(w)
		s.held += w.n
		w.granted = true
		w.grantGen = s.gen
		s.syncAtomicsLocked()
		close(w.ready)
		w = next
	}
}

func (s *semaphore) notifyEmptyLocked() {
	if s.held == 0 && s.emptyCh != nil {
		close(s.emptyCh)
		s.emptyCh = nil
	}
}

func (s *semaphore) availableLocked() int {
	if avail := s.cap - s.held; avail > 0 {
		return avail
	}
	return 0
}
