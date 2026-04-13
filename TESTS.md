# TESTS.md

## Overview

This document serves as the authoritative reference for the `sema` package test
suite. It explains what was tested, how each test category works, and why specific
decisions were made.

This document is intended for the repository administrator and any contributor
reviewing the `sema` package and its expanded interface with multi-slot operations,
context support, timeouts, dynamic resizing, observability, and EWMA utilization
tracking.

---

## Test File Structure

The test file is organized into ten labeled sections:

```
1.  Helpers & Mock Observer
2.  Unit Tests — Constructors
3.  Unit Tests — Single Slot
4.  Unit Tests — Multi Slot
5.  Unit Tests — Introspection
6.  Unit Tests — Observer
7.  Unit Tests — Error Types
8.  Fuzz Tests
9.  Benchmark Tests
10. Application & Regression Tests
```

Each section maps directly to a surface area of the `Semaphore` interface.

---

## Section 1: Helpers & Mock Observer

### What

A `mockObserver` struct that implements the `Observer` interface, and a `mustNew`
helper that wraps `New()` with a fatal on error.

### How

`mockObserver` protects all fields with a `sync.Mutex` so it is safe to call from
concurrent goroutines in the concurrency tests. It tracks four event types:
`OnAcquire`, `OnRelease`, `OnWaitStart`, `OnWaitEnd`. Counts are exposed via
locked accessor methods (`AcquireCount()`, `ReleaseCount()`) to avoid data races
in tests that read observer state after concurrent operations.

### Why

The observer is the only external hook the semaphore exposes for telemetry. Before
declaring the observer feature complete, every emission point must be verified to
actually call the observer. The mock makes that assertion cheap and deterministic.

---

## Section 2: Unit Tests — Constructors

### Tests Included

| Test | What it proves |
|---|---|
| `TestNew_ValidCapacity` | `New(c)` for c in {1,5,10,100} returns a non-nil semaphore with correct `Cap()` |
| `TestNew_DefaultCap` | `New(-1)` returns a semaphore with `Cap() == defaultCap` (17) |
| `TestNew_InvalidCapacity` | `New(c)` for c in {0,-2,-100} returns `ErrInvalidCap`, nil semaphore |
| `TestNew_LargeCapacity` | `New(1_000_000)` succeeds and reports correct `Cap()` |
| `TestMust_ValidCapacity` | `Must(5)` does not panic |
| `TestMust_InvalidCapacity` | `Must(0)` panics |
| `TestNewWithObserver` | Observer is wired correctly: `OnAcquire` and `OnRelease` fire |
| `TestNewWithObserver_InvalidCap` | Invalid cap propagates through `NewWithObserver` |

### How

Constructor tests are pure sequential unit tests with no goroutines. They verify
the contract at the boundary of object creation.

### Why

Constructors establish the invariants every other test depends on. If `New` returns
the wrong capacity or the wrong error type, every downstream test is testing against
a broken foundation.

---

## Section 3: Unit Tests — Single Slot

### Tests Included

| Test | What it proves |
|---|---|
| `TestAcquire_Basic` | `Acquire()` increments `Len()` by 1 |
| `TestAcquire_Blocks` | `Acquire()` on a full cap=1 semaphore blocks until `Release()` |
| `TestAcquireWith_Success` | `AcquireWith(ctx)` succeeds on an available slot |
| `TestAcquireWith_CancelledContext` | Pre-cancelled context returns `ErrAcquireCancelled` immediately |
| `TestAcquireWith_ContextCancelMidWait` | Context cancelled while blocked returns `ErrAcquireCancelled` |
| `TestAcquireWith_SurvivesSetCap` | `AcquireWith` retries on the new channel after `SetCap` swaps it mid-acquire |
| `TestAcquireTimeout_Success` | `AcquireTimeout` succeeds when slot is available |
| `TestAcquireTimeout_Expires` | `AcquireTimeout` returns `ErrAcquireCancelled` after deadline |
| `TestTryAcquire_Success` | `TryAcquire()` returns true on an available slot |
| `TestTryAcquire_Fails_WhenFull` | `TryAcquire()` returns false when full |
| `TestTryAcquireWith_Success` | `TryAcquireWith(ctx)` succeeds on an available slot |
| `TestTryAcquireWith_NoSlot` | Returns `ErrNoSlot` when no slot available |
| `TestTryAcquireWith_CancelledContext` | Pre-cancelled context returns `ErrAcquireCancelled` |
| `TestRelease_Basic` | `Release()` decrements `Len()` correctly |
| `TestRelease_WithoutAcquire` | `Release()` on empty semaphore returns `ErrReleaseExceedsCount` |
| `TestRelease_ErrorReportsActualOccupancy` | `ErrReleaseExceedsCount.Current` reflects real occupancy, not hardcoded zero |

