package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andreimerlescu/sema"
)

// ============================================================
// GATEWAY CONSTRUCTOR
// ============================================================

func TestNewGateway_Valid(t *testing.T) {
	gw, err := NewGateway(8)
	if err != nil {
		t.Fatalf("NewGateway(8): %v", err)
	}
	if gw.sem.Cap() != 8 {
		t.Errorf("Cap() = %d, want 8", gw.sem.Cap())
	}
}

func TestNewGateway_Invalid(t *testing.T) {
	_, err := NewGateway(0)
	if err == nil {
		t.Fatal("NewGateway(0): expected error")
	}
}

// ============================================================
// PHASE 2: HANDLE REQUESTS
// ============================================================

func TestHandleRequest_Success(t *testing.T) {
	gw, _ := NewGateway(4)
	ctx := context.Background()

	called := false
	ok := gw.HandleRequest(ctx, func() { called = true })

	if !ok {
		t.Error("HandleRequest: expected true")
	}
	if !called {
		t.Error("work function was not called")
	}
	if gw.sem.Len() != 0 {
		t.Errorf("Len() = %d after HandleRequest, want 0 (slot leaked)", gw.sem.Len())
	}
}

func TestHandleRequest_ContextCancelled(t *testing.T) {
	gw, _ := NewGateway(1)

	// Fill the gateway.
	gw.sem.Acquire()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ok := gw.HandleRequest(ctx, func() {
		t.Error("work should not run on cancelled context")
	})

	if ok {
		t.Error("HandleRequest: expected false on cancelled context")
	}

	gw.sem.Release()
}

func TestHandleRequest_Concurrent(t *testing.T) {
	gw, _ := NewGateway(4)

	var served atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			if gw.HandleRequest(ctx, func() {
				time.Sleep(10 * time.Millisecond)
			}) {
				served.Add(1)
			}
		}()
	}

	wg.Wait()

	if served.Load() == 0 {
		t.Error("no requests served")
	}
	if gw.sem.Len() != 0 {
		t.Errorf("Len() = %d after concurrent requests, want 0", gw.sem.Len())
	}
}

func TestHandlePremiumRequest_Success(t *testing.T) {
	gw, _ := NewGateway(6)
	ctx := context.Background()

	called := false
	ok := gw.HandlePremiumRequest(ctx, 3, func() { called = true })

	if !ok {
		t.Error("HandlePremiumRequest: expected true")
	}
	if !called {
		t.Error("work function was not called")
	}
	if gw.sem.Len() != 0 {
		t.Errorf("Len() = %d, want 0", gw.sem.Len())
	}
}

func TestHandlePremiumRequest_Rejected(t *testing.T) {
	gw, _ := NewGateway(2)
	gw.sem.Acquire()
	gw.sem.Acquire() // fill

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	ok := gw.HandlePremiumRequest(ctx, 2, func() {
		t.Error("work should not run when full")
	})

	if ok {
		t.Error("HandlePremiumRequest: expected false when full")
	}

	gw.sem.ReleaseN(2)
}

// ============================================================
// PHASE 3: NON-BLOCKING FAST PATH
// ============================================================

func TestHealthCheck_OK(t *testing.T) {
	gw, _ := NewGateway(4)
	status := gw.HealthCheck()
	if status != "ok" {
		t.Errorf("HealthCheck() = %q, want %q", status, "ok")
	}
}

func TestHealthCheck_Degraded(t *testing.T) {
	gw, _ := NewGateway(1)
	gw.sem.Acquire() // fill

	status := gw.HealthCheck()
	if status != "degraded" {
		t.Errorf("HealthCheck() = %q, want %q", status, "degraded")
	}

	gw.sem.Release()
}

func TestHealthCheckWith_OK(t *testing.T) {
	gw, _ := NewGateway(4)
	status, err := gw.HealthCheckWith(context.Background())
	if err != nil {
		t.Errorf("HealthCheckWith: unexpected error %v", err)
	}
	if status != "ok" {
		t.Errorf("status = %q, want %q", status, "ok")
	}
}

