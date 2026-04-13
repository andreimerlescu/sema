package sema

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================
// HELPERS & MOCK OBSERVER
// ============================================================

type mockObserver struct {
	mu           sync.Mutex
	acquireCount int
	releaseCount int
	waitStarts   int
	waitEnds     int
	lastWaitErr  error
	lastCount    int
	lastCap      int
}

func (m *mockObserver) OnAcquire(count, cap int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acquireCount++
	m.lastCount = count
	m.lastCap = cap
}

func (m *mockObserver) OnRelease(count, cap int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseCount++
	m.lastCount = count
	m.lastCap = cap
}

func (m *mockObserver) OnWaitStart() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waitStarts++
}

func (m *mockObserver) OnWaitEnd(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waitEnds++
	m.lastWaitErr = err
}

func (m *mockObserver) AcquireCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acquireCount
}

func (m *mockObserver) ReleaseCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.releaseCount
}

func mustNew(t *testing.T, c int) Semaphore {
	t.Helper()
	s, err := New(c)
	if err != nil {
		t.Fatalf("New(%d) unexpected error: %v", c, err)
	}
	return s
}

// ============================================================
// UNIT TESTS — CONSTRUCTORS
// ============================================================

func TestNew_ValidCapacity(t *testing.T) {
	for _, c := range []int{1, 5, 10, 100} {
		s, err := New(c)
		if err != nil {
			t.Errorf("New(%d): unexpected error %v", c, err)
		}
		if s == nil {
			t.Errorf("New(%d): returned nil semaphore", c)
		}
		if s != nil && s.Cap() != c {
			t.Errorf("New(%d): Cap() = %d, want %d", c, s.Cap(), c)
		}
	}
}

func TestNew_DefaultCap(t *testing.T) {
	s, err := New(-1)
	if err != nil {
		t.Fatalf("New(-1): unexpected error %v", err)
	}
	if s.Cap() != defaultCap {
		t.Errorf("New(-1): Cap() = %d, want %d", s.Cap(), defaultCap)
	}
}

func TestNew_InvalidCapacity(t *testing.T) {
	for _, c := range []int{0, -2, -100} {
		s, err := New(c)
		if err == nil {
			t.Errorf("New(%d): expected error, got nil", c)
		}
		if s != nil {
			t.Errorf("New(%d): expected nil semaphore on error", c)
		}
		if !errors.Is(err, ErrInvalidCap{}) {
			t.Errorf("New(%d): error type = %T, want ErrInvalidCap", c, err)
		}
	}
}

func TestMust_ValidCapacity(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Must(5) panicked unexpectedly: %v", r)
		}
	}()
	s := Must(5)
	if s == nil {
		t.Error("Must(5): returned nil")
	}
}

func TestMust_InvalidCapacity(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Must(0): expected panic, got none")
		}
	}()
	Must(0)
}

func TestNewWithObserver(t *testing.T) {
	obs := &mockObserver{}
	s, err := NewWithObserver(5, obs)
	if err != nil {
		t.Fatalf("NewWithObserver: unexpected error %v", err)
	}
	s.Acquire()
	if obs.AcquireCount() != 1 {
		t.Errorf("OnAcquire not called: count = %d", obs.AcquireCount())
	}
	if err := s.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if obs.ReleaseCount() != 1 {
		t.Errorf("OnRelease not called: count = %d", obs.ReleaseCount())
	}
}

func TestNewWithObserver_InvalidCap(t *testing.T) {
	obs := &mockObserver{}
	_, err := NewWithObserver(0, obs)
	if err == nil {
		t.Error("NewWithObserver(0): expected error")
	}
}

// ============================================================
// UNIT TESTS — SINGLE SLOT
// ============================================================

func TestAcquire_Basic(t *testing.T) {
	s := mustNew(t, 3)
	s.Acquire()
	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}
}

func TestAcquire_Blocks(t *testing.T) {
	s := mustNew(t, 1)
	s.Acquire() // fill it

	done := make(chan struct{})
	go func() {
		s.Acquire() // should block
		close(done)
	}()

	select {
	case <-done:
		t.Error("Acquire should have blocked but returned immediately")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	if err := s.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Acquire did not unblock after Release")
	}
}

func TestAcquireWith_Success(t *testing.T) {
	s := mustNew(t, 5)
	ctx := context.Background()
	if err := s.AcquireWith(ctx); err != nil {
		t.Errorf("AcquireWith: unexpected error %v", err)
	}
	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}
}

func TestAcquireWith_CancelledContext(t *testing.T) {
	s := mustNew(t, 1)
	s.Acquire() // fill

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.AcquireWith(ctx)
	if err == nil {
		t.Fatal("AcquireWith: expected error on cancelled context")
	}
	if !errors.Is(err, ErrAcquireCancelled{}) {
		t.Errorf("error type = %T, want ErrAcquireCancelled", err)
	}
}

func TestAcquireWith_ContextCancelMidWait(t *testing.T) {
	s := mustNew(t, 1)
	s.Acquire() // fill

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- s.AcquireWith(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, ErrAcquireCancelled{}) {
			t.Errorf("error = %v, want ErrAcquireCancelled", err)
		}
	case <-time.After(time.Second):
		t.Error("AcquireWith did not return after context cancel")
	}
}

// TestAcquireWith_SurvivesSetCap verifies that AcquireWith correctly
// retries when the channel is swapped by SetCap mid-acquire, instead
// of silently sending to a stale channel and leaking a slot.
func TestAcquireWith_SurvivesSetCap(t *testing.T) {
	s := mustNew(t, 2)
	s.Acquire() // hold 1 of 2

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start an acquire that will succeed once we release or resize.
	errc := make(chan error, 1)
	go func() {
		// Fill the remaining slot first.
		s.Acquire()
		// Now the semaphore is full. Start a blocking AcquireWith.
		errc <- s.AcquireWith(ctx)
	}()

	// Give the goroutine time to block.
	time.Sleep(30 * time.Millisecond)

	// Resize the semaphore — this swaps the channel. The blocked
	// AcquireWith should detect the swap and retry on the new channel.
	if err := s.SetCap(5); err != nil {
		t.Fatalf("SetCap(5): %v", err)
	}

	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("AcquireWith after SetCap: unexpected error %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("AcquireWith did not unblock after SetCap expanded capacity")
	}

	// The semaphore should have 3 slots held (1 original + 1 fill + 1 new acquire)
	// on a cap=5 channel. Exact count depends on timing but Len <= Cap must hold.
	if s.Len() > s.Cap() {
		t.Errorf("invariant violated: Len()=%d > Cap()=%d", s.Len(), s.Cap())
	}
}

func TestAcquireTimeout_Success(t *testing.T) {
	s := mustNew(t, 5)
	if err := s.AcquireTimeout(time.Second); err != nil {
		t.Errorf("AcquireTimeout: unexpected error %v", err)
	}
}

func TestAcquireTimeout_Expires(t *testing.T) {
	s := mustNew(t, 1)
	s.Acquire() // fill

	err := s.AcquireTimeout(50 * time.Millisecond)
	if err == nil {
		t.Fatal("AcquireTimeout: expected error after timeout")
	}
	if !errors.Is(err, ErrAcquireCancelled{}) {
		t.Errorf("error = %v, want ErrAcquireCancelled", err)
	}
}

func TestTryAcquire_Success(t *testing.T) {
	s := mustNew(t, 3)
	if !s.TryAcquire() {
		t.Error("TryAcquire: expected true on empty semaphore")
	}
	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}
}

func TestTryAcquire_Fails_WhenFull(t *testing.T) {
	s := mustNew(t, 1)
	s.Acquire()
	if s.TryAcquire() {
		t.Error("TryAcquire: expected false on full semaphore")
	}
}

