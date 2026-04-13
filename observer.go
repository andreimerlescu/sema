package sema

// Observer receives non-blocking event notifications from the semaphore.
// All methods must return immediately — never acquire locks inside them.
type Observer interface {
	OnAcquire(count, cap int)
	OnRelease(count, cap int)
	OnWaitStart()
	OnWaitEnd(err error)
}
