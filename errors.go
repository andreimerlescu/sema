package sema

import "fmt"

func (e ErrInvalidCap) String() string {
	return fmt.Sprintf("sema: capacity must be >= 1 or -1 for default, got %d", e.Value)
}
func (e ErrInvalidCap) Error() string { return e.String() }
func (e ErrInvalidCap) Is(target error) bool {
	_, ok := target.(ErrInvalidCap)
	return ok
}

func (e ErrInvalidN) String() string {
	return fmt.Sprintf("sema: n must be >= 1, got %d", e.Value)
}
func (e ErrInvalidN) Error() string { return e.String() }
func (e ErrInvalidN) Is(target error) bool {
	_, ok := target.(ErrInvalidN)
	return ok
}

func (e ErrReleaseExceedsCount) String() string {
	return fmt.Sprintf("sema: release called without matching acquire: attempted %d, current occupancy %d", e.Attempted, e.Current)
}
func (e ErrReleaseExceedsCount) Error() string { return e.String() }
func (e ErrReleaseExceedsCount) Is(target error) bool {
	_, ok := target.(ErrReleaseExceedsCount)
	return ok
}

func (e ErrNoSlot) String() string {
	return fmt.Sprintf("sema: no slot available: requested %d, available %d", e.Requested, e.Available)
}
func (e ErrNoSlot) Error() string { return e.String() }
func (e ErrNoSlot) Is(target error) bool {
	_, ok := target.(ErrNoSlot)
	return ok
}

func (e ErrAcquireCancelled) String() string {
	return fmt.Sprintf("sema: acquire cancelled by context: %v", e.Cause)
}
func (e ErrAcquireCancelled) Error() string { return e.String() }
func (e ErrAcquireCancelled) Unwrap() error { return e.Cause }
func (e ErrAcquireCancelled) Is(target error) bool {
	_, ok := target.(ErrAcquireCancelled)
	return ok
}

func (e ErrDrain) String() string { return fmt.Sprintf("sema: drain error: %s", e.Cause) }
func (e ErrDrain) Error() string  { return e.String() }
func (e ErrDrain) Is(target error) bool {
	_, ok := target.(ErrDrain)
	return ok
}

func (e ErrRecovered) String() string {
	return fmt.Sprintf("sema: recovered from panic in AcquireN: %v", e.Cause)
}
func (e ErrRecovered) Error() string { return e.String() }
func (e ErrRecovered) Unwrap() error { return e.AsError }
func (e ErrRecovered) Is(target error) bool {
	_, ok := target.(ErrRecovered)
	return ok
}

func (e ErrNExceedsCap) String() string {
	return fmt.Sprintf("sema: requested %d exceeds cap %d, would block forever", e.Requested, e.Cap)
}
func (e ErrNExceedsCap) Error() string { return e.String() }
func (e ErrNExceedsCap) Is(target error) bool {
	_, ok := target.(ErrNExceedsCap)
	return ok
}