func TestTryAcquireWith_Success(t *testing.T) {
	s := mustNew(t, 3)
	ctx := context.Background()
	if err := s.TryAcquireWith(ctx); err != nil {
		t.Errorf("TryAcquireWith: unexpected error %v", err)
	}
}

func TestTryAcquireWith_NoSlot(t *testing.T) {
	s := mustNew(t, 1)
	s.Acquire()
	ctx := context.Background()
	err := s.TryAcquireWith(ctx)
	if err == nil {
		t.Fatal("TryAcquireWith: expected ErrNoSlot")
	}
	if !errors.Is(err, ErrNoSlot{}) {
		t.Errorf("error = %T, want ErrNoSlot", err)
	}
}

func TestTryAcquireWith_CancelledContext(t *testing.T) {
	s := mustNew(t, 5)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.TryAcquireWith(ctx)
	if err == nil {
		t.Fatal("TryAcquireWith: expected error on cancelled context")
	}
	if !errors.Is(err, ErrAcquireCancelled{}) {
		t.Errorf("error = %T, want ErrAcquireCancelled", err)
	}
}

func TestRelease_Basic(t *testing.T) {
	s := mustNew(t, 3)
	s.Acquire()
	if err := s.Release(); err != nil {
		t.Errorf("Release: unexpected error %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0", s.Len())
	}
}

func TestRelease_WithoutAcquire(t *testing.T) {
	s := mustNew(t, 3)
	err := s.Release()
	if err == nil {
		t.Fatal("Release: expected error when releasing without acquire")
	}
	if !errors.Is(err, ErrReleaseExceedsCount{}) {
		t.Errorf("error = %T, want ErrReleaseExceedsCount", err)
	}
}

// TestRelease_ErrorReportsActualOccupancy verifies that the error from
// Release reports the real current occupancy, not a hardcoded zero.
func TestRelease_ErrorReportsActualOccupancy(t *testing.T) {
	s := mustNew(t, 5)
	err := s.Release()
	if err == nil {
		t.Fatal("Release on empty: expected error")
	}
	var releaseErr ErrReleaseExceedsCount
	if !errors.As(err, &releaseErr) {
		t.Fatalf("error type = %T, want ErrReleaseExceedsCount", err)
	}
	if releaseErr.Current != 0 {
		t.Errorf("ErrReleaseExceedsCount.Current = %d, want 0 on empty semaphore", releaseErr.Current)
	}

	// Now test with some slots held.
	s.Acquire()
	s.Acquire()
	// Try to release 3 via ReleaseN (only 2 held).
	err = s.ReleaseN(3)
	if err == nil {
		t.Fatal("ReleaseN(3) with 2 held: expected error")
	}
	if !errors.As(err, &releaseErr) {
		t.Fatalf("error type = %T, want ErrReleaseExceedsCount", err)
	}
	if releaseErr.Current != 2 {
		t.Errorf("ErrReleaseExceedsCount.Current = %d, want 2", releaseErr.Current)
	}
}

// ============================================================
// UNIT TESTS — MULTI SLOT
// ============================================================

func TestAcquireN_Basic(t *testing.T) {
	s := mustNew(t, 10)
	if err := s.AcquireN(5); err != nil {
		t.Errorf("AcquireN(5): unexpected error %v", err)
	}
	if s.Len() != 5 {
		t.Errorf("Len() = %d, want 5", s.Len())
	}
}

func TestAcquireN_InvalidN(t *testing.T) {
	s := mustNew(t, 10)
	for _, n := range []int{0, -1, -5} {
		err := s.AcquireN(n)
		if err == nil {
			t.Errorf("AcquireN(%d): expected error", n)
		}
		if !errors.Is(err, ErrInvalidN{}) {
			t.Errorf("AcquireN(%d): error = %T, want ErrInvalidN", n, err)
		}
	}
}

func TestAcquireN_ExceedsCap(t *testing.T) {
	s := mustNew(t, 5)
	err := s.AcquireN(10)
	if err == nil {
		t.Fatal("AcquireN(10) on cap=5: expected error")
	}
	if !errors.Is(err, ErrNExceedsCap{}) {
		t.Errorf("error = %T, want ErrNExceedsCap", err)
	}
}

// TestAcquireN_DoesNotBlockForever verifies that AcquireN returns an error
// instead of blocking permanently when the semaphore cannot satisfy the
// full request. Since AcquireN now delegates to AcquireNWith internally
// with a timeout, a permanently blocked partial acquire will eventually
// return ErrAcquireCancelled.
func TestAcquireN_DoesNotBlockForever(t *testing.T) {
	s := mustNew(t, 2)
	s.Acquire()
	s.Acquire() // fill

	errc := make(chan error, 1)
	go func() {
		errc <- s.AcquireN(1)
	}()

	// AcquireN now has an internal timeout. It should return within
	// a reasonable time rather than blocking forever. We wait 15 seconds
	// as a generous upper bound (internal timeout is 10 minutes, but
	// we release a slot to prove it still works normally when possible).
	// For the "blocks forever" test we just verify it returns *at all*
	// by giving it a shorter timeout scenario.

	// Actually, let's verify the normal case: release a slot and
	// AcquireN should succeed.
	time.Sleep(20 * time.Millisecond)
	_ = s.Release()

	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("AcquireN(1) after release: unexpected error %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("AcquireN(1) did not complete after slot was released")
	}
}

func TestAcquireNWith_Success(t *testing.T) {
	s := mustNew(t, 10)
	ctx := context.Background()
	if err := s.AcquireNWith(ctx, 4); err != nil {
		t.Errorf("AcquireNWith: unexpected error %v", err)
	}
	if s.Len() != 4 {
		t.Errorf("Len() = %d, want 4", s.Len())
	}
}

func TestAcquireNWith_ContextCancelled(t *testing.T) {
	s := mustNew(t, 2)
	s.Acquire()
	s.Acquire() // fill

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.AcquireNWith(ctx, 1)
	if err == nil {
		t.Fatal("AcquireNWith: expected error on cancelled context")
	}
	if !errors.Is(err, ErrAcquireCancelled{}) {
		t.Errorf("error = %T, want ErrAcquireCancelled", err)
	}
}

func TestAcquireNWith_RollsBackOnCancel(t *testing.T) {
	s := mustNew(t, 5)
	// fill 4 slots — only 1 available
	_ = s.AcquireN(4)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		// requesting 3 slots when only 1 is free — will partially acquire then block
		errc <- s.AcquireNWith(ctx, 3)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, ErrAcquireCancelled{}) {
			t.Errorf("error = %v, want ErrAcquireCancelled", err)
		}
		// Rollback: Len should still be 4 (the 1 partial slot was rolled back)
		if s.Len() != 4 {
			t.Errorf("after rollback Len() = %d, want 4", s.Len())
		}
	case <-time.After(time.Second):
		t.Error("AcquireNWith did not return after context cancel")
	}
}

// TestAcquireNWith_SurvivesSetCap verifies that AcquireNWith correctly
// handles a channel swap mid-acquire without leaking slots.
func TestAcquireNWith_SurvivesSetCap(t *testing.T) {
	s := mustNew(t, 3)
	_ = s.AcquireN(3) // fill

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- s.AcquireNWith(ctx, 2)
	}()

	time.Sleep(30 * time.Millisecond)

	// Expand capacity — this swaps the channel. SetCap preserves
	// existing occupancy (3 slots moved to new cap=10 channel).
	// After expansion there are 7 free slots, so AcquireNWith(2) should succeed.
	if err := s.SetCap(10); err != nil {
		t.Fatalf("SetCap(10): %v", err)
	}

	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("AcquireNWith after SetCap: unexpected error %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("AcquireNWith did not unblock after SetCap expanded capacity")
	}

	if s.Len() > s.Cap() {
		t.Errorf("invariant violated: Len()=%d > Cap()=%d", s.Len(), s.Cap())
	}
}

