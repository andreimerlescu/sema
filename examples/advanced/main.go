// Package main demonstrates every method on the sema.Semaphore interface
// in a simulated API gateway scenario. Each phase exercises a different
// part of the API, building into a complete lifecycle from startup through
// graceful shutdown.
package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andreimerlescu/sema"
)

// --- Observer ---

// gatewayObserver collects basic telemetry from the semaphore. All methods
// return immediately as required by the Observer contract.
type gatewayObserver struct {
	acquires   atomic.Int64
	releases   atomic.Int64
	waits      atomic.Int64
	waitErrors atomic.Int64
}

func (o *gatewayObserver) OnAcquire(count, cap int) {
	o.acquires.Add(1)
}

func (o *gatewayObserver) OnRelease(count, cap int) {
	o.releases.Add(1)
}

func (o *gatewayObserver) OnWaitStart() {
	o.waits.Add(1)
}

func (o *gatewayObserver) OnWaitEnd(err error) {
	if err != nil {
		o.waitErrors.Add(1)
	}
}

func (o *gatewayObserver) snapshot() (acquires, releases, waits, waitErrors int64) {
	return o.acquires.Load(), o.releases.Load(), o.waits.Load(), o.waitErrors.Load()
}

// --- Gateway ---

// Gateway wraps a semaphore to simulate an API gateway with rate limiting,
// health checks, batch processing, dynamic scaling, and graceful shutdown.
type Gateway struct {
	sem sema.Semaphore
	obs *gatewayObserver
}

// NewGateway creates a gateway with the given concurrency limit and an
// attached observer for telemetry.
func NewGateway(maxConcurrent int) (*Gateway, error) {
	obs := &gatewayObserver{}
	sem, err := sema.NewWithObserver(maxConcurrent, obs)
	if err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	return &Gateway{sem: sem, obs: obs}, nil
}

// HandleRequest simulates a standard request that blocks until a slot is
// available or the context expires. Returns true if the request was served.
func (g *Gateway) HandleRequest(ctx context.Context, work func()) bool {
	if err := g.sem.AcquireWith(ctx); err != nil {
		return false
	}
	defer g.sem.Release()
	work()
	return true
}

// HandlePremiumRequest claims multiple slots for a high-priority request.
// Returns true if the request was served.
func (g *Gateway) HandlePremiumRequest(ctx context.Context, weight int, work func()) bool {
	if err := g.sem.AcquireNWith(ctx, weight); err != nil {
		return false
	}
	defer g.sem.ReleaseN(weight)
	work()
	return true
}

// HealthCheck performs a non-blocking health probe. Returns "ok" if the
// gateway has capacity, "degraded" if it is full.
func (g *Gateway) HealthCheck() string {
	if !g.sem.TryAcquire() {
		return "degraded"
	}
	defer g.sem.Release()
	return "ok"
}

// HealthCheckWith performs a context-aware non-blocking health probe.
func (g *Gateway) HealthCheckWith(ctx context.Context) (string, error) {
	if err := g.sem.TryAcquireWith(ctx); err != nil {
		return "degraded", err
	}
	defer g.sem.Release()
	return "ok", nil
}

// BatchAcquire claims n slots with a hard timeout for batch processing.
// Returns true if the slots were acquired.
func (g *Gateway) BatchAcquire(n int, timeout time.Duration) bool {
	if err := g.sem.AcquireNTimeout(n, timeout); err != nil {
		return false
	}
	return true
}

// BatchRelease releases n slots after batch processing.
func (g *Gateway) BatchRelease(n int) error {
	return g.sem.ReleaseN(n)
}

// TimeoutAcquire claims a single slot with a hard timeout.
func (g *Gateway) TimeoutAcquire(timeout time.Duration) bool {
	if err := g.sem.AcquireTimeout(timeout); err != nil {
		return false
	}
	return true
}

// TimeoutRelease releases a single slot.
func (g *Gateway) TimeoutRelease() error {
	return g.sem.Release()
}

// TryBatch attempts a non-blocking batch acquire of n slots.
func (g *Gateway) TryBatch(n int) bool {
	return g.sem.TryAcquireN(n)
}

// TryBatchWith attempts a context-aware non-blocking batch acquire.
func (g *Gateway) TryBatchWith(ctx context.Context, n int) error {
	return g.sem.TryAcquireNWith(ctx, n)
}

// Stats returns a snapshot of the gateway's current state.
func (g *Gateway) Stats() (length, capacity int, util, smoothed float64, empty, full bool) {
	return g.sem.Len(), g.sem.Cap(),
		g.sem.Utilization(), g.sem.UtilizationSmoothed(),
		g.sem.IsEmpty(), g.sem.IsFull()
}

// Resize changes the gateway's concurrency limit at runtime.
func (g *Gateway) Resize(newCap int) error {
	return g.sem.SetCap(newCap)
}

// WaitIdle blocks until all in-flight requests complete or the context expires.
func (g *Gateway) WaitIdle(ctx context.Context) error {
	return g.sem.Wait(ctx)
}

// ForceDrain forcibly removes all in-flight slots.
func (g *Gateway) ForceDrain() error {
	return g.sem.Drain()
}

// HardReset replaces the internal channel, discarding all state.
func (g *Gateway) HardReset() error {
	return g.sem.Reset()
}

