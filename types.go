package sema

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type Semaphore interface {
	Acquire()
	AcquireWith(ctx context.Context) error
	AcquireTimeout(d time.Duration) error
	TryAcquire() bool
	TryAcquireWith(ctx context.Context) error
	Release() error

	AcquireN(n int) error
	AcquireNWith(ctx context.Context, n int) error
	AcquireNTimeout(n int, d time.Duration) error
	TryAcquireN(n int) bool
	TryAcquireNWith(ctx context.Context, n int) error
	ReleaseN(n int) error

	Wait(ctx context.Context) error

	// Drain forcibly removes all held slots. Goroutines that previously
	// acquired slots will receive ErrReleaseExceedsCount when they call
	// Release. Coordinate with Wait or application-level flags to avoid this.
	Drain() error
	Reset() error
	Len() int
	Cap() int
	Utilization() float64
	UtilizationSmoothed() float64
	IsEmpty() bool
	IsFull() bool
	SetCap(c int) error
}

type (
	semaphore struct {
		mu       sync.Mutex
		cond     *sync.Cond
		ch       atomic.Pointer[chan struct{}]
		ewma     atomic.Uint64
		observer Observer
	}
	ErrInvalidCap          struct{ Value int }
	ErrInvalidN            struct{ Value int }
	ErrReleaseExceedsCount struct {
		Attempted int
		Current   int
	}
	ErrNoSlot struct {
		Requested int
		Available int
	}
	ErrAcquireCancelled struct{ Cause error }
	ErrDrain            struct{ Cause string }
	ErrRecovered        struct {
		Cause   any
		AsError error
	}
	ErrNExceedsCap struct {
		Requested int
		Cap       int
	}
)
