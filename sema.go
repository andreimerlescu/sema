// Package sema provides a simple semaphore implementation to control concurrency in Go programs.
// A semaphore is a synchronization primitive that limits the number of goroutines that can run concurrently,
// ensuring that resources (e.g., CPU, memory, I/O) are not overwhelmed by too many simultaneous tasks.
// This package is useful for scenarios like parallel file processing, API rate limiting, or managing worker pools.
package sema

// Semaphore defines the interface for a semaphore that controls concurrent access.
// It provides methods to acquire and release slots, check the current usage, and determine if the semaphore is empty.
type Semaphore interface {
	// Acquire takes a slot in the semaphore, blocking if no slots are available until one is freed.
	// This method should be called before starting a concurrent task.
	Acquire()

	// Release frees a slot in the semaphore, allowing another goroutine to acquire it.
	// This method should be called when a concurrent task is complete.
	Release()

	// Len returns the current number of occupied slots in the semaphore.
	// It indicates how many goroutines are currently active.
	Len() int

	// IsEmpty returns true if no slots are occupied (i.e., no goroutines are active).
	IsEmpty() bool
}

// semaphore is the concrete implementation of the Semaphore interface.
// It uses a buffered channel to manage a fixed number of slots, where each slot represents a concurrent task.
type semaphore struct {
	semC chan struct{} // Buffered channel acting as the semaphore; each slot holds an empty struct{}.
}

// safe ensures that the maxConcurrency value is within a valid range.
// It prevents invalid or unsafe values for the semaphore's capacity.
func safe(maxConcurrency int) int {
	// If maxConcurrency is -1, set a high default limit (333,333) to allow significant concurrency.
	if maxConcurrency == -1 {
		maxConcurrency = 333_333
	}

	// Ensure maxConcurrency is at least 1 to avoid a zero-capacity channel, which would block forever.
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}

	return maxConcurrency
}

// New creates a new Semaphore with the specified maximum concurrency.
// The maxConcurrency parameter determines how many goroutines can run concurrently before blocking.
// If maxConcurrency is invalid (e.g., -1 or 0), it is adjusted by the safe function.
func New(maxConcurrency int) Semaphore {
	// Create a semaphore with a buffered channel of the adjusted maxConcurrency size.
	return &semaphore{
		semC: make(chan struct{}, safe(maxConcurrency)),
	}
}

// IsEmpty checks if the semaphore has any occupied slots.
// It returns true if no goroutines are currently active (i.e., the channel is empty).
func (s *semaphore) IsEmpty() bool {
	return s.Len() == 0
}

// Len returns the number of currently occupied slots in the semaphore.
// This represents how many goroutines are actively holding a slot.
func (s *semaphore) Len() int {
	return len(s.semC)
}

// Acquire takes a slot in the semaphore, blocking if no slots are available.
// It sends an empty struct{} to the channel, occupying a slot until Release is called.
// This method should be called before starting a concurrent task.
func (s *semaphore) Acquire() {
	s.semC <- struct{}{}
}

// Release frees a slot in the semaphore, allowing another goroutine to acquire it.
// It receives from the channel, removing an occupied slot.
// This method should be called when a concurrent task is complete.
func (s *semaphore) Release() {
	<-s.semC
}
