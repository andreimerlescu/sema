package sema

import (
	"math"
	"math/bits"
)

const (
	defaultCap = 10
	ewmaAlpha  = 0.1
)

func validateN(n int) error {
	if n < 1 {
		return ErrInvalidN{Value: n}
	}
	return nil
}

func clampPriority(p Priority) Priority {
	if p < PriorityLow {
		return PriorityLow
	}
	if p > PriorityCritical {
		return PriorityCritical
	}
	return p
}

func priorityBucket(p Priority) int {
	return int(clampPriority(p)) + 1
}

func bucketPriority(idx int) Priority {
	return Priority(idx - 1)
}

func higherOrEqualMask(p Priority) uint8 {
	return (uint8(0x0F) << uint(priorityBucket(p))) & 0x0F
}

func (q *waiterQueue) pushBack(w *waiter) {
	w.prev = q.tail
	w.next = nil
	if q.tail == nil {
		q.head = w
		q.tail = w
	} else {
		q.tail.next = w
		q.tail = w
	}
	q.count++
}

func (q *waiterQueue) remove(w *waiter) {
	if w.prev != nil {
		w.prev.next = w.next
	} else if q.head == w {
		q.head = w.next
	}
	if w.next != nil {
		w.next.prev = w.prev
	} else if q.tail == w {
		q.tail = w.prev
	}
	w.prev = nil
	w.next = nil
	q.count--
}

func (q *waiterQueue) insertBySeq(w *waiter) {
	if q.tail == nil || w.seq >= q.tail.seq {
		q.pushBack(w)
		return
	}
	if w.seq < q.head.seq {
		w.prev = nil
		w.next = q.head
		q.head.prev = w
		q.head = w
		q.count++
		return
	}
	cur := q.tail
	for cur != nil && cur.seq > w.seq {
		cur = cur.prev
	}
	w.prev = cur
	w.next = cur.next
	if cur.next != nil {
		cur.next.prev = w
	} else {
		q.tail = w
	}
	cur.next = w
	q.count++
}

func (s *semaphore) notify(fn func(Observer)) {
	if s.observer != nil {
		fn(s.observer)
	}
}

func (s *semaphore) syncAtomicsLocked() {
	s.heldAtom.Store(int64(s.held))
	s.capAtom.Store(int64(s.cap))
}

func (s *semaphore) grantLocked(n int) {
	s.held += n
	s.syncAtomicsLocked()
	s.notify(func(o Observer) { o.OnAcquire(s.held, s.cap) })
}

func (s *semaphore) setActiveBitLocked(idx int) {
	bit := uint8(1) << uint(idx)
	if s.activeMask&bit == 0 {
		s.activeMask |= bit
		s.maskAtom.Store(uint32(s.activeMask))
	}
}

func (s *semaphore) clearActiveBitLocked(idx int) {
	bit := uint8(1) << uint(idx)
	if s.activeMask&bit != 0 {
		s.activeMask &^= bit
		s.maskAtom.Store(uint32(s.activeMask))
	}
}

// canFitLocked checks whether n slots fit without signed-int overflow,
// even when s.cap or n is math.MaxInt. Requires s.mu held.
func (s *semaphore) canFitLocked(n int) bool {
	return s.held <= s.cap && n <= s.cap-s.held
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
	old := math.Float64frombits(s.ewma.Load())
	next := ewmaAlpha*current + (1-ewmaAlpha)*old
	s.ewma.Store(math.Float64bits(next))
}

func (s *semaphore) resetEWMA() {
	s.ewma.Store(0)
}

func (s *semaphore) hasHigherOrEqualWaiterLocked(p Priority) bool {
	return (s.activeMask & higherOrEqualMask(p)) != 0
}

func (s *semaphore) enqueueWaiterLocked(n int, p Priority) *waiter {
	p = clampPriority(p)
	s.nextSeq++
	w := &waiter{
		n:     n,
		prio:  p,
		seq:   s.nextSeq,
		ready: make(chan struct{}),
	}
	idx := priorityBucket(p)
	s.queues[idx].pushBack(w)
	if n > 1 {
		s.multiActive++
	}
	s.setActiveBitLocked(idx)
	return w
}

func (s *semaphore) removeWaiterLocked(w *waiter) {
	if w.parked {
		s.parked.remove(w)
		w.parked = false
		return
	}
	idx := priorityBucket(w.prio)
	q := &s.queues[idx]
	q.remove(w)
	if w.n > 1 {
		s.multiActive--
	}
	if q.count == 0 {
		s.clearActiveBitLocked(idx)
	}
}

// parkOversizedLocked moves active waiters with w.n > s.cap into s.parked
// so hot-path acquire/release never scan unsatisfiable waiters.
func (s *semaphore) parkOversizedLocked() {
	if s.multiActive == 0 {
		return
	}
	for idx := 0; idx < numPriorities; idx++ {
		q := &s.queues[idx]
		for w := q.head; w != nil; {
			next := w.next
			if w.n > s.cap {
				q.remove(w)
				s.multiActive--
				w.parked = true
				s.parked.pushBack(w)
			}
			w = next
		}
		if q.count == 0 {
			s.clearActiveBitLocked(idx)
		}
	}
}

// unparkEligibleLocked restores parked waiters with w.n <= s.cap back into
// their priority queue in ascending arrival seq order.
func (s *semaphore) unparkEligibleLocked() {
	if s.parked.count == 0 {
		return
	}
	for w := s.parked.head; w != nil; {
		next := w.next
		if w.n <= s.cap {
			s.parked.remove(w)
			w.parked = false
			idx := priorityBucket(w.prio)
			s.queues[idx].insertBySeq(w)
			if w.n > 1 {
				s.multiActive++
			}
			s.setActiveBitLocked(idx)
		}
		w = next
	}
}

// dispatchLocked wakes satisfiable waiters strictly by highest priority
// tier and FIFO within each tier. Every active waiter satisfies w.n <= s.cap,
// so stopping at the first head that does not fit is O(1) per woken waiter.
func (s *semaphore) dispatchLocked() {
	for s.activeMask != 0 {
		idx := 31 - bits.LeadingZeros32(uint32(s.activeMask))
		w := s.queues[idx].head
		if !s.canFitLocked(w.n) {
			return
		}
		s.removeWaiterLocked(w)
		s.held += w.n
		w.granted = true
		w.grantGen = s.gen
		s.syncAtomicsLocked()
		close(w.ready)
	}
}

func (s *semaphore) notifyEmptyLocked() {
	if s.held == 0 && s.emptyCh != nil {
		close(s.emptyCh)
		s.emptyCh = nil
	}
}

func (s *semaphore) availableLocked() int {
	if s.held >= s.cap {
		return 0
	}
	return s.cap - s.held
}
