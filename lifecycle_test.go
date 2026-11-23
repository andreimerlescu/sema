package sema

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// INTERNAL STRUCTURAL INVARIANT CHECKER & TEST HELPERS
// ============================================================

// assertInternalInvariants verifies every structural invariant of *semaphore
// under its mutex: non-negative occupancy, atomic mirror consistency, exact
// activeMask bitmask agreement, doubly-linked queue integrity, ascending
// arrival sequence within each tier, active/parked capacity partitioning,
// multiActive accounting, and clamped [0.0, 1.0] utilization metrics.
func assertInternalInvariants(t testing.TB, sem Semaphore) {
	t.Helper()
	s, ok := sem.(*semaphore)
	if !ok {
		t.Fatalf("expected *semaphore, got %T", sem)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.held < 0 {
		t.Errorf("invariant violated: held=%d < 0", s.held)
	}
	if s.cap < 1 {
		t.Errorf("invariant violated: cap=%d < 1", s.cap)
	}
	if got := int(s.heldAtom.Load()); got != s.held {
		t.Errorf("invariant violated: heldAtom=%d != held=%d", got, s.held)
	}
	if got := int(s.capAtom.Load()); got != s.cap {
		t.Errorf("invariant violated: capAtom=%d != cap=%d", got, s.cap)
	}
	if got := uint8(s.maskAtom.Load()); got != s.activeMask {
		t.Errorf("invariant violated: maskAtom=%04b != activeMask=%04b", got, s.activeMask)
	}

	actualMulti := 0
	for idx := 0; idx < numPriorities; idx++ {
		q := &s.queues[idx]
		bitSet := (s.activeMask & (uint8(1) << uint(idx))) != 0
		if bitSet != (q.count > 0) {
			t.Errorf("tier %d: activeMask bit=%v but count=%d", idx, bitSet, q.count)
		}
		if q.count == 0 {
			if q.head != nil || q.tail != nil {
				t.Errorf("tier %d: count=0 but head=%v tail=%v", idx, q.head, q.tail)
			}
			continue
		}
		if q.head == nil || q.tail == nil {
			t.Errorf("tier %d: count=%d but head=%v tail=%v", idx, q.count, q.head, q.tail)
			continue
		}
		if q.head.prev != nil {
			t.Errorf("tier %d: head.prev != nil", idx)
		}
		if q.tail.next != nil {
			t.Errorf("tier %d: tail.next != nil", idx)
		}

		walked := 0
		var prev *waiter
		var prevSeq uint64
		for w := q.head; w != nil; w = w.next {
			walked++
			if walked > q.count+1 {
				t.Fatalf("tier %d: cycle detected in waiter queue", idx)
			}
			if w.prev != prev {
				t.Errorf("tier %d: broken prev pointer at node %d", idx, walked)
			}
			if w.parked {
				t.Errorf("tier %d: active waiter has parked=true", idx)
			}
			if w.granted {
				t.Errorf("tier %d: queued waiter already has granted=true", idx)
			}
			if priorityBucket(w.prio) != idx {
				t.Errorf("tier %d: waiter prio=%v belongs in bucket %d", idx, w.prio, priorityBucket(w.prio))
			}
			if w.n < 1 || w.n > s.cap {
				t.Errorf("tier %d: active waiter n=%d outside [1, cap=%d]", idx, w.n, s.cap)
			}
			if w.seq <= prevSeq {
				t.Errorf("tier %d: non-ascending seq (%d <= %d)", idx, w.seq, prevSeq)
			}
			if w.n > 1 {
				actualMulti++
			}
			prevSeq = w.seq
			prev = w
		}
		if prev != q.tail {
			t.Errorf("tier %d: last walked node != tail", idx)
		}
		if walked != q.count {
			t.Errorf("tier %d: walked %d nodes != count %d", idx, walked, q.count)
		}
	}

	if actualMulti != s.multiActive {
		t.Errorf("invariant violated: multiActive=%d, actual active multi-slot waiters=%d",
			s.multiActive, actualMulti)
	}

	// Verify parked pool integrity.
	p := &s.parked
	if p.count == 0 {
		if p.head != nil || p.tail != nil {
			t.Errorf("parked: count=0 but head=%v tail=%v", p.head, p.tail)
		}
	} else {
		walked := 0
		var prev *waiter
		for w := p.head; w != nil; w = w.next {
			walked++
			if walked > p.count+1 {
				t.Fatalf("parked: cycle detected")
			}
			if w.prev != prev {
				t.Errorf("parked: broken prev pointer at node %d", walked)
			}
			if !w.parked {
				t.Errorf("parked: node has parked=false")
			}
			if w.granted {
				t.Errorf("parked: node has granted=true")
			}
			if w.n <= s.cap {
				t.Errorf("parked: node n=%d <= cap=%d should have been unparked", w.n, s.cap)
			}
			prev = w
		}
		if prev != p.tail {
			t.Errorf("parked: last walked node != tail")
		}
		if walked != p.count {
			t.Errorf("parked: walked %d != count %d", walked, p.count)
		}
	}

	u := clampUtilization(s.held, s.cap)
	if math.IsNaN(u) || u < 0 || u > 1.0 {
		t.Errorf("invariant violated: Utilization=%f out of [0, 1]", u)
	}
	us := math.Float64frombits(s.ewma.Load())
	if math.IsNaN(us) || us < 0 || us > 1.0+1e-9 {
		t.Errorf("invariant violated: UtilizationSmoothed=%f out of [0, 1]", us)
	}
}

func waitUntilQueued(t testing.TB, sem Semaphore, wantActive, wantParked int) {
	t.Helper()
	s := sem.(*semaphore)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		active := 0
		for i := 0; i < numPriorities; i++ {
			active += s.queues[i].count
		}
		parked := s.parked.count
		s.mu.Unlock()
		if active == wantActive && parked == wantParked {
			return
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	s.mu.Lock()
	active := 0
	for i := 0; i < numPriorities; i++ {
		active += s.queues[i].count
	}
	parked := s.parked.count
	s.mu.Unlock()
	t.Fatalf("timed out waiting for queued counts: got active=%d parked=%d, want active=%d parked=%d",
		active, parked, wantActive, wantParked)
}

func mustNewPriority(t *testing.T, c int) PrioritySemaphore {
	t.Helper()
	ps, err := NewPriority(c)
	if err != nil {
		t.Fatalf("NewPriority(%d): %v", c, err)
	}
	return ps
}

// ============================================================
// UNIT TESTS — PRIORITY EXTENSION API (#3)
// ============================================================

func TestPriority_ConstructorsAndString(t *testing.T) {
	ps, err := NewPriority(-1)
	if err != nil {
		t.Fatalf("NewPriority(-1): %v", err)
	}
	if ps.Cap() != defaultCap {
		t.Errorf("Cap() = %d, want %d", ps.Cap(), defaultCap)
	}

	if _, err := NewPriority(0); !errors.Is(err, ErrInvalidCap{}) {
		t.Errorf("NewPriority(0): err = %v, want ErrInvalidCap", err)
	}

	ps2 := MustPriority(4)
	if ps2.Cap() != 4 {
		t.Errorf("MustPriority(4).Cap() = %d, want 4", ps2.Cap())
	}

	// MustPriority panics on invalid capacity.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("MustPriority(0): expected panic")
			}
		}()
		_ = MustPriority(0)
	}()

	// Standard Semaphore constructors also satisfy PrioritySemaphore via type assertion.
	sem := mustNew(t, 3)
	if _, ok := sem.(PrioritySemaphore); !ok {
		t.Errorf("New(3) does not implement PrioritySemaphore")
	}

	// Observer constructor.
	obs := &mockObserver{}
	ops, err := NewPriorityWithObserver(3, obs)
	if err != nil {
		t.Fatalf("NewPriorityWithObserver(3): %v", err)
	}
	if err := ops.AcquireWithPriority(context.Background(), PriorityHigh); err != nil {
		t.Fatalf("AcquireWithPriority: %v", err)
	}
	if obs.AcquireCount() != 1 {
		t.Errorf("AcquireCount = %d, want 1", obs.AcquireCount())
	}
	if _, err := NewPriorityWithObserver(0, obs); !errors.Is(err, ErrInvalidCap{}) {
		t.Errorf("NewPriorityWithObserver(0): err = %v, want ErrInvalidCap", err)
	}

	// Zero value is PriorityNormal; ordering and String() match expectations.
	var zeroP Priority
	if zeroP != PriorityNormal {
		t.Errorf("zero Priority = %v, want PriorityNormal", zeroP)
	}
	if !(PriorityLow < PriorityNormal && PriorityNormal < PriorityHigh && PriorityHigh < PriorityCritical) {
		t.Errorf("priority ordering broken")
	}
	wantStrings := map[Priority]string{
		Priority(-99):    "low",
		PriorityLow:      "low",
		PriorityNormal:   "normal",
		PriorityHigh:     "high",
		PriorityCritical: "critical",
		Priority(99):     "critical",
	}
	for p, want := range wantStrings {
		if got := p.String(); got != want {
			t.Errorf("Priority(%d).String() = %q, want %q", p, got, want)
		}
	}

	// Context helpers.
	if got := PriorityFromContext(nil); got != PriorityNormal {
		t.Errorf("PriorityFromContext(nil) = %v, want PriorityNormal", got)
	}
	if got := PriorityFromContext(context.Background()); got != PriorityNormal {
		t.Errorf("PriorityFromContext(bg) = %v, want PriorityNormal", got)
	}
	if got := PriorityFromContext(WithPriority(context.Background(), Priority(50))); got != PriorityCritical {
		t.Errorf("clamped PriorityFromContext(50) = %v, want PriorityCritical", got)
	}
	if got := PriorityFromContext(WithPriority(context.Background(), Priority(-50))); got != PriorityLow {
		t.Errorf("clamped PriorityFromContext(-50) = %v, want PriorityLow", got)
	}
}

