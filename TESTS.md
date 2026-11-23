# Test Suite Architecture

All tests run cleanly under Go's race detector (`go test -race ./...`) with zero
external dependencies.

## File Layout

| File | Scope |
|---|---|
| `sema_test.go` | Core `Semaphore` unit tests (constructors with `defaultCap = 10`, single/multi-slot acquire & release, introspection, EWMA, non-draining `SetCap` shrink/expand, `Drain`/`Reset`/`Wait`, observer positive & negative emission, error `Is`/`Unwrap`/`Error`), goroutine-leak checks, 9 fuzz targets, and core benchmarks. |
| `priority_test.go` | `PrioritySemaphore` unit tests (`NewPriority`, type assertion from `New`/`NewWithObserver`/`Must`, strict `Critical > High > Normal > Low` + intra-tier FIFO ordering, higher-priority bypass of blocked multi-slot waiters, `HighestWaitingPriority` transitions, cooperative yielding, observer emission), `FuzzPrioritySetCapShrink`, and priority/parked benchmarks. |
| `lifecycle_test.go` | Structural invariant checker (`assertInternalInvariants`), `math.MaxInt` signed-overflow boundaries (`TestOverflow_MaxIntBoundaries`), parked/unparked FIFO `seq` preservation (`TestSetCap_ParkedWaiterSeqOrder`), Wing & Gong memoized DFS linearizability checker (`TestLifecycle_Linearizability`), `O(1)` parked-waiter latency test (`TestLifecycle_BoundedLatencyWithParkedWaiters`), and shrink → shed → recover → `Wait` lifecycle (`TestLifecycle_ShrinkShedRecoverWait`). |

## Core Invariants Verified

1. **Slot conservation & strict ceilings:** Active holders never exceed the
   configured ceiling; shrinking `SetCap` below `Len()` preserves held slots
   without draining, blocks new acquisitions while `Len() >= Cap()`, and lets
   in-flight holders release without `ErrReleaseExceedsCount`.
2. **Overflow safety:** Capacity arithmetic uses subtraction (`held <= cap && n <= cap - held`), verified at `math.MaxInt` boundaries.
3. **Structural queue integrity (`assertInternalInvariants`):**
   - `held >= 0`, `cap >= 1`, and atomic mirrors (`heldAtom`, `capAtom`, `maskAtom`) match mutex-guarded state.
   - Each bit in `activeMask` matches non-emptiness of `queues[i]`; doubly-linked links and `count` are consistent; arrival `seq` is strictly ascending within every tier.
   - Every active waiter satisfies `1 <= w.n <= cap && !w.parked`; every parked waiter satisfies `w.n > cap && w.parked`; `multiActive` matches the exact count of active waiters with `w.n > 1`.
4. **Linearizability (`TestLifecycle_Linearizability`):** Records concurrent
   `(call, return, op, result)` intervals across `TryAcquire`,
   `TryAcquireWithPriority`, `AcquireWithPriority`, `Release`, and `SetCap`,
   verifying via Wing & Gong memoized DFS that every history matches a valid
   sequential `(held, cap)` execution (plus a negative self-test ensuring
   impossible histories are rejected).
5. **Bounded `O(1)` hot path & zero goroutine leaks:** `Acquire*`, `TryAcquire*`,
   `Release*`, and `Wait` spawn zero helper goroutines and never scan parked
   oversized waiters.
6. **Metric & error bounds:** `Utilization()` and `UtilizationSmoothed()` stay
   non-NaN in `[0.0, 1.0]`, and `ErrNoSlot.Available` clamps at `>= 0`.

## Fuzz Targets (`make test-fuzz`)

All 10 fuzz targets run in CI (`.github/workflows/go.yml`) and via `Makefile`:

| Target | Focus |
|---|---|
| `FuzzNew` | Capacity validation (`-1`, `0`, negative, positive) |
| `FuzzAcquireN` | `ErrInvalidN`, `ErrNExceedsCap`, and valid bulk acquire |
| `FuzzReleaseN` | `ErrInvalidN`, `ErrReleaseExceedsCount`, and valid bulk release |
| `FuzzSetCap` | Valid, default `-1`, and invalid capacity transitions |
| `FuzzTryAcquireN` | Atomic all-or-nothing non-blocking bulk acquire |
| `FuzzUtilization` | `[0.0, 1.0]` metric bounds |
| `FuzzConcurrentAcquireRelease` | Fixed-cap concurrent `AcquireNWith` / `ReleaseN` |
| `FuzzConcurrentMixedOps` | Fixed-cap concurrent `TryAcquire` / `AcquireWith` / `Release` |
| `FuzzConcurrentSetCapShrink` | Concurrent acquire/release + dynamic `SetCap` shrink/expand |
| `FuzzPrioritySetCapShrink` | Multi-tier priority acquire/try + dynamic `SetCap` shrink/expand + structural invariants |

## Benchmarks (`make test-bench`)

Every uncontended hot path (`Acquire`, `TryAcquire`, `AcquireWith`,
`AcquireWithPriority`, `AcquireN`, `ReleaseN`, `SetCap`, `Drain`, `Reset`,
`Wait`), lock-free introspection (`Len`, `Cap`, `Utilization`,
`UtilizationSmoothed`, `HighestWaitingPriority`), parallel contention, observer
dispatch, and `BenchmarkAcquireRelease_WithParkedWaiters` (1,000 parked
oversized waiters) must stay at **`0 B/op, 0 allocs/op`**.

## Running Locally

```shell
go test -race -count=1 -v ./...
make test-bench
FUZZTIME=15s make test-fuzz
```
