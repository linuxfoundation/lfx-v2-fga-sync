// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
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

func (r *cacheWriteRecorder) Delete(context.Context, string, ...jetstream.KVDeleteOpt) error {
	return nil
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

// TestSyncObjectTuplesDoesNotSeedOverlongRelationKey covers a tuple OpenFGA
// accepts (its fields are within the character limits) whose relation key is
// longer than maxRelationKeyBytes. The tuple is still written, but its cache
// key is not seeded, so no KV key exceeds the bound.
func TestSyncObjectTuplesDoesNotSeedOverlongRelationKey(t *testing.T) {
	longUser := "user:" + strings.Repeat("é", 507) // 512 characters, 1,019 bytes
	fgaClient := new(MockFgaClient)
	fgaClient.
		On("Read", mock.Anything, mock.Anything, client.ClientReadOptions{}).
		Return(&client.ClientReadResponse{}, nil)
	fgaClient.
		On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
			return len(req.Writes) == 2
		}), mock.Anything).
		Return(&client.ClientWriteResponse{}, nil).
		Once()

	cache := &cacheWriteRecorder{writes: make(chan string, 2)}
	service := newFgaService(fgaClient, cache, false)

	writes := []client.ClientTupleKey{
		{User: longUser, Relation: "viewer", Object: "project:resource-1"},
		{User: "user:bob", Relation: "viewer", Object: "project:resource-1"},
	}

	_, _, err := service.SyncObjectTuples(context.Background(), "project:resource-1", writes)
	require.NoError(t, err)
	fgaClient.AssertExpectations(t)

	select {
	case key := <-cache.writes:
		assert.Equal(t, "rel."+cacheKeyEncoder.EncodeToString([]byte("project:resource-1#viewer@user:bob")), key)
	default:
		t.Fatal("expected the normal tuple's cache entry to be seeded")
	}
	select {
	case key := <-cache.writes:
		t.Fatalf("unexpected cache write of %d bytes; the over-long relation key must not be seeded", len(key))
	default:
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

// TestCacheLayerNilBucket proves every CacheLayer method is safe when the
// cache bucket could not be bound at startup.
func TestCacheLayerNilBucket(t *testing.T) {
	cache := CacheLayer{}
	ctx := context.Background()
	tuple := client.ClientBatchCheckItem{Object: "project:example", Relation: "viewer", User: "user:example-user"}

	assert.NotPanics(t, func() {
		cache.seedPositiveEntries(ctx, []string{"rel.KEY1", "rel.KEY2"})
	})
	require.NoError(t, cache.invalidate(ctx))

	lastInvalidation, err := cache.getLastInvalidation(ctx)
	require.NoError(t, err)
	assert.True(t, lastInvalidation.IsZero())

	outcome := cache.lookupEntry(ctx, tuple, time.Time{})
	assert.True(t, outcome.needsCheck)
	assert.Equal(t, cacheOutcomeMiss, outcome.kind)

	var message []byte
	assert.NotPanics(t, func() {
		message = cache.buildResponseAndWriteBack(ctx, nil,
			map[string]openfga.BatchCheckSingleResult{"1": {Allowed: openfga.PtrBool(true)}},
			map[string]client.ClientBatchCheckItem{"1": tuple}, time.Time{})
	})
	assert.Equal(t, "project:example#viewer@user:example-user\ttrue\n", string(message))
}

func TestCacheLayerRecoversBucketForInvalidation(t *testing.T) {
	ctx := context.Background()
	kv := &keyRecordingKV{}
	binds := 0
	var bound INatsKeyValue
	cache := CacheLayer{recovery: &bucketRecovery{bind: func(context.Context) (INatsKeyValue, error) {
		binds++
		if bound == nil {
			return nil, jetstream.ErrBucketNotFound
		}
		return bound, nil
	}}}

	require.NoError(t, cache.invalidate(ctx), "a still-missing bucket is not an error")
	assert.Empty(t, kv.puts)

	bound = kv
	require.NoError(t, cache.invalidate(ctx))
	require.NoError(t, cache.invalidate(ctx))
	assert.Equal(t, []string{"inv", "inv"}, kv.puts)
	assert.Equal(t, 2, binds, "the bucket is bound once and then reused")

	tuple := client.ClientBatchCheckItem{Object: "project:example", Relation: "viewer", User: "user:example-user"}
	outcome := cache.lookupEntry(ctx, tuple, time.Time{})
	assert.Equal(t, cacheOutcomeMiss, outcome.kind, "reads stay disabled after recovery")
	cache.seedPositiveEntries(ctx, []string{"rel.KEY1"})
	assert.Equal(t, []string{"inv", "inv"}, kv.puts, "seeding stays disabled after recovery")
	assert.Empty(t, kv.gets)
}

func TestCacheLayerRecoveryConcurrentCopies(t *testing.T) {
	kv := &keyRecordingKV{}
	shared := CacheLayer{recovery: &bucketRecovery{bind: func(context.Context) (INatsKeyValue, error) {
		return kv, nil
	}}}

	const writers = 20
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func(cache CacheLayer) {
			defer wg.Done()
			assert.NoError(t, cache.invalidate(context.Background()))
		}(shared)
	}
	wg.Wait()

	assert.Len(t, kv.puts, writers, "every write invalidates")
	assert.NotNil(t, shared.recovery.bucket)
}

