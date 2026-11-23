package sema

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func mustNewPriority(t *testing.T, c int) PrioritySemaphore {
	t.Helper()
	ps, err := NewPriority(c)
	if err != nil {
		t.Fatalf("NewPriority(%d): %v", c, err)
	}
	return ps
}

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

	// All standard Semaphore constructors satisfy PrioritySemaphore.
	if _, ok := mustNew(t, 3).(PrioritySemaphore); !ok {
		t.Errorf("New(3) does not implement PrioritySemaphore")
	}
	if _, ok := Must(3).(PrioritySemaphore); !ok {
		t.Errorf("Must(3) does not implement PrioritySemaphore")
	}
	semObs, err := NewWithObserver(3, &mockObserver{})
	if err != nil {
		t.Fatalf("NewWithObserver(3): %v", err)
	}
	if _, ok := semObs.(PrioritySemaphore); !ok {
		t.Errorf("NewWithObserver(3) does not implement PrioritySemaphore")
	}

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
}

func TestPriority_StrictTierOrdering(t *testing.T) {
	ps := mustNewPriority(t, 1)
	ps.Acquire()

	type req struct {
		name string
		prio Priority
	}
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

	if err := ps.Wait(ctx); err != nil {
		t.Fatalf("final Wait: %v", err)
	}
	assertInternalInvariants(t, ps)
}