func TestHealthCheckWith_CancelledContext(t *testing.T) {
	gw, _ := NewGateway(4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	status, err := gw.HealthCheckWith(ctx)
	if err == nil {
		t.Error("HealthCheckWith: expected error on cancelled context")
	}
	if status != "degraded" {
		t.Errorf("status = %q, want %q", status, "degraded")
	}
}

func TestHealthCheckWith_Full(t *testing.T) {
	gw, _ := NewGateway(1)
	gw.sem.Acquire()

	status, err := gw.HealthCheckWith(context.Background())
	if err == nil {
		t.Error("HealthCheckWith: expected error when full")
	}
	if status != "degraded" {
		t.Errorf("status = %q, want %q", status, "degraded")
	}

	gw.sem.Release()
}

func TestTryBatch_Success(t *testing.T) {
	gw, _ := NewGateway(10)
	if !gw.TryBatch(5) {
		t.Error("TryBatch(5): expected true on empty gateway")
	}
	if gw.sem.Len() != 5 {
		t.Errorf("Len() = %d, want 5", gw.sem.Len())
	}
	gw.BatchRelease(5)
}

func TestTryBatch_Fails(t *testing.T) {
	gw, _ := NewGateway(3)
	gw.sem.AcquireN(3) // fill

	if gw.TryBatch(1) {
		t.Error("TryBatch(1): expected false when full")
	}

	gw.sem.ReleaseN(3)
}

func TestTryBatchWith_Success(t *testing.T) {
	gw, _ := NewGateway(10)
	err := gw.TryBatchWith(context.Background(), 3)
	if err != nil {
		t.Errorf("TryBatchWith: unexpected error %v", err)
	}
	if gw.sem.Len() != 3 {
		t.Errorf("Len() = %d, want 3", gw.sem.Len())
	}
	gw.BatchRelease(3)
}

func TestTryBatchWith_CancelledContext(t *testing.T) {
	gw, _ := NewGateway(10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := gw.TryBatchWith(ctx, 1)
	if err == nil {
		t.Error("TryBatchWith: expected error on cancelled context")
	}
	if !errors.Is(err, sema.ErrAcquireCancelled{}) {
		t.Errorf("error = %T, want ErrAcquireCancelled", err)
	}
}

func TestTryBatchWith_NoSlot(t *testing.T) {
	gw, _ := NewGateway(2)
	gw.sem.AcquireN(2)

	err := gw.TryBatchWith(context.Background(), 1)
	if err == nil {
		t.Error("TryBatchWith: expected error when full")
	}
	if !errors.Is(err, sema.ErrNoSlot{}) {
		t.Errorf("error = %T, want ErrNoSlot", err)
	}

	gw.sem.ReleaseN(2)
}

// ============================================================
// PHASE 4: TIMEOUT ACQUISITION
// ============================================================

func TestTimeoutAcquire_Success(t *testing.T) {
	gw, _ := NewGateway(4)
	if !gw.TimeoutAcquire(time.Second) {
		t.Error("TimeoutAcquire: expected true")
	}
	if err := gw.TimeoutRelease(); err != nil {
		t.Errorf("TimeoutRelease: %v", err)
	}
}

func TestTimeoutAcquire_Expires(t *testing.T) {
	gw, _ := NewGateway(1)
	gw.sem.Acquire()

	if gw.TimeoutAcquire(50 * time.Millisecond) {
		t.Error("TimeoutAcquire: expected false when full")
	}

	gw.sem.Release()
}

func TestBatchAcquire_Success(t *testing.T) {
	gw, _ := NewGateway(10)
	if !gw.BatchAcquire(3, time.Second) {
		t.Error("BatchAcquire: expected true")
	}
	if gw.sem.Len() != 3 {
		t.Errorf("Len() = %d, want 3", gw.sem.Len())
	}
	gw.BatchRelease(3)
}

func TestBatchAcquire_Expires(t *testing.T) {
	gw, _ := NewGateway(1)
	gw.sem.Acquire()

	if gw.BatchAcquire(1, 50*time.Millisecond) {
		t.Error("BatchAcquire: expected false when full")
	}

	gw.sem.Release()
}

func TestBatchRelease_Error(t *testing.T) {
	gw, _ := NewGateway(5)
	err := gw.BatchRelease(1) // nothing held
	if err == nil {
		t.Error("BatchRelease: expected error on empty gateway")
	}
}

func TestTimeoutRelease_Error(t *testing.T) {
	gw, _ := NewGateway(5)
	err := gw.TimeoutRelease() // nothing held
	if err == nil {
		t.Error("TimeoutRelease: expected error on empty gateway")
	}
}

// ============================================================
// PHASE 5: INTROSPECTION
// ============================================================

func TestStats_EmptyGateway(t *testing.T) {
	gw, _ := NewGateway(8)

	l, c, u, s, empty, full := gw.Stats()

	if l != 0 {
		t.Errorf("len = %d, want 0", l)
	}
	if c != 8 {
		t.Errorf("cap = %d, want 8", c)
	}
	if u != 0.0 {
		t.Errorf("util = %f, want 0", u)
	}
	if s != 0.0 {
		t.Errorf("smoothed = %f, want 0", s)
	}
	if !empty {
		t.Error("empty = false, want true")
	}
	if full {
		t.Error("full = true, want false")
	}
}

func TestStats_FullGateway(t *testing.T) {
	gw, _ := NewGateway(2)
	gw.sem.Acquire()
	gw.sem.Acquire()

	l, c, u, _, empty, full := gw.Stats()

	if l != 2 {
		t.Errorf("len = %d, want 2", l)
	}
	if c != 2 {
		t.Errorf("cap = %d, want 2", c)
	}
	if u != 1.0 {
		t.Errorf("util = %f, want 1.0", u)
	}
	if empty {
		t.Error("empty = true, want false")
	}
	if !full {
		t.Error("full = false, want true")
	}

	gw.sem.ReleaseN(2)
}

func TestObserverSnapshot(t *testing.T) {
	gw, _ := NewGateway(5)

	gw.sem.Acquire()
	gw.sem.Release()
	gw.sem.Acquire()
	gw.sem.Release()

	acq, rel, _, _ := gw.ObserverSnapshot()
	if acq != 2 {
		t.Errorf("acquires = %d, want 2", acq)
	}
	if rel != 2 {
		t.Errorf("releases = %d, want 2", rel)
	}
}

func TestObserverSnapshot_WaitTracking(t *testing.T) {
	gw, _ := NewGateway(5)

	// Wait on empty semaphore — immediate return.
	gw.WaitIdle(context.Background())

	_, _, waits, wErr := gw.ObserverSnapshot()
	if waits != 1 {
		t.Errorf("waits = %d, want 1", waits)
	}
	if wErr != 0 {
		t.Errorf("waitErrors = %d, want 0", wErr)
	}
}

func TestObserverSnapshot_WaitError(t *testing.T) {
	gw, _ := NewGateway(1)
	gw.sem.Acquire() // hold a slot so Wait blocks

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	gw.WaitIdle(ctx) // will time out

	_, _, waits, wErr := gw.ObserverSnapshot()
	if waits != 1 {
		t.Errorf("waits = %d, want 1", waits)
	}
	if wErr != 1 {
		t.Errorf("waitErrors = %d, want 1", wErr)
	}

	gw.sem.Release()
}

// ============================================================
// PHASE 6: DYNAMIC SCALING
// ============================================================

func TestResize_Expand(t *testing.T) {
	gw, _ := NewGateway(4)
	gw.sem.Acquire()

	if err := gw.Resize(10); err != nil {
		t.Fatalf("Resize(10): %v", err)
	}

	if gw.sem.Cap() != 10 {
		t.Errorf("Cap() = %d, want 10", gw.sem.Cap())
	}
	if gw.sem.Len() != 1 {
		t.Errorf("Len() = %d, want 1 (preserved)", gw.sem.Len())
	}

	gw.sem.Release()
}

func TestResize_Shrink(t *testing.T) {
	gw, _ := NewGateway(10)
	gw.sem.AcquireN(8)

	if err := gw.Resize(3); err != nil {
		t.Fatalf("Resize(3): %v", err)
	}

	if gw.sem.Cap() != 3 {
		t.Errorf("Cap() = %d, want 3", gw.sem.Cap())
	}
	if gw.sem.Len() != 0 {
		t.Errorf("Len() = %d, want 0 (drained on shrink)", gw.sem.Len())
	}
}

func TestResize_Default(t *testing.T) {
	gw, _ := NewGateway(4)
	if err := gw.Resize(-1); err != nil {
		t.Fatalf("Resize(-1): %v", err)
	}
	// Default cap is 17 (from const.go).
	if gw.sem.Cap() != 17 {
		t.Errorf("Cap() = %d, want 17 (defaultCap)", gw.sem.Cap())
	}
}

func TestResize_Invalid(t *testing.T) {
	gw, _ := NewGateway(4)
	err := gw.Resize(0)
	if err == nil {
		t.Error("Resize(0): expected error")
	}
}

// ============================================================
// PHASE 7: GRACEFUL SHUTDOWN
// ============================================================

func TestWaitIdle_AlreadyEmpty(t *testing.T) {
	gw, _ := NewGateway(4)
	err := gw.WaitIdle(context.Background())
	if err != nil {
		t.Errorf("WaitIdle on empty: unexpected error %v", err)
	}
}

func TestWaitIdle_WaitsForRelease(t *testing.T) {
	gw, _ := NewGateway(4)
	gw.sem.Acquire()
	gw.sem.Acquire()

	done := make(chan error, 1)
	go func() {
		done <- gw.WaitIdle(context.Background())
	}()

	time.Sleep(30 * time.Millisecond)
	gw.sem.Release()
	gw.sem.Release()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WaitIdle: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("WaitIdle did not return after releases")
	}
}

func TestWaitIdle_ContextExpires(t *testing.T) {
	gw, _ := NewGateway(1)
	gw.sem.Acquire()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := gw.WaitIdle(ctx)
	if err == nil {
		t.Error("WaitIdle: expected error on timeout")
	}
	if !errors.Is(err, sema.ErrAcquireCancelled{}) {
		t.Errorf("error = %T, want ErrAcquireCancelled", err)
	}

	gw.sem.Release()
}

func TestForceDrain(t *testing.T) {
	gw, _ := NewGateway(5)
	gw.sem.AcquireN(4)

	if err := gw.ForceDrain(); err != nil {
		t.Fatalf("ForceDrain: %v", err)
	}

	if gw.sem.Len() != 0 {
		t.Errorf("Len() = %d after drain, want 0", gw.sem.Len())
	}
	if !gw.sem.IsEmpty() {
		t.Error("IsEmpty() = false after drain")
	}
}

func TestHardReset(t *testing.T) {
	gw, _ := NewGateway(5)
	gw.sem.AcquireN(5)

	if err := gw.HardReset(); err != nil {
		t.Fatalf("HardReset: %v", err)
	}

	if gw.sem.Len() != 0 {
		t.Errorf("Len() = %d after reset, want 0", gw.sem.Len())
	}
	if gw.sem.Cap() != 5 {
		t.Errorf("Cap() = %d after reset, want 5", gw.sem.Cap())
	}
}

// ============================================================
// FULL LIFECYCLE
// ============================================================

func TestFullLifecycle(t *testing.T) {
	// This test runs the same sequence as main() but with assertions
	// at each step to verify correctness.
	gw, err := NewGateway(8)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	// Must constructor.
	bgPool := sema.Must(4)
	if bgPool.Cap() != 4 {
		t.Errorf("bgPool.Cap() = %d, want 4", bgPool.Cap())
	}

	// Concurrent requests.
	var served atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			if gw.HandleRequest(ctx, func() {
				time.Sleep(10 * time.Millisecond)
			}) {
				served.Add(1)
			}
		}()
	}
	wg.Wait()
	if served.Load() == 0 {
		t.Error("lifecycle: no requests served")
	}
	if gw.sem.Len() != 0 {
		t.Errorf("lifecycle: Len()=%d after requests, want 0", gw.sem.Len())
	}

	// Health checks.
	if s := gw.HealthCheck(); s != "ok" {
		t.Errorf("health = %q, want ok", s)
	}

	// Timeout acquire.
	if !gw.TimeoutAcquire(time.Second) {
		t.Error("timeout acquire failed")
	}
	gw.TimeoutRelease()

	// Batch acquire.
	if !gw.BatchAcquire(2, time.Second) {
		t.Error("batch acquire failed")
	}
	gw.BatchRelease(2)

	// Stats.
	l, c, _, _, empty, _ := gw.Stats()
	if l != 0 || c != 8 || !empty {
		t.Errorf("stats unexpected: len=%d cap=%d empty=%v", l, c, empty)
	}

	// Resize.
	gw.Resize(16)
	if gw.sem.Cap() != 16 {
		t.Errorf("Cap() = %d after resize, want 16", gw.sem.Cap())
	}

	// Shutdown.
	gw.sem.Acquire()
	gw.ForceDrain()
	gw.HardReset()

	if err := gw.WaitIdle(context.Background()); err != nil {
		t.Errorf("final wait: %v", err)
	}
	if !gw.sem.IsEmpty() {
		t.Errorf("not empty after shutdown: Len()=%d", gw.sem.Len())
	}

	// Background pool.
	bgPool.Acquire()
	bgPool.Release()
}