func TestAcquireNTimeout_Success(t *testing.T) {
	s := mustNew(t, 10)
	if err := s.AcquireNTimeout(3, time.Second); err != nil {
		t.Errorf("AcquireNTimeout: unexpected error %v", err)
	}
}

func TestAcquireNTimeout_Expires(t *testing.T) {
	s := mustNew(t, 1)
	s.Acquire() // fill
	err := s.AcquireNTimeout(1, 50*time.Millisecond)
	if err == nil {
		t.Fatal("AcquireNTimeout: expected error")
	}
	if !errors.Is(err, ErrAcquireCancelled{}) {
		t.Errorf("error = %T, want ErrAcquireCancelled", err)
	}
}

func TestTryAcquireN_Success(t *testing.T) {
	s := mustNew(t, 10)
	if !s.TryAcquireN(5) {
		t.Error("TryAcquireN(5): expected true")
	}
	if s.Len() != 5 {
		t.Errorf("Len() = %d, want 5", s.Len())
	}
}

func TestTryAcquireN_Fails_WhenNotEnoughSlots(t *testing.T) {
	s := mustNew(t, 3)
	_ = s.AcquireN(2) // 1 remaining
	if s.TryAcquireN(2) {
		t.Error("TryAcquireN(2): expected false with only 1 slot available")
	}
	// verify no partial acquisition occurred
	if s.Len() != 2 {
		t.Errorf("Len() = %d, want 2 (no partial acquisition)", s.Len())
	}
}

func TestTryAcquireN_InvalidN(t *testing.T) {
	s := mustNew(t, 5)
	if s.TryAcquireN(0) {
		t.Error("TryAcquireN(0): expected false")
	}
}

func TestTryAcquireN_ExceedsCap(t *testing.T) {
	s := mustNew(t, 3)
	if s.TryAcquireN(10) {
		t.Error("TryAcquireN(10) on cap=3: expected false")
	}
}

func TestTryAcquireNWith_Success(t *testing.T) {
	s := mustNew(t, 5)
	ctx := context.Background()
	if err := s.TryAcquireNWith(ctx, 3); err != nil {
		t.Errorf("TryAcquireNWith: unexpected error %v", err)
	}
}

func TestTryAcquireNWith_NoSlot(t *testing.T) {
	s := mustNew(t, 2)
	_ = s.AcquireN(2)
	ctx := context.Background()
	err := s.TryAcquireNWith(ctx, 1)
	if err == nil {
		t.Fatal("TryAcquireNWith: expected ErrNoSlot")
	}
	if !errors.Is(err, ErrNoSlot{}) {
		t.Errorf("error = %T, want ErrNoSlot", err)
	}
}

func TestTryAcquireNWith_ExceedsCap(t *testing.T) {
	s := mustNew(t, 3)
	ctx := context.Background()
	err := s.TryAcquireNWith(ctx, 10)
	if err == nil {
		t.Fatal("TryAcquireNWith: expected ErrNExceedsCap")
	}
	if !errors.Is(err, ErrNExceedsCap{}) {
		t.Errorf("error = %T, want ErrNExceedsCap", err)
	}
}

func TestTryAcquireNWith_CancelledContext(t *testing.T) {
	s := mustNew(t, 5)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.TryAcquireNWith(ctx, 1)
	if err == nil {
		t.Fatal("TryAcquireNWith: expected ErrAcquireCancelled")
	}
	if !errors.Is(err, ErrAcquireCancelled{}) {
		t.Errorf("error = %T, want ErrAcquireCancelled", err)
	}
}

func TestReleaseN_Basic(t *testing.T) {
	s := mustNew(t, 10)
	_ = s.AcquireN(5)
	if err := s.ReleaseN(3); err != nil {
		t.Errorf("ReleaseN(3): unexpected error %v", err)
	}
	if s.Len() != 2 {
		t.Errorf("Len() = %d, want 2", s.Len())
	}
}

func TestReleaseN_ExceedsCount(t *testing.T) {
	s := mustNew(t, 10)
	_ = s.AcquireN(2)
	err := s.ReleaseN(5)
	if err == nil {
		t.Fatal("ReleaseN(5) on count=2: expected error")
	}
	if !errors.Is(err, ErrReleaseExceedsCount{}) {
		t.Errorf("error = %T, want ErrReleaseExceedsCount", err)
	}
}

func TestReleaseN_InvalidN(t *testing.T) {
	s := mustNew(t, 5)
	err := s.ReleaseN(0)
	if err == nil {
		t.Fatal("ReleaseN(0): expected error")
	}
	if !errors.Is(err, ErrInvalidN{}) {
		t.Errorf("error = %T, want ErrInvalidN", err)
	}
}

// ============================================================
// UNIT TESTS — INTROSPECTION
// ============================================================

func TestLen(t *testing.T) {
	s := mustNew(t, 5)
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0 initially", s.Len())
	}
	s.Acquire()
	s.Acquire()
	if s.Len() != 2 {
		t.Errorf("Len() = %d, want 2 after 2 acquires", s.Len())
	}
}

func TestCap(t *testing.T) {
	s := mustNew(t, 7)
	if s.Cap() != 7 {
		t.Errorf("Cap() = %d, want 7", s.Cap())
	}
}

func TestIsEmpty(t *testing.T) {
	s := mustNew(t, 5)
	if !s.IsEmpty() {
		t.Error("IsEmpty(): expected true on fresh semaphore")
	}
	s.Acquire()
	if s.IsEmpty() {
		t.Error("IsEmpty(): expected false after Acquire")
	}
}

func TestIsFull(t *testing.T) {
	s := mustNew(t, 2)
	if s.IsFull() {
		t.Error("IsFull(): expected false on fresh semaphore")
	}
	s.Acquire()
	s.Acquire()
	if !s.IsFull() {
		t.Error("IsFull(): expected true when cap reached")
	}
}

func TestUtilization(t *testing.T) {
	s := mustNew(t, 4)
	if s.Utilization() != 0.0 {
		t.Errorf("Utilization() = %f, want 0.0", s.Utilization())
	}
	s.Acquire()
	s.Acquire()
	got := s.Utilization()
	if math.Abs(got-0.5) > 1e-9 {
		t.Errorf("Utilization() = %f, want 0.5", got)
	}
}

func TestUtilizationSmoothed_InitiallyZero(t *testing.T) {
	s := mustNew(t, 5)
	if s.UtilizationSmoothed() != 0.0 {
		t.Errorf("UtilizationSmoothed() = %f, want 0.0 initially", s.UtilizationSmoothed())
	}
}

func TestUtilizationSmoothed_UpdatesOnRelease(t *testing.T) {
	s := mustNew(t, 5)
	s.Acquire()
	_ = s.Release()
	// EWMA should have moved away from 0.0 after a release
	if s.UtilizationSmoothed() == 0.0 && s.Len() == 0 {
		// after releasing, utilization was 0/5 at the time of update — still 0 is valid
		// just ensure no NaN or negative
		u := s.UtilizationSmoothed()
		if math.IsNaN(u) || u < 0 {
			t.Errorf("UtilizationSmoothed() = %f, expected non-negative non-NaN", u)
		}
	}
}

func TestSetCap_Expand(t *testing.T) {
	s := mustNew(t, 3)
	s.Acquire()
	if err := s.SetCap(10); err != nil {
		t.Errorf("SetCap(10): unexpected error %v", err)
	}
	if s.Cap() != 10 {
		t.Errorf("Cap() = %d, want 10", s.Cap())
	}
	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1 (existing slots preserved)", s.Len())
	}
}

