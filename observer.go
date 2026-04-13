package sema

// Observer is an interface for receiving lifecycle event notifications
// from a [Semaphore]. Implementations can use these callbacks to collect
// metrics, emit structured logs, trigger alerts, or drive adaptive
// concurrency control — without modifying the semaphore itself.
//
// An Observer is attached at construction time via [NewWithObserver].
// A semaphore created with [New] or [Must] has no observer and incurs
// no callback overhead.
//
// # Concurrency and performance contract
//
// All Observer methods are called synchronously while the semaphore's
// internal lock is held. Implementations MUST return immediately and
// MUST NOT:
//
//   - Acquire additional locks that could create a lock-ordering cycle
//     with the semaphore's own mutex.
//   - Call back into the same semaphore (e.g. Acquire, Release, SetCap),
//     which would deadlock.
//   - Perform blocking I/O, network calls, or any operation with
//     unbounded latency.
//
// If you need to perform expensive work in response to an event, buffer
// it in a channel or queue and process it asynchronously.
//
// # Method summary
//
//   - [Observer.OnAcquire] — a slot was successfully acquired.
//   - [Observer.OnRelease] — a slot was successfully released.
//   - [Observer.OnWaitStart] — a call to [semaphore.Wait] has begun.
//   - [Observer.OnWaitEnd] — a call to [semaphore.Wait] has finished.
//
// # Examples
//
// A minimal no-op observer (useful as a base for partial implementations):
//
//	type noopObserver struct{}
//
//	func (noopObserver) OnAcquire(count, cap int) {}
//	func (noopObserver) OnRelease(count, cap int) {}
//	func (noopObserver) OnWaitStart()              {}
//	func (noopObserver) OnWaitEnd(err error)       {}
//
// A Prometheus-style metrics observer:
//
//	type prometheusObserver struct {
//	    acquireTotal  prometheus.Counter
//	    releaseTotal  prometheus.Counter
//	    utilization   prometheus.Gauge
//	}
//
//	func (p *prometheusObserver) OnAcquire(count, cap int) {
//	    p.acquireTotal.Inc()
//	    p.utilization.Set(float64(count) / float64(cap))
//	}
//
//	func (p *prometheusObserver) OnRelease(count, cap int) {
//	    p.releaseTotal.Inc()
//	    p.utilization.Set(float64(count) / float64(cap))
//	}
//
//	func (p *prometheusObserver) OnWaitStart()        {}
//	func (p *prometheusObserver) OnWaitEnd(err error) {}
//
// An observer that buffers events for asynchronous processing:
//
//	type asyncObserver struct {
//	    events chan Event
//	}
//
//	func (a *asyncObserver) OnAcquire(count, cap int) {
//	    select {
//	    case a.events <- Event{Kind: "acquire", Count: count, Cap: cap}:
//	    default:
//	        // Drop event if buffer is full — never block.
//	    }
//	}
//
//	func (a *asyncObserver) OnRelease(count, cap int) {
//	    select {
//	    case a.events <- Event{Kind: "release", Count: count, Cap: cap}:
//	    default:
//	    }
//	}
//
//	func (a *asyncObserver) OnWaitStart()        {}
//	func (a *asyncObserver) OnWaitEnd(err error) {}
type Observer interface {
	// OnAcquire is called immediately after a slot is successfully
	// acquired by any of the acquire methods ([semaphore.Acquire],
	// [semaphore.AcquireWith], [semaphore.TryAcquire],
	// [semaphore.AcquireN], etc.).
	//
	// Parameters:
	//   - count: the number of slots currently held after this
	//     acquisition (equivalent to [semaphore.Len] at call time).
	//   - cap: the total capacity of the semaphore (equivalent to
	//     [semaphore.Cap] at call time).
	//
	// The ratio count/cap gives the instantaneous utilization at the
	// moment of acquisition. Note that for bulk acquires via
	// [semaphore.AcquireN], OnAcquire is called once after all n slots
	// have been claimed, not once per slot.
	//
	// Example usage inside an implementation:
	//
	//	func (o *myObserver) OnAcquire(count, cap int) {
	//	    utilization := float64(count) / float64(cap)
	//	    o.gauge.Set(utilization)
	//	}
	OnAcquire(count, cap int)

	// OnRelease is called immediately after one or more slots are
	// successfully released by [semaphore.Release] or
	// [semaphore.ReleaseN].
	//
	// Parameters:
	//   - count: the number of slots still held after this release
	//     (equivalent to [semaphore.Len] at call time).
	//   - cap: the total capacity of the semaphore (equivalent to
	//     [semaphore.Cap] at call time).
	//
	// Like OnAcquire, for bulk releases via [semaphore.ReleaseN],
	// OnRelease is called once after all n slots have been freed, not
	// once per slot.
	//
	// Example usage inside an implementation:
	//
	//	func (o *myObserver) OnRelease(count, cap int) {
	//	    if count == 0 {
	//	        o.logger.Println("semaphore fully idle")
	//	    }
	//	}
	OnRelease(count, cap int)

	// OnWaitStart is called at the beginning of [semaphore.Wait],
	// before the semaphore checks whether it is already empty. This
	// allows observers to track how often callers enter a wait state
	// and to measure wait duration by pairing with [Observer.OnWaitEnd].
	//
	// OnWaitStart receives no parameters because the wait has not yet
	// inspected semaphore state — use the count and cap values from
	// [Observer.OnAcquire] or [Observer.OnRelease] for utilization data.
	//
	// Example usage inside an implementation:
	//
	//	func (o *myObserver) OnWaitStart() {
	//	    o.waitTimer = time.Now()
	//	    o.waitCount.Inc()
	//	}
	OnWaitStart()

	// OnWaitEnd is called when [semaphore.Wait] returns, whether it
	// completed successfully (all slots drained) or was cancelled by
	// the context.
	//
	// Parameters:
	//   - err: nil if the semaphore reached zero occupancy, or an
	//     [ErrAcquireCancelled] if the context was cancelled or its
	//     deadline expired before the semaphore became empty.
	//
	// By pairing OnWaitEnd with [Observer.OnWaitStart], observers can
	// measure the wall-clock duration of the wait and distinguish
	// successful drains from cancellations.
	//
	// Example usage inside an implementation:
	//
	//	func (o *myObserver) OnWaitEnd(err error) {
	//	    elapsed := time.Since(o.waitTimer)
	//	    o.waitDuration.Observe(elapsed.Seconds())
	//	    if err != nil {
	//	        o.waitCancellations.Inc()
	//	    }
	//	}
	OnWaitEnd(err error)
}