// ObserverSnapshot returns the observer's current counters.
func (g *Gateway) ObserverSnapshot() (acquires, releases, waits, waitErrors int64) {
	return g.obs.snapshot()
}

// --- Main ---

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	// ── Phase 1: Startup ────────────────────────────────────────────────
	log.Println("=== Phase 1: Startup ===")

	gw, err := NewGateway(8)
	if err != nil {
		return err
	}

	// Demonstrate Must for a background worker pool.
	bgPool := sema.Must(4)
	log.Printf("gateway cap=%d, background pool cap=%d", gw.sem.Cap(), bgPool.Cap())

	// ── Phase 2: Handle Requests ────────────────────────────────────────
	log.Println("=== Phase 2: Handle Requests ===")

	var served, rejected atomic.Int32
	var wg sync.WaitGroup

	// Launch 12 standard requests against a cap=8 gateway.
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			ok := gw.HandleRequest(ctx, func() {
				time.Sleep(50 * time.Millisecond)
			})
			if ok {
				served.Add(1)
			} else {
				rejected.Add(1)
			}
		}(i)
	}

	// Launch 2 premium requests (weight=3 each).
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			ok := gw.HandlePremiumRequest(ctx, 3, func() {
				time.Sleep(30 * time.Millisecond)
			})
			if ok {
				served.Add(1)
			} else {
				rejected.Add(1)
			}
		}(i)
	}

	wg.Wait()
	log.Printf("requests: served=%d rejected=%d", served.Load(), rejected.Load())

	// ── Phase 3: Non-Blocking Fast Path ─────────────────────────────────
	log.Println("=== Phase 3: Non-Blocking Fast Path ===")

	// Health check when gateway is idle.
	status := gw.HealthCheck()
	log.Printf("health (idle): %s", status)

	// Health check with context.
	statusCtx, err := gw.HealthCheckWith(context.Background())
	log.Printf("health with ctx: %s (err=%v)", statusCtx, err)

	// TryAcquireN — batch non-blocking.
	if gw.TryBatch(3) {
		log.Printf("tryBatch(3): acquired, len=%d", gw.sem.Len())
		gw.BatchRelease(3)
	}

	// TryAcquireNWith — batch non-blocking with context.
	if err := gw.TryBatchWith(context.Background(), 2); err == nil {
		log.Printf("tryBatchWith(2): acquired, len=%d", gw.sem.Len())
		gw.BatchRelease(2)
	}

	// ── Phase 4: Timeout Acquisition ────────────────────────────────────
	log.Println("=== Phase 4: Timeout Acquisition ===")

	if gw.TimeoutAcquire(200 * time.Millisecond) {
		log.Println("timeout acquire: success")
		gw.TimeoutRelease()
	}

	if gw.BatchAcquire(2, 200*time.Millisecond) {
		log.Println("batch timeout acquire(2): success")
		gw.BatchRelease(2)
	}

	// ── Phase 5: Introspection ──────────────────────────────────────────
	log.Println("=== Phase 5: Introspection ===")

	l, c, u, s, empty, full := gw.Stats()
	log.Printf("stats: len=%d cap=%d util=%.2f smoothed=%.4f empty=%v full=%v",
		l, c, u, s, empty, full)

	acq, rel, waits, wErr := gw.ObserverSnapshot()
	log.Printf("observer: acquires=%d releases=%d waits=%d waitErrors=%d",
		acq, rel, waits, wErr)

	// ── Phase 6: Dynamic Scaling ────────────────────────────────────────
	log.Println("=== Phase 6: Dynamic Scaling ===")

	log.Printf("before resize: cap=%d", gw.sem.Cap())
	if err := gw.Resize(16); err != nil {
		return fmt.Errorf("resize: %w", err)
	}
	log.Printf("after resize: cap=%d", gw.sem.Cap())

	// Resize with -1 to reset to default capacity.
	if err := gw.Resize(-1); err != nil {
		return fmt.Errorf("resize default: %w", err)
	}
	log.Printf("after default resize: cap=%d", gw.sem.Cap())

	// ── Phase 7: Graceful Shutdown ──────────────────────────────────────
	log.Println("=== Phase 7: Graceful Shutdown ===")

	// Simulate some in-flight work.
	gw.sem.Acquire()
	gw.sem.Acquire()
	log.Printf("pre-shutdown: len=%d", gw.sem.Len())

	// Try Wait with a short deadline — will fail because slots are held.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shortCancel()
	if err := gw.WaitIdle(shortCtx); err != nil {
		log.Printf("wait timed out (expected): %v", err)
	}

	// Force drain the remaining slots.
	if err := gw.ForceDrain(); err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	log.Printf("after drain: len=%d empty=%v", gw.sem.Len(), gw.sem.IsEmpty())

	// Hard reset to clean state.
	if err := gw.HardReset(); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	log.Printf("after reset: len=%d cap=%d", gw.sem.Len(), gw.sem.Cap())

	// Final idle confirmation.
	if err := gw.WaitIdle(context.Background()); err != nil {
		return fmt.Errorf("final wait: %w", err)
	}
	log.Println("shutdown complete")

	// Use the background pool to prove Must worked.
	bgPool.Acquire()
	bgPool.Release()
	log.Printf("background pool verified: cap=%d", bgPool.Cap())

	return nil
}