// ============================================================
// RUN FUNCTION
// ============================================================

func TestRun(t *testing.T) {
	// Execute the full run() function and verify it completes without error.
	if err := run(); err != nil {
		t.Fatalf("run(): %v", err)
	}
}

// ============================================================
// EDGE CASES
// ============================================================

func TestGatewayObserver_AllFields(t *testing.T) {
	obs := &gatewayObserver{}

	obs.OnAcquire(1, 5)
	obs.OnAcquire(2, 5)
	obs.OnRelease(1, 5)
	obs.OnWaitStart()
	obs.OnWaitEnd(nil)
	obs.OnWaitStart()
	obs.OnWaitEnd(context.DeadlineExceeded)

	a, r, w, we := obs.snapshot()
	if a != 2 {
		t.Errorf("acquires = %d, want 2", a)
	}
	if r != 1 {
		t.Errorf("releases = %d, want 1", r)
	}
	if w != 2 {
		t.Errorf("waits = %d, want 2", w)
	}
	if we != 1 {
		t.Errorf("waitErrors = %d, want 1", we)
	}
}

func TestHandleRequest_WorkPanicsDoesNotLeakSlot(t *testing.T) {
	gw, _ := NewGateway(4)

	defer func() {
		recover() // swallow the panic from HandleRequest
	}()

	// HandleRequest uses defer Release(), so even if work panics
	// the slot should be released. However since the panic propagates
	// out of HandleRequest, we need to recover here.
	gw.HandleRequest(context.Background(), func() {
		panic("boom")
	})

	// After recovery, the slot should have been released by the deferred Release.
	if gw.sem.Len() != 0 {
		t.Errorf("Len() = %d after panic, want 0 (slot leaked)", gw.sem.Len())
	}
}

func TestHandlePremiumRequest_WorkPanicsDoesNotLeakSlots(t *testing.T) {
	gw, _ := NewGateway(6)

	defer func() {
		recover()
	}()

	gw.HandlePremiumRequest(context.Background(), 3, func() {
		panic("boom")
	})

	if gw.sem.Len() != 0 {
		t.Errorf("Len() = %d after panic, want 0", gw.sem.Len())
	}
}
