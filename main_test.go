// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestWaitForSubscriptionWorkersReturnsWhenDrained confirms
// waitForSubscriptionWorkers returns as soon as every in-flight
// subscribeToSubject handler goroutine finishes, well before the timeout
// budget is exhausted -- the fast path shutdown depends on.
func TestWaitForSubscriptionWorkersReturnsWhenDrained(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		wg.Done()
	}()

	start := time.Now()
	waitForSubscriptionWorkers(&wg, time.Second)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 500*time.Millisecond, "should return once workers finish, not wait out the full timeout")
}

// TestWaitForSubscriptionWorkersTimesOutOnStuckWorker confirms that a worker
// which never finishes does not block shutdown forever -- the function must
// give up once the timeout budget is spent instead of blocking on wg.Wait()
// indefinitely.
func TestWaitForSubscriptionWorkersTimesOutOnStuckWorker(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	// Deliberately never call Done before the timeout -- simulates a stuck
	// handler goroutine. Released at the end of the test via wg.Done() below
	// so it doesn't leak into other tests.
	defer wg.Done()

	start := time.Now()
	waitForSubscriptionWorkers(&wg, 50*time.Millisecond)
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond, "should wait out the full timeout before giving up")
	assert.Less(t, elapsed, 500*time.Millisecond, "should give up promptly once the timeout elapses")
}
