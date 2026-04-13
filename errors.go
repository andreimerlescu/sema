package sema

import "fmt"

// ErrInvalidCap is returned by [semaphore.SetCap] when the requested
// capacity is less than 1 and not the special value -1 (which resets to
// [defaultCap]).
//
// The Value field contains the invalid capacity that was passed.
//
// Example:
//
//	if err := sem.SetCap(0); err != nil {
//	    var capErr sema.ErrInvalidCap
//	    if errors.As(err, &capErr) {
//	        fmt.Printf("bad capacity: %d\n", capErr.Value)
//	    }
//	}
func (e ErrInvalidCap) String() string {
	return fmt.Sprintf("sema: capacity must be >= 1 or -1 for default, got %d", e.Value)
}

// Error implements the [error] interface.
func (e ErrInvalidCap) Error() string { return e.String() }

// Is supports matching via [errors.Is] by type. Any ErrInvalidCap
// matches any other ErrInvalidCap regardless of the Value field.
func (e ErrInvalidCap) Is(target error) bool {
	_, ok := target.(ErrInvalidCap)
	return ok
}

// ErrInvalidN is returned by [semaphore.AcquireN], [semaphore.ReleaseN],
// and related multi-slot methods when n is less than 1.
//
// The Value field contains the invalid n that was passed.
//
// Example:
//
//	if err := sem.AcquireN(0); err != nil {
//	    var nErr sema.ErrInvalidN
//	    if errors.As(err, &nErr) {
//	        fmt.Printf("invalid n: %d\n", nErr.Value)
//	    }
//	}
func (e ErrInvalidN) String() string {
	return fmt.Sprintf("sema: n must be >= 1, got %d", e.Value)
}

// Error implements the [error] interface.
func (e ErrInvalidN) Error() string { return e.String() }

// Is supports matching via [errors.Is] by type.
func (e ErrInvalidN) Is(target error) bool {
	_, ok := target.(ErrInvalidN)
	return ok
}

// ErrReleaseExceedsCount is returned by [semaphore.Release] and
// [semaphore.ReleaseN] when the caller attempts to release more slots
// than are currently held. This typically indicates a mismatched
// acquire/release pair, or a release after [semaphore.Drain] or
// [semaphore.Reset] has already cleared the slots.
//
//   - Attempted: the number of slots the caller tried to release.
//   - Current:   the number of slots actually held at the time of the call.
//
// Example:
//
//	sem := sema.New(5)
//	// No slots acquired — releasing is an error.
//	if err := sem.Release(); err != nil {
//	    var relErr sema.ErrReleaseExceedsCount
//	    if errors.As(err, &relErr) {
//	        fmt.Printf("tried to release %d, but only %d held\n",
//	            relErr.Attempted, relErr.Current)
//	    }
//	}
func (e ErrReleaseExceedsCount) String() string {
	return fmt.Sprintf("sema: release called without matching acquire: attempted %d, current occupancy %d", e.Attempted, e.Current)
}

// Error implements the [error] interface.
func (e ErrReleaseExceedsCount) Error() string { return e.String() }

// Is supports matching via [errors.Is] by type.
func (e ErrReleaseExceedsCount) Is(target error) bool {
	_, ok := target.(ErrReleaseExceedsCount)
	return ok
}

// ErrNoSlot is returned by [semaphore.TryAcquireWith] and
// [semaphore.TryAcquireNWith] when the semaphore does not have enough
// free slots to satisfy a non-blocking acquire.
//
//   - Requested: the number of slots the caller asked for.
//   - Available: the number of free slots at the time of the attempt.
//
// Example:
//
//	err := sem.TryAcquireNWith(ctx, 3)
//	var slotErr sema.ErrNoSlot
//	if errors.As(err, &slotErr) {
//	    fmt.Printf("wanted %d slots, only %d free\n",
//	        slotErr.Requested, slotErr.Available)
//	}
func (e ErrNoSlot) String() string {
	return fmt.Sprintf("sema: no slot available: requested %d, available %d", e.Requested, e.Available)
}

// Error implements the [error] interface.
func (e ErrNoSlot) Error() string { return e.String() }

// Is supports matching via [errors.Is] by type.
func (e ErrNoSlot) Is(target error) bool {
	_, ok := target.(ErrNoSlot)
	return ok
}