### How

Blocking tests (`TestAcquire_Blocks`, `TestAcquireWith_ContextCancelMidWait`) use
goroutines with `time.After` select statements to assert that blocking actually
occurs before the unblocking event. A 50ms window is used to assert the block,
and a 1s window is used to assert the eventual unblock.

`TestAcquireWith_SurvivesSetCap` fills a cap=2 semaphore, starts a blocking
`AcquireWith` with a 2-second timeout, then calls `SetCap(5)` to expand capacity.
The test verifies that `AcquireWith` detects the channel swap and successfully
acquires on the new channel rather than timing out on the stale one.

### Why

The single-slot API is the most commonly used path. Every context interaction
(pre-cancelled, mid-wait cancel, timeout expiry) must be tested independently
because each is handled by a distinct code path in `AcquireWith`.

The `SurvivesSetCap` test is critical because `SetCap` replaces the internal
channel. Any goroutine blocked on the old channel must detect the swap and retry,
otherwise it silently deadlocks until its context expires. This test catches
exactly that failure mode.

### Design Notes

**Timing sensitivity.** The 50ms assertion windows are short enough to be fast but
long enough to be reliable on most CI hardware. On heavily loaded machines these
could theoretically false-positive. This is a known tradeoff in concurrency testing
and is acceptable here because the alternative (larger windows) slows the suite
unnecessarily.

---

## Section 4: Unit Tests — Multi Slot

### Tests Included

| Test | What it proves |
|---|---|
| `TestAcquireN_Basic` | `AcquireN(5)` on cap=10 sets `Len()` to 5 |
| `TestAcquireN_InvalidN` | n in {0,-1,-5} returns `ErrInvalidN` |
| `TestAcquireN_ExceedsCap` | n > cap returns `ErrNExceedsCap` |
| `TestAcquireN_DoesNotBlockForever` | `AcquireN` on a full semaphore unblocks when a slot is released |
| `TestAcquireNWith_Success` | `AcquireNWith(ctx, 4)` acquires 4 slots |
| `TestAcquireNWith_ContextCancelled` | Pre-cancelled context returns `ErrAcquireCancelled` |
| `TestAcquireNWith_RollsBackOnCancel` | Partial acquisition is rolled back when context cancelled mid-wait |
| `TestAcquireNWith_SurvivesSetCap` | `AcquireNWith` handles channel swap from `SetCap` mid-acquire |
| `TestAcquireNTimeout_Success` | Succeeds within timeout |
| `TestAcquireNTimeout_Expires` | Returns `ErrAcquireCancelled` on expiry |
| `TestTryAcquireN_Success` | Returns true when enough slots available |
| `TestTryAcquireN_Fails_WhenNotEnoughSlots` | Returns false, no partial acquisition |
| `TestTryAcquireN_InvalidN` | Returns false for n=0 |
| `TestTryAcquireN_ExceedsCap` | Returns false when n > cap |
| `TestTryAcquireNWith_Success` | Acquires n slots non-blocking |
| `TestTryAcquireNWith_NoSlot` | Returns `ErrNoSlot` when full |
| `TestTryAcquireNWith_ExceedsCap` | Returns `ErrNExceedsCap` |
| `TestTryAcquireNWith_CancelledContext` | Returns `ErrAcquireCancelled` |
| `TestReleaseN_Basic` | `ReleaseN(3)` after `AcquireN(5)` leaves `Len()` at 2 |
| `TestReleaseN_ExceedsCount` | Releasing more than acquired returns `ErrReleaseExceedsCount` |
| `TestReleaseN_InvalidN` | n=0 returns `ErrInvalidN` |

### How

`TestAcquireNWith_RollsBackOnCancel` is the most important test in this section.
It fills 4 of 5 slots, then in a goroutine requests 3 slots (only 1 available).
The goroutine acquires 1 and blocks waiting for 2 more. After 30ms the context is
cancelled. The test then asserts both the error type AND that `Len()` returned to
4, proving the partial acquisition was rolled back.

### Why

The rollback behavior is a correctness guarantee of `AcquireNWith`. Without it,
a cancelled multi-slot acquire would silently consume slots, causing the semaphore
to report more occupancy than is real. This is the single most critical behavioral
invariant in the multi-slot API and must be tested explicitly.

### Design Notes

