# Semaphore (for GoLang)

This package provides a fast implementation of a powerful
computing technique called the semaphore. These allow you to
control the quantity of computation, disk I/O, network I/O, 
etc related load on your application.

## Installation

```shell
go get -u github.com/andreimerlescu/sema
```

## Usage

Integrating `sema` into your project is easy.

```go
package main

import (
	`fmt`
	`sync`
	`sync/atomic`
	`time`

	"github.com/andreimerlescu/sema"
)

func main() {
	workers := 10 // 10 workers
	mySemaphore := sema.New(workers)
	var wg sync.WaitGroup
	var delaySeconds atomic.Int32
	for i := 0; i < workers*2; i++ {
		wg.Add(1)
		mySemaphore.Acquire()
		go func(i int, wg *sync.WaitGroup) {
			defer wg.Done()
			defer mySemaphore.Release()
			delay := delaySeconds.Add(1)
			time.Sleep(time.Duration(delay)*time.Second)
			fmt.Printf("worker %d finished after %d seconds\n", i, delaySeconds.Load())
		}(i, &wg)
	}
	wg.Wait()
	fmt.Printf("wait group released with %d workers left in the semaphore\n", mySemaphore.Len())
}
```

This code has been included in the [sema_test.go](sema_test.go) file and is part of the build verification of
this package. A `sync.WaitGroup` is needed to ensure that the main process does not quit until all of the
workers have completed their task. The `sync.WaitGroup` effectively demonstrates through the `; i < workers*2 ;`
middle segment from the `for` loop that only `workers := 10` will be able to even start running their
go routine. 

## License

This is open source under the Apache 2.0 license.