// ErrAcquireCancelled is returned by context-aware acquire methods
// ([semaphore.AcquireWith], [semaphore.AcquireNWith],
// [semaphore.AcquireTimeout], etc.) when the context is cancelled or
// its deadline expires before a slot can be acquired.
//
// The Cause field holds the underlying context error (typically
// [context.Canceled] or [context.DeadlineExceeded]).
//
// ErrAcquireCancelled implements [errors.Unwrap], so the cause can be
// inspected with [errors.Is] or [errors.As].
//
// Example:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
//	defer cancel()
//	time.Sleep(5 * time.Millisecond)
//
//	if err := sem.AcquireWith(ctx); err != nil {
//	    var acqErr sema.ErrAcquireCancelled
//	    if errors.As(err, &acqErr) {
//	        fmt.Printf("acquire cancelled: %v\n", acqErr.Cause)
//	    }
//	    if errors.Is(err, context.DeadlineExceeded) {
//	        fmt.Println("it was a timeout")
//	    }
//	}
func (e ErrAcquireCancelled) String() string {
	return fmt.Sprintf("sema: acquire cancelled by context: %v", e.Cause)
}

// Error implements the [error] interface.
func (e ErrAcquireCancelled) Error() string { return e.String() }

// Unwrap returns the underlying context error, enabling use with
// [errors.Is] and [errors.As] for matching [context.Canceled] or
// [context.DeadlineExceeded].
func (e ErrAcquireCancelled) Unwrap() error { return e.Cause }

// Is supports matching via [errors.Is] by type.
func (e ErrAcquireCancelled) Is(target error) bool {
	_, ok := target.(ErrAcquireCancelled)
	return ok
}

// ErrDrain is returned by [semaphore.Drain] when the internal channel
// could not be fully emptied. The Cause field contains a human-readable
// description of what went wrong.
//
// Example:
//
//	if err := sem.Drain(); err != nil {
//	    var drainErr sema.ErrDrain
//	    if errors.As(err, &drainErr) {
//	        fmt.Printf("drain problem: %s\n", drainErr.Cause)
//	    }
//	}
func (e ErrDrain) String() string { return fmt.Sprintf("sema: drain error: %s", e.Cause) }

// Error implements the [error] interface.
func (e ErrDrain) Error() string { return e.String() }

// Is supports matching via [errors.Is] by type.
func (e ErrDrain) Is(target error) bool {
	_, ok := target.(ErrDrain)
	return ok
}

// ErrRecovered is returned when [semaphore.AcquireN] (or a related
// method) catches a panic during execution and wraps it as an error.
//
//   - Cause:   the raw value recovered from the panic (interface{}).
//   - AsError: the Cause converted to an error, if possible, for use
//     with [errors.Unwrap].
//
// ErrRecovered implements [errors.Unwrap] via AsError, so callers can
// use [errors.Is] and [errors.As] to inspect the original panic value
// when it satisfies the error interface.
//
// Example:
//
//	if err := sem.AcquireN(2); err != nil {
//	    var recErr sema.ErrRecovered
//	    if errors.As(err, &recErr) {
//	        fmt.Printf("panic recovered: %v\n", recErr.Cause)
//	    }
//	}
func (e ErrRecovered) String() string {
	return fmt.Sprintf("sema: recovered from panic in AcquireN: %v", e.Cause)
}

// Error implements the [error] interface.
func (e ErrRecovered) Error() string { return e.String() }

// Unwrap returns the panic value as an error (if it implements [error]),
// enabling [errors.Is] and [errors.As] chains through the recovered panic.
func (e ErrRecovered) Unwrap() error { return e.AsError }

// Is supports matching via [errors.Is] by type.
func (e ErrRecovered) Is(target error) bool {
	_, ok := target.(ErrRecovered)
	return ok
}

// ErrNExceedsCap is returned by [semaphore.AcquireN],
// [semaphore.AcquireNWith], and related multi-slot methods when the
// requested count n exceeds the semaphore's current capacity. Acquiring
// more slots than the capacity would block forever, so the call is
// rejected immediately.
//
//   - Requested: the number of slots the caller asked for.
//   - Cap:       the semaphore's capacity at the time of the call.
//
// Example:
//
//	sem := sema.New(3)
//	if err := sem.AcquireN(5); err != nil {
//	    var excErr sema.ErrNExceedsCap
//	    if errors.As(err, &excErr) {
//	        fmt.Printf("requested %d but cap is only %d\n",
//	            excErr.Requested, excErr.Cap)
//	    }
//	}
func (e ErrNExceedsCap) String() string {
	return fmt.Sprintf("sema: requested %d exceeds cap %d, would block forever", e.Requested, e.Cap)
}

// Error implements the [error] interface.
func (e ErrNExceedsCap) Error() string { return e.String() }

// Is supports matching via [errors.Is] by type.
func (e ErrNExceedsCap) Is(target error) bool {
	_, ok := target.(ErrNExceedsCap)
	return ok
}