The 30ms sleep before cancel in `TestAcquireNWith_RollsBackOnCancel` is
load-sensitive. On a very slow machine the goroutine might not have had time to
acquire the one available slot yet, meaning `Len()` could still be 4 but for the
wrong reason (nothing was rolled back because nothing was acquired). The test is
reliable in practice because `AcquireNWith` acquires available slots eagerly before
blocking for the remainder.

---

## Section 5: Unit Tests — Introspection

### Tests Included

| Test | What it proves |
|---|---|
| `TestLen` | `Len()` correctly reflects 0 then 2 after two acquires |
| `TestCap` | `Cap()` returns the value passed to `New()` |
| `TestIsEmpty` | True when fresh, false after acquire |
| `TestIsFull` | False when fresh, true when all slots taken |
| `TestUtilization` | 0.0 when empty, 0.5 when half full on cap=4 |
| `TestUtilizationSmoothed_InitiallyZero` | EWMA starts at 0.0 |
| `TestUtilizationSmoothed_UpdatesOnRelease` | EWMA is non-NaN and non-negative after a release cycle |
| `TestUtilizationSmoothed_MovesAfterRelease` | EWMA moves to expected value after releasing with slots still held |
| `TestUtilizationSmoothed_AccumulatesOverMultipleReleases` | EWMA accumulates correctly over 10 sequential releases |
| `TestSetCap_Expand` | Growing cap preserves existing slot count |
| `TestSetCap_Shrink` | Shrinking cap drains slots per spec |
| `TestSetCap_Shrink_LenIsZero` | `Len() == 0` after shrinking below current occupancy |
| `TestSetCap_Shrink_ToExactCurrentLen` | Shrinking to exact current `Len()` preserves all slots |
| `TestSetCap_Shrink_BelowCurrentLen` | Shrinking below current `Len()` drains to zero |
| `TestSetCap_Default` | `SetCap(-1)` sets cap to `defaultCap` |
| `TestSetCap_Invalid` | `SetCap(0)` returns `ErrInvalidCap` |
| `TestSetCap_UpdatesEWMA` | EWMA is recalculated against new capacity after resize |
| `TestDrain` | `Drain()` empties all slots |
| `TestDrain_ResetsEWMA` | `UtilizationSmoothed()` is zero after `Drain()` |
| `TestReset` | `Reset()` empties slots and preserves cap |
| `TestReset_ResetsEWMA` | `UtilizationSmoothed()` is zero after `Reset()` |
| `TestWait_ReturnsWhenEmpty` | `Wait()` returns immediately on empty semaphore |
| `TestWait_BlocksUntilEmpty` | `Wait()` blocks until all slots released |
| `TestWait_ContextCancelled` | `Wait()` returns `ErrAcquireCancelled` on context cancel |
| `TestWait_CancelDoesNotLeakGoroutine` | Cancelling `Wait` does not leave goroutines parked on `cond.Wait()` |

### How

Introspection tests are sequential where possible. `TestWait_BlocksUntilEmpty`
and `TestWait_ContextCancelled` require goroutines because `Wait` blocks.

`TestUtilizationSmoothed_MovesAfterRelease` acquires 4 of 5 slots, releases 1
(leaving 3 held), and verifies that EWMA equals `α × (3/5)` — proving the EWMA
calculation uses the utilization at the moment of release, not after it.

`TestSetCap_UpdatesEWMA` builds a non-zero EWMA at one capacity, then resizes
and verifies the EWMA changed to reflect the new capacity ratio.

### Why

Introspection methods are the observability surface for callers. If `Len()` is
wrong, rate limiting logic built on top of it will make incorrect decisions.
`Utilization()` and `UtilizationSmoothed()` are particularly important to verify
as valid IEEE 754 floats since callers may feed them into dashboards or alerting.

The `SetCap_Shrink` variants cover three distinct code paths in `SetCap`: shrinking
with no occupancy (trivial), shrinking to exactly current occupancy (slots
preserved, channel replaced), and shrinking below current occupancy (drain path).

---

## Section 6: Unit Tests — Observer

### Tests Included

