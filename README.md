# sema — Semaphore for Go

> The semaphore you wish the standard library had — with dynamic resizing,
> context cancellation, graceful drain/reset, EWMA utilization tracking, and
> observability hooks your production system actually needs.

[![Go Reference](https://pkg.go.dev/badge/github.com/andreimerlescu/sema.svg)](https://pkg.go.dev/github.com/andreimerlescu/sema)
[![Apache 2.0 License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
![Unique](https://counthis.app/github?repository=andreimerlescu/sema&label=Unique) ![Visitors](https://counthis.app/g/qyjy5vwjtjgbjfzy)

---

## Why sema?

You've hit the point where `golang.org/x/sync/semaphore` isn't enough. You
need to resize capacity at runtime without restarting. You need to drain
in-flight work for a maintenance window. You need utilization metrics without
bolting on a separate monitoring layer. You need `Wait` to know when the
semaphore is idle so you can shut down cleanly.

`sema` is a drop-in semaphore that handles all of this out of the box.

### Feature comparison

| Feature | `x/sync/semaphore` | `marusama/semaphore` | **sema** |
|---|:---:|:---:|:---:|
| Weighted acquire / release | ✅ | ✅ | ✅ |
| Context cancellation | ✅ | ✅ | ✅ |
| Non-blocking try-acquire | ✅ | ✅ | ✅ |
| Dedicated timeout acquire | — | — | ✅ |
| Dynamic capacity resize | — | ✅ | ✅ |
| O(1) priority tiers (`PrioritySemaphore`) | — | — | ✅ |
| Drain / Reset for maintenance | — | — | ✅ |
| Wait until idle | — | — | ✅ |
| Instant utilization (`Len/Cap`) | — | — | ✅ |
| Smoothed utilization (EWMA) | — | — | ✅ |
| Observer / metrics hook | — | — | ✅ |
| Structured error types | — | — | ✅ |
| Zero-alloc hot path | ✅ | — | ✅ |

`x/sync/semaphore` is excellent for simple bounded concurrency.
`marusama/semaphore` adds resizing via CAS. `sema` goes further: lifecycle
management (`Drain`, `Reset`, `Wait`), real-time utilization tracking, and a
pluggable observer that wires directly into Prometheus, OpenTelemetry, or
structured logging — without polling, without a metrics goroutine, and without
any overhead when no observer is attached.

---

## When to use sema

**HTTP request throttling** — cap concurrent handler goroutines per endpoint
and return 429 instantly via `TryAcquire` when the server is saturated.

**Database connection limiting** — wrap your connection pool with a semaphore
so burst traffic queues gracefully instead of hammering the database with
rejected connections.

**Fan-out control in ETL pipelines** — use `AcquireNWith` to give
resource-heavy transforms weighted slots while lightweight stages get single
slots, all sharing one concurrency budget.

**Graceful shutdown** — call `Wait` to block until all in-flight work finishes,
then `Drain` and `Reset` for a clean restart. No leaked goroutines, no
abandoned connections.

**Config-driven concurrency with hot reload** — call `SetCap` when your config
file changes. The semaphore resizes live without draining in-flight work:
expanding immediately dispatches queued waiters, and shrinking lets existing
holders finish and release cleanly while holding back new acquires until
occupancy drops below the new ceiling.

**Autoscaler feedback loop** — feed `UtilizationSmoothed()` into your scaling
logic. The EWMA smooths out bursts so your autoscaler doesn't thrash on
transient spikes.

---

## Installation

```shell
go get github.com/andreimerlescu/sema
```

Requires **Go 1.21+** (uses `sync/atomic` generic types).

---

## Quick start

```go
package main

import (
    "fmt"
    "sync"

    "github.com/andreimerlescu/sema"
)

func main() {
    // Allow at most 5 concurrent workers.
    sem := sema.Must(5)

    var wg sync.WaitGroup
    for i := 0; i < 20; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()

            sem.Acquire()          // block until a slot is free
            defer sem.Release()    // always give the slot back

            fmt.Printf("worker %d running (%d/%d slots used)\n",
                id, sem.Len(), sem.Cap())
        }(i)
    }

    wg.Wait()
    fmt.Printf("done — semaphore empty: %v\n", sem.IsEmpty())
}
```

---

## The full interface

Every method is safe for concurrent use.

```go
type Semaphore interface {
    // ── Single-slot ─────────────────────────────────────────────────────
    Acquire()                                  // block until a slot is free
    AcquireWith(ctx context.Context) error     // block, honour context
    AcquireTimeout(d time.Duration) error      // block, honour deadline
    TryAcquire() bool                          // succeed or return false immediately
    TryAcquireWith(ctx context.Context) error  // succeed or return ErrNoSlot / ErrAcquireCancelled

    Release() error                            // free one slot

    // ── Multi-slot ──────────────────────────────────────────────────────
    AcquireN(n int) error
    AcquireNWith(ctx context.Context, n int) error
    AcquireNTimeout(n int, d time.Duration) error
    TryAcquireN(n int) bool
    TryAcquireNWith(ctx context.Context, n int) error

    ReleaseN(n int) error                      // free n slots atomically

    // ── Lifecycle ───────────────────────────────────────────────────────
    Wait(ctx context.Context) error            // block until Len() == 0
    Drain() error                              // forcibly clear all held slots
    Reset() error                              // forcibly clear slots; preserves Cap
    SetCap(c int) error                        // non-draining resize at runtime

    // ── Introspection ───────────────────────────────────────────────────
    Len() int                                  // current occupancy
    Cap() int                                  // current capacity
    Utilization() float64                      // clamped Len/Cap snapshot in [0.0, 1.0]
    UtilizationSmoothed() float64              // EWMA of clamped Len/Cap in [0.0, 1.0]
    IsEmpty() bool
    IsFull() bool                              // true when Len() >= Cap()
}
```

---

## Constructors

```go
// New returns a Semaphore with capacity c.
// Pass -1 to use the default capacity (10).
// Returns ErrInvalidCap for c == 0 or c < -1.
s, err := sema.New(10)

// Must panics instead of returning an error.
// Safe for package-level var declarations.
s := sema.Must(10)

// NewWithObserver wires a metrics/logging hook into every state change.
s, err := sema.NewWithObserver(10, myObserver)
```

---

## Recipes

### Worker pool

The most common pattern. Exactly `N` goroutines run at any moment.

```go
sem := sema.Must(N)

for _, job := range jobs {
    sem.Acquire()
    go func(j Job) {
        defer sem.Release()
        process(j)
    }(job)
}

// Wait for every in-flight goroutine to release its slot.
ctx := context.Background()
sem.Wait(ctx)
```

### Context-aware acquire

Cancel or time-out a waiting goroutine without leaking it.

```go
ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
defer cancel()

if err := sem.AcquireWith(ctx); err != nil {
    // errors.Is(err, sema.ErrAcquireCancelled{}) == true
    log.Println("request dropped — semaphore full")
    return
}
defer sem.Release()
```

### Non-blocking fast path

Reject immediately when the semaphore is full, without touching the scheduler.

```go
if !sem.TryAcquire() {
    http.Error(w, "server busy", http.StatusTooManyRequests)
    return
}
defer sem.Release()
serveRequest(w, r)
```

### Premium / burst clients (multi-slot)

Allocate weighted slots for high-priority or resource-intensive operations.

```go
const premiumWeight = 3

ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
defer cancel()

if err := sem.AcquireNWith(ctx, premiumWeight); err != nil {
    http.Error(w, "capacity unavailable", http.StatusServiceUnavailable)
    return
}
defer sem.ReleaseN(premiumWeight)

servePremiumRequest(w, r)
```

### Priority scheduling & cooperative preemption (`PrioritySemaphore`)

`PrioritySemaphore` extends `Semaphore` without breaking `v1.x` compatibility.
Construct one directly via `NewPriority`, or type-assert any existing
`Semaphore` returned by `New`, `NewWithObserver`, or `Must`
(`ps, ok := sem.(sema.PrioritySemaphore)`). Four fixed tiers (`PriorityLow`,
`PriorityNormal`, `PriorityHigh`, `PriorityCritical`) provide strict `O(1)`
bitmask dispatch with FIFO order within each tier.

```go
ps, err := sema.NewPriority(16)
if err != nil {
    return err
}

if err := ps.AcquireWithPriority(ctx, sema.PriorityCritical); err != nil {
    return err
}
defer ps.Release()
```

> **Priority inversion in Go:** Go's goroutine scheduler does not provide OS
> thread priority inheritance on `sync.Mutex`. `PrioritySemaphore` eliminates
> queue-level priority inversion via strict `O(1)` tier dispatch and provides
> lock-free `HighestWaitingPriority()` so long-running `PriorityLow` holders
> can cooperatively checkpoint and yield early:
>
> ```go
> if p, ok := ps.HighestWaitingPriority(); ok && p >= sema.PriorityHigh {
>     return // checkpoint and release slot early for urgent work
> }
> ```

### Dynamic resize (config reload)

Adjust capacity at runtime without restarting the process.

```go
func onConfigReload(newWorkerCount int) error {
    return sem.SetCap(newWorkerCount)
}
```

> **Expanding** (new cap ≥ current occupancy): all held slots are preserved and
> queued waiters are dispatched in FIFO order.
> **Shrinking** (new cap < current occupancy): all held slots are preserved
> without draining. Existing holders finish and call `Release()` cleanly without
> `ErrReleaseExceedsCount`, while new acquires block (or fail for `Try*`) until
> occupancy drops below the new capacity. While oversubscribed (`Len() > Cap()`),
> `IsFull()` returns `true` and `Utilization()` / `UtilizationSmoothed()` remain
> clamped to `[0.0, 1.0]`.

### Maintenance window

Wait for in-flight work to finish gracefully, force-drain only if stragglers
exceed the deadline, perform maintenance, then resume.

```go
// 1. Stop dispatching new work (application-level flag, not shown).

// 2. Wait for in-flight holders to release gracefully.
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
if err := sem.Wait(ctx); err != nil {
    // 3. Timed out — force-clear any remaining stragglers.
    _ = sem.Drain()
}

// 4. Perform maintenance.
rotateLogs()
```

### Utilization monitoring

Feed semaphore metrics into your observability stack without adding lock
contention to the hot path.

```go
// Snapshot — suitable for a Prometheus gauge collector.
util := sem.Utilization()

// Exponentially weighted moving average — suitable for dashboards
// and alerting. Smooths out short bursts automatically.
smooth := sem.UtilizationSmoothed()

metrics.GaugeSet("worker_pool.utilization", util)
metrics.GaugeSet("worker_pool.utilization_smoothed", smooth)
```

### Observer — structured metrics hook

`Observer` lets you attach counters, histograms, or structured logs to every
semaphore event without polling.

```go
type prometheusObserver struct {
    acquireTotal prometheus.Counter
    releaseTotal prometheus.Counter
    waitDuration prometheus.Histogram
    waitStart    time.Time
}

func (o *prometheusObserver) OnAcquire(count, cap int) {
    o.acquireTotal.Inc()
}
func (o *prometheusObserver) OnRelease(count, cap int) {
    o.releaseTotal.Inc()
}
func (o *prometheusObserver) OnWaitStart() {
    o.waitStart = time.Now()
}
func (o *prometheusObserver) OnWaitEnd(err error) {
    o.waitDuration.Observe(time.Since(o.waitStart).Seconds())
}

sem, err := sema.NewWithObserver(10, &prometheusObserver{
    acquireTotal: promauto.NewCounter(prometheus.CounterOpts{
        Name: "sema_acquire_total",
    }),
    releaseTotal: promauto.NewCounter(prometheus.CounterOpts{
        Name: "sema_release_total",
    }),
    waitDuration: promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "sema_wait_duration_seconds",
        Buckets: prometheus.DefBuckets,
    }),
})
```

> **Observer contract:** every method must return immediately. Never acquire
> a lock inside an observer method — it is called while the semaphore's
> internal state is being updated.

---

## Performance

Benchmarks on Apple Silicon (`darwin/arm64`, 14 cores), Go 1.24, `make test-bench`:

| Operation | ns/op | allocs/op |
|---|---:|---:|
| Acquire + Release | 8.2 | 0 |
| Acquire + Release (1,000 parked oversized waiters) | 8.3 | 0 |
| TryAcquire + Release | 9.3 | 0 |
| AcquireWithPriority + Release | 9.5 | 0 |
| AcquireWith + Release | 10.1 | 0 |
| AcquireN + ReleaseN | 14.3 | 0 |
| Acquire + Release (parallel, 14 cores) | 78.2 | 0 |
| Acquire + Release with Observer | 29.0 | 0 |
| SetCap | 11.0 | 0 |
| HighestWaitingPriority | 0.7 | 0 |
| Len | 1.6 | 0 |
| Cap | 1.4 | 0 |
| Utilization | 2.2 | 0 |
| UtilizationSmoothed | 1.2 | 0 |

All uncontended hot paths, parallel acquire/release, `SetCap`, `Drain`, `Reset`,
`Wait`, and lock-free introspection methods run with **zero heap allocations**.

---

## Error reference

All errors implement `errors.Is` with type-only matching, so you never need
to compare field values:

```go
err := sem.AcquireWith(ctx)
if errors.Is(err, sema.ErrAcquireCancelled{}) {
    // context was cancelled or deadline exceeded
}
```

| Error | When returned |
|---|---|
| `ErrInvalidCap` | `New` or `SetCap` called with `c == 0` or `c < -1` |
| `ErrInvalidN` | Any `*N` method called with `n < 1` |
| `ErrNExceedsCap` | `AcquireN` / `AcquireNWith` / `TryAcquireNWith` with `n > Cap()` |
| `ErrNoSlot` | `TryAcquireWith` / `TryAcquireNWith` when no slot is immediately available |
| `ErrAcquireCancelled` | Any `*With` or `*Timeout` method when the context expires or is cancelled |
| `ErrReleaseExceedsCount` | `Release` / `ReleaseN` called more times than `Acquire` |
| `ErrDrain` | Internal invariant failure during `Drain` (indicates a bug — please open an issue) |
| `ErrRecovered` | A panic was recovered inside `AcquireN` — wraps the original panic value |

`ErrAcquireCancelled` and `ErrRecovered` both implement `Unwrap()`, so
`errors.Is(err, context.DeadlineExceeded)` works as expected through the chain.

---

## Design notes

### Counter core, overflow safety & `O(1)` priority waiter queues

The semaphore tracks occupancy (`held`) and capacity (`cap`) as mutex-guarded
integers mirrored to `atomic.Int64` fields for lock-free introspection (`Len`,
`Cap`, `IsEmpty`, `IsFull`, `Utilization`). All capacity checks use
subtraction (`held <= cap && n <= cap - held`), preventing signed integer
overflow even when `Cap()` or `n` reaches `math.MaxInt`.

Blocking acquires register an intrusive waiter node into one of four fixed
priority-tier FIFO queues (`[4]waiterQueue`) indexed by a 4-bit `activeMask`
(mirrored to `maskAtom` for lock-free `HighestWaitingPriority()`). Admission,
cancellation, and dispatch run in strict **`O(1)`** via `bits.LeadingZeros32`,
spawning **zero helper goroutines** and eliminating missed wakeups.

Multi-slot acquires (`AcquireN`, `AcquireNWith`, `AcquireNWithPriority`) wait
until all `n` slots are available and claim them atomically, eliminating
partial-hold deadlocks between competing callers. `Wait(ctx)` uses a lazily
allocated broadcast channel (`emptyCh`) closed whenever occupancy hits zero,
also spawning zero helper goroutines even with hundreds of concurrent waiters.

### Atomic EWMA

`UtilizationSmoothed()` is updated under `s.mu` on every `Release`, `ReleaseN`,
and `SetCap` via an atomic store of the [IEEE 754](https://standards.ieee.org/ieee/754/6210/)
bits of a `float64` (so lock-free readers observe consistent values).
Instantaneous utilization is clamped to `[0.0, 1.0]` even while oversubscribed
during a shrink drain. The smoothing factor `α = 0.1` smooths transient bursts
for dashboards and autoscalers.

### Non-draining `SetCap` & `O(1)` parked oversized pool

`SetCap` updates capacity in place under the mutex without draining held slots
or disrupting in-flight holders. When shrinking below current occupancy, `Len()`
temporarily exceeds `Cap()`, `IsFull()` reports `true`, `ErrNoSlot.Available`
clamps at `0`, and no new slots are granted until existing holders release below
the new ceiling.

If a shrink reduces capacity below queued multi-slot waiters (`w.n > cap`),
`SetCap` moves those temporarily unsatisfiable waiters out of the active queues
into a dedicated `parked` pool so active `Acquire`, `TryAcquire`, `Release`,
and `Cancel` calls **never scan parked waiters** and remain strictly `O(1)`.
When capacity later expands back, eligible parked waiters re-enter their tier's
active queue in original arrival (`seq`) order.

### Zero observer overhead

When no observer is registered (`NewWithObserver` was not used), the `notify`
call resolves to a nil check and returns. There is no interface dispatch, no
allocation, and no additional branch in the hot path.

---

## Testing

The package ships with comprehensive test coverage across four categories:

**Unit tests** ([sema_test.go](sema_test.go), [priority_test.go](priority_test.go)) —
every method on `Semaphore` and `PrioritySemaphore`, error matching/unwrapping,
strict priority-tier ordering, cooperative yielding via `HighestWaitingPriority`,
context cancellation mid-wait and during handoff, atomic multi-slot acquisition,
observer positive/negative emission, EWMA accuracy, non-draining `SetCap`
shrink/expand, and zero-goroutine `Wait`.

**Fuzz tests** — 10 fuzz targets including `FuzzConcurrentAcquireRelease`,
`FuzzConcurrentMixedOps`, `FuzzConcurrentSetCapShrink`, and
`FuzzPrioritySetCapShrink` that race multi-tier acquires, releases, and dynamic
shrink/expand resizes to verify invariants (`Utilization` in `[0, 1]`, `Len ≥ 0`,
internal queue/bitmask integrity, zero release errors, and no slot leaks).

**Benchmark tests** — per-operation throughput for every method (including
`AcquireWithPriority`, lock-free `HighestWaitingPriority`, and `AcquireRelease`
with 1,000 parked oversized waiters), parallel contention, and observer overhead.

**Lifecycle & invariant tests** ([lifecycle_test.go](lifecycle_test.go)) —
`math.MaxInt` overflow boundaries, parked/unparked FIFO `seq` preservation,
a Wing & Gong memoized DFS linearizability history checker
(`TestLifecycle_Linearizability`), `O(1)` bounded latency with 1,000 parked
oversized waiters, and end-to-end shrink → shed → recover → `Wait` shutdown.

All tests pass under the race detector (`go test -race ./...`).

```shell
# Unit tests with race detector
go test -race -count=1 -v ./...

# Benchmarks with memory stats
go test -bench=. -benchmem ./...

# Fuzz targets (30 seconds each)
make test-fuzz

# Everything
make test-all
```

The test suite design is documented in [TESTS.md](TESTS.md).

---

## Contributing

Pull requests are welcome. Before opening one:

1. `go test -race ./...` must pass cleanly.
2. New methods require a unit test, a fuzz target, and a benchmark.
3. Observer emission points require a positive and a negative observer test.
4. Update [TESTS.md](TESTS.md) with any new coverage decisions.

---

## License

[![Apache 2.0 License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE) &copy; [Andrei Merlescu](https://github.com/andreimerlescu)

---

*Built with care and a lot of `go test -race`.*