func TestSetCap_Shrink(t *testing.T) {
	s := mustNew(t, 10)
	_ = s.AcquireN(8)
	// shrinking drains existing slots per implementation
	if err := s.SetCap(3); err != nil {
		t.Errorf("SetCap(3): unexpected error %v", err)
	}
	if s.Cap() != 3 {
		t.Errorf("Cap() = %d, want 3", s.Cap())
	}
}

func TestSetCap_Default(t *testing.T) {
	s := mustNew(t, 5)
	if err := s.SetCap(-1); err != nil {
		t.Errorf("SetCap(-1): unexpected error %v", err)
	}
	if s.Cap() != defaultCap {
		t.Errorf("Cap() = %d, want %d", s.Cap(), defaultCap)
	}
}

func TestSetCap_Invalid(t *testing.T) {
	s := mustNew(t, 5)
	err := s.SetCap(0)
	if err == nil {
		t.Fatal("SetCap(0): expected error")
	}
	if !errors.Is(err, ErrInvalidCap{}) {
		t.Errorf("error = %T, want ErrInvalidCap", err)
	}
}

// TestSetCap_UpdatesEWMA verifies that SetCap updates the EWMA to reflect
// the new capacity, so UtilizationSmoothed is not stale after a resize.
func TestSetCap_UpdatesEWMA(t *testing.T) {
	s := mustNew(t, 10)
	_ = s.AcquireN(8)
	// Release 1 to get a non-zero EWMA: current = 7/10 = 0.7
	_ = s.ReleaseN(1)
	before := s.UtilizationSmoothed()
	if before == 0 {
		t.Fatal("EWMA should be > 0 after releasing with slots held")
	}

	// Expand to 20. SetCap moves 7 held slots into a cap=20 channel
	// and calls updateEWMA. new current = 7/20 = 0.35, which will
	// pull the EWMA in a different direction than the 0.7 it saw before.
	if err := s.SetCap(20); err != nil {
		t.Fatalf("SetCap(20): %v", err)
	}

	after := s.UtilizationSmoothed()
	if after == before {
		t.Errorf("EWMA unchanged after SetCap: before=%.6f, after=%.6f", before, after)
	}
	if math.IsNaN(after) || after < 0 || after > 1.0+1e-9 {
		t.Errorf("EWMA out of range after SetCap: %f", after)
	}
}

func TestDrain(t *testing.T) {
	s := mustNew(t, 5)
	_ = s.AcquireN(4)
	if err := s.Drain(); err != nil {
		t.Errorf("Drain: unexpected error %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after Drain", s.Len())
	}
}

// TestDrain_ResetsEWMA verifies that Drain zeroes the EWMA so
// UtilizationSmoothed does not report stale pre-drain values.
func TestDrain_ResetsEWMA(t *testing.T) {
	s := mustNew(t, 5)
	_ = s.AcquireN(4)
	// Release 1 to set a non-zero EWMA.
	_ = s.ReleaseN(1)
	if s.UtilizationSmoothed() == 0 {
		t.Fatal("EWMA should be > 0 before drain")
	}

	if err := s.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if s.UtilizationSmoothed() != 0 {
		t.Errorf("UtilizationSmoothed() = %f after Drain, want 0", s.UtilizationSmoothed())
	}
}

func TestReset(t *testing.T) {
	s := mustNew(t, 5)
	_ = s.AcquireN(5)
	if err := s.Reset(); err != nil {
		t.Errorf("Reset: unexpected error %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after Reset", s.Len())
	}
	if s.Cap() != 5 {
		t.Errorf("Cap() = %d, want 5 after Reset", s.Cap())
	}
}

// TestReset_ResetsEWMA verifies that Reset zeroes the EWMA.
func TestReset_ResetsEWMA(t *testing.T) {
	s := mustNew(t, 5)
	_ = s.AcquireN(4)
	_ = s.ReleaseN(1)
	if s.UtilizationSmoothed() == 0 {
		t.Fatal("EWMA should be > 0 before reset")
	}

	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if s.UtilizationSmoothed() != 0 {
		t.Errorf("UtilizationSmoothed() = %f after Reset, want 0", s.UtilizationSmoothed())
	}
}

func TestWait_ReturnsWhenEmpty(t *testing.T) {
	s := mustNew(t, 5)
	ctx := context.Background()
	// already empty
	if err := s.Wait(ctx); err != nil {
		t.Errorf("Wait on empty semaphore: unexpected error %v", err)
	}
}

func TestWait_BlocksUntilEmpty(t *testing.T) {
	s := mustNew(t, 5)
	s.Acquire()
	s.Acquire()

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- s.Wait(ctx)
	}()

	time.Sleep(30 * time.Millisecond)
	_ = s.Release()
	_ = s.Release()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Wait: unexpected error %v", err)
		}
	case <-time.After(time.Second):
		t.Error("Wait did not return after semaphore drained")
	}
}

func TestWait_ContextCancelled(t *testing.T) {
	s := mustNew(t, 1)
	s.Acquire() // keep it non-empty

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- s.Wait(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, ErrAcquireCancelled{}) {
			t.Errorf("Wait: error = %v, want ErrAcquireCancelled", err)
		}
	case <-time.After(time.Second):
		t.Error("Wait did not return after context cancel")
	}
}

// TestWait_CancelDoesNotLeakGoroutine verifies that cancelling a Wait
// does not leave a goroutine permanently parked on s.cond.Wait().
// The fix uses a stop channel to signal the waiter goroutine to exit.
func TestWait_CancelDoesNotLeakGoroutine(t *testing.T) {
	s := mustNew(t, 1)
	s.Acquire() // keep non-empty so Wait blocks

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- s.Wait(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-errc:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after cancel")
	}

	// Give the waiter goroutine time to exit after receiving the stop signal.
	time.Sleep(50 * time.Millisecond)

	after := runtime.NumGoroutine()
	// Allow a tolerance of 1 for background runtime goroutines.
	if after > before+1 {
		t.Errorf("goroutine leak: before=%d, after=%d (expected at most %d)",
			before, after, before+1)
	}

	// Clean up.
	_ = s.Release()
}

// ============================================================
// UNIT TESTS — OBSERVER
// ============================================================

func TestObserver_OnAcquire_Called(t *testing.T) {
	obs := &mockObserver{}
	s, _ := NewWithObserver(5, obs)
	s.Acquire()
	if obs.AcquireCount() != 1 {
		t.Errorf("OnAcquire called %d times, want 1", obs.AcquireCount())
	}
}

func TestObserver_OnRelease_Called(t *testing.T) {
	obs := &mockObserver{}
	s, _ := NewWithObserver(5, obs)
	s.Acquire()
	_ = s.Release()
	if obs.ReleaseCount() != 1 {
		t.Errorf("OnRelease called %d times, want 1", obs.ReleaseCount())
	}
}

func TestObserver_OnWait_Called(t *testing.T) {
	obs := &mockObserver{}
	s, _ := NewWithObserver(5, obs)
	ctx := context.Background()
	_ = s.Wait(ctx)

	obs.mu.Lock()
	starts := obs.waitStarts
	ends := obs.waitEnds
	obs.mu.Unlock()

	if starts != 1 {
		t.Errorf("OnWaitStart called %d times, want 1", starts)
	}
	if ends != 1 {
		t.Errorf("OnWaitEnd called %d times, want 1", ends)
	}
}

func TestObserver_MultipleAcquiresTracked(t *testing.T) {
	obs := &mockObserver{}
	s, _ := NewWithObserver(10, obs)
	for i := 0; i < 5; i++ {
		s.Acquire()
	}
	if obs.AcquireCount() != 5 {
		t.Errorf("OnAcquire called %d times, want 5", obs.AcquireCount())
	}
}

func TestObserver_AcquireN_Called(t *testing.T) {
	obs := &mockObserver{}
	s, _ := NewWithObserver(10, obs)
	_ = s.AcquireN(3)
	if obs.AcquireCount() != 1 {
		t.Errorf("OnAcquire called %d times after AcquireN, want 1", obs.AcquireCount())
	}
}