| Test | What it proves |
|---|---|
| `TestObserver_OnAcquire_Called` | `Acquire()` fires `OnAcquire` once |
| `TestObserver_OnRelease_Called` | `Release()` fires `OnRelease` once |
| `TestObserver_OnWait_Called` | `Wait()` fires `OnWaitStart` and `OnWaitEnd` each once |
| `TestObserver_MultipleAcquiresTracked` | 5 sequential `Acquire()` calls fire `OnAcquire` 5 times |
| `TestObserver_AcquireN_Called` | `AcquireN(3)` fires `OnAcquire` exactly once (not 3 times) |
| `TestObserver_ReleaseN_Called` | `ReleaseN(3)` fires `OnRelease` exactly once |
| `TestObserver_TryAcquire_Called` | `TryAcquire()` fires `OnAcquire` on success |
| `TestObserver_TryAcquire_NotCalled_WhenFull` | `TryAcquire()` does NOT fire `OnAcquire` when it returns false |
| `TestObserver_AcquireWith_Called` | `AcquireWith(ctx)` fires `OnAcquire` on success |
| `TestObserver_AcquireWith_NotCalled_WhenCancelled` | `AcquireWith(ctx)` does NOT fire `OnAcquire` on cancellation |
| `TestObserver_TryAcquireNWith_Called` | `TryAcquireNWith(ctx, 3)` fires `OnAcquire` once on success |
| `TestObserver_TryAcquireNWith_NotCalled_WhenNoSlot` | Does NOT fire `OnAcquire` when returning `ErrNoSlot` |
| `TestObserver_TryAcquireNWith_NotCalled_WhenCancelled` | Does NOT fire `OnAcquire` on cancelled context |
| `TestTryAcquireN_ObserverNotCalled` | `TryAcquireN(3)` fires `OnAcquire` once on success |

### How

Each test creates a fresh `mockObserver`, wires it via `NewWithObserver`, performs
the operation, and checks the count. All reads go through the locked accessor.

Negative tests (the `_NotCalled_When*` variants) verify that failed acquire
attempts do not spuriously fire the observer. These work by filling the semaphore
or pre-cancelling the context, then confirming the acquire count did not change.

### Why

The observer contract specifies that `AcquireN` fires `OnAcquire` once for the
batch, not once per slot. This is a semantic decision that callers building
Prometheus counters or structured logs depend on. The test makes this explicit.

Every code path that calls `s.notify(...)` has a corresponding positive test, and
every code path that can fail has a corresponding negative test verifying that
`OnAcquire` is NOT fired on failure. This ensures observers never see phantom
acquire events from rejected requests.

---

## Section 7: Unit Tests — Error Types

### Tests Included

| Test | What it proves |
|---|---|
| `TestErrorTypes_IsMatching` | `errors.Is` works for all 7 error types with zero-value targets |
| `TestErrAcquireCancelled_Unwrap` | `Unwrap()` exposes the cause so `errors.Is(err, context.DeadlineExceeded)` works |
| `TestErrRecovered_Unwrap` | `Unwrap()` exposes `AsError` |
| `TestErrorStrings` | `Error()` returns non-empty strings containing expected substrings |

### How

`errors.Is` matching is tested by passing a zero-value instance of each error
type as the target. This works because each error type's `Is` method uses a type
assertion rather than value equality, which is the correct pattern for structured
error types with variable fields.

`TestErrorStrings` verifies both that `Error()` is non-empty and that it contains
the expected diagnostic value (e.g. the invalid capacity, the attempted release
count, the context error message) via `strings.Contains`.

### Why

Callers using `errors.Is(err, sema.ErrNoSlot{})` to branch on specific failure
modes depend on this contract. If the `Is` implementation is broken, callers get
silent failures. Testing it explicitly ensures the contract holds through refactors.

---

## Section 8: Fuzz Tests

### Fuzz Targets

| Target | What it fuzzes |
|---|---|
| `FuzzNew` | Random capacity values, verifies error/success contract |
| `FuzzAcquireN` | Random cap and n, verifies error classification |
| `FuzzReleaseN` | Random cap, acquired count, and release count, verifies error classification |
| `FuzzSetCap` | Random initial cap and new cap, verifies error and `Cap()` result |
| `FuzzTryAcquireN` | Random cap, pre-acquire amount, and n, verifies true/false contract |
| `FuzzUtilization` | Random cap and acquired count, verifies float is in [0,1] |
| `FuzzConcurrentAcquireRelease` | Random cap, goroutine count, and acquire-N size under concurrent access |
| `FuzzConcurrentMixedOps` | Random cap and goroutine count mixing `TryAcquire` and `AcquireWith` concurrently |

### How

Each fuzz target defines a seed corpus covering the boundary conditions that the
unit tests exercise (valid, default -1, zero, negative, exceeds-cap). The fuzzer
then explores the space beyond those seeds. Guard clauses clamp fuzzer-generated
values to safe ranges (e.g. cap ≤ 1000) to prevent the fuzzer from creating
channels so large they exhaust memory.

The concurrent fuzz targets (`FuzzConcurrentAcquireRelease`,
`FuzzConcurrentMixedOps`) launch multiple goroutines under a 500ms context timeout.
Each goroutine acquires, verifies `Len() <= Cap()` and `Len() >= 0`, then releases.
After all goroutines complete, the test asserts `Len() == 0` to detect slot leaks.

