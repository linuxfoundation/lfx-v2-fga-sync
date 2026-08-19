// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestWaitForSubscriptionWorkersReturnsWhenDrained confirms
// waitForSubscriptionWorkers returns as soon as every in-flight
// subscribeToSubject handler goroutine finishes, well before the timeout
// budget is exhausted -- the fast path shutdown depends on.
func TestWaitForSubscriptionWorkersReturnsWhenDrained(t *testing.T) {
	subscriptionWG.Add(1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		subscriptionWG.Done()
	}()

	start := time.Now()
	waitForSubscriptionWorkers(time.Second)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 500*time.Millisecond, "should return once workers finish, not wait out the full timeout")
}

// TestWaitForSubscriptionWorkersTimesOutOnStuckWorker confirms that a worker
// which never finishes does not block shutdown forever -- the function must
// give up once the timeout budget is spent instead of blocking on
// subscriptionWG.Wait() indefinitely.
func TestWaitForSubscriptionWorkersTimesOutOnStuckWorker(t *testing.T) {
	subscriptionWG.Add(1)
	// Deliberately never call Done -- simulates a stuck handler goroutine.
	// Leaves subscriptionWG permanently incremented; harmless for a package
	// global at process/test-binary exit and does not affect other tests,
	// which each Add/Done their own delta.

	start := time.Now()
	waitForSubscriptionWorkers(50 * time.Millisecond)
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond, "should wait out the full timeout before giving up")
	assert.Less(t, elapsed, 500*time.Millisecond, "should give up promptly once the timeout elapses")
}