func TestPriority_HigherBypassesLowerMultiSlotWaiter(t *testing.T) {
	ps := mustNewPriority(t, 5)
	if err := ps.AcquireN(3); err != nil {
		t.Fatalf("AcquireN(3): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	highDone := make(chan error, 1)
	go func() {
		highDone <- ps.AcquireNWithPriority(ctx, 4, PriorityHigh)
	}()
	waitUntilQueued(t, ps, 1, 0)

	// Same-or-lower priority callers must not starve High(4).
	for _, p := range []Priority{PriorityHigh, PriorityNormal, PriorityLow} {
		if err := ps.TryAcquireWithPriority(ctx, p); !errors.Is(err, ErrNoSlot{}) {
			t.Errorf("%v TryAcquireWithPriority = %v, want ErrNoSlot", p, err)
		}
	}

	// Strictly higher Critical(1) bypasses High(4).
	if err := ps.TryAcquireWithPriority(ctx, PriorityCritical); err != nil {
		t.Fatalf("Critical TryAcquireWithPriority failed: %v", err)
	}
	if err := ps.Release(); err != nil {
		t.Fatalf("Release Critical: %v", err)
	}

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
		t.Fatalf("empty: got (%v, %v), want (normal, false)", p, ok)
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

	// Shrink to 2 parks High(4), dropping highest active waiter back to Low.
	if err := ps.SetCap(2); err != nil {
		t.Fatalf("SetCap(2): %v", err)
	}
	assertInternalInvariants(t, ps)
	if p, ok := ps.HighestWaitingPriority(); !ok || p != PriorityLow {
		t.Fatalf("after parking High(4): got (%v, %v), want (low, true)", p, ok)
	}

	cancelLow()
	waitUntilQueued(t, ps, 0, 1)
	if p, ok := ps.HighestWaitingPriority(); ok || p != PriorityNormal {
		t.Fatalf("only parked High(4): got (%v, %v), want (normal, false)", p, ok)
	}

	// Expand to 4 unparks High(4).
	if err := ps.SetCap(4); err != nil {
		t.Fatalf("SetCap(4): %v", err)
	}
	assertInternalInvariants(t, ps)
	if p, ok := ps.HighestWaitingPriority(); !ok || p != PriorityHigh {
		t.Fatalf("after unparking High(4): got (%v, %v), want (high, true)", p, ok)
	}

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

func TestPriority_CooperativeYield(t *testing.T) {
	ps := mustNewPriority(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Low holder acquires the sole slot and yields as soon as HighestWaitingPriority() >= High.
	lowHeld := make(chan struct{})
	lowYielded := make(chan Priority, 1)
	go func() {
		if err := ps.AcquireWithPriority(ctx, PriorityLow); err != nil {
			t.Errorf("Low acquire: %v", err)
			return
		}
		close(lowHeld)
		for {
			if p, ok := ps.HighestWaitingPriority(); ok && p >= PriorityHigh {
				_ = ps.Release()
				lowYielded <- p
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	<-lowHeld

	// Critical waiter triggers the cooperative yield and acquires cleanly.
	critDone := make(chan error, 1)
	go func() {
		err := ps.AcquireWithPriority(ctx, PriorityCritical)
		if err == nil {
			err = ps.Release()
		}
		critDone <- err
	}()

	select {
	case seen := <-lowYielded:
		if seen != PriorityCritical {
			t.Errorf("Low holder observed %v, want PriorityCritical", seen)
		}
	case <-time.After(time.Second):
		t.Fatal("Low holder did not cooperatively yield")
	}

	select {
	case err := <-critDone:
		if err != nil {
			t.Fatalf("Critical acquire/release failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Critical waiter did not proceed after cooperative yield")
	}
	assertInternalInvariants(t, ps)
}

func TestPriority_ObserverEmission(t *testing.T) {
	obs := &mockObserver{}
	sem, err := NewWithObserver(3, obs)
	if err != nil {
		t.Fatalf("NewWithObserver: %v", err)
	}
	ps := sem.(PrioritySemaphore)

	bg := context.Background()
	cancelledCtx, cancel := context.WithCancel(bg)
	cancel()

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

	// Failed/cancelled/invalid calls must not emit OnAcquire.
	negCalls := []struct {
		name string
		err  error
		want error
	}{
		{"cancelled AcquireWithPriority", ps.AcquireWithPriority(cancelledCtx, PriorityCritical), ErrAcquireCancelled{}},
		{"cancelled AcquireNWithPriority", ps.AcquireNWithPriority(cancelledCtx, 1, PriorityHigh), ErrAcquireCancelled{}},
		{"invalid AcquireNWithPriority", ps.AcquireNWithPriority(bg, 0, PriorityHigh), ErrInvalidN{}},
		{"exceeds cap AcquireNWithPriority", ps.AcquireNWithPriority(bg, 10, PriorityHigh), ErrNExceedsCap{}},
		{"cancelled TryAcquireWithPriority", ps.TryAcquireWithPriority(cancelledCtx, PriorityCritical), ErrAcquireCancelled{}},
		{"full TryAcquireWithPriority", ps.TryAcquireWithPriority(bg, PriorityCritical), ErrNoSlot{}},
		{"cancelled TryAcquireNWithPriority", ps.TryAcquireNWithPriority(cancelledCtx, 1, PriorityHigh), ErrAcquireCancelled{}},
		{"invalid TryAcquireNWithPriority", ps.TryAcquireNWithPriority(bg, 0, PriorityHigh), ErrInvalidN{}},
		{"exceeds cap TryAcquireNWithPriority", ps.TryAcquireNWithPriority(bg, 10, PriorityHigh), ErrNExceedsCap{}},
		{"full TryAcquireNWithPriority", ps.TryAcquireNWithPriority(bg, 1, PriorityHigh), ErrNoSlot{}},
	}
	for _, tc := range negCalls {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: got %v, want %T", tc.name, tc.err, tc.want)
		}
	}
	if obs.AcquireCount() != 3 {
		t.Errorf("AcquireCount after failures = %d, want 3", obs.AcquireCount())
	}

	if err := ps.ReleaseN(3); err != nil {
		t.Fatalf("ReleaseN(3): %v", err)
	}
	if err := ps.AcquireNWithPriority(bg, 2, PriorityHigh); err != nil {
		t.Fatalf("AcquireNWithPriority(2): %v", err)
	}
	if obs.AcquireCount() != 4 {
		t.Errorf("AcquireCount after bulk acquire = %d, want 4", obs.AcquireCount())
	}
	if err := ps.ReleaseN(2); err != nil {
		t.Fatalf("ReleaseN(2): %v", err)
	}
	assertInternalInvariants(t, ps)
}

func FuzzPrioritySetCapShrink(f *testing.F) {
	f.Add(8, 2, 12, 8, 20)
	f.Add(16, 1, 6, 12, 25)
	f.Add(4, 3, 8, 10, 20)
	f.Add(6, 6, 3, 8, 15)

	f.Fuzz(func(t *testing.T, initCap, shrinkCap, expandCap, goroutines, ops int) {
		if initCap < 1 || initCap > 32 || shrinkCap < 1 || shrinkCap > 32 || expandCap < 1 || expandCap > 32 {
			return
		}
		if goroutines < 2 || goroutines > 16 || ops < 1 || ops > 40 {
			return
		}

		ps, err := NewPriority(initCap)
		if err != nil {
			t.Fatalf("NewPriority(%d): %v", initCap, err)
		}

		prios := [numPriorities]Priority{PriorityLow, PriorityNormal, PriorityHigh, PriorityCritical}
		var wg sync.WaitGroup
		wg.Add(goroutines)

		for g := 0; g < goroutines; g++ {
			go func(id int) {
				defer wg.Done()
				for i := 0; i < ops; i++ {
					p := prios[(id+i)%numPriorities]
					switch (id + i) % 5 {
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
						if err := ps.SetCap(shrinkCap); err != nil {
							t.Errorf("SetCap(%d): %v", shrinkCap, err)
						}
						_, _ = ps.HighestWaitingPriority()
					case 4:
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
			t.Errorf("final Len()=%d IsEmpty()=%v, want 0/true", ps.Len(), ps.IsEmpty())
		}
		assertInternalInvariants(t, ps)
	})
}

func BenchmarkAcquireWithPriority(b *testing.B) {
	ps, _ := NewPriority(b.N + 1)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ps.AcquireWithPriority(ctx, PriorityCritical)
		_ = ps.Release()
	}
}

func BenchmarkHighestWaitingPriority(b *testing.B) {
	ps, _ := NewPriority(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ps.HighestWaitingPriority()
	}
}

func BenchmarkAcquireRelease_WithParkedWaiters(b *testing.B) {
	const parkedCount = 1000
	ps, _ := NewPriority(5)
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
	_ = ps.ReleaseN(4)

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
