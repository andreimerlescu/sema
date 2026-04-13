# sema — Semaphore for Go

> The semaphore you wish the standard library had — with dynamic resizing,
> context cancellation, graceful drain/reset, EWMA utilization tracking, and
> observability hooks your production system actually needs.

[![Go Reference](https://pkg.go.dev/badge/github.com/andreimerlescu/sema.svg)](https://pkg.go.dev/github.com/andreimerlescu/sema)
[![Go Report Card](https://goreportcard.com/badge/github.com/andreimerlescu/sema)](https://goreportcard.com/report/github.com/andreimerlescu/sema)
[![Apache 2.0 License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

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
file changes. The semaphore resizes live, preserving in-flight work when
expanding and draining when shrinking.

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
    Drain() error                              // forcibly empty all slots
    Reset() error                              // replace channel; preserves Cap
    SetCap(c int) error                        // resize at runtime

    // ── Introspection ───────────────────────────────────────────────────
    Len() int                                  // current occupancy
    Cap() int                                  // current capacity
    Utilization() float64                      // Len/Cap snapshot
    UtilizationSmoothed() float64              // EWMA of Len/Cap over time
    IsEmpty() bool
    IsFull() bool
}
```

---

## Constructors

```go
// New returns a Semaphore with capacity c.
// Pass -1 to use the default capacity (17).
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

### Dynamic resize (config reload)

Adjust capacity at runtime without restarting the process.

```go
func onConfigReload(newWorkerCount int) error {
    return sem.SetCap(newWorkerCount)
}
```

> **Expanding** (new cap ≥ current occupancy): existing slots are preserved
> and the channel grows.
> **Shrinking** (new cap < current occupancy): all slots are drained first,
> then the channel shrinks. Plan accordingly.

### Maintenance window

Drain in-flight work, verify idle, perform maintenance, then resume.

```go
// 1. Signal no new work should start (application-level flag, not shown).
// 2. Wait for all current slots to drain — or force it.
if err := sem.Drain(); err != nil {
    return err
}

// 3. Confirm idle before touching shared resources.
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
if err := sem.Wait(ctx); err != nil {
    return fmt.Errorf("timed out waiting for idle: %w", err)
}

// 4. Perform maintenance.
rotateLogs()

// 5. Reset to a guaranteed-clean state and resume.
sem.Reset()
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

Benchmarks on Apple M3 Ultra (28 cores), Go 1.21, `go test -bench=. -benchmem`:

| Operation | ns/op | allocs/op |
|---|---:|---:|
| Acquire + Release | 30 | 0 |
| TryAcquire + Release | 32 | 0 |
| AcquireWith + Release | 33 | 0 |
| AcquireN + ReleaseN | 38 | 0 |
| Acquire + Release (parallel, 28 cores) | 173 | 0 |
| Acquire + Release with Observer | 40 | 0 |
| Len | 2.2 | 0 |
| Cap | 1.6 | 0 |
| Utilization | 3.0 | 0 |
| UtilizationSmoothed | 0.9 | 0 |

The hot path (acquire/release) is zero-allocation. Observer dispatch adds ~10ns
when attached and zero cost when no observer is registered. Introspection methods
(`Len`, `Cap`, `Utilization`, `UtilizationSmoothed`) are lock-free atomic reads.

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

### Channel-based core

The semaphore is backed by a buffered `chan struct{}`. Acquiring a slot sends
to the channel; releasing receives from it. This delegates scheduling to the
Go runtime's existing channel machinery — no spin loops, no custom queues.

### Atomic EWMA

`UtilizationSmoothed()` is updated on every `Release` / `ReleaseN` using a
compare-and-swap loop on an `atomic.Uint64` storing the [IEEE 754](https://standards.ieee.org/ieee/754/6210/) bits of a
`float64`. The smoothing factor `α = 0.1` means recent activity is weighted
lightly, providing a stable trend signal for dashboards and autoscalers.

### SetCap safety

`SetCap` holds the mutex for the full duration of the channel swap. Goroutines
blocked on `Acquire` will unblock via `cond.Broadcast()` after the swap
completes and will find the new channel. Expanding preserves current occupancy;
shrinking drains first. There is no "safe resize while goroutines are mid-acquire"
— plan maintenance windows accordingly using `Wait` or `Drain`.

### Zero observer overhead

When no observer is registered (`NewWithObserver` was not used), the `notify`
call resolves to a nil check and returns. There is no interface dispatch, no
allocation, and no additional branch in the hot path.

---

## Testing

The package ships with comprehensive test coverage across four categories:

**Unit tests** — every interface method, every error type, every edge case.
Constructor validation, blocking behavior, context cancellation mid-wait,
partial-acquire rollback, observer emission on every code path, EWMA
correctness, and `SetCap` channel-swap survival.

**Fuzz tests** — 8 fuzz targets including `FuzzConcurrentAcquireRelease` and
`FuzzConcurrentMixedOps` that race acquires against releases under randomized
parameters to verify invariants (`Len ≤ Cap`, `Len ≥ 0`, no slot leaks).

**Benchmark tests** — per-operation throughput for every method, parallel
contention under `GOMAXPROCS` cores, and isolated observer overhead
measurement.

**Integration tests** — multi-phase lifecycle scenarios covering
drain → wait → reset → resize → resume sequences and context cancellation
during drain.

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