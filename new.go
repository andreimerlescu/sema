package sema

import (
	"fmt"
	"sync"
)

// New creates a new [Semaphore] with the given capacity c, which defines
// the maximum number of slots that may be held concurrently. The capacity
// can be changed later at runtime via [semaphore.SetCap].
//
// The special value -1 may be passed for c to use the package-level
// [defaultCap]. Any other value less than 1 returns [ErrInvalidCap].
//
// The returned [Semaphore] starts empty (zero slots held) and is
// immediately ready for use. All methods on the returned value are safe
// for concurrent use from multiple goroutines.
//
// # Capacity semantics
//
// The capacity represents the upper bound on concurrent access. When all
// c slots are held, calls to [semaphore.Acquire] block and calls to
// [semaphore.TryAcquire] return false until a slot is freed by
// [semaphore.Release].
//
// # Observability
//
// The semaphore returned by New has no observer attached. Use
// [NewWithObserver] to attach an [Observer] that receives callbacks on
// acquire, release, and wait events.
//
// # Examples
//
// Creating a semaphore with an explicit capacity:
//
//	sem, err := sema.New(10)
//	if err != nil {
//	    log.Fatalf("failed to create semaphore: %v", err)
//	}
//	sem.Acquire()
//	defer sem.Release()
//
// Using the default capacity:
//
//	sem, err := sema.New(-1)
//	if err != nil {
//	    log.Fatalf("failed to create semaphore: %v", err)
//	}
//	fmt.Printf("default capacity: %d\n", sem.Cap())
//
// Handling invalid input:
//
//	sem, err := sema.New(0)
//	if err != nil {
//	    // err is ErrInvalidCap{Value: 0}
//	    var capErr sema.ErrInvalidCap
//	    if errors.As(err, &capErr) {
//	        fmt.Printf("invalid capacity: %d\n", capErr.Value)
//	    }
//	}
func New(c int) (Semaphore, error) {
	if c == -1 {
		c = defaultCap
	}
	if c < 1 {
		return nil, ErrInvalidCap{Value: c}
	}
	s := &semaphore{}
	s.cond = sync.NewCond(&s.mu)
	ch := newChannel(c)
	s.ch.Store(&ch)
	return s, nil
}

// NewWithObserver creates a new [Semaphore] with the given capacity and
// attaches an [Observer] that receives lifecycle callbacks. It is
// otherwise identical to [New] — the same capacity rules, error
// conditions, and concurrency guarantees apply.
//
// The observer is called synchronously under the semaphore's internal
// lock for acquire and release events, so observer implementations
// should be fast and non-blocking to avoid degrading throughput.
//
// # Observer callbacks
//
// The attached [Observer] receives the following notifications:
//
//   - [Observer.OnAcquire](len, cap) — called after a slot is
//     successfully acquired, with the current occupancy and capacity.
//   - [Observer.OnRelease](len, cap) — called after a slot is released.
//   - [Observer.OnWaitStart]() — called when [semaphore.Wait] begins.
//   - [Observer.OnWaitEnd](err) — called when [semaphore.Wait] finishes,
//     with nil on success or the cancellation error.
//
// # Examples
//
// Attaching a metrics observer:
//
//	type metricsObserver struct{}
//
//	func (m metricsObserver) OnAcquire(len, cap int) {
//	    metrics.Gauge("semaphore.utilization").Set(float64(len) / float64(cap))
//	}
//	func (m metricsObserver) OnRelease(len, cap int) {
//	    metrics.Gauge("semaphore.utilization").Set(float64(len) / float64(cap))
//	}
//	func (m metricsObserver) OnWaitStart()        {}
//	func (m metricsObserver) OnWaitEnd(err error)  {}
//
//	sem, err := sema.NewWithObserver(50, metricsObserver{})
//	if err != nil {
//	    log.Fatalf("failed to create semaphore: %v", err)
//	}
//
// Attaching a logging observer for debugging:
//
//	type debugObserver struct{ logger *log.Logger }
//
//	func (d debugObserver) OnAcquire(len, cap int) {
//	    d.logger.Printf("acquired: %d/%d slots in use", len, cap)
//	}
//	func (d debugObserver) OnRelease(len, cap int) {
//	    d.logger.Printf("released: %d/%d slots in use", len, cap)
//	}
//	func (d debugObserver) OnWaitStart()       { d.logger.Println("wait started") }
//	func (d debugObserver) OnWaitEnd(err error) { d.logger.Printf("wait ended: %v", err) }
//
//	sem, err := sema.NewWithObserver(5, debugObserver{logger: log.Default()})
//	if err != nil {
//	    log.Fatalf("failed to create semaphore: %v", err)
//	}
func NewWithObserver(c int, obs Observer) (Semaphore, error) {
	s, err := New(c)
	if err != nil {
		return nil, err
	}
	s.(*semaphore).observer = obs
	return s, nil
}

// Must is a convenience wrapper around [New] that panics instead of
// returning an error. It is intended for use in package-level variable
// declarations and program initialization where an invalid capacity is
// a programmer error that should be caught immediately.
//
// Must panics with a message prefixed by "sema.Must:" if c is invalid.
// The same capacity rules as [New] apply: pass a positive integer for
// an explicit capacity, or -1 to use [defaultCap].
//
// # When to use Must vs New
//
// Use Must when the capacity is a compile-time constant or derived from
// a trusted configuration value that has already been validated. Use
// [New] when the capacity comes from user input, a dynamic configuration
// source, or any context where a graceful error is preferable to a panic.
//
// # Examples
//
// Package-level semaphore with a fixed capacity:
//
//	var dbPool = sema.Must(25)
//
//	func QueryDB(ctx context.Context, q string) (*Result, error) {
//	    if err := dbPool.AcquireWith(ctx); err != nil {
//	        return nil, fmt.Errorf("pool full: %w", err)
//	    }
//	    defer dbPool.Release()
//	    return execQuery(q)
//	}
//
// Using the default capacity:
//
//	var workers = sema.Must(-1)
//
// Panics on invalid input (useful for catching bugs early):
//
//	var bad = sema.Must(0) // panics: "sema.Must: sema: capacity must be >= 1 or -1 for default, got 0"
func Must(c int) Semaphore {
	s, err := New(c)
	if err != nil {
		panic(fmt.Sprintf("sema.Must: %v", err))
	}
	return s
}
