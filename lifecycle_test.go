package sema

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func verifyWaiterQueue(t testing.TB, name string, q *waiterQueue, check func(w *waiter)) {
	t.Helper()
	if q.count == 0 {
		if q.head != nil || q.tail != nil {
			t.Errorf("%s: count=0 but head=%v tail=%v", name, q.head, q.tail)
		}
		return
	}
	if q.head == nil || q.tail == nil {
		t.Errorf("%s: count=%d but head=%v tail=%v", name, q.count, q.head, q.tail)
		return
	}
	if q.head.prev != nil || q.tail.next != nil {
		t.Errorf("%s: broken boundary links (head.prev=%v tail.next=%v)", name, q.head.prev, q.tail.next)
	}
	walked := 0
	var prev *waiter
	for w := q.head; w != nil; w = w.next {
		walked++
		if walked > q.count+1 {
			t.Fatalf("%s: cycle detected in waiter queue", name)
		}
		if w.prev != prev {
			t.Errorf("%s: broken prev link at node %d", name, walked)
		}
		if w.granted {
			t.Errorf("%s: queued waiter already marked granted", name)
		}
		check(w)
		prev = w
	}
	if prev != q.tail || walked != q.count {
		t.Errorf("%s: walked=%d count=%d tailMatch=%v", name, walked, q.count, prev == q.tail)
	}
}

func assertInternalInvariants(t testing.TB, sem Semaphore) {
	t.Helper()
	s := sem.(*semaphore)
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.held < 0 || s.cap < 1 {
		t.Errorf("invalid counters: held=%d cap=%d", s.held, s.cap)
	}
	if int(s.heldAtom.Load()) != s.held || int(s.capAtom.Load()) != s.cap {
		t.Errorf("atomic mirror drift: held=%d/%d cap=%d/%d",
			s.heldAtom.Load(), s.held, s.capAtom.Load(), s.cap)
	}
	if uint8(s.maskAtom.Load()) != s.activeMask {
		t.Errorf("maskAtom=%04b != activeMask=%04b", s.maskAtom.Load(), s.activeMask)
	}

	actualMulti := 0
	for idx := 0; idx < numPriorities; idx++ {
		q := &s.queues[idx]
		bitSet := (s.activeMask & (uint8(1) << uint(idx))) != 0
		if bitSet != (q.count > 0) {
			t.Errorf("tier %d: bit=%v count=%d", idx, bitSet, q.count)
		}
		var prevSeq uint64
		verifyWaiterQueue(t, priorityNames[idx], q, func(w *waiter) {
			if w.parked {
				t.Errorf("tier %d: active waiter has parked=true", idx)
			}
			if priorityBucket(w.prio) != idx {
				t.Errorf("tier %d: waiter prio=%v in wrong bucket", idx, w.prio)
			}
			if w.n < 1 || w.n > s.cap {
				t.Errorf("tier %d: active waiter n=%d outside [1, %d]", idx, w.n, s.cap)
			}
			if w.seq <= prevSeq {
				t.Errorf("tier %d: non-ascending seq (%d <= %d)", idx, w.seq, prevSeq)
			}
			if w.n > 1 {
				actualMulti++
			}
			prevSeq = w.seq
		})
	}
	if actualMulti != s.multiActive {
		t.Errorf("multiActive=%d, actual=%d", s.multiActive, actualMulti)
	}

	verifyWaiterQueue(t, "parked", &s.parked, func(w *waiter) {
		if !w.parked || w.n <= s.cap {
			t.Errorf("parked waiter invalid: parked=%v n=%d cap=%d", w.parked, w.n, s.cap)
		}
	})

	u := clampUtilization(s.held, s.cap)
	us := math.Float64frombits(s.ewma.Load())
	if math.IsNaN(u) || u < 0 || u > 1.0 || math.IsNaN(us) || us < 0 || us > 1.0+1e-9 {
		t.Errorf("metrics out of [0,1]: util=%f smoothed=%f", u, us)
	}
}

func queuedCounts(s *semaphore) (active, parked int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < numPriorities; i++ {
		active += s.queues[i].count
	}
	return active, s.parked.count
}

