package main

import (
	"sync"
	"testing"
	"time"
)

// TestWriteLockSerializes confirms two concurrent withWriteLock calls run
// sequentially, not in parallel. If the lock is broken, the two 200ms sleeps
// would overlap and total wall time would be ~200ms; with the lock, ~400ms.
func TestWriteLockSerializes(t *testing.T) {
	out := t.TempDir()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		overlap  bool
		inFlight int
	)
	worker := func() {
		defer wg.Done()
		err := withWriteLock(out, func() error {
			mu.Lock()
			inFlight++
			if inFlight > 1 {
				overlap = true
			}
			mu.Unlock()
			time.Sleep(200 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		})
		if err != nil {
			t.Errorf("withWriteLock: %v", err)
		}
	}

	wg.Add(2)
	start := time.Now()
	go worker()
	go worker()
	wg.Wait()
	elapsed := time.Since(start)

	if overlap {
		t.Error("two workers held the lock simultaneously")
	}
	if elapsed < 380*time.Millisecond {
		t.Errorf("expected ~400ms (serial), got %v — lock not blocking", elapsed)
	}
}

func TestWriteLockReleasesOnError(t *testing.T) {
	out := t.TempDir()

	// First call returns an error; lock must still release so the second can proceed.
	_ = withWriteLock(out, func() error {
		return errAbort
	})
	done := make(chan struct{})
	go func() {
		_ = withWriteLock(out, func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock never acquired — first didn't release on error")
	}
}

// errAbort is a sentinel for the release-on-error test.
var errAbort = &lockTestError{}

type lockTestError struct{}

func (*lockTestError) Error() string { return "abort" }