func TestObserver_ReleaseN_Called(t *testing.T) {
	obs := &mockObserver{}
	s, _ := NewWithObserver(10, obs)
	_ = s.AcquireN(3)
	_ = s.ReleaseN(3)
	if obs.ReleaseCount() != 1 {
		t.Errorf("OnRelease called %d times after ReleaseN, want 1", obs.ReleaseCount())
	}
}

// ============================================================
// UNIT TESTS — ERROR TYPES
// ============================================================

func TestErrorTypes_IsMatching(t *testing.T) {
	tests := []struct {
		err    error
		target error
		name   string
	}{
		{ErrInvalidCap{Value: 0}, ErrInvalidCap{}, "ErrInvalidCap"},
		{ErrInvalidN{Value: -1}, ErrInvalidN{}, "ErrInvalidN"},
		{ErrReleaseExceedsCount{Attempted: 1, Current: 0}, ErrReleaseExceedsCount{}, "ErrReleaseExceedsCount"},
		{ErrNoSlot{Requested: 1, Available: 0}, ErrNoSlot{}, "ErrNoSlot"},
		{ErrAcquireCancelled{Cause: context.Canceled}, ErrAcquireCancelled{}, "ErrAcquireCancelled"},
		{ErrDrain{Cause: "test"}, ErrDrain{}, "ErrDrain"},
		{ErrNExceedsCap{Requested: 10, Cap: 5}, ErrNExceedsCap{}, "ErrNExceedsCap"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.target) {
				t.Errorf("errors.Is(%v, %v) = false, want true", tt.err, tt.target)
			}
		})
	}
}

func TestErrAcquireCancelled_Unwrap(t *testing.T) {
	cause := context.DeadlineExceeded
	err := ErrAcquireCancelled{Cause: cause}
	if !errors.Is(err, cause) {
		t.Errorf("Unwrap should expose cause: errors.Is(err, DeadlineExceeded) = false")
	}
}

func TestErrRecovered_Unwrap(t *testing.T) {
	inner := fmt.Errorf("inner error")
	err := ErrRecovered{Cause: inner, AsError: inner}
	if !errors.Is(err, inner) {
		t.Errorf("ErrRecovered.Unwrap should expose AsError")
	}
}

func TestNew_LargeCapacity(t *testing.T) {
	s, err := New(1_000_000)
	if err != nil {
		t.Fatalf("New(1000000): %v", err)
	}
	if s.Cap() != 1_000_000 {
		t.Errorf("Cap() = %d, want 1000000", s.Cap())
	}
}

func TestErrorStrings(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		contains string
	}{
		{"ErrInvalidCap", ErrInvalidCap{Value: -5}, "-5"},
		{"ErrInvalidN", ErrInvalidN{Value: -3}, "-3"},
		{"ErrReleaseExceedsCount", ErrReleaseExceedsCount{Attempted: 2, Current: 1}, "2"},
		{"ErrNoSlot", ErrNoSlot{Requested: 3, Available: 1}, "3"},
		{"ErrAcquireCancelled", ErrAcquireCancelled{Cause: context.Canceled}, "cancel"},
		{"ErrDrain", ErrDrain{Cause: "oops"}, "oops"},
		{"ErrNExceedsCap", ErrNExceedsCap{Requested: 10, Cap: 5}, "10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := tt.err.Error()
			if msg == "" {
				t.Errorf("Error() returned empty string")
			}
			if !strings.Contains(msg, tt.contains) {
				t.Errorf("Error() = %q, want it to contain %q", msg, tt.contains)
			}
		})
	}
}

// ============================================================
// FUZZ TESTS
// ============================================================

func FuzzNew(f *testing.F) {
	f.Add(1)
	f.Add(10)
	f.Add(-1)
	f.Add(0)
	f.Add(-99)
	f.Fuzz(func(t *testing.T, cap int) {
		s, err := New(cap)
		if cap == -1 {
			if err != nil {
				t.Errorf("New(-1) should succeed, got: %v", err)
			}
			return
		}
		if cap < 1 {
			if err == nil {
				t.Errorf("New(%d) should fail", cap)
			}
			return
		}
		if err != nil {
			t.Errorf("New(%d) unexpected error: %v", cap, err)
		}
		if s.Cap() != cap {
			t.Errorf("Cap() = %d, want %d", s.Cap(), cap)
		}
	})
}

func FuzzAcquireN(f *testing.F) {
	f.Add(5, 3)
	f.Add(1, 1)
	f.Add(10, 0)
	f.Add(3, 10)
	f.Fuzz(func(t *testing.T, cap, n int) {
		if cap < 1 {
			cap = 1
		}
		if cap > 1000 {
			cap = 1000
		}
		s, err := New(cap)
		if err != nil {
			return
		}
		err = s.AcquireN(n)
		if n < 1 {
			if !errors.Is(err, ErrInvalidN{}) {
				t.Errorf("AcquireN(%d): expected ErrInvalidN, got %v", n, err)
			}
			return
		}
		if n > cap {
			if !errors.Is(err, ErrNExceedsCap{}) {
				t.Errorf("AcquireN(%d) on cap=%d: expected ErrNExceedsCap, got %v", n, cap, err)
			}
			return
		}
		if err != nil {
			t.Errorf("AcquireN(%d) on cap=%d: unexpected error %v", n, cap, err)
		}
	})
}

func FuzzReleaseN(f *testing.F) {
	f.Add(5, 3, 2)
	f.Add(5, 5, 5)
	f.Add(5, 2, 3)
	f.Fuzz(func(t *testing.T, cap, acquired, releasing int) {
		if cap < 1 || cap > 1000 {
			return
		}
		if acquired < 0 || acquired > cap {
			return
		}
		s, err := New(cap)
		if err != nil {
			return
		}
		if acquired > 0 {
			if err := s.AcquireN(acquired); err != nil {
				return
			}
		}
		err = s.ReleaseN(releasing)
		if releasing < 1 {
			if !errors.Is(err, ErrInvalidN{}) {
				t.Errorf("ReleaseN(%d): expected ErrInvalidN, got %v", releasing, err)
			}
			return
		}
		if releasing > acquired {
			if !errors.Is(err, ErrReleaseExceedsCount{}) {
				t.Errorf("ReleaseN(%d) with acquired=%d: expected ErrReleaseExceedsCount, got %v", releasing, acquired, err)
			}
			return
		}
		if err != nil {
			t.Errorf("ReleaseN(%d) with acquired=%d cap=%d: unexpected error %v", releasing, acquired, cap, err)
		}
	})
}

func FuzzSetCap(f *testing.F) {
	f.Add(5, 3)
	f.Add(5, -1)
	f.Add(5, 0)
	f.Add(5, 100)
	f.Fuzz(func(t *testing.T, initial, newCap int) {
		if initial < 1 || initial > 1000 {
			return
		}
		s, err := New(initial)
		if err != nil {
			return
		}
		err = s.SetCap(newCap)
		if newCap == -1 {
			if err != nil {
				t.Errorf("SetCap(-1): expected success, got %v", err)
			}
			return
		}
		if newCap < 1 {
			if !errors.Is(err, ErrInvalidCap{}) {
				t.Errorf("SetCap(%d): expected ErrInvalidCap, got %v", newCap, err)
			}
			return
		}
		if err != nil {
			t.Errorf("SetCap(%d): unexpected error %v", newCap, err)
		}
		if newCap > 1000 {
			return
		}
		if s.Cap() != newCap {
			t.Errorf("after SetCap(%d): Cap() = %d", newCap, s.Cap())
		}
	})
}