func waitUntilQueued(t testing.TB, sem Semaphore, wantActive, wantParked int) {
	t.Helper()
	s := sem.(*semaphore)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		active, parked := queuedCounts(s)
		if active == wantActive && parked == wantParked {
			return
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	active, parked := queuedCounts(s)
	t.Fatalf("queued counts timed out: got active=%d parked=%d, want active=%d parked=%d",
		active, parked, wantActive, wantParked)
}

func waitUntilWaiting(t testing.TB, sem Semaphore) {
	t.Helper()
	s := sem.(*semaphore)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		waiting := s.emptyCh != nil
		s.mu.Unlock()
		if waiting {
			return
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for Wait() caller to block on emptyCh")
}

func TestOverflow_MaxIntBoundaries(t *testing.T) {
	ps := mustNewPriority(t, math.MaxInt)
	assertInternalInvariants(t, ps)

	// Hold 1 slot so cap - held == math.MaxInt - 1; requesting math.MaxInt
	// must not overflow signed int arithmetic.
	ps.Acquire()
	assertInternalInvariants(t, ps)

	if ps.TryAcquireN(math.MaxInt) {
		t.Fatal("TryAcquireN(MaxInt) succeeded with 1 slot held (signed int overflow)")
	}
	var slotErr ErrNoSlot
	err := ps.TryAcquireNWith(context.Background(), math.MaxInt)
	if !errors.As(err, &slotErr) || slotErr.Available != math.MaxInt-1 {
		t.Fatalf("TryAcquireNWith(MaxInt) = %v (%+v), want Available=%d", err, slotErr, math.MaxInt-1)
	}
	if err := ps.AcquireNTimeout(math.MaxInt, 10*time.Millisecond); !errors.Is(err, ErrAcquireCancelled{}) {
		t.Fatalf("AcquireNTimeout(MaxInt) = %v, want ErrAcquireCancelled", err)
	}
	assertInternalInvariants(t, ps)

	if err := ps.Release(); err != nil {
		t.Fatalf("Release(): %v", err)
	}
	if err := ps.AcquireN(math.MaxInt); err != nil {
		t.Fatalf("AcquireN(MaxInt) on empty sem: %v", err)
	}
	if ps.Len() != math.MaxInt || !ps.IsFull() {
		t.Fatalf("after AcquireN(MaxInt): Len=%d IsFull=%v", ps.Len(), ps.IsFull())
	}

	// Shrink from math.MaxInt to 1 while holding math.MaxInt slots.
	if err := ps.SetCap(1); err != nil {
		t.Fatalf("SetCap(1): %v", err)
	}
	assertInternalInvariants(t, ps)
	if ps.TryAcquire() || ps.Utilization() != 1.0 {
		t.Fatalf("oversubscribed MaxInt state invalid: Util=%f", ps.Utilization())
	}
	if err := ps.TryAcquireWith(context.Background()); !errors.As(err, &slotErr) || slotErr.Available != 0 {
		t.Fatalf("oversubscribed TryAcquireWith = %v (%+v), want Available=0", err, slotErr)
	}

	if err := ps.ReleaseN(math.MaxInt); err != nil {
		t.Fatalf("ReleaseN(MaxInt): %v", err)
	}
	if !ps.IsEmpty() {
		t.Fatalf("expected empty after ReleaseN(MaxInt), got Len=%d", ps.Len())
	}
	assertInternalInvariants(t, ps)
}

func TestSetCap_ParkedWaiterSeqOrder(t *testing.T) {
	ps := mustNewPriority(t, 6)
	if err := ps.AcquireN(6); err != nil {
		t.Fatalf("AcquireN(6): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	woken := make(chan string, 4)
	w2Ctx, cancelW2 := context.WithCancel(ctx)
	defer cancelW2()

	// Enqueue W1(4), W2(5, cancelled while parked), W3(4) in High tier.
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

	// Shrink to 3 parks W1, W2, W3.
	if err := ps.SetCap(3); err != nil {
		t.Fatalf("SetCap(3): %v", err)
	}
	waitUntilQueued(t, ps, 0, 3)
	assertInternalInvariants(t, ps)

	// Queue newer W4(2) while W1..W3 are parked.
	go func() {
		if err := ps.AcquireNWithPriority(ctx, 2, PriorityHigh); err == nil {
			woken <- "W4"
		}
	}()
	waitUntilQueued(t, ps, 1, 3)

	// Cancel W2 while parked.
	cancelW2()
	if err := <-w2Err; !errors.Is(err, ErrAcquireCancelled{}) {
		t.Fatalf("parked W2 cancel err = %v, want ErrAcquireCancelled", err)
	}
	waitUntilQueued(t, ps, 1, 2)
	assertInternalInvariants(t, ps)

	// Expand to 4 restores W1 and W3 ahead of newer W4 by original seq.
	if err := ps.SetCap(4); err != nil {
		t.Fatalf("SetCap(4): %v", err)
	}
	waitUntilQueued(t, ps, 3, 0)
	assertInternalInvariants(t, ps)

	if err := ps.ReleaseN(6); err != nil {
		t.Fatalf("ReleaseN(6): %v", err)
	}
	if got := <-woken; got != "W1" {
		t.Fatalf("first wake = %q, want W1", got)
	}
	if err := ps.ReleaseN(4); err != nil {
		t.Fatalf("ReleaseN(4) W1: %v", err)
	}
	if got := <-woken; got != "W3" {
		t.Fatalf("second wake = %q, want W3", got)
	}
	if err := ps.ReleaseN(4); err != nil {
		t.Fatalf("ReleaseN(4) W3: %v", err)
	}
	if got := <-woken; got != "W4" {
		t.Fatalf("third wake = %q, want W4", got)
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- ps.Wait(ctx)
	}()
	waitUntilWaiting(t, ps)
	if err := ps.Reset(); err != nil {
		t.Fatalf("Reset(): %v", err)
	}
	if err := <-waitDone; err != nil {
		t.Fatalf("Wait after Reset: %v", err)
	}
	assertInternalInvariants(t, ps)
}

type linOpKind uint8

const (
	linTryAcquire linOpKind = iota
	linAcquire
	linRelease
	linSetCap
)

type linOp struct {
	kind linOpKind
	arg  int
	ok   bool
	call int64
	ret  int64
}

func stepLinModel(held, cap int, op linOp) (nextHeld, nextCap int, match bool) {
	switch op.kind {
	case linTryAcquire:
		if held < cap {
			return held + 1, cap, op.ok
		}
		return held, cap, !op.ok
	case linAcquire:
		if held < cap && op.ok {
			return held + 1, cap, true
		}
		return held, cap, false
	case linRelease:
		if held > 0 {
			return held - 1, cap, op.ok
		}
		return held, cap, !op.ok
	case linSetCap:
		return held, op.arg, op.ok
	default:
		return held, cap, false
	}
}

// isLinearizable performs Wing & Gong memoized DFS backtracking over a
// recorded concurrent history against the exact sequential (held, cap) model.
func isLinearizable(initCap int, ops []linOp) bool {
	type state struct {
		done uint32
		held int
		cap  int
	}
	allDone := uint32((1 << len(ops)) - 1)
	visited := make(map[state]bool)

	var dfs func(done uint32, held, cap int) bool
	dfs = func(done uint32, held, cap int) bool {
		if done == allDone {
			return true
		}
		st := state{done: done, held: held, cap: cap}
		if visited[st] {
			return false
		}
		visited[st] = true

		minRet := int64(math.MaxInt64)
		for i := range ops {
			if done&(1<<uint(i)) == 0 && ops[i].ret < minRet {
				minRet = ops[i].ret
			}
		}
		for i := range ops {
			if done&(1<<uint(i)) != 0 || ops[i].call >= minRet {
				continue
			}
			nh, nc, ok := stepLinModel(held, cap, ops[i])
			if ok && dfs(done|(1<<uint(i)), nh, nc) {
				return true
			}
		}
		return false
	}
	return dfs(0, 0, initCap)
}

// TestLifecycle_Linearizability records 30 randomized concurrent histories
// mixing TryAcquire, TryAcquireWithPriority, AcquireWithPriority, Release,
// and SetCap (shrink/expand) and proves via Wing & Gong backtracking that
// every history admits a valid sequential linearization.
func TestLifecycle_Linearizability(t *testing.T) {
	// Self-test: a known non-linearizable history (2 sequential TryAcquires
	// succeeding on cap=1 without Release) MUST be rejected.
	bad := []linOp{
		{kind: linTryAcquire, ok: true, call: 1, ret: 2},
		{kind: linTryAcquire, ok: true, call: 3, ret: 4},
	}
	if isLinearizable(1, bad) {
		t.Fatal("linearizability checker accepted an impossible history")
	}

	rng := rand.New(rand.NewSource(130))
	prios := [numPriorities]Priority{PriorityLow, PriorityNormal, PriorityHigh, PriorityCritical}

	for trial := 0; trial < 30; trial++ {
		initCap := 1 + rng.Intn(3)
		ps := mustNewPriority(t, initCap)

		var (
			clock atomic.Int64
			mu    sync.Mutex
			ops   = make([]linOp, 0, 18)
			wg    sync.WaitGroup
		)
		record := func(op linOp) {
			mu.Lock()
			ops = append(ops, op)
			mu.Unlock()
		}

		// Two blocking acquire+release pairs (deadlock-free since cap >= 1 always).
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		for g := 0; g < 2; g++ {
			p1 := prios[rng.Intn(numPriorities)]
			p2 := prios[rng.Intn(numPriorities)]
			wg.Add(1)
			go func(a, b Priority) {
				defer wg.Done()
				for _, p := range []Priority{a, b} {
					c := clock.Add(1)
					err := ps.AcquireWithPriority(ctx, p)
					r := clock.Add(1)
					record(linOp{kind: linAcquire, ok: err == nil, call: c, ret: r})
					if err == nil {
						c = clock.Add(1)
						relErr := ps.Release()
						r = clock.Add(1)
						record(linOp{kind: linRelease, ok: relErr == nil, call: c, ret: r})
					}
				}
			}(p1, p2)
		}

		// One non-blocking try/release worker.
		tryPrio := prios[rng.Intn(numPriorities)]
		wg.Add(1)
		go func(p Priority) {
			defer wg.Done()
			for i := 0; i < 2; i++ {
				c := clock.Add(1)
				err := ps.TryAcquireWithPriority(ctx, p)
				r := clock.Add(1)
				record(linOp{kind: linTryAcquire, ok: err == nil, call: c, ret: r})
				// Always call Release (succeeds if held, exercises ErrReleaseExceedsCount if empty).
				c = clock.Add(1)
				relErr := ps.Release()
				r = clock.Add(1)
				record(linOp{kind: linRelease, ok: relErr == nil, call: c, ret: r})
			}
		}(tryPrio)

		// One dynamic SetCap shrink/expand + TryAcquire worker.
		caps := [2]int{1 + rng.Intn(3), 1 + rng.Intn(3)}
		wg.Add(1)
		go func(cs [2]int) {
			defer wg.Done()
			for _, nc := range cs {
				c := clock.Add(1)
				err := ps.SetCap(nc)
				r := clock.Add(1)
				record(linOp{kind: linSetCap, arg: nc, ok: err == nil, call: c, ret: r})

				c = clock.Add(1)
				ok := ps.TryAcquire()
				r = clock.Add(1)
				record(linOp{kind: linTryAcquire, ok: ok, call: c, ret: r})
				if ok {
					c = clock.Add(1)
					relErr := ps.Release()
					r = clock.Add(1)
					record(linOp{kind: linRelease, ok: relErr == nil, call: c, ret: r})
				}
			}
		}(caps)

		wg.Wait()
		cancel()

		if !isLinearizable(initCap, ops) {
			t.Fatalf("trial %d (initCap=%d): non-linearizable history: %+v", trial, initCap, ops)
		}
		for ps.Len() > 0 {
			if err := ps.Release(); err != nil {
				t.Fatalf("trial %d cleanup Release: %v", trial, err)
			}
		}
		assertInternalInvariants(t, ps)
	}
}

func TestLifecycle_BoundedLatencyWithParkedWaiters(t *testing.T) {
	const (
		parkedWaiters = 1000
		hotOps        = 10000
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
		p := Priority(i%numPriorities - 1)
		go func(prio Priority) {
			defer parkWG.Done()
			if err := ps.AcquireNWithPriority(parkCtx, 4, prio); !errors.Is(err, ErrAcquireCancelled{}) {
				t.Errorf("parked waiter returned %v, want ErrAcquireCancelled", err)
			}
		}(p)
	}
	waitUntilQueued(t, ps, parkedWaiters, 0)

	// Shrink to 2 moves all 1,000 oversized waiters out of active queues.
	if err := ps.SetCap(2); err != nil {
		t.Fatalf("SetCap(2): %v", err)
	}
	waitUntilQueued(t, ps, 0, parkedWaiters)
	assertInternalInvariants(t, ps)
	if p, ok := ps.HighestWaitingPriority(); ok {
		t.Fatalf("expected no active waiters after parking, got (%v, true)", p)
	}

	// Free 4 slots (1 free at cap=2) and run 10,000 O(1) hot-path ops.
	if err := ps.ReleaseN(4); err != nil {
		t.Fatalf("ReleaseN(4): %v", err)
	}
	bg := context.Background()
	start := time.Now()
	for i := 0; i < hotOps; i++ {
		if i%2 == 0 {
			if !ps.TryAcquire() {
				t.Fatalf("op %d: TryAcquire failed", i)
			}
		} else if err := ps.AcquireWithPriority(bg, PriorityCritical); err != nil {
			t.Fatalf("op %d: AcquireWithPriority failed: %v", i, err)
		}
		if err := ps.Release(); err != nil {
			t.Fatalf("op %d: Release failed: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("%d ops with %d parked waiters took %v (suspected O(W) scan)", hotOps, parkedWaiters, elapsed)
	}

	cancelParked()
	parkWG.Wait()
	if err := ps.Release(); err != nil {
		t.Fatalf("final Release: %v", err)
	}
	assertInternalInvariants(t, ps)

	for i := 0; i < 20 && runtime.NumGoroutine() > baseGoroutines+10; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > baseGoroutines+10 {
		t.Errorf("goroutine leak: before=%d after=%d", baseGoroutines, after)
	}
}

func TestLifecycle_ShrinkShedRecoverWait(t *testing.T) {
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()

	ps := mustNewPriority(t, 8)
	if err := ps.AcquireN(8); err != nil {
		t.Fatalf("AcquireN(8): %v", err)
	}

	// Shrink while full (oversubscribed: 8 held, cap 3).
	if err := ps.SetCap(3); err != nil {
		t.Fatalf("SetCap(3): %v", err)
	}
	assertInternalInvariants(t, ps)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Shed short-deadline Low requests while oversubscribed; Critical waits and succeeds.
	lowCtx, lowCancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer lowCancel()
	if err := ps.AcquireWithPriority(lowCtx, PriorityLow); !errors.Is(err, ErrAcquireCancelled{}) {
		t.Fatalf("oversubscribed Low acquire = %v, want ErrAcquireCancelled", err)
	}

	critDone := make(chan error, 1)
	go func() {
		critDone <- ps.AcquireWithPriority(ctx, PriorityCritical)
	}()
	waitUntilQueued(t, ps, 1, 0)

	// Drain 6 slots (8 -> 2 < cap 3): Critical immediately acquires (held -> 3).
	if err := ps.ReleaseN(6); err != nil {
		t.Fatalf("ReleaseN(6): %v", err)
	}
	if err := <-critDone; err != nil {
		t.Fatalf("Critical acquire after shrink drain: %v", err)
	}

	// Queue High(3) while full at cap=3, shrink to 2 to park it, expand to 8 to recover.
	highDone := make(chan error, 1)
	go func() {
		highDone <- ps.AcquireNWithPriority(ctx, 3, PriorityHigh)
	}()
	waitUntilQueued(t, ps, 1, 0)

	if err := ps.SetCap(2); err != nil {
		t.Fatalf("SetCap(2): %v", err)
	}
	waitUntilQueued(t, ps, 0, 1)
	assertInternalInvariants(t, ps)

	if err := ps.SetCap(8); err != nil {
		t.Fatalf("SetCap(8): %v", err)
	}
	if err := <-highDone; err != nil {
		t.Fatalf("High(3) recovery acquire: %v", err)
	}

	// Graceful Wait(ctx) shutdown.
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- ps.Wait(ctx)
	}()
	waitUntilWaiting(t, ps)

	// Release remaining 6 held slots (2 original + 1 Critical + 3 High).
	if err := ps.ReleaseN(6); err != nil {
		t.Fatalf("ReleaseN(6) shutdown: %v", err)
	}
	if err := <-waitDone; err != nil {
		t.Fatalf("Wait(ctx): %v", err)
	}
	assertInternalInvariants(t, ps)

	for i := 0; i < 20 && runtime.NumGoroutine() > baseGoroutines+5; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > baseGoroutines+5 {
		t.Errorf("goroutine leak: before=%d after=%d", baseGoroutines, after)
	}
}
