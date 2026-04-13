package sema

import (
	"fmt"
	"sync"
)

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

func NewWithObserver(c int, obs Observer) (Semaphore, error) {
	s, err := New(c)
	if err != nil {
		return nil, err
	}
	s.(*semaphore).observer = obs
	return s, nil
}

func Must(c int) Semaphore {
	s, err := New(c)
	if err != nil {
		panic(fmt.Sprintf("sema.Must: %v", err))
	}
	return s
}