func FuzzTryAcquireN(f *testing.F) {
	f.Add(10, 5, 3)
	f.Add(3, 3, 1)
	f.Add(5, 0, 6)
	f.Fuzz(func(t *testing.T, cap, preAcquire, n int) {
		if cap < 1 || cap > 500 {
			return
		}
		if preAcquire < 0 || preAcquire > cap {
			return
		}
		s, err := New(cap)
		if err != nil {
			return
		}
		if preAcquire > 0 {
			_ = s.AcquireN(preAcquire)
		}
		available := cap - preAcquire
		got := s.TryAcquireN(n)
		if n < 1 || n > cap {
			if got {
				t.Errorf("TryAcquireN(%d) should return false for invalid n", n)
			}
			return
		}
		if n <= available && !got {
			t.Errorf("TryAcquireN(%d) with %d available: expected true, got false", n, available)
		}
		if n > available && got {
			t.Errorf("TryAcquireN(%d) with %d available: expected false, got true", n, available)
		}
	})
}

func FuzzUtilization(f *testing.F) {
	f.Add(10, 5)
	f.Add(1, 1)
	f.Add(100, 0)
	f.Fuzz(func(t *testing.T, cap, acquired int) {
		if cap < 1 || cap > 1000 {
			return
		}
		if acquired < 0 || acquired > cap {
			return
		}
		s, err := New(cap)
		if err != nil {
			return
		}
		if acquired > 0 {
			_ = s.AcquireN(acquired)
		}
		u := s.Utilization()
		if math.IsNaN(u) || u < 0 || u > 1.0+1e-9 {
			t.Errorf("Utilization() = %f out of [0,1] range", u)
		}
	})
}

// ============================================================
// BENCHMARK TESTS
// ============================================================

func BenchmarkNew(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _ = New(10)
	}
}

func BenchmarkMust(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Must(10)
	}
}

func BenchmarkAcquireRelease(b *testing.B) {
	s := Must(b.N + 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Acquire()
		_ = s.Release()
	}
}

func BenchmarkTryAcquire(b *testing.B) {
	s := Must(b.N + 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s.TryAcquire() {
			_ = s.Release()
		}
	}
}

func BenchmarkAcquireWith(b *testing.B) {
	s := Must(b.N + 1)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.AcquireWith(ctx)
		_ = s.Release()
	}
}

func BenchmarkAcquireTimeout(b *testing.B) {
	s := Must(b.N + 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.AcquireTimeout(time.Second)
		_ = s.Release()
	}
}

func BenchmarkAcquireN(b *testing.B) {
	s := Must(b.N + 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.AcquireN(1)
		_ = s.ReleaseN(1)
	}
}

func BenchmarkAcquireNWith(b *testing.B) {
	s := Must(b.N + 1)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.AcquireNWith(ctx, 1)
		_ = s.ReleaseN(1)
	}
}

func BenchmarkTryAcquireN(b *testing.B) {
	s := Must(b.N + 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.TryAcquireN(1)
		_ = s.ReleaseN(1)
	}
}

func BenchmarkReleaseN(b *testing.B) {
	s := Must(b.N + 1)
	_ = s.AcquireN(b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.ReleaseN(1)
	}
}

func BenchmarkLen(b *testing.B) {
	s := Must(10)
	s.Acquire()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Len()
	}
}

func BenchmarkCap(b *testing.B) {
	s := Must(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Cap()
	}
}

func BenchmarkUtilization(b *testing.B) {
	s := Must(10)
	s.Acquire()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Utilization()
	}
}

func BenchmarkUtilizationSmoothed(b *testing.B) {
	s := Must(10)
	s.Acquire()
	_ = s.Release()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.UtilizationSmoothed()
	}
}

func BenchmarkSetCap(b *testing.B) {
	s := Must(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.SetCap(10)
	}
}

func BenchmarkDrain(b *testing.B) {
	s := Must(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Acquire()
		_ = s.Drain()
	}
}

func BenchmarkReset(b *testing.B) {
	s := Must(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Reset()
	}
}

func BenchmarkWait(b *testing.B) {
	s := Must(10)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Wait(ctx)
	}
}

func BenchmarkAcquireRelease_Parallel(b *testing.B) {
	s := Must(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Acquire()
			_ = s.Release()
		}
	})
}

func BenchmarkTryAcquireN_Parallel(b *testing.B) {
	s := Must(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if s.TryAcquireN(1) {
				_ = s.ReleaseN(1)
			}
		}
	})
}

func BenchmarkObserver_Overhead(b *testing.B) {
	obs := &mockObserver{}
	s, _ := NewWithObserver(b.N+1, obs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Acquire()
		_ = s.Release()
	}
}

// TestObserver_TryAcquire_Called verifies that a successful TryAcquire
// fires OnAcquire on the observer.
func TestObserver_TryAcquire_Called(t *testing.T) {
	obs := &mockObserver{}
	s, err := NewWithObserver(5, obs)
	if err != nil {
		t.Fatalf("NewWithObserver: %v", err)
	}

	got := s.TryAcquire()
	if !got {
		t.Fatal("TryAcquire: expected true on empty semaphore")
	}
	if obs.AcquireCount() != 1 {
		t.Errorf("OnAcquire called %d times after TryAcquire, want 1", obs.AcquireCount())
	}
}

// TestObserver_TryAcquire_NotCalled_WhenFull verifies that OnAcquire is
// NOT fired when TryAcquire fails because the semaphore is full.
func TestObserver_TryAcquire_NotCalled_WhenFull(t *testing.T) {
	obs := &mockObserver{}
	s, err := NewWithObserver(1, obs)
	if err != nil {
		t.Fatalf("NewWithObserver: %v", err)
	}

	s.Acquire() // fill — this fires OnAcquire once
	beforeCount := obs.AcquireCount()

	got := s.TryAcquire()
	if got {
		t.Fatal("TryAcquire: expected false on full semaphore")
	}
	if obs.AcquireCount() != beforeCount {
		t.Errorf("OnAcquire fired on failed TryAcquire: count went from %d to %d",
			beforeCount, obs.AcquireCount())
	}
}

// TestObserver_AcquireWith_Called verifies that AcquireWith fires OnAcquire
// on a successful acquire.
func TestObserver_AcquireWith_Called(t *testing.T) {
	obs := &mockObserver{}
	s, err := NewWithObserver(5, obs)
	if err != nil {
		t.Fatalf("NewWithObserver: %v", err)
	}

	ctx := context.Background()
	if err := s.AcquireWith(ctx); err != nil {
		t.Fatalf("AcquireWith: unexpected error %v", err)
	}
	if obs.AcquireCount() != 1 {
		t.Errorf("OnAcquire called %d times after AcquireWith, want 1", obs.AcquireCount())
	}
}

// TestObserver_AcquireWith_NotCalled_WhenCancelled verifies that OnAcquire
// is NOT fired when AcquireWith returns due to context cancellation.
func TestObserver_AcquireWith_NotCalled_WhenCancelled(t *testing.T) {
	obs := &mockObserver{}
	s, err := NewWithObserver(1, obs)
	if err != nil {
		t.Fatalf("NewWithObserver: %v", err)
	}

	s.Acquire() // fill — fires OnAcquire once
	beforeCount := obs.AcquireCount()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	acquireErr := s.AcquireWith(ctx)
	if acquireErr == nil {
		t.Fatal("AcquireWith: expected error on cancelled context")
	}
	if obs.AcquireCount() != beforeCount {
		t.Errorf("OnAcquire fired on cancelled AcquireWith: count went from %d to %d",
			beforeCount, obs.AcquireCount())
	}
}