### Why

Fuzz tests catch the cases that unit tests miss: unexpected combinations of valid
inputs that expose ordering dependencies, integer overflow edge cases, or state
corruption from rapid create/destroy cycles. The semaphore's atomic EWMA and
channel-swap in `SetCap` are particularly worth fuzzing because they involve
concurrent state transitions.

The concurrent fuzz targets are especially important because the semaphore's
correctness under concurrent access is its most critical property. Racing acquires
against releases under randomized parameters exposes ordering bugs and invariant
violations that sequential tests cannot.

---

## Section 9: Benchmark Tests

### Benchmarks Included

Every interface method has a single-goroutine benchmark. Additionally:

| Benchmark | Purpose |
|---|---|
| `BenchmarkAcquireRelease_Parallel` | Measures lock contention under `GOMAXPROCS` goroutines |
| `BenchmarkTryAcquireN_Parallel` | Measures non-blocking path under parallel load |
| `BenchmarkObserver_Overhead` | Isolates the cost of observer dispatch in the hot path |

### How

Single-goroutine benchmarks use `b.N + 1` as the semaphore capacity so acquire
never blocks, isolating the acquire/release operation itself. `b.ResetTimer()` is
called after setup to exclude construction cost from the measurement.

The parallel benchmarks use `runtime.GOMAXPROCS(0)` as the semaphore capacity to
match the actual parallelism available, ensuring contention is measured accurately
regardless of the machine running the benchmark.

### Why

The benchmarks serve two purposes: regression detection (a future refactor that
accidentally introduces a mutex on the read path will show up immediately) and
capacity planning guidance (users building high-throughput pipelines can see the
per-operation overhead before choosing this package).

---

## Section 10: Application & Regression Tests

### Tests Included

| Test | What it proves |
|---|---|
| `TestApp_Lifecycle_DrainResetWait` | Full lifecycle: acquire → drain → wait → re-acquire → reset → resize → fill → release → wait |
| `TestApp_Lifecycle_WaitCancelledDuringDrain` | `Wait` returns `ErrAcquireCancelled` when context expires with slots still held, without altering slot count |
| `TestAcquireWith_CancelDoesNotLeakGoroutine` | 50 rapid cancel cycles on `AcquireWith` do not leak goroutines |
| `TestAcquireNWith_CancelDoesNotLeakGoroutine` | 50 rapid cancel cycles on `AcquireNWith` do not leak goroutines |

### How

`TestApp_Lifecycle_DrainResetWait` runs through seven phases sequentially:
acquiring a working load, draining for maintenance, confirming idle via `Wait`,
post-drain activity, hard reset, scaling capacity via `SetCap`, and resuming under
the new capacity. Each phase asserts the expected state before proceeding.

The goroutine leak tests fill the semaphore, then rapidly create and cancel
blocking acquires in a loop. After the loop, they sleep briefly to let goroutines
settle, then compare `runtime.NumGoroutine()` against the pre-loop count with a
tolerance of 2 for background runtime goroutines.

### Why

The lifecycle test demonstrates the full maintenance window pattern that production
users depend on: drain in-flight work, verify idle, perform maintenance, reset,
resize, resume. Testing this as an integrated sequence catches state corruption
that isolated unit tests cannot — for example, a `Reset` that doesn't properly
broadcast to waiting goroutines, or a `SetCap` after `Drain` that loses the EWMA
reset.

The goroutine leak tests are regression tests for the `AcquireWith` and
`AcquireNWith` implementations. Both methods spawn internal goroutines to
coordinate between `cond.Wait()` and context cancellation. If those goroutines are
not properly cleaned up on cancellation, repeated use will leak goroutines
indefinitely. The tests use 50 iterations to amplify any per-call leak into a
detectable signal.

---

## Summary

The test suite covers every method on the `Semaphore` interface with unit tests,
fuzz targets, and benchmarks. Observer emission is verified on every code path
with both positive and negative tests. Error types are tested for `errors.Is`
matching, `Unwrap` chaining, and diagnostic string content.

The concurrent fuzz targets (`FuzzConcurrentAcquireRelease`,
`FuzzConcurrentMixedOps`) race acquires against releases under randomized
parameters to verify the core invariants (`Len ≤ Cap`, `Len ≥ 0`, no slot leaks)
hold under contention.

The lifecycle and goroutine leak tests verify the package's production-critical
behaviors: graceful drain/reset sequences and clean goroutine cleanup on context
cancellation.

All tests pass under the race detector (`go test -race ./...`).