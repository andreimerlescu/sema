# Advanced Example — API Gateway with sema

This example builds a simulated API gateway that uses every method on the
`sema.Semaphore` interface. It demonstrates how a production service might
use the semaphore for rate limiting, graceful shutdown, dynamic scaling,
and utilization monitoring.

---

## What This Example Covers

The gateway processes incoming requests through a semaphore-controlled
pipeline. Each phase of the program exercises a different slice of the
API, building on the previous phase so the full lifecycle is visible in
a single runnable program.

### Phase 1 — Startup and Basic Acquisition

The gateway creates a semaphore with `New` and wires in an observer with
`NewWithObserver`. It then uses `Must` to create a second semaphore for
a background worker pool, demonstrating the panic-on-failure constructor
for package-level declarations.

```go
gw, err := sema.NewWithObserver(maxConcurrent, &metricsObserver{})
if err != nil {
    log.Fatal(err)
}
bg := sema.Must(4)
```

### Phase 2 — Handling Requests

Standard requests use `AcquireWith` to honour a per-request context
deadline. If the gateway is full, the request is rejected:

```go
ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
defer cancel()

if err := gw.AcquireWith(ctx); err != nil {
    http.Error(w, "busy", 503)
    return
}
defer gw.Release()
```

Premium requests use `AcquireNWith` to claim multiple slots, giving
them a larger share of the gateway's capacity:

```go
if err := gw.AcquireNWith(ctx, premiumWeight); err != nil {
    // rejected
}
defer gw.ReleaseN(premiumWeight)
```

### Phase 3 — Non-Blocking Fast Path

Health checks and internal probes use `TryAcquire` to avoid blocking.
If the gateway is overloaded, the health check reports degraded status
instead of queueing behind real requests:

```go
if !gw.TryAcquire() {
    return HealthDegraded
}
defer gw.Release()
return HealthOK
```

The `TryAcquireWith` variant adds context awareness, and `TryAcquireN` /
`TryAcquireNWith` extend this to multi-slot batch operations.

### Phase 4 — Timeout Acquisition

Background batch jobs use `AcquireTimeout` and `AcquireNTimeout` to claim
slots with a hard deadline. If the deadline passes, the job is skipped
rather than blocking the batch pipeline:

```go
if err := gw.AcquireTimeout(200 * time.Millisecond); err != nil {
    log.Println("batch slot unavailable, skipping")
    return
}
defer gw.Release()
```

### Phase 5 — Introspection and Monitoring

A metrics goroutine periodically samples the semaphore's state and feeds
it into a dashboard:

```go
fmt.Printf("len=%d cap=%d util=%.2f smoothed=%.4f empty=%v full=%v\n",
    gw.Len(), gw.Cap(),
    gw.Utilization(), gw.UtilizationSmoothed(),
    gw.IsEmpty(), gw.IsFull())
```

### Phase 6 — Dynamic Scaling

When a config reload signal arrives, the gateway resizes the semaphore
without restarting:

```go
if err := gw.SetCap(newCap); err != nil {
    log.Printf("resize failed: %v", err)
}
```

### Phase 7 — Graceful Shutdown

The shutdown sequence uses `Wait` to let in-flight requests finish,
`Drain` to force-clear any stragglers, and `Reset` to return the
semaphore to a clean state:

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := gw.Wait(ctx); err != nil {
    log.Println("timed out waiting for idle, forcing drain")
    gw.Drain()
}
gw.Reset()
```

---

## Running

```shell
go run .
```

The program runs through all phases sequentially and prints status at
each step. It exits cleanly after demonstrating the full API.

## Testing

```shell
go test -v -race -cover .
```

The test suite covers every code path in `main.go` and verifies the
semaphore behaves correctly under each scenario.