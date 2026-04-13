# TESTS.md

## Overview

This document serves as the authoritative reference for the `sema` package test
suite. It explains what was tested, how each test category works, why specific
decisions were made, and flags known issues identified during code review that
must be resolved before the suite compiles and passes cleanly.

This document is intended for the repository administrator and any contributor
reviewing the rewrite of the `sema` package from its original minimal interface
(`Acquire`, `Release`, `Len`, `IsEmpty`) to its current expanded interface with
multi-slot operations, context support, timeouts, dynamic resizing, observability,
and EWMA utilization tracking.

---

## Test File Structure

The test file is organized into eight labeled sections:

```
1. Helpers & Mock Observer
2. Unit Tests — Constructors
3. Unit Tests — Single Slot
4. Unit Tests — Multi Slot
5. Unit Tests — Introspection
6. Unit Tests — Observer
7. Unit Tests — Error Types
8. Fuzz Tests
9. Benchmark Tests
10. Application Tests
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

### Code Review Issues

**None.** The mock is correct. The mutex discipline is sound. The accessor methods
are appropriately narrow.

---

## Section 2: Unit Tests — Constructors

### Tests Included

| Test | What it proves |
|---|---|
| `TestNew_ValidCapacity` | `New(c)` for c in {1,5,10,100} returns a non-nil semaphore with correct `Cap()` |
| `TestNew_DefaultCap` | `New(-1)` returns a semaphore with `Cap() == defaultCap` (10) |
| `TestNew_InvalidCapacity` | `New(c)` for c in {0,-2,-100} returns `ErrInvalidCap`, nil semaphore |
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

### Code Review Issues

**None.** These tests are straightforward and correct.

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
| `TestAcquireTimeout_Success` | `AcquireTimeout` succeeds when slot is available |
| `TestAcquireTimeout_Expires` | `AcquireTimeout` returns `ErrAcquireCancelled` after deadline |
| `TestTryAcquire_Success` | `TryAcquire()` returns true on an available slot |
| `TestTryAcquire_Fails_WhenFull` | `TryAcquire()` returns false when full |
| `TestTryAcquireWith_Success` | `TryAcquireWith(ctx)` succeeds on an available slot |
| `TestTryAcquireWith_NoSlot` | Returns `ErrNoSlot` when no slot available |
| `TestTryAcquireWith_CancelledContext` | Pre-cancelled context returns `ErrAcquireCancelled` |
| `TestRelease_Basic` | `Release()` decrements `Len()` correctly |
| `TestRelease_WithoutAcquire` | `Release()` on empty semaphore returns `ErrReleaseExceedsCount` |

### How

Blocking tests (`TestAcquire_Blocks`, `TestAcquireWith_ContextCancelMidWait`) use
goroutines with `time.After` select statements to assert that blocking actually
occurs before the unblocking event. A 50ms window is used to assert the block,
and a 1s window is used to assert the eventual unblock.

### Why

The single-slot API is the most commonly used path. Every context interaction
(pre-cancelled, mid-wait cancel, timeout expiry) must be tested independently
because each is handled by a distinct code path in `AcquireWith`.

### Code Review Issues

**Issue 1 — `TestAcquire_Blocks` has a goroutine leak risk.**

The goroutine that calls `s.Acquire()` and then `close(done)` will be left running
after the test completes if `Release()` is never called at the end. In the current
test, `Release()` is called to unblock it, and the goroutine closes `done`.
However if the goroutine acquires and the test exits before `close(done)` is
consumed, the channel close is still safe. This is acceptable but worth noting.

**Issue 2 — Timing sensitivity.**

The 50ms assertion windows are short enough to be fast but long enough to be
reliable on most CI hardware. On heavily loaded machines these could theoretically
false-positive. This is a known tradeoff in concurrency testing and is acceptable
here because the alternative (larger windows) slows the suite unnecessarily.

---

## Section 4: Unit Tests — Multi Slot

### Tests Included

| Test | What it proves |
|---|---|
| `TestAcquireN_Basic` | `AcquireN(5)` on cap=10 sets `Len()` to 5 |
| `TestAcquireN_InvalidN` | n in {0,-1,-5} returns `ErrInvalidN` |
| `TestAcquireN_ExceedsCap` | n > cap returns `ErrNExceedsCap` |
| `TestAcquireNWith_Success` | `AcquireNWith(ctx, 4)` acquires 4 slots |
| `TestAcquireNWith_ContextCancelled` | Pre-cancelled context returns `ErrAcquireCancelled` |
| `TestAcquireNWith_RollsBackOnCancel` | Partial acquisition is rolled back when context cancelled mid-wait |
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

### Code Review Issues

**Issue 3 — `TestAcquireNWith_RollsBackOnCancel` has a race on `Len()` read.**

After `cancel()` is called and the goroutine returns via the `errc` channel, the
test immediately reads `s.Len()`. The rollback in `AcquireNWith` drains from the
channel (`<-s.channel()`) before returning the error. Since the error is returned
before `Len()` is observed, and both happen in the same goroutine (the `errc`
receive happens-before the `Len()` call), this is safe. **No race condition.**
However the 30ms sleep before cancel is load-sensitive. On a very slow machine the
goroutine might not have had time to acquire the one available slot yet, meaning
Len() could still be 4 but for the wrong reason (nothing was rolled back because
nothing was acquired). Consider adding a brief spin-wait on `s.Len() == 5` before
cancelling to make the test intent reliable.

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
| `TestSetCap_Expand` | Growing cap preserves existing slot count |
| `TestSetCap_Shrink` | Shrinking cap drains slots per spec |
| `TestSetCap_Default` | `SetCap(-1)` sets cap to `defaultCap` |
| `TestSetCap_Invalid` | `SetCap(0)` returns `ErrInvalidCap` |
| `TestDrain` | `Drain()` empties all slots |
| `TestReset` | `Reset()` empties slots and preserves cap |
| `TestWait_ReturnsWhenEmpty` | `Wait()` returns immediately on empty semaphore |
| `TestWait_BlocksUntilEmpty` | `Wait()` blocks until all slots released |
| `TestWait_ContextCancelled` | `Wait()` returns `ErrAcquireCancelled` on context cancel |

### How

Introspection tests are sequential where possible. `TestWait_BlocksUntilEmpty`
and `TestWait_ContextCancelled` require goroutines because `Wait` blocks.

### Why

Introspection methods are the observability surface for callers. If `Len()` is
wrong, rate limiting logic built on top of it will make incorrect decisions.
`Utilization()` and `UtilizationSmoothed()` are particularly important to verify
as valid IEEE 754 floats since callers may feed them into dashboards or alerting.

### Code Review Issues

**Issue 4 — `TestUtilizationSmoothed_UpdatesOnRelease` is weak.**

The test structure has a nested `if` that only checks for NaN/negative if the
EWMA is still 0.0 after a release. The EWMA update happens in `Release()` at the
point where `Len()` is already 0 (the slot was removed before `updateEWMA()` is
called). So `current = 0/5 = 0.0` and `EWMA = 0.1*0.0 + 0.9*0.0 = 0.0`. The
EWMA will legitimately still be 0.0. The inner check runs and passes vacuously.

**This test does not actually verify that EWMA updates.** It only verifies it is
not NaN or negative — which was already true before the acquire/release cycle.

**Recommendation:** Test EWMA after a `ReleaseN` while some slots are still held,
so that `current > 0` at the time of the update. For example: acquire 4 of 5,
then release 1 (leaving 3 held). The EWMA update sees `current = 3/5 = 0.6`, so
EWMA moves to `0.1*0.6 + 0.9*0.0 = 0.06`. Assert `UtilizationSmoothed() > 0`.

**Issue 5 — `TestSetCap_Shrink` does not verify `Len()` after shrink.**

The implementation drains all slots when shrinking. The test verifies `Cap()` is
correct but does not assert `Len() == 0`. This should be added.

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

### How

Each test creates a fresh `mockObserver`, wires it via `NewWithObserver`, performs
the operation, and checks the count. All reads go through the locked accessor.

### Why

The observer contract specifies that `AcquireN` fires `OnAcquire` once for the
batch, not once per slot. This is a semantic decision that callers building
Prometheus counters or structured logs depend on. The test makes this explicit.

### Code Review Issues

**Issue 6 — `AcquireWith`, `TryAcquire`, `TryAcquireWith`, `TryAcquireN`, and
`TryAcquireNWith` are not tested for observer emission.**

Only `Acquire`, `AcquireN`, `Release`, `ReleaseN`, and `Wait` have observer tests.
Every code path that calls `s.notify(...)` should have a corresponding observer
test. The missing paths are:

- `AcquireWith` → calls `notify(OnAcquire)`
- `TryAcquire` → calls `notify(OnAcquire)`
- `TryAcquireWith` → calls `notify(OnAcquire)`
- `TryAcquireN` → does NOT call notify (calls `tryAcquireNLocked` which does not notify — this is a bug in the implementation worth flagging)
- `TryAcquireNWith` → calls `notify(OnAcquire)`
- `AcquireNWith` → calls `notify(OnAcquire)`

**`TryAcquireN` missing observer call is an implementation gap**, not a test gap.
The test suite should add a test that catches this so the implementation can be
corrected.

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

### Why

Callers using `errors.Is(err, sema.ErrNoSlot{})` to branch on specific failure
modes depend on this contract. If the `Is` implementation is broken, callers get
silent failures. Testing it explicitly ensures the contract holds through refactors.

### Code Review Issues

**Issue 7 — `TestErrorStrings` does not assert the `contains` field.**

The test struct has a `contains string` field, but the test body only checks
`tt.err.Error() == ""`. It never checks whether the error string actually contains
the expected substring. The `contains` field is populated but ignored.

**This test is incomplete.** It should use `strings.Contains(tt.err.Error(), tt.contains)`.

---

## Section 8: Fuzz Tests

### Fuzz Targets

| Target | What it fuzzes |
|---|---|
| `FuzzNew` | Random capacity values, verifies error/success contract |
| `FuzzAcquireN` | Random cap and n, verifies error classification |
| `FuzzReleaseN` | Random cap, acquired count, and release count, verifies error classification |
| `FuzzSetCap` | Random initial cap and new cap, verifies error and Cap() result |
| `FuzzTryAcquireN` | Random cap, pre-acquire amount, and n, verifies true/false contract |
| `FuzzUtilization` | Random cap and acquired count, verifies float is in [0,1] |

### How

Each fuzz target defines a seed corpus covering the boundary conditions that the
unit tests exercise (valid, default -1, zero, negative, exceeds-cap). The fuzzer
then explores the space beyond those seeds. Guard clauses clamp fuzzer-generated
values to safe ranges (e.g. cap ≤ 1000) to prevent the fuzzer from creating
channels so large they exhaust memory.

### Why

Fuzz tests catch the cases that unit tests miss: unexpected combinations of valid
inputs that expose ordering dependencies, integer overflow edge cases, or state
corruption from rapid create/destroy cycles. The semaphore's atomic EWMA and
channel-swap in `SetCap` are particularly worth fuzzing because they involve
concurrent state transitions.

### Code Review Issues

**Issue 8 — Fuzz targets do not exercise concurrent access.**

All fuzz targets are single-goroutine. The semaphore's correctness guarantees
under concurrent access (the most critical property) are not fuzz-tested. Adding
a `FuzzConcurrentAcquireRelease` target that launches N goroutines and races
acquires against releases would significantly strengthen the suite.

**Issue 9 — `FuzzTryAcquireN` has a logical gap.**

When `n == available` exactly, both `n <= available` and `n > available` branches
are evaluated with `n <= available` being true, so `got` must be true. This is
correct. However the fuzz target does not guard against the case where `preAcquire`
already consumed all slots and `available == 0`. In that case `n >= 1` and
`n > available` (0) is always true, so `got` must be false. This works correctly
but is worth adding an explicit comment so the logic is not misread.

---

## Section 9: Benchmark Tests

### Benchmarks Included

Every interface method has a single-goroutine benchmark. Additionally:

| Benchmark | Purpose |
|---|---|
| `BenchmarkAcquireRelease_Parallel` | Measures lock contention under GOMAXPROCS goroutines |
| `BenchmarkTryAcquireN_Parallel` | Measures non-blocking path under parallel load |
| `BenchmarkObserver_Overhead` | Isolates the cost of observer dispatch in the hot path |

### How

Single-goroutine benchmarks use `b.N + 1` as the semaphore capacity so acquire
never blocks, isolating the acquire/release operation itself. `b.ResetTimer()` is
called after setup to exclude construction cost from the measurement.

### Why

The benchmarks serve two purposes: regression detection (a future refactor that
accidentally introduces a mutex on the read path will show up immediately) and
capacity planning guidance (users building high-throughput pipelines can see the
per-operation overhead before choosing this package).

### Code Review Issues

**Issue 10 — `BenchmarkAcquireRelease_Parallel` uses a hardcoded capacity of 8.**

The `runtime_GOMAXPROCS()` helper returns the constant `8` rather than calling
`runtime.GOMAXPROCS(0)`. On machines with fewer than 8 cores the benchmark will
still run, but the semaphore cap (8) may be larger than the actual parallelism,
meaning the benchmark never actually measures contention. On machines with more
than 8 cores the benchmark underrepresents real contention.

**Recommendation:** Import `runtime` and call `runtime.GOMAXPROCS(0)` directly,
or at minimum name the helper accurately so it is not mistaken for the real value.

**Issue 11 — `BenchmarkTryAcquire` calls `s.Release()` unconditionally.**

`TryAcquire` may return false if the semaphore is full (which should not happen
here since cap = b.N + 1, but the pattern is misleading). If `TryAcquire` returns
false, `Release()` is called with nothing to release, returning `ErrReleaseExceedsCount`.
The error is discarded with `_`, so the benchmark does not fail, but it is
measuring error-path overhead alongside the happy path. This should be guarded:

```go
if s.TryAcquire() {
    _ = s.Release()
}
```

---

## Section 10: Application Tests

### Overview

Three application tests demonstrate the package in realistic scenarios, ordered
by complexity. These are integration-style tests that verify the package behaves
correctly as a component, not just as isolated methods.

---

### App Test 1: `TestApp_NoConcurrency_BasicSequentialGuard`

**Scenario:** A configuration loader protected by a cap=1 semaphore used
sequentially. No goroutines.

**What it exercises:** `New`, `AcquireTimeout`, `Release` (via defer), `IsEmpty`,
`Utilization`, `Cap`, `Len`.

**Why:** Demonstrates the simplest valid use case. A developer reading the
repository for the first time should be able to understand how to wrap a
resource-loading operation with a semaphore from this test alone.

**Code Review — Scope Assessment:** ✅ **Appropriate for "basic, no concurrency".**
The scenario is coherent, self-contained, and genuinely useful as reference
material. No scope creep.

---

### App Test 2: `TestApp_Concurrency_WorkerPool`

**Scenario:** 20 jobs dispatched as goroutines, with a cap=5 semaphore limiting
concurrent execution. Peak concurrency is tracked and verified to never exceed
the cap.

**What it exercises:** `New`, `NewWithObserver`, `Acquire`, `Release`, `Wait`,
`IsEmpty`, observer `AcquireCount`.

**Why:** The worker pool is the canonical semaphore use case. The peak-tracking
logic directly verifies the semaphore's core correctness guarantee: no more than
N goroutines run concurrently.

**Code Review — Scope Assessment:** ✅ **Appropriate for "basic, concurrent."**

**Issue 12 — `pool` is declared twice.**

```go
pool, err := New(maxWorkers)   // first declaration — pool is Semaphore
...
pool, err = NewWithObserver(maxWorkers, obs)  // reassignment
```

The first `New(maxWorkers)` call is immediately discarded. This is confusing and
wastes an allocation. Remove the first `New` call entirely — `NewWithObserver` is
the one being used. The variable declaration should be:

```go
pool, err := NewWithObserver(maxWorkers, obs)
```

**Issue 13 — `peakActive` tracking has a TOCTOU gap.**

The pattern is: acquire semaphore → increment `active` → read `active` → update
`peakMu`-protected `peakActive` → sleep → decrement `active`. The `active` counter
and `peakActive` tracking are not atomic with respect to each other. A goroutine
could increment `active` to 6 but the peak read happens before another goroutine
decrements, making this technically safe. However the `active.Add(1)` and the
subsequent `peakMu.Lock()` read can be separated by a preemption, allowing another
goroutine to also increment before the first goroutine reads. This means `peakActive`
could be 5 even if 6 goroutines transiently held the slot simultaneously — which
would be a false pass. In practice this is extremely unlikely with the 5ms sleep,
but it is not a rigorous peak measurement.

**This does not invalidate the test** because the semaphore itself prevents more
than 5 concurrent acquires. The peak tracking is measuring post-acquire active
count, not semaphore occupancy. The semaphore correctness is guaranteed by the
channel capacity, not by this counter. The counter's imprecision is acceptable.

---

### App Test 3: `TestApp_ConcurrencyAdvanced_APIGatewayRateLimiter`

**Scenario:** An API gateway with premium clients (2-slot burst via `AcquireNWith`),
standard clients (`AcquireTimeout`), fast-path clients (`TryAcquireWith`,
`TryAcquire`, `TryAcquireN`, `TryAcquireNWith`), a mid-flight `SetCap` resize,
a maintenance `Drain`, a `Reset`, a shutdown `Wait`, and smoothed utilization
checks — all with an active observer.

**What it exercises:** Every method on the `Semaphore` interface.

**Code Review — Scope Assessment:** ⚠️ **Partially appropriate, but has real problems.**

The intent is correct: a single coherent scenario that exercises the full
interface. However several specific issues need to be addressed.

**Issue 14 — The `total == 18` assertion is fragile and likely wrong.**

The comment says `4 premium + 6 standard + 5 fast + 1 tryAcquire + 1 tryAcquireN + 1 tryAcquireNWith = 18`, which adds to 18 correctly. However the goroutine count is
actually 4 + 6 + 5 + 1 + 1 + 1 + 1 (SetCap goroutine) = 19 goroutines, but the
SetCap goroutine does not touch accepted/rejected, so total should still be 18.
The assertion is arithmetically correct but the comment miscounts the goroutines,
which will confuse future readers. The SetCap goroutine should be clearly separated
in the comment.

More critically: `total == 18` asserts that every goroutine increments either
`accepted` or `rejected` exactly once. This is true by construction if every code
path through each goroutine hits exactly one of the two counters. A careful reading
confirms this is the case. **The assertion is correct.**

**Issue 15 — The `SetCap` goroutine calls `t.Errorf` which is not safe from a
non-test goroutine after the test function returns.**

If `wg.Wait()` returns before the `SetCap` goroutine's `time.Sleep(15ms)` has
elapsed and `SetCap` is called, and the test then proceeds to `t.Errorf` from
within the goroutine while the test is already finishing, this triggers a panic:
`testing: t.Errorf called after test finished`.

In practice `wg.Wait()` blocks until all goroutines including the SetCap goroutine
complete (because it calls `wg.Done()` after `SetCap`), so this is safe. But it
is worth noting as a pattern to be careful with.

**Issue 16 — The advanced test is doing too much for a single test.**

As noted in your original brief, this test crosses from "complex" into "scope
creep" territory. The `Drain`, `Reset`, and `Wait`-for-shutdown sequence appended
after `wg.Wait()` makes the test read as two tests concatenated: the concurrent
gateway scenario and a lifecycle management scenario. These are both valid and
important to test, but they should ideally be separate tests:

- `TestApp_ConcurrencyAdvanced_APIGatewayRateLimiter` — the concurrent path only
- `TestApp_Lifecycle_DrainResetWait` — the maintenance lifecycle path

Splitting them makes failures easier to diagnose: if `Drain` is broken, it shows
up clearly rather than being buried in gateway output.

---

## Compilation Blockers

Before running `go test ./...`, the following must be resolved:

| # | Location | Issue | Fix |
|---|---|---|---|
| B1 | `BenchmarkAcquireRelease_Parallel` | `runtime_GOMAXPROCS()` returns constant 8, not real value | Replace with `runtime.GOMAXPROCS(0)` or add `import "runtime"` |
| B2 | `TestErrorStrings` | `contains` field is populated but never used in assertions | Add `strings.Contains` assertion or remove the field |
| B3 | `TestApp_Concurrency_WorkerPool` | `pool` declared twice | Remove first `New(maxWorkers)` call |

None of these are compile errors — the file will compile. But B2 and B3 are
logical errors that make tests pass vacuously or wastefully.

---

## Missing Tests (Recommended Additions)

| Priority | Missing Test | Reason |
|---|---|---|
| High | `TestObserver_TryAcquire_Called` | `TryAcquire` calls `notify` — not verified |
| High | `TestObserver_AcquireWith_Called` | `AcquireWith` calls `notify` — not verified |
| High | `TestObserver_TryAcquireNWith_Called` | `TryAcquireNWith` calls `notify` — not verified |
| High | `TestTryAcquireN_ObserverNotCalled` | `TryAcquireN` does NOT call `notify` — this is an implementation gap worth a failing test to drive the fix |
| Medium | `TestUtilizationSmoothed_MovesAfterRelease` | Replace weak EWMA test with one that verifies actual movement |
| Medium | `TestSetCap_Shrink_LenIsZero` | Verify `Len() == 0` after shrink drain |
| Medium | `FuzzConcurrentAcquireRelease` | Fuzz concurrent access patterns |
| Low | `TestApp_Lifecycle_DrainResetWait` | Split lifecycle test out of App Test 3 |

---

## Summary

The test suite is comprehensive in breadth — every interface method has at least
one unit test, fuzz target, and benchmark. The application tests demonstrate
realistic usage clearly. The core correctness guarantees (blocking behavior,
rollback on cancel, error type contracts) are well-covered.

The issues identified range from a vacuous assertion (`TestErrorStrings`) to a
missing observer coverage gap for `TryAcquireN` that reveals a genuine
implementation defect. Addressing the compilation blockers and the high-priority
missing tests will produce a suite that can be confidently used as both a
correctness gate and a reference for users of the package.