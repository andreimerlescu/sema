package sema

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBakersDozen(t *testing.T) {
	type args struct {
		maxConcurrency int
	}
	tests := []struct {
		name string
		args args
	}{
		{"test one", args{3}},
		{"test two", args{6}},
		{"test tri", args{9}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := New(tt.args.maxConcurrency)
			if got == nil {
				t.Error("New() returned nil, expected a non-nil Semaphore")
				return
			}
			for i := 0; i < tt.args.maxConcurrency; i++ {
				got.Acquire()
				// Check that i is within bounds (replacing assert.LessOrEqual)
				if i > tt.args.maxConcurrency {
					t.Errorf("i = %d, exceeds maxConcurrency %d", i, tt.args.maxConcurrency)
				}
				// Check the length of the semaphore (replacing assert.Equal)
				if gotLen := got.Len(); gotLen != i+1 {
					t.Errorf("got.Len() = %d, want %d", gotLen, i+1)
				}
			}
		})
	}
}

func TestSemaphoreAcquireRelease(t *testing.T) {
	sem := New(3)

	for i := 0; i < 10; i++ {
		sem.Acquire()
		sem.Acquire()
		sem.Acquire()

		sem.Release()
		sem.Release()
		sem.Release()
	}
	// Optionally, we can add a check to ensure the semaphore is empty at the end.
	if !sem.IsEmpty() {
		t.Error("semaphore should be empty after all releases")
	}
}

func TestSemaphoreEmpty(t *testing.T) {
	sem := New(2)

	if !sem.IsEmpty() {
		t.Error("semaphore should be empty")
	}

	sem.Acquire()

	if sem.IsEmpty() {
		t.Error("semaphore should not be empty")
	}

	sem.Release()

	if !sem.IsEmpty() {
		t.Error("semaphore should be empty")
	}
}

func TestReadMeExample(t *testing.T) {
	workers := 10 // 10 workers
	mySemaphore := New(workers)
	var wg sync.WaitGroup
	var delaySeconds atomic.Int32
	for i := 0; i < workers*2; i++ {
		wg.Add(1)
		mySemaphore.Acquire()
		go func(i int, wg *sync.WaitGroup) {
			defer wg.Done()
			defer mySemaphore.Release()
			delay := delaySeconds.Add(1)
			time.Sleep(time.Duration(delay) * time.Second)
			fmt.Printf("worker %d finished after %d seconds\n", i, delaySeconds.Load())
		}(i, &wg)
	}
	wg.Wait()
	fmt.Printf("wait group released with %d workers left in the semaphore\n", mySemaphore.Len())
	if !mySemaphore.IsEmpty() {
		t.Error("semaphore should be empty")
	}
}