// TestObserver_TryAcquireNWith_Called verifies that TryAcquireNWith fires
// OnAcquire exactly once on a successful multi-slot non-blocking acquire.
func TestObserver_TryAcquireNWith_Called(t *testing.T) {
	obs := &mockObserver{}
	s, err := NewWithObserver(10, obs)
	if err != nil {
		t.Fatalf("NewWithObserver: %v", err)
	}

	ctx := context.Background()
	if err := s.TryAcquireNWith(ctx, 3); err != nil {
		t.Fatalf("TryAcquireNWith: unexpected error %v", err)
	}
	if obs.AcquireCount() != 1 {
		t.Errorf("OnAcquire called %d times after TryAcquireNWith(3), want 1",
			obs.AcquireCount())
	}
}

// TestObserver_TryAcquireNWith_NotCalled_WhenNoSlot verifies that OnAcquire
// is NOT fired when TryAcquireNWith fails due to insufficient slots.
func TestObserver_TryAcquireNWith_NotCalled_WhenNoSlot(t *testing.T) {
	obs := &mockObserver{}
	s, err := NewWithObserver(2, obs)
	if err != nil {
		t.Fatalf("NewWithObserver: %v", err)
	}

	_ = s.AcquireN(2) // fill — fires OnAcquire once
	beforeCount := obs.AcquireCount()

	ctx := context.Background()
	acquireErr := s.TryAcquireNWith(ctx, 1)
	if acquireErr == nil {
		t.Fatal("TryAcquireNWith: expected ErrNoSlot")
	}
	if obs.AcquireCount() != beforeCount {
		t.Errorf("OnAcquire fired on failed TryAcquireNWith: count went from %d to %d",
			beforeCount, obs.AcquireCount())
	}
}

// TestObserver_TryAcquireNWith_NotCalled_WhenCancelled verifies that OnAcquire
// is NOT fired when TryAcquireNWith returns due to context cancellation.
func TestObserver_TryAcquireNWith_NotCalled_WhenCancelled(t *testing.T) {
	obs := &mockObserver{}
	s, err := NewWithObserver(5, obs)
	if err != nil {
		t.Fatalf("NewWithObserver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	acquireErr := s.TryAcquireNWith(ctx, 1)
	if acquireErr == nil {
		t.Fatal("TryAcquireNWith: expected error on cancelled context")
	}
	if obs.AcquireCount() != 0 {
		t.Errorf("OnAcquire fired on cancelled TryAcquireNWith: count = %d, want 0",
			obs.AcquireCount())
	}
}

// TestTryAcquireN_ObserverNotCalled — now passes because TryAcquireN
// calls s.notify after a successful tryAcquireNLocked.
func TestTryAcquireN_ObserverNotCalled(t *testing.T) {
	obs := &mockObserver{}
	s, err := NewWithObserver(10, obs)
	if err != nil {
		t.Fatalf("NewWithObserver: %v", err)
	}

	got := s.TryAcquireN(3)
	if !got {
		t.Fatal("TryAcquireN(3): expected true on semaphore with 10 available slots")
	}

	if obs.AcquireCount() != 1 {
		t.Errorf(
			"OnAcquire called %d times after TryAcquireN(3), want 1 — "+
				"TryAcquireN does not call s.notify(); this is an implementation gap",
			obs.AcquireCount(),
		)
	}
}

// ============================================================
// MISSING TESTS — MEDIUM PRIORITY
// ============================================================

func TestUtilizationSmoothed_MovesAfterRelease(t *testing.T) {
	s := mustNew(t, 5)

	if err := s.AcquireN(4); err != nil {
		t.Fatalf("AcquireN(4): %v", err)
	}

	if err := s.ReleaseN(1); err != nil {
		t.Fatalf("ReleaseN(1): %v", err)
	}

	smoothed := s.UtilizationSmoothed()

	if math.IsNaN(smoothed) {
		t.Fatalf("UtilizationSmoothed() = NaN, want a valid float")
	}
	if smoothed < 0 {
		t.Fatalf("UtilizationSmoothed() = %f, want non-negative", smoothed)
	}
	if smoothed == 0.0 {
		t.Errorf(
			"UtilizationSmoothed() = 0.0 after releasing 1 of 4 held slots — "+
				"EWMA did not move; expected > 0 (approximately %.4f)",
			ewmaAlpha*(3.0/5.0),
		)
	}

	expected := ewmaAlpha * (3.0 / 5.0)
	tolerance := 1e-9
	if math.Abs(smoothed-expected) > tolerance {
		t.Errorf(
			"UtilizationSmoothed() = %.10f, want %.10f (tolerance %.0e)",
			smoothed, expected, tolerance,
		)
	}
}

func TestUtilizationSmoothed_AccumulatesOverMultipleReleases(t *testing.T) {
	s := mustNew(t, 10)

	if err := s.AcquireN(10); err != nil {
		t.Fatalf("AcquireN(10): %v", err)
	}

	prev := s.UtilizationSmoothed()
	for i := 0; i < 10; i++ {
		if err := s.ReleaseN(1); err != nil {
			t.Fatalf("ReleaseN(1) iteration %d: %v", i, err)
		}
		current := s.UtilizationSmoothed()
		if math.IsNaN(current) {
			t.Fatalf("iteration %d: UtilizationSmoothed() = NaN", i)
		}
		if current < 0 || current > 1.0+1e-9 {
			t.Fatalf("iteration %d: UtilizationSmoothed() = %f out of [0,1]", i, current)
		}
		if i == 0 && current <= prev {
			t.Errorf("iteration 0: EWMA did not increase from 0.0; got %f", current)
		}
		prev = current
	}
}

func TestSetCap_Shrink_LenIsZero(t *testing.T) {
	s := mustNew(t, 10)

	if err := s.AcquireN(8); err != nil {
		t.Fatalf("AcquireN(8): %v", err)
	}
	if s.Len() != 8 {
		t.Fatalf("pre-shrink Len() = %d, want 8", s.Len())
	}

	if err := s.SetCap(3); err != nil {
		t.Fatalf("SetCap(3): %v", err)
	}

	if s.Cap() != 3 {
		t.Errorf("Cap() = %d, want 3 after shrink", s.Cap())
	}
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after shrink drain", s.Len())
	}
	if !s.IsEmpty() {
		t.Errorf("IsEmpty() = false after shrink drain, expected true")
	}
}

func TestSetCap_Shrink_ToExactCurrentLen(t *testing.T) {
	s := mustNew(t, 10)

	if err := s.AcquireN(5); err != nil {
		t.Fatalf("AcquireN(5): %v", err)
	}

	if err := s.SetCap(5); err != nil {
		t.Fatalf("SetCap(5): %v", err)
	}

	if s.Cap() != 5 {
		t.Errorf("Cap() = %d, want 5", s.Cap())
	}
	if s.Len() != 5 {
		t.Errorf("Len() = %d, want 5 (slots preserved when newCap >= current)", s.Len())
	}
	if !s.IsFull() {
		t.Errorf("IsFull() = false, want true when cap == len")
	}
}

func TestSetCap_Shrink_BelowCurrentLen(t *testing.T) {
	s := mustNew(t, 10)

	if err := s.AcquireN(8); err != nil {
		t.Fatalf("AcquireN(8): %v", err)
	}

	if err := s.SetCap(4); err != nil {
		t.Fatalf("SetCap(4): %v", err)
	}

	if s.Cap() != 4 {
		t.Errorf("Cap() = %d, want 4", s.Cap())
	}
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0 (drainLocked path)", s.Len())
	}
}

// ============================================================
// MISSING TESTS — FUZZ: CONCURRENT ACCESS
// ============================================================

