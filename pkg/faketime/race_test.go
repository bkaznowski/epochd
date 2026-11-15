//go:build linux

package faketime

import (
	"sync"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/inject"
)

// Exec re-injection swaps a Handle's backend while callers may be reading it.
// Run under -race: SetTime, Freeze and PID must take the same lock as the swap.
func TestHandleBackendSwapIsRaceFree(t *testing.T) {
	h := newAdvancingHandle(&inject.Handle{PID: 1}, time.Now())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // what ChildTracker.handleExec does after an exec
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.mu.Lock()
			h.h = &inject.Handle{PID: 1}
			h.mu.Unlock()
		}
	}()

	for i := 0; i < 2000; i++ {
		// The writes target PID 1's address 0 and fail; only the access pattern matters.
		_ = h.SetTime(time.Now())
		_ = h.Freeze(time.Now())
		_ = h.PID()
	}
	close(stop)
	wg.Wait()
}