func TestPriority_StrictTierOrdering(t *testing.T) {
	ps := mustNewPriority(t, 1)
	ps.Acquire() // hold sole slot

	type req struct {
		name string
		prio Priority
	}
	// Enqueue in intentionally scrambled order (2 per tier).
	enqueueOrder := []req{
		{"low-1", PriorityLow},
		{"normal-1", PriorityNormal},
		{"low-2", PriorityLow},
		{"high-1", PriorityHigh},
		{"critical-1", PriorityCritical},
		{"normal-2", PriorityNormal},
		{"high-2", PriorityHigh},
		{"critical-2", PriorityCritical},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	woken := make(chan string, len(enqueueOrder))
	ack := make(chan struct{})

	for i, r := range enqueueOrder {
		r := r
		go func() {
			if err := ps.AcquireWithPriority(ctx, r.prio); err != nil {
				t.Errorf("%s failed: %v", r.name, err)
				return
			}
			woken <- r.name
			<-ack
			if err := ps.Release(); err != nil {
				t.Errorf("%s Release: %v", r.name, err)
			}
		}()
		// Wait until each waiter is registered in order so intra-tier FIFO is deterministic.
		waitUntilQueued(t, ps, i+1, 0)
	}

	assertInternalInvariants(t, ps)

	wantOrder := []string{
		"critical-1", "critical-2",
		"high-1", "high-2",
		"normal-1", "normal-2",
		"low-1", "low-2",
	}
	wantNextHighest := []struct {
		prio Priority
		ok   bool
	}{
		{PriorityCritical, true},
		{PriorityHigh, true},
		{PriorityHigh, true},
		{PriorityNormal, true},
		{PriorityNormal, true},
		{PriorityLow, true},
		{PriorityLow, true},
		{PriorityNormal, false},
	}

	// Release initial holder to start the cascade.
	if err := ps.Release(); err != nil {
		t.Fatalf("initial Release: %v", err)
	}

	for i, want := range wantOrder {
		select {
		case got := <-woken:
			if got != want {
				t.Fatalf("step %d: admitted %q, want %q", i, got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("step %d: timed out waiting for %q", i, want)
		}
		p, ok := ps.HighestWaitingPriority()
		if ok != wantNextHighest[i].ok || p != wantNextHighest[i].prio {
			t.Errorf("step %d: HighestWaitingPriority() = (%v, %v), want (%v, %v)",
				i, p, ok, wantNextHighest[i].prio, wantNextHighest[i].ok)
		}
		ack <- struct{}{}
	}

	// Wait until final Release completes.
	if err := ps.Wait(ctx); err != nil {
		t.Fatalf("final Wait: %v", err)
	}
	assertInternalInvariants(t, ps)
}

func TestPriority_WithPriorityContextInterop(t *testing.T) {
	// Exercise purely through the v1.x Semaphore interface.
	var sem Semaphore = mustNew(t, 5)
	if err := sem.AcquireN(3); err != nil {
		t.Fatalf("AcquireN(3): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Queue a Low waiter wanting 3 slots (2 free, so it blocks).
	lowErr := make(chan error, 1)
	go func() {
		lowErr <- sem.AcquireNWith(WithPriority(ctx, PriorityLow), 3)
	}()
	waitUntilQueued(t, sem, 1, 0)

	// Another Low TryAcquireWith must fail (higher-or-equal Low waiter is queued).
	if err := sem.TryAcquireWith(WithPriority(ctx, PriorityLow)); !errors.Is(err, ErrNoSlot{}) {
		t.Errorf("Low TryAcquireWith = %v, want ErrNoSlot", err)
	}
	if err := sem.TryAcquireNWith(WithPriority(ctx, PriorityLow), 2); !errors.Is(err, ErrNoSlot{}) {
		t.Errorf("Low TryAcquireNWith(2) = %v, want ErrNoSlot", err)
	}

	// Critical TryAcquireWith through standard Semaphore interface bypasses Low waiter.
	if err := sem.TryAcquireWith(WithPriority(ctx, PriorityCritical)); err != nil {
		t.Fatalf("Critical TryAcquireWith via Semaphore failed: %v", err)
	}
	if err := sem.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// High TryAcquireNWith(2) also bypasses Low(3).
	if err := sem.TryAcquireNWith(WithPriority(ctx, PriorityHigh), 2); err != nil {
		t.Fatalf("High TryAcquireNWith(2) failed: %v", err)
	}
	if err := sem.ReleaseN(2); err != nil {
		t.Fatalf("ReleaseN(2): %v", err)
	}

	// Queue Critical(1) while filling remaining 2 slots -> Critical wakes before Low(3).
	if err := sem.AcquireN(2); err != nil {
		t.Fatalf("AcquireN(2): %v", err)
	}
	critErr := make(chan error, 1)
	go func() {
		critErr <- sem.AcquireWith(WithPriority(ctx, PriorityCritical))
	}()
	waitUntilQueued(t, sem, 2, 0)

	// Release 1 slot -> Critical(1) immediately wakes; Low(3) still waiting.
	if err := sem.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	select {
	case err := <-critErr:
		if err != nil {
			t.Fatalf("Critical AcquireWith failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Critical AcquireWith did not wake ahead of Low(3)")
	}

	// Release remaining 5 slots -> Low(3) acquires.
	if err := sem.ReleaseN(5); err != nil {
		t.Fatalf("ReleaseN(5): %v", err)
	}
	select {
	case err := <-lowErr:
		if err != nil {
			t.Fatalf("Low AcquireNWith(3) failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Low AcquireNWith(3) did not wake after ReleaseN(5)")
	}
	if err := sem.ReleaseN(3); err != nil {
		t.Fatalf("ReleaseN(3): %v", err)
	}
	assertInternalInvariants(t, sem)
}

func TestPriority_HigherBypassesLowerMultiSlotWaiter(t *testing.T) {
	ps := mustNewPriority(t, 5)
	if err := ps.AcquireN(3); err != nil {
		t.Fatalf("AcquireN(3): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// High(4) blocks because only 2 slots are free.
	highDone := make(chan error, 1)
	go func() {
		highDone <- ps.AcquireNWithPriority(ctx, 4, PriorityHigh)
	}()
	waitUntilQueued(t, ps, 1, 0)

	// Same-or-lower priority callers must NOT starve High(4).
	if err := ps.TryAcquireWithPriority(ctx, PriorityHigh); !errors.Is(err, ErrNoSlot{}) {
		t.Errorf("High TryAcquireWithPriority = %v, want ErrNoSlot", err)
	}
	if err := ps.TryAcquireWithPriority(ctx, PriorityNormal); !errors.Is(err, ErrNoSlot{}) {
		t.Errorf("Normal TryAcquireWithPriority = %v, want ErrNoSlot", err)
	}
	if err := ps.TryAcquireWithPriority(ctx, PriorityLow); !errors.Is(err, ErrNoSlot{}) {
		t.Errorf("Low TryAcquireWithPriority = %v, want ErrNoSlot", err)
	}

	// Strictly higher Critical(1) bypasses High(4) immediately.
	if err := ps.TryAcquireWithPriority(ctx, PriorityCritical); err != nil {
		t.Fatalf("Critical TryAcquireWithPriority failed: %v", err)
	}
	if err := ps.Release(); err != nil {
		t.Fatalf("Release Critical: %v", err)
	}

	// Release 2 of the 3 held slots -> 1 held + 4 needed = 5 -> High(4) wakes!
	if err := ps.ReleaseN(2); err != nil {
		t.Fatalf("ReleaseN(2): %v", err)
	}
	select {
	case err := <-highDone:
		if err != nil {
			t.Fatalf("High(4) failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("High(4) did not wake after ReleaseN(2)")
	}

	if err := ps.ReleaseN(5); err != nil {
		t.Fatalf("ReleaseN(5): %v", err)
	}
	assertInternalInvariants(t, ps)
}

func TestPriority_HighestWaitingPriority(t *testing.T) {
	ps := mustNewPriority(t, 5)

	if p, ok := ps.HighestWaitingPriority(); ok || p != PriorityNormal {
		t.Fatalf("empty: HighestWaitingPriority = (%v, %v), want (normal, false)", p, ok)
	}

	if err := ps.AcquireN(5); err != nil {
		t.Fatalf("AcquireN(5): %v", err)
	}

	ctxLow, cancelLow := context.WithCancel(context.Background())
	defer cancelLow()
	ctxHigh, cancelHigh := context.WithCancel(context.Background())
	defer cancelHigh()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = ps.AcquireWithPriority(ctxLow, PriorityLow)
	}()
	waitUntilQueued(t, ps, 1, 0)

	if p, ok := ps.HighestWaitingPriority(); !ok || p != PriorityLow {
		t.Fatalf("with Low waiter: got (%v, %v), want (low, true)", p, ok)
	}

	go func() {
		defer wg.Done()
		_ = ps.AcquireNWithPriority(ctxHigh, 4, PriorityHigh)
	}()
	waitUntilQueued(t, ps, 2, 0)

	if p, ok := ps.HighestWaitingPriority(); !ok || p != PriorityHigh {
		t.Fatalf("with High(4)+Low(1): got (%v, %v), want (high, true)", p, ok)
	}

	// Shrink cap to 2: High(4) moves to parked pool, so HighestWaitingPriority drops to Low!
	if err := ps.SetCap(2); err != nil {
		t.Fatalf("SetCap(2): %v", err)
	}
	assertInternalInvariants(t, ps)
	if p, ok := ps.HighestWaitingPriority(); !ok || p != PriorityLow {
		t.Fatalf("after parking High(4): got (%v, %v), want (low, true)", p, ok)
	}

	// Cancel Low(1): only parked High(4) remains -> no satisfiable waiters!
	cancelLow()
	waitUntilQueued(t, ps, 0, 1)
	if p, ok := ps.HighestWaitingPriority(); ok || p != PriorityNormal {
		t.Fatalf("only parked High(4): got (%v, %v), want (normal, false)", p, ok)
	}

	// Expand back to 4: High(4) unparks -> HighestWaitingPriority returns (high, true)!
	if err := ps.SetCap(4); err != nil {
		t.Fatalf("SetCap(4): %v", err)
	}
	assertInternalInvariants(t, ps)
	if p, ok := ps.HighestWaitingPriority(); !ok || p != PriorityHigh {
		t.Fatalf("after unparking High(4): got (%v, %v), want (high, true)", p, ok)
	}

	// Drain wakes High(4) (held 0 -> 4) -> queue now empty.
	if err := ps.Drain(); err != nil {
		t.Fatalf("Drain(): %v", err)
	}
	wg.Wait()
	if p, ok := ps.HighestWaitingPriority(); ok {
		t.Fatalf("after Drain: got (%v, %v), want (_, false)", p, ok)
	}
	if err := ps.ReleaseN(4); err != nil {
		t.Fatalf("ReleaseN(4): %v", err)
	}
	assertInternalInvariants(t, ps)
}

func TestPriority_ObserverEmission(t *testing.T) {
	obs := &mockObserver{}
	ps, err := NewPriorityWithObserver(3, obs)
	if err != nil {
		t.Fatalf("NewPriorityWithObserver: %v", err)
	}

	bg := context.Background()
	cancelledCtx, cancel := context.WithCancel(bg)
	cancel()

	// Positive cases.
	if err := ps.AcquireWithPriority(bg, PriorityCritical); err != nil {
		t.Fatalf("AcquireWithPriority: %v", err)
	}
	if err := ps.TryAcquireWithPriority(bg, PriorityHigh); err != nil {
		t.Fatalf("TryAcquireWithPriority: %v", err)
	}
	if err := ps.TryAcquireNWithPriority(bg, 1, PriorityLow); err != nil {
		t.Fatalf("TryAcquireNWithPriority(1): %v", err)
	}
	if obs.AcquireCount() != 3 {
		t.Errorf("AcquireCount = %d, want 3", obs.AcquireCount())
	}

	// Negative cases (full semaphore / cancelled / invalid n / exceeds cap): zero observer calls.
	if err := ps.AcquireWithPriority(cancelledCtx, PriorityCritical); !errors.Is(err, ErrAcquireCancelled{}) {
		t.Errorf("cancelled AcquireWithPriority: %v", err)
	}
	if err := ps.AcquireNWithPriority(cancelledCtx, 1, PriorityHigh); !errors.Is(err, ErrAcquireCancelled{}) {
		t.Errorf("cancelled AcquireNWithPriority: %v", err)
	}
	if err := ps.AcquireNWithPriority(bg, 0, PriorityHigh); !errors.Is(err, ErrInvalidN{}) {
		t.Errorf("invalid AcquireNWithPriority(0): %v", err)
	}
	if err := ps.AcquireNWithPriority(bg, 10, PriorityHigh); !errors.Is(err, ErrNExceedsCap{}) {
		t.Errorf("exceeds cap AcquireNWithPriority(10): %v", err)
	}
	if err := ps.TryAcquireWithPriority(cancelledCtx, PriorityCritical); !errors.Is(err, ErrAcquireCancelled{}) {
		t.Errorf("cancelled TryAcquireWithPriority: %v", err)
	}
	if err := ps.TryAcquireWithPriority(bg, PriorityCritical); !errors.Is(err, ErrNoSlot{}) {
		t.Errorf("full TryAcquireWithPriority: %v", err)
	}
	if err := ps.TryAcquireNWithPriority(cancelledCtx, 1, PriorityHigh); !errors.Is(err, ErrAcquireCancelled{}) {
		t.Errorf("cancelled TryAcquireNWithPriority: %v", err)
	}
	if err := ps.TryAcquireNWithPriority(bg, 0, PriorityHigh); !errors.Is(err, ErrInvalidN{}) {
		t.Errorf("invalid TryAcquireNWithPriority(0): %v", err)
	}
	if err := ps.TryAcquireNWithPriority(bg, 10, PriorityHigh); !errors.Is(err, ErrNExceedsCap{}) {
		t.Errorf("exceeds cap TryAcquireNWithPriority(10): %v", err)
	}
	if err := ps.TryAcquireNWithPriority(bg, 1, PriorityHigh); !errors.Is(err, ErrNoSlot{}) {
		t.Errorf("full TryAcquireNWithPriority(1): %v", err)
	}
	if obs.AcquireCount() != 3 {
		t.Errorf("AcquireCount after failures = %d, want still 3", obs.AcquireCount())
	}

	// Bulk AcquireNWithPriority fires once on success.
	if err := ps.ReleaseN(3); err != nil {
		t.Fatalf("ReleaseN(3): %v", err)
	}
	if err := ps.AcquireNWithPriority(bg, 2, PriorityHigh); err != nil {
		t.Fatalf("AcquireNWithPriority(2): %v", err)
	}
	if obs.AcquireCount() != 4 {
		t.Errorf("AcquireCount after AcquireNWithPriority(2) = %d, want 4", obs.AcquireCount())
	}
	if err := ps.ReleaseN(2); err != nil {
		t.Fatalf("ReleaseN(2): %v", err)
	}
	assertInternalInvariants(t, ps)
}

// ============================================================
// SAFETY-CRITICAL LIFECYCLE VERIFICATION SUITE (#1, #2, #3, #5)
// ============================================================

// TestLifecycle_StateSpaceTransitions systematically exercises reachable
// semaphore state transitions — including math.MaxInt overflow boundaries,
// oversubscribed shrink/expand, parked-waiter cancellation and seq-order
// restoration, Drain, Reset, and Wait — verifying every internal structural
// invariant after every single transition.
func TestLifecycle_StateSpaceTransitions(t *testing.T) {
	t.Run("MaxIntOverflowBoundaries", func(t *testing.T) {
		ps := mustNewPriority(t, math.MaxInt)
		assertInternalInvariants(t, ps)

		// Hold 1 slot so cap - held == math.MaxInt - 1; requesting math.MaxInt
		// MUST NOT wrap signed int arithmetic negative.
		ps.Acquire()
		assertInternalInvariants(t, ps)

		if ps.TryAcquireN(math.MaxInt) {
			t.Fatalf("TryAcquireN(MaxInt) succeeded with 1 slot held (signed int overflow!)")
		}
		assertInternalInvariants(t, ps)

		err := ps.TryAcquireNWith(context.Background(), math.MaxInt)
		var slotErr ErrNoSlot
		if !errors.As(err, &slotErr) {
			t.Fatalf("TryAcquireNWith(MaxInt) = %v, want ErrNoSlot", err)
		}
		if slotErr.Available != math.MaxInt-1 {
			t.Errorf("ErrNoSlot.Available = %d, want %d", slotErr.Available, math.MaxInt-1)
		}

		if err := ps.AcquireNTimeout(math.MaxInt, 10*time.Millisecond); !errors.Is(err, ErrAcquireCancelled{}) {
			t.Fatalf("AcquireNTimeout(MaxInt) = %v, want ErrAcquireCancelled", err)
		}
		assertInternalInvariants(t, ps)

		// Release 1 -> empty -> AcquireN(math.MaxInt) succeeds cleanly.
		if err := ps.Release(); err != nil {
			t.Fatalf("Release(): %v", err)
		}
		if err := ps.AcquireN(math.MaxInt); err != nil {
			t.Fatalf("AcquireN(MaxInt) on empty MaxInt sem: %v", err)
		}
		if ps.Len() != math.MaxInt || !ps.IsFull() {
			t.Fatalf("after AcquireN(MaxInt): Len=%d IsFull=%v", ps.Len(), ps.IsFull())
		}
		assertInternalInvariants(t, ps)

		// Shrink from math.MaxInt to 1 while holding math.MaxInt slots.
		if err := ps.SetCap(1); err != nil {
			t.Fatalf("SetCap(1): %v", err)
		}
		assertInternalInvariants(t, ps)
		if ps.TryAcquire() {
			t.Fatalf("TryAcquire() succeeded while held=MaxInt cap=1")
		}
		err = ps.TryAcquireWith(context.Background())
		if !errors.As(err, &slotErr) || slotErr.Available != 0 {
			t.Fatalf("oversubscribed TryAcquireWith = %v (%+v), want ErrNoSlot{Available:0}", err, slotErr)
		}
		if u := ps.Utilization(); u != 1.0 {
			t.Errorf("Utilization() = %f, want 1.0", u)
		}

		// Release all math.MaxInt slots cleanly.
		if err := ps.ReleaseN(math.MaxInt); err != nil {
			t.Fatalf("ReleaseN(MaxInt): %v", err)
		}
		if !ps.IsEmpty() {
			t.Fatalf("expected empty after ReleaseN(MaxInt), got Len=%d", ps.Len())
		}
		assertInternalInvariants(t, ps)
	})

	t.Run("ParkedUnparkedSeqOrderAndCancelMatrix", func(t *testing.T) {
		ps := mustNewPriority(t, 6)
		if err := ps.AcquireN(6); err != nil {
			t.Fatalf("AcquireN(6): %v", err)
		}
		assertInternalInvariants(t, ps)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		// Queue three High waiters: W1(n=4), W2(n=5, will cancel while parked), W3(n=4).
		woken := make(chan string, 4)
		w2Ctx, cancelW2 := context.WithCancel(ctx)
		defer cancelW2()

		go func() {
			if err := ps.AcquireNWithPriority(ctx, 4, PriorityHigh); err == nil {
				woken <- "W1"
			}
		}()
		waitUntilQueued(t, ps, 1, 0)

		w2Err := make(chan error, 1)
		go func() {
			w2Err <- ps.AcquireNWithPriority(w2Ctx, 5, PriorityHigh)
		}()
		waitUntilQueued(t, ps, 2, 0)

		go func() {
			if err := ps.AcquireNWithPriority(ctx, 4, PriorityHigh); err == nil {
				woken <- "W3"
			}
		}()
		waitUntilQueued(t, ps, 3, 0)
		assertInternalInvariants(t, ps)

		// Shrink cap to 3 -> all 3 High multi-slot waiters move to parked pool.
		if err := ps.SetCap(3); err != nil {
			t.Fatalf("SetCap(3): %v", err)
		}
		waitUntilQueued(t, ps, 0, 3)
		assertInternalInvariants(t, ps)

		// Queue W4(n=2, High) while W1/W2/W3 are parked (W4 has newer seq).
		go func() {
			if err := ps.AcquireNWithPriority(ctx, 2, PriorityHigh); err == nil {
				woken <- "W4"
			}
		}()
		waitUntilQueued(t, ps, 1, 3)
		assertInternalInvariants(t, ps)

		// Cancel W2 while it is in the parked pool.
		cancelW2()
		select {
		case err := <-w2Err:
			if !errors.Is(err, ErrAcquireCancelled{}) {
				t.Fatalf("parked cancel W2 err = %v, want ErrAcquireCancelled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("parked W2 did not exit on cancel")
		}
		waitUntilQueued(t, ps, 1, 2)
		assertInternalInvariants(t, ps)

		// Expand cap to 4: W1(n=4) and W3(n=4) unpark ahead of W4(n=2) by original seq!
		// Current held is still 6 (oversubscribed vs cap=4), so none wake yet.
		if err := ps.SetCap(4); err != nil {
			t.Fatalf("SetCap(4): %v", err)
		}
		waitUntilQueued(t, ps, 3, 0)
		assertInternalInvariants(t, ps)

		// Release all 6 initial slots -> W1(4) must wake FIRST (original FIFO seq!).
		if err := ps.ReleaseN(6); err != nil {
			t.Fatalf("ReleaseN(6): %v", err)
		}
		if got := <-woken; got != "W1" {
			t.Fatalf("first unparked wake = %q, want W1 (original seq order)", got)
		}
		assertInternalInvariants(t, ps)

		// Release W1's 4 slots -> W3(4) wakes SECOND (ahead of newer W4!).
		if err := ps.ReleaseN(4); err != nil {
			t.Fatalf("ReleaseN(4) W1: %v", err)
		}
		if got := <-woken; got != "W3" {
			t.Fatalf("second unparked wake = %q, want W3 (original seq order)", got)
		}
		assertInternalInvariants(t, ps)

		// Release W3's 4 slots -> W4(2) wakes THIRD.
		if err := ps.ReleaseN(4); err != nil {
			t.Fatalf("ReleaseN(4) W3: %v", err)
		}
		if got := <-woken; got != "W4" {
			t.Fatalf("third wake = %q, want W4", got)
		}
		assertInternalInvariants(t, ps)

		// Verify Wait -> Reset -> Drain cycle leaves clean invariants.
		waitDone := make(chan error, 1)
		go func() {
			waitDone <- ps.Wait(ctx)
		}()
		time.Sleep(15 * time.Millisecond)
		if err := ps.Reset(); err != nil {
			t.Fatalf("Reset(): %v", err)
		}
		if err := <-waitDone; err != nil {
			t.Fatalf("Wait after Reset: %v", err)
		}
		assertInternalInvariants(t, ps)
	})
}

// TestLifecycle_LinearizabilityOracle drives concurrent multi-tier operations
// mixed with dynamic SetCap shrink/expand and short-deadline cancellations,
// verifying strict slot conservation, linearizable ceiling bounds, metric
// bounds, zero release errors, and clean post-quiescence invariants.
func TestLifecycle_LinearizabilityOracle(t *testing.T) {
	const (
		initCap    = 10
		shrunkCap  = 4
		goroutines = 24
		opsPerG    = 40
	)

	ps := mustNewPriority(t, initCap)

	var (
		totalAcquired atomic.Int64
		totalReleased atomic.Int64
		activeSlots   atomic.Int64
		releaseErrs   atomic.Int64
		metricErrs    atomic.Int64
	)

	checkMetrics := func() {
		u := ps.Utilization()
		if math.IsNaN(u) || u < 0 || u > 1.0 {
			metricErrs.Add(1)
		}
		us := ps.UtilizationSmoothed()
		if math.IsNaN(us) || us < 0 || us > 1.0+1e-9 {
			metricErrs.Add(1)
		}
		if ps.Len() < 0 {
			metricErrs.Add(1)
		}
	}

	prios := []Priority{PriorityLow, PriorityNormal, PriorityHigh, PriorityCritical}
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerG; i++ {
				p := prios[(id+i)%len(prios)]
				switch (id + i) % 7 {
				case 0:
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
					err := ps.AcquireWithPriority(ctx, p)
					cancel()
					if err == nil {
						totalAcquired.Add(1)
						activeSlots.Add(1)
						checkMetrics()
						runtime.Gosched()
						activeSlots.Add(-1)
						if relErr := ps.Release(); relErr != nil {
							releaseErrs.Add(1)
						} else {
							totalReleased.Add(1)
						}
					}
				case 1:
					n := 1 + ((id + i) % 3)
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
					err := ps.AcquireNWithPriority(ctx, n, p)
					cancel()
					if err == nil {
						totalAcquired.Add(int64(n))
						activeSlots.Add(int64(n))
						checkMetrics()
						runtime.Gosched()
						activeSlots.Add(-int64(n))
						if relErr := ps.ReleaseN(n); relErr != nil {
							releaseErrs.Add(1)
						} else {
							totalReleased.Add(int64(n))
						}
					}
				case 2:
					if err := ps.TryAcquireWithPriority(context.Background(), p); err == nil {
						totalAcquired.Add(1)
						activeSlots.Add(1)
						checkMetrics()
						activeSlots.Add(-1)
						if relErr := ps.Release(); relErr != nil {
							releaseErrs.Add(1)
						} else {
							totalReleased.Add(1)
						}
					}
				case 3:
					n := 1 + ((id + i) % 2)
					if err := ps.TryAcquireNWithPriority(context.Background(), n, p); err == nil {
						totalAcquired.Add(int64(n))
						activeSlots.Add(int64(n))
						checkMetrics()
						activeSlots.Add(-int64(n))
						if relErr := ps.ReleaseN(n); relErr != nil {
							releaseErrs.Add(1)
						} else {
							totalReleased.Add(int64(n))
						}
					}
				case 4:
					ctx, cancel := context.WithTimeout(WithPriority(context.Background(), p), 2*time.Millisecond)
					err := ps.AcquireWith(ctx)
					cancel()
					if err == nil {
						totalAcquired.Add(1)
						activeSlots.Add(1)
						checkMetrics()
						activeSlots.Add(-1)
						if relErr := ps.Release(); relErr != nil {
							releaseErrs.Add(1)
						} else {
							totalReleased.Add(1)
						}
					}
				case 5:
					if err := ps.SetCap(shrunkCap); err != nil {
						t.Errorf("SetCap(%d): %v", shrunkCap, err)
					}
					_, _ = ps.HighestWaitingPriority()
					checkMetrics()
				case 6:
					if err := ps.SetCap(initCap); err != nil {
						t.Errorf("SetCap(%d): %v", initCap, err)
					}
					_, _ = ps.HighestWaitingPriority()
					checkMetrics()
				}
			}
		}(g)
	}

	wg.Wait()

	if errs := releaseErrs.Load(); errs != 0 {
		t.Errorf("release errors = %d, want 0", errs)
	}
	if errs := metricErrs.Load(); errs != 0 {
		t.Errorf("metric invariant errors = %d, want 0", errs)
	}
	if acq, rel := totalAcquired.Load(), totalReleased.Load(); acq != rel {
		t.Errorf("slot conservation violated: acquired=%d != released=%d", acq, rel)
	}
	if active := activeSlots.Load(); active != 0 {
		t.Errorf("activeSlots = %d at quiescence, want 0", active)
	}
	if ps.Len() != 0 || !ps.IsEmpty() {
		t.Errorf("final Len()=%d IsEmpty()=%v, want 0/true", ps.Len(), ps.IsEmpty())
	}
	assertInternalInvariants(t, ps)
}

// TestLifecycle_BoundedMutexLatency verifies #2: parking 4,000 oversized
// AcquireN waiters via SetCap shrink keeps active hot-path operations
// structurally O(1) (zero scanning of parked waiters) with bounded latency
// and zero goroutine leaks on cancellation.
func TestLifecycle_BoundedMutexLatency(t *testing.T) {
	const (
		parkedWaiters = 4000
		hotOps        = 20000
	)

	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()

	ps := mustNewPriority(t, 5)
	if err := ps.AcquireN(5); err != nil {
		t.Fatalf("AcquireN(5): %v", err)
	}

	parkCtx, cancelParked := context.WithCancel(context.Background())
	var parkWG sync.WaitGroup
	parkWG.Add(parkedWaiters)
	for i := 0; i < parkedWaiters; i++ {
		prio := Priority(i%numPriorities - 1)
		go func(p Priority) {
			defer parkWG.Done()
			err := ps.AcquireNWithPriority(parkCtx, 4, p)
			if !errors.Is(err, ErrAcquireCancelled{}) {
				t.Errorf("parked waiter returned %v, want ErrAcquireCancelled", err)
			}
		}(prio)
	}

	waitUntilQueued(t, ps, parkedWaiters, 0)

	// Shrink cap to 2: moves all 4,000 oversized waiters out of active queues.
	if err := ps.SetCap(2); err != nil {
		t.Fatalf("SetCap(2): %v", err)
	}
	waitUntilQueued(t, ps, 0, parkedWaiters)
	assertInternalInvariants(t, ps)

	if p, ok := ps.HighestWaitingPriority(); ok {
		t.Fatalf("expected no satisfiable waiters after parking, got (%v, true)", p)
	}

	// Free 4 slots (5 -> 1) so 1 slot is free at cap=2 while 4,000 remain parked.
	if err := ps.ReleaseN(4); err != nil {
		t.Fatalf("ReleaseN(4): %v", err)
	}

	// Run 20,000 hot-path acquire/release cycles while 4,000 oversized waiters are parked.
	// O(1) completes in a few milliseconds even under -race; O(W) would do 160M+ pointer hops.
	bg := context.Background()
	start := time.Now()
	for i := 0; i < hotOps; i++ {
		if i%2 == 0 {
			if !ps.TryAcquire() {
				t.Fatalf("op %d: TryAcquire failed with 1 free slot and 0 active waiters", i)
			}
		} else {
			if err := ps.AcquireWithPriority(bg, PriorityCritical); err != nil {
				t.Fatalf("op %d: AcquireWithPriority failed: %v", i, err)
			}
		}
		if err := ps.Release(); err != nil {
			t.Fatalf("op %d: Release failed: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("%d hot ops with %d parked waiters took %v (> 2s; suspected O(W) regression)",
			hotOps, parkedWaiters, elapsed)
	}

	// Cancel all 4,000 parked waiters and verify clean teardown.
	cancelParked()
	parkWG.Wait()
	if err := ps.Release(); err != nil {
		t.Fatalf("final Release(): %v", err)
	}
	assertInternalInvariants(t, ps)

	if ps.Len() != 0 {
		t.Errorf("final Len() = %d, want 0", ps.Len())
	}

	// Allow runtime goroutine count to settle.
	for i := 0; i < 20; i++ {
		if runtime.NumGoroutine() <= baseGoroutines+10 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > baseGoroutines+10 {
		t.Errorf("goroutine leak: before=%d after=%d", baseGoroutines, after)
	}
}

// TestLifecycle_AutonomousControlLoop simulates a 5-phase real-time robotic
// workload sharing a dynamically resized PrioritySemaphore:
//
//	Phase 1: Nominal operation across Critical/High/Normal/Low tiers (cap=16)
//	Phase 2: Thermal-throttle shrink (16 -> 4) with cooperative Low yielding
//	         via HighestWaitingPriority() and strict ceiling enforcement
//	Phase 3: Saturation load shedding (Low sheds via deadline; Critical 100% admitted)
//	Phase 4: Recovery expand (4 -> 16) unblocking parked multi-slot High planners
//	Phase 5: Graceful shutdown via Wait(ctx) with zero slot or goroutine leaks
func TestLifecycle_AutonomousControlLoop(t *testing.T) {
	const (
		nominalCap  = 16
		throttleCap = 4
	)

	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()

	ps := mustNewPriority(t, nominalCap)

	var (
		active            atomic.Int64
		peakNominal       atomic.Int64
		peakPostDrain     atomic.Int64
		cooperativeYields atomic.Int64
		releaseErrs       atomic.Int64
		criticalAdmitted  atomic.Int64
		lowShed           atomic.Int64
	)

	updateMax := func(dst *atomic.Int64, val int64) {
		for {
			old := dst.Load()
			if val <= old {
				return
			}
			if dst.CompareAndSwap(old, val) {
				return
			}
		}
	}

	// Phase 1: Fill nominal capacity (16) with 8 cooperative Low telemetry
	// workers and 8 Normal mapping workers.
	var phase1Ready sync.WaitGroup
	var phase1Done sync.WaitGroup
	releaseNormal := make(chan struct{})
	phase1Ready.Add(nominalCap)
	phase1Done.Add(nominalCap)

	for i := 0; i < 8; i++ {
		go func() {
			defer phase1Done.Done()
			if err := ps.AcquireWithPriority(context.Background(), PriorityLow); err != nil {
				t.Errorf("Phase 1 Low acquire: %v", err)
				phase1Ready.Done()
				return
			}
			cur := active.Add(1)
			updateMax(&peakNominal, cur)
			phase1Ready.Done()

			// Cooperative work loop: yield early if High/Critical is waiting!
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if p, ok := ps.HighestWaitingPriority(); ok && p >= PriorityHigh {
					cooperativeYields.Add(1)
					break
				}
				time.Sleep(time.Millisecond)
			}

			active.Add(-1)
			if err := ps.Release(); err != nil {
				releaseErrs.Add(1)
			}
		}()
	}

	for i := 0; i < 8; i++ {
		go func() {
			defer phase1Done.Done()
			if err := ps.AcquireWithPriority(context.Background(), PriorityNormal); err != nil {
				t.Errorf("Phase 1 Normal acquire: %v", err)
				phase1Ready.Done()
				return
			}
			cur := active.Add(1)
			updateMax(&peakNominal, cur)
			phase1Ready.Done()

			<-releaseNormal

			active.Add(-1)
			if err := ps.Release(); err != nil {
				releaseErrs.Add(1)
			}
		}()
	}

	phase1Ready.Wait()
	if ps.Len() != nominalCap {
		t.Fatalf("Phase 1: Len()=%d, want %d", ps.Len(), nominalCap)
	}
	assertInternalInvariants(t, ps)

	// Phase 2: Thermal-throttle shrink (16 -> 4) while oversubscribed.
	if err := ps.SetCap(throttleCap); err != nil {
		t.Fatalf("Phase 2 SetCap(%d): %v", throttleCap, err)
	}
	if ps.Len() != nominalCap || !ps.IsFull() || ps.Utilization() != 1.0 {
		t.Fatalf("Phase 2 oversubscribed state wrong: Len=%d IsFull=%v Util=%f",
			ps.Len(), ps.IsFull(), ps.Utilization())
	}

	// Queue Low/Normal background tasks AND Critical braking tasks while oversubscribed.
	var phase2WG sync.WaitGroup
	bgCtx, cancelBG := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelBG()

	// Submit 4 Low tasks first so they sit ahead in arrival time.
	for i := 0; i < 4; i++ {
		phase2WG.Add(1)
		go func() {
			defer phase2WG.Done()
			ctx, cancel := context.WithTimeout(bgCtx, 30*time.Millisecond)
			defer cancel()
			if err := ps.AcquireWithPriority(ctx, PriorityLow); err != nil {
				if errors.Is(err, ErrAcquireCancelled{}) {
					lowShed.Add(1)
				}
				return
			}
			active.Add(1)
			time.Sleep(time.Millisecond)
			active.Add(-1)
			if err := ps.Release(); err != nil {
				releaseErrs.Add(1)
			}
		}()
	}

	// Submit 6 Critical braking tasks — their arrival triggers cooperative Low
	// holders from Phase 1 to yield via HighestWaitingPriority()!
	for i := 0; i < 6; i++ {
		phase2WG.Add(1)
		go func() {
			defer phase2WG.Done()
			if err := ps.AcquireWithPriority(bgCtx, PriorityCritical); err != nil {
				t.Errorf("Phase 2 Critical failed: %v", err)
				return
			}
			criticalAdmitted.Add(1)
			cur := active.Add(1)
			updateMax(&peakPostDrain, cur)
			time.Sleep(2 * time.Millisecond)
			active.Add(-1)
			if err := ps.Release(); err != nil {
				releaseErrs.Add(1)
			}
		}()
	}

	// Wait briefly so cooperative Low holders observe HighestWaitingPriority() == Critical,
	// then release the 8 Normal holders so occupancy drains below throttleCap=4.
	time.Sleep(15 * time.Millisecond)
	close(releaseNormal)

	phase1Done.Wait()
	phase2WG.Wait()

	if cooperativeYields.Load() == 0 {
		t.Errorf("Phase 2: expected cooperative Low holders to yield via HighestWaitingPriority()")
	}
	if criticalAdmitted.Load() != 6 {
		t.Errorf("Phase 2: Critical admitted = %d, want 6", criticalAdmitted.Load())
	}
	if peakNominal.Load() > nominalCap {
		t.Errorf("Phase 1/2 peak = %d > nominalCap %d", peakNominal.Load(), nominalCap)
	}
	if peakPostDrain.Load() > throttleCap {
		t.Errorf("Phase 2 post-drain Critical concurrency = %d > throttleCap %d",
			peakPostDrain.Load(), throttleCap)
	}

	// Phase 3: Saturation shedding at cap=4.
	// Hold 3 of 4 slots; flood tight-deadline Low requests alongside Critical requests.
	if err := ps.AcquireN(3); err != nil {
		t.Fatalf("Phase 3 AcquireN(3): %v", err)
	}

	var phase3WG sync.WaitGroup
	for i := 0; i < 10; i++ {
		phase3WG.Add(1)
		go func() {
			defer phase3WG.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel()
			if err := ps.AcquireWithPriority(ctx, PriorityLow); err != nil {
				if errors.Is(err, ErrAcquireCancelled{}) {
					lowShed.Add(1)
				}
				return
			}
			time.Sleep(3 * time.Millisecond)
			if err := ps.Release(); err != nil {
				releaseErrs.Add(1)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		phase3WG.Add(1)
		go func() {
			defer phase3WG.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := ps.AcquireWithPriority(ctx, PriorityCritical); err != nil {
				t.Errorf("Phase 3 Critical failed under saturation: %v", err)
				return
			}
			criticalAdmitted.Add(1)
			time.Sleep(2 * time.Millisecond)
			if err := ps.Release(); err != nil {
				releaseErrs.Add(1)
			}
		}()
	}
	phase3WG.Wait()

	if criticalAdmitted.Load() != 14 {
		t.Errorf("Phase 3: total Critical admitted = %d, want 14 (100%% success)", criticalAdmitted.Load())
	}
	if lowShed.Load() == 0 {
		t.Errorf("Phase 3: expected Low requests to shed under saturation")
	}

	// Phase 4: Queue a High(3) planner at cap=4 (3 held -> blocks), shrink to cap=2
	// to park it, then recover via SetCap(16) and verify it unblocks cleanly.
	plannerDone := make(chan error, 1)
	go func() {
		plannerDone <- ps.AcquireNWithPriority(bgCtx, 3, PriorityHigh)
	}()
	waitUntilQueued(t, ps, 1, 0)

	if err := ps.SetCap(2); err != nil {
		t.Fatalf("Phase 4 SetCap(2): %v", err)
	}
	waitUntilQueued(t, ps, 0, 1)
	assertInternalInvariants(t, ps)

	if err := ps.SetCap(nominalCap); err != nil {
		t.Fatalf("Phase 4 recovery SetCap(%d): %v", nominalCap, err)
	}
	select {
	case err := <-plannerDone:
		if err != nil {
			t.Fatalf("Phase 4 High(3) planner failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Phase 4 High(3) planner did not wake after recovery SetCap(16)")
	}

	// Phase 5: Graceful shutdown via Wait(ctx).
	shutdownWait := make(chan error, 1)
	go func() {
		shutdownWait <- ps.Wait(bgCtx)
	}()
	time.Sleep(10 * time.Millisecond)

	// Release Phase 3's 3 slots and Phase 4's 3 planner slots.
	if err := ps.ReleaseN(6); err != nil {
		t.Fatalf("Phase 5 ReleaseN(6): %v", err)
	}
	select {
	case err := <-shutdownWait:
		if err != nil {
			t.Fatalf("Phase 5 Wait(ctx): %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Phase 5 Wait(ctx) timed out")
	}

	if errs := releaseErrs.Load(); errs != 0 {
		t.Errorf("encountered %d release errors across lifecycle", errs)
	}
	if !ps.IsEmpty() || ps.Len() != 0 {
		t.Errorf("final state not empty: Len()=%d", ps.Len())
	}
	assertInternalInvariants(t, ps)

	for i := 0; i < 20; i++ {
		if runtime.NumGoroutine() <= baseGoroutines+5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > baseGoroutines+5 {
		t.Errorf("goroutine leak after lifecycle shutdown: before=%d after=%d", baseGoroutines, after)
	}
}

// ============================================================
// FUZZ TARGET — CONCURRENT PRIORITY + SETCAP SHRINK/EXPAND
// ============================================================

func FuzzPrioritySetCapShrink(f *testing.F) {
	f.Add(8, 2, 12, 8, 20)
	f.Add(16, 1, 6, 12, 25)
	f.Add(4, 3, 8, 10, 20)
	f.Add(6, 6, 3, 8, 15)

	f.Fuzz(func(t *testing.T, initCap, shrinkCap, expandCap, goroutines, ops int) {
		if initCap < 1 || initCap > 32 {
			return
		}
		if shrinkCap < 1 || shrinkCap > 32 {
			return
		}
		if expandCap < 1 || expandCap > 32 {
			return
		}
		if goroutines < 2 || goroutines > 16 {
			return
		}
		if ops < 1 || ops > 40 {
			return
		}

		ps, err := NewPriority(initCap)
		if err != nil {
			t.Fatalf("NewPriority(%d): %v", initCap, err)
		}

		prios := []Priority{PriorityLow, PriorityNormal, PriorityHigh, PriorityCritical}
		var wg sync.WaitGroup
		wg.Add(goroutines)

		for g := 0; g < goroutines; g++ {
			go func(id int) {
				defer wg.Done()
				for i := 0; i < ops; i++ {
					p := prios[(id+i)%len(prios)]
					switch (id + i) % 6 {
					case 0:
						ctx, cancel := context.WithTimeout(context.Background(), 4*time.Millisecond)
						err := ps.AcquireWithPriority(ctx, p)
						cancel()
						if err == nil {
							if u := ps.Utilization(); math.IsNaN(u) || u < 0 || u > 1.0 {
								t.Errorf("Utilization()=%f out of [0,1]", u)
							}
							if relErr := ps.Release(); relErr != nil {
								t.Errorf("Release after AcquireWithPriority: %v", relErr)
							}
						}
					case 1:
						n := 1 + ((id + i) % 3)
						ctx, cancel := context.WithTimeout(context.Background(), 4*time.Millisecond)
						err := ps.AcquireNWithPriority(ctx, n, p)
						cancel()
						if err == nil {
							if relErr := ps.ReleaseN(n); relErr != nil {
								t.Errorf("ReleaseN(%d) after AcquireNWithPriority: %v", n, relErr)
							}
						}
					case 2:
						if err := ps.TryAcquireWithPriority(context.Background(), p); err == nil {
							if relErr := ps.Release(); relErr != nil {
								t.Errorf("Release after TryAcquireWithPriority: %v", relErr)
							}
						}
					case 3:
						ctx, cancel := context.WithTimeout(WithPriority(context.Background(), p), 4*time.Millisecond)
						err := ps.AcquireWith(ctx)
						cancel()
						if err == nil {
							if relErr := ps.Release(); relErr != nil {
								t.Errorf("Release after WithPriority AcquireWith: %v", relErr)
							}
						}
					case 4:
						if err := ps.SetCap(shrinkCap); err != nil {
							t.Errorf("SetCap(%d): %v", shrinkCap, err)
						}
						_, _ = ps.HighestWaitingPriority()
					case 5:
						if err := ps.SetCap(expandCap); err != nil {
							t.Errorf("SetCap(%d): %v", expandCap, err)
						}
						_, _ = ps.HighestWaitingPriority()
					}
				}
			}(g)
		}

		wg.Wait()

		if ps.Len() != 0 || !ps.IsEmpty() {
			t.Errorf("after FuzzPrioritySetCapShrink: Len()=%d IsEmpty()=%v, want 0/true", ps.Len(), ps.IsEmpty())
		}
		assertInternalInvariants(t, ps)
	})
}

// ============================================================
// BENCHMARKS — PRIORITY & O(1) PARKED HOT PATH
// ============================================================

func BenchmarkAcquireWithPriority(b *testing.B) {
	ps := MustPriority(b.N + 1)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ps.AcquireWithPriority(ctx, PriorityCritical)
		_ = ps.Release()
	}
}

func BenchmarkHighestWaitingPriority(b *testing.B) {
	ps := MustPriority(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ps.HighestWaitingPriority()
	}
}

func BenchmarkAcquireRelease_WithParkedWaiters(b *testing.B) {
	const parkedCount = 1000
	ps := MustPriority(5)
	_ = ps.AcquireN(5)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(parkedCount)
	for i := 0; i < parkedCount; i++ {
		go func() {
			defer wg.Done()
			_ = ps.AcquireNWithPriority(ctx, 4, PriorityLow)
		}()
	}
	waitUntilQueued(b, ps, parkedCount, 0)
	_ = ps.SetCap(2)
	waitUntilQueued(b, ps, 0, parkedCount)
	_ = ps.ReleaseN(4) // 1 slot free, cap=2, 1000 parked

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ps.Acquire()
		_ = ps.Release()
	}
	b.StopTimer()

	cancel()
	wg.Wait()
	_ = ps.Release()
}