// clockKV is an in-memory bucket whose creation times come from a counter, so
// the order of writes is exact. onRelPut, when set, runs before each write of a
// relation key. Reads of inv fail once more than invGetsOK have succeeded.
type clockKV struct {
	mu        sync.Mutex
	tick      int
	entries   map[string]fixedEntry
	onRelPut  func()
	deleteErr error
	invGets   int
	invGetsOK int
}

func newClockKV() *clockKV { return &clockKV{entries: map[string]fixedEntry{}} }

func (k *clockKV) Get(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key == "inv" {
		k.invGets++
		if k.invGetsOK > 0 && k.invGets > k.invGetsOK {
			return nil, assert.AnError
		}
	}
	entry, ok := k.entries[key]
	if !ok {
		return nil, jetstream.ErrKeyNotFound
	}
	return entry, nil
}

func (k *clockKV) Put(_ context.Context, key string, value []byte) (uint64, error) {
	k.mu.Lock()
	hook := k.onRelPut
	k.mu.Unlock()
	if hook != nil && strings.HasPrefix(key, "rel.") {
		hook()
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.tick++
	k.entries[key] = fixedEntry{value: value, created: time.Unix(0, 0).Add(time.Duration(k.tick) * time.Millisecond)}
	return uint64(k.tick), nil
}

func (k *clockKV) PutString(ctx context.Context, key, value string) (uint64, error) {
	return k.Put(ctx, key, []byte(value))
}

func (k *clockKV) Delete(_ context.Context, key string, _ ...jetstream.KVDeleteOpt) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.deleteErr != nil {
		return k.deleteErr
	}
	delete(k.entries, key)
	return nil
}

func (k *clockKV) relEntries() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for key := range k.entries {
		if strings.HasPrefix(key, "rel.") {
			n++
		}
	}
	return n
}

// TestCheckWriteBackRacingInvalidation covers a check whose "allowed" answer
// is revoked, and the cache invalidated, before or while fga-sync writes the
// answer back. The next check must not be answered "allowed" from the cache.
func TestCheckWriteBackRacingInvalidation(t *testing.T) {
	tuple := client.ClientCheckRequest{User: "user:example-user", Relation: "viewer", Object: "project:example"}
	revoke := func(kv *clockKV) func() {
		var once sync.Once
		return func() {
			once.Do(func() { _, _ = kv.Put(context.Background(), "inv", []byte("1")) })
		}
	}

	tests := []struct {
		name            string
		existingMarker  bool
		revokeInCheck   bool
		revokeInWrite   bool
		deleteErr       error
		invGetsOK       int
		wantCached      bool
		wantNoRelWrites bool
	}{
		{name: "no invalidation, no marker yet", wantCached: true},
		{name: "no invalidation, existing marker", existingMarker: true, wantCached: true},
		{name: "invalidation while OpenFGA answers", revokeInCheck: true},
		{name: "invalidation while OpenFGA answers, existing marker", existingMarker: true, revokeInCheck: true},
		{name: "invalidation during the write-back", revokeInWrite: true},
		{name: "invalidation during the write-back, delete fails", revokeInWrite: true, deleteErr: assert.AnError},
		{name: "marker re-read fails before the write-back", invGetsOK: 1, wantNoRelWrites: true},
		{name: "marker re-read fails after the write-back", invGetsOK: 2, wantNoRelWrites: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			kv := newClockKV()
			kv.deleteErr = tt.deleteErr
			kv.invGetsOK = tt.invGetsOK
			if tt.existingMarker {
				_, _ = kv.Put(ctx, "inv", []byte("1"))
			}
			if tt.revokeInWrite {
				kv.onRelPut = revoke(kv)
			}

			fgaClient := new(MockFgaClient)
			call := fgaClient.On("BatchCheck", mock.Anything, mock.Anything).
				Return(&openfga.BatchCheckResponse{Result: &map[string]openfga.BatchCheckSingleResult{
					"1": {Allowed: openfga.PtrBool(true)},
				}}, nil)
			if tt.revokeInCheck {
				call.Run(func(mock.Arguments) { revoke(kv)() })
			}
			service := newFgaService(fgaClient, kv, true)

			result, err := service.CheckRelationships(ctx, []client.ClientCheckRequest{tuple})
			require.NoError(t, err, "the guard must never fail the check")
			assert.Equal(t, "project:example#viewer@user:example-user\ttrue", string(result))

			if tt.wantNoRelWrites {
				assert.Zero(t, kv.relEntries(), "no write-back is left when the marker cannot be re-read")
				return
			}
			lastInvalidation, err := service.cache.getLastInvalidation(ctx)
			require.NoError(t, err)
			outcome := service.cache.lookupEntry(ctx,
				client.ClientBatchCheckItem{User: tuple.User, Relation: tuple.Relation, Object: tuple.Object},
				lastInvalidation)
			cachedAllowed := outcome.kind == cacheOutcomeHit && strings.HasSuffix(string(outcome.hitLine), "\ttrue\n")
			assert.Equal(t, tt.wantCached, cachedAllowed, "cached allowed")
		})
	}
}

func TestCacheLayerRecoveryBindErrorIsReturned(t *testing.T) {
	cache := CacheLayer{recovery: &bucketRecovery{bind: func(context.Context) (INatsKeyValue, error) {
		return nil, context.DeadlineExceeded
	}}}

	err := cache.invalidate(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
