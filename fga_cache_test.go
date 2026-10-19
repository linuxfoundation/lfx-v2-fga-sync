// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type cacheWriteRecorder struct {
	writes chan string
}

func (r *cacheWriteRecorder) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	return nil, jetstream.ErrKeyNotFound
}

func (r *cacheWriteRecorder) Put(context.Context, string, []byte) (uint64, error) {
	return 1, nil
}

func (r *cacheWriteRecorder) PutString(_ context.Context, key, _ string) (uint64, error) {
	r.writes <- key
	return 1, nil
}

func TestSyncObjectTuplesSeedsPositiveCacheOnlyAfterSuccessfulWrite(t *testing.T) {
	tests := []struct {
		name          string
		writeErr      error
		wantCacheSeed bool
	}{
		{name: "successful OpenFGA write", wantCacheSeed: true},
		{name: "failed OpenFGA write", writeErr: assert.AnError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fgaClient := new(MockFgaClient)
			fgaClient.
				On("Read", mock.Anything, mock.Anything, client.ClientReadOptions{}).
				Return(&client.ClientReadResponse{}, nil)
			fgaClient.
				On("Write", mock.Anything, mock.Anything, mock.Anything).
				Return(&client.ClientWriteResponse{}, tt.writeErr)
			cache := &cacheWriteRecorder{writes: make(chan string, 1)}
			service := newFgaService(fgaClient, cache, false)
			tuple := client.ClientTupleKey{User: "user:alice", Relation: "writer", Object: "project:resource-1"}

			_, _, err := service.SyncObjectTuples(
				context.Background(),
				"project:resource-1",
				[]client.ClientTupleKey{tuple},
			)

			if tt.writeErr != nil {
				require.ErrorIs(t, err, tt.writeErr)
			} else {
				require.NoError(t, err)
			}

			// seedPositiveCacheEntries blocks until all cache writes complete, so by
			// the time SyncObjectTuples returns the write has either already
			// happened or will never happen; no wait is needed either way.
			select {
			case <-cache.writes:
				assert.True(t, tt.wantCacheSeed, "cache was seeded after failed OpenFGA write")
			default:
				assert.False(t, tt.wantCacheSeed, "cache was not seeded after successful OpenFGA write")
			}
		})
	}
}

// TestSyncObjectTuplesDoesNotSeedCacheForTupleSkippedDuringInvalidTupleRetry
// covers the case where writeAndDeleteTuplesBatch removes one OpenFGA-rejected
// tuple and retries successfully with the rest. The overall write succeeds,
// but the removed tuple was never stored, so its cache key must not be seeded
// alongside the tuple that did survive.
func TestSyncObjectTuplesDoesNotSeedCacheForTupleSkippedDuringInvalidTupleRetry(t *testing.T) {
	fgaClient := new(MockFgaClient)
	fgaClient.
		On("Read", mock.Anything, mock.Anything, client.ClientReadOptions{}).
		Return(&client.ClientReadResponse{}, nil)
	// First attempt includes both tuples; OpenFGA rejects alice's writer grant.
	fgaClient.
		On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
			return len(req.Writes) == 2
		}), mock.Anything).
		Return((*client.ClientWriteResponse)(nil), makeValidationError(
			"Invalid tuple 'project:resource-1#writer@user:alice'. Reason: relation 'project#writer' not found",
		)).
		Once()
	// Retry with only bob's viewer grant succeeds.
	fgaClient.
		On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
			return len(req.Writes) == 1 && req.Writes[0].User == "user:bob"
		}), mock.Anything).
		Return(&client.ClientWriteResponse{}, nil).
		Once()

	cache := &cacheWriteRecorder{writes: make(chan string, 2)}
	service := newFgaService(fgaClient, cache, false)

	writes := []client.ClientTupleKey{
		{User: "user:alice", Relation: "writer", Object: "project:resource-1"},
		{User: "user:bob", Relation: "viewer", Object: "project:resource-1"},
	}

	_, _, err := service.SyncObjectTuples(context.Background(), "project:resource-1", writes)
	require.NoError(t, err)

	survivingCacheKey := "rel." + cacheKeyEncoder.EncodeToString(
		[]byte("project:resource-1#viewer@user:bob"),
	)

	// seedPositiveCacheEntries blocks until all cache writes complete, so both
	// checks below are deterministic by the time SyncObjectTuples has returned.
	select {
	case key := <-cache.writes:
		assert.Equal(t, survivingCacheKey, key, "seeded cache key should be for the tuple OpenFGA actually stored")
	default:
		t.Fatal("expected the surviving tuple's cache entry to be seeded")
	}

	select {
	case key := <-cache.writes:
		t.Fatalf("unexpected second cache write for %q; the skipped invalid tuple must not be seeded", key)
	default:
		// No further writes: the skipped tuple's cache key was correctly excluded.
	}
}

// TestGetLastInvalidationReturnsContextErrorWhenCacheBudgetFull proves a read
// of the inv key waiting on a saturated cacheOpSem gives up when its context
// is canceled: it returns the context error, never reaches the bucket, and
// leaves the occupied slot count untouched.
func TestGetLastInvalidationReturnsContextErrorWhenCacheBudgetFull(t *testing.T) {
	for range cacheOpConcurrency {
		cacheOpSem <- struct{}{}
	}
	t.Cleanup(func() {
		for range cacheOpConcurrency {
			<-cacheOpSem
		}
	})

	kv := &concurrencyTrackingKV{}
	cache := CacheLayer{bucket: kv}
	ctx, cancel := context.WithCancel(context.Background())

	type result struct {
		lastInvalidation time.Time
		err              error
	}
	done := make(chan result, 1)
	go func() {
		lastInvalidation, err := cache.getLastInvalidation(ctx)
		done <- result{lastInvalidation, err}
	}()

	select {
	case res := <-done:
		t.Fatalf("getLastInvalidation returned before cancellation: %v", res.err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()

	select {
	case res := <-done:
		require.ErrorIs(t, res.err, context.Canceled)
		assert.True(t, res.lastInvalidation.IsZero())
	case <-time.After(time.Second):
		t.Fatal("getLastInvalidation did not return after its context was canceled")
	}

	assert.Zero(t, kv.peak, "bucket Get must not run without a cache slot")
	assert.Equal(t, cacheOpConcurrency, len(cacheOpSem), "occupied slot count must be unchanged")
}

// TestInvalidateWritesWithoutCacheBudgetSlot proves the inv write in
// invalidate never waits for a cacheOpSem slot: with the budget full, the
// Put still runs and returns, and the occupied slot count is unchanged.
func TestInvalidateWritesWithoutCacheBudgetSlot(t *testing.T) {
	for range cacheOpConcurrency {
		cacheOpSem <- struct{}{}
	}
	t.Cleanup(func() {
		for range cacheOpConcurrency {
			<-cacheOpSem
		}
	})

	kv := &concurrencyTrackingKV{}
	cache := CacheLayer{bucket: kv}

	done := make(chan error, 1)
	go func() {
		done <- cache.invalidate(context.Background())
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("invalidate blocked on a full cache budget")
	}

	assert.Equal(t, 1, kv.putPeak, "inv Put must run while the cache budget is full")
	assert.Equal(t, cacheOpConcurrency, len(cacheOpSem), "occupied slot count must be unchanged")
}