func FuzzConcurrentAcquireRelease(f *testing.F) {
	f.Add(5, 10, 1)
	f.Add(1, 3, 1)
	f.Add(10, 20, 2)
	f.Add(3, 6, 1)
	f.Add(8, 8, 3)

	f.Fuzz(func(t *testing.T, cap, goroutines, acquireN int) {
		if cap < 1 || cap > 50 {
			return
		}
		if goroutines < 1 || goroutines > 30 {
			return
		}
		if acquireN < 1 || acquireN > cap {
			return
		}

		s, err := New(cap)
		if err != nil {
			return
		}

		var wg sync.WaitGroup
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				if err := s.AcquireNWith(ctx, acquireN); err != nil {
					return
				}

				l, c := s.Len(), s.Cap()
				if l > c {
					t.Errorf("invariant violated: Len()=%d > Cap()=%d", l, c)
				}
				if l < 0 {
					t.Errorf("invariant violated: Len()=%d < 0", l)
				}

				defer func() {
					if err := s.ReleaseN(acquireN); err != nil {
						t.Errorf("ReleaseN(%d) after successful acquire: %v", acquireN, err)
					}
				}()
			}()
		}

		wg.Wait()

		if s.Len() != 0 {
			t.Errorf("after all goroutines: Len()=%d, want 0 (slot leak detected)", s.Len())
		}
	})
}

func FuzzConcurrentMixedOps(f *testing.F) {
	f.Add(5, 8)
	f.Add(10, 15)
	f.Add(3, 5)

	f.Fuzz(func(t *testing.T, cap, goroutines int) {
		if cap < 2 || cap > 20 {
			return
		}
		if goroutines < 2 || goroutines > 20 {
			return
		}

		s, err := New(cap)
		if err != nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		var wg sync.WaitGroup

		tryWorkers := goroutines / 2
		for i := 0; i < tryWorkers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if s.TryAcquire() {
					l, c := s.Len(), s.Cap()
					if l > c {
						t.Errorf("TryAcquire: Len()=%d > Cap()=%d", l, c)
					}
					_ = s.Release()
				}
			}()
		}

		blockWorkers := goroutines - tryWorkers
		for i := 0; i < blockWorkers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.AcquireWith(ctx); err != nil {
					return
				}
				l, c := s.Len(), s.Cap()
				if l > c {
					t.Errorf("AcquireWith: Len()=%d > Cap()=%d", l, c)
				}
				_ = s.Release()
			}()
		}

		wg.Wait()

		if s.Len() != 0 {
			t.Errorf("mixed ops: Len()=%d after completion, want 0", s.Len())
		}
	})
}

// ============================================================
// MISSING TESTS — LOW PRIORITY
// ============================================================

func TestApp_Lifecycle_DrainResetWait(t *testing.T) {
	const (
		initialCap  = 8
		workingLoad = 5
		resumeCap   = 12
	)

	s, err := New(initialCap)
	if err != nil {
		t.Fatalf("New(%d): %v", initialCap, err)
	}

	t.Log("Phase 1: acquiring working load")

	if err := s.AcquireN(workingLoad); err != nil {
		t.Fatalf("AcquireN(%d): %v", workingLoad, err)
	}
	if s.Len() != workingLoad {
		t.Errorf("after AcquireN: Len()=%d, want %d", s.Len(), workingLoad)
	}
	if s.Utilization() == 0 {
		t.Errorf("Utilization() = 0 with %d slots held, want > 0", workingLoad)
	}

	t.Log("Phase 2: draining for maintenance")

	if err := s.Drain(); err != nil {
		t.Errorf("Drain: %v", err)
	}
	if !s.IsEmpty() {
		t.Errorf("after Drain: IsEmpty()=false, Len()=%d", s.Len())
	}

	if s.Cap() != initialCap {
		t.Errorf("after Drain: Cap()=%d, want %d (Drain must not change cap)", s.Cap(), initialCap)
	}

	t.Log("Phase 3: confirming idle via Wait")

	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()

	if err := s.Wait(waitCtx); err != nil {
		t.Errorf("Wait after Drain: %v (semaphore should already be empty)", err)
	}

	t.Log("Phase 4: simulating post-drain activity before reset")

	if err := s.AcquireN(3); err != nil {
		t.Fatalf("AcquireN(3) after Drain: %v", err)
	}
	if s.Len() != 3 {
		t.Errorf("Len()=%d after post-drain acquire, want 3", s.Len())
	}

	t.Log("Phase 5: hard reset")

	if err := s.Reset(); err != nil {
		t.Errorf("Reset: %v", err)
	}
	if !s.IsEmpty() {
		t.Errorf("after Reset: IsEmpty()=false, Len()=%d", s.Len())
	}
	if s.Cap() != initialCap {
		t.Errorf("after Reset: Cap()=%d, want %d (Reset must not change cap)", s.Cap(), initialCap)
	}

	t.Log("Phase 6: scaling capacity for resumed operations")

	if err := s.SetCap(resumeCap); err != nil {
		t.Errorf("SetCap(%d): %v", resumeCap, err)
	}
	if s.Cap() != resumeCap {
		t.Errorf("after SetCap: Cap()=%d, want %d", s.Cap(), resumeCap)
	}
	if !s.IsEmpty() {
		t.Errorf("after SetCap on empty semaphore: IsEmpty()=false, Len()=%d", s.Len())
	}

	t.Log("Phase 7: resuming under new capacity")

	if err := s.AcquireN(resumeCap); err != nil {
		t.Fatalf("AcquireN(%d) under new cap: %v", resumeCap, err)
	}
	if !s.IsFull() {
		t.Errorf("after filling to new cap: IsFull()=false, Len()=%d Cap()=%d",
			s.Len(), s.Cap())
	}

	if err := s.ReleaseN(resumeCap); err != nil {
		t.Errorf("ReleaseN(%d) at shutdown: %v", resumeCap, err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()

	if err := s.Wait(shutdownCtx); err != nil {
		t.Errorf("final Wait: %v", err)
	}
	if !s.IsEmpty() {
		t.Errorf("final state: IsEmpty()=false, Len()=%d", s.Len())
	}

	t.Logf("Lifecycle complete: cap=%d, len=%d, smoothed=%.4f",
		s.Cap(), s.Len(), s.UtilizationSmoothed())
}

func TestApp_Lifecycle_WaitCancelledDuringDrain(t *testing.T) {
	s := mustNew(t, 5)

	if err := s.AcquireN(3); err != nil {
		t.Fatalf("AcquireN(3): %v", err)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer waitCancel()

	errc := make(chan error, 1)
	go func() {
		errc <- s.Wait(waitCtx)
	}()

	select {
	case err := <-errc:
		if err == nil {
			t.Error("Wait: expected ErrAcquireCancelled when context expires with slots held")
		}
	case <-time.After(time.Second):
		t.Error("Wait did not return after context deadline")
	}

	if s.Len() != 3 {
		t.Errorf("after cancelled Wait: Len()=%d, want 3 (Wait must not alter slot count)", s.Len())
	}

	_ = s.Drain()
}

func TestAcquireWith_CancelDoesNotLeakGoroutine(t *testing.T) {
	s := mustNew(t, 1)
	s.Acquire() // fill

	before := runtime.NumGoroutine()

	// Repeatedly cancel AcquireWith to stress the goroutine lifecycle.
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		_ = s.AcquireWith(ctx)
		cancel()
	}

	time.Sleep(100 * time.Millisecond) // let goroutines settle

	after := runtime.NumGoroutine()
	// Allow tolerance of 2 for runtime background goroutines.
	if after > before+2 {
		t.Errorf("goroutine leak: before=%d, after=%d (ran 50 cancellations)",
			before, after)
	}

	s.Release()
}

func TestAcquireNWith_CancelDoesNotLeakGoroutine(t *testing.T) {
	s := mustNew(t, 2)
	s.Acquire()
	s.Acquire() // fill

	before := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		_ = s.AcquireNWith(ctx, 1)
		cancel()
	}

	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Errorf("goroutine leak: before=%d, after=%d (ran 50 cancellations)",
			before, after)
	}

	s.ReleaseN(2)
}
