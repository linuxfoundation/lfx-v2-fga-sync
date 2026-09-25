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

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/cachekey"
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

func TestCacheLayerInvalidateDedupesPairs(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Put", mock.Anything, mock.AnythingOfType("string"), []byte("1")).Return(uint64(1), nil)
	cache := CacheLayer{bucket: kv}

	pairs := []invalidationPair{
		{object: "project:1", relation: "viewer"},
		{object: "project:1", relation: "viewer"},
		{object: "project:2", relation: "viewer"},
	}
	cache.invalidate(context.Background(), pairs)

	kv.AssertNumberOfCalls(t, "Put", 2)
	kv.AssertCalled(t, "Put", mock.Anything, cachekey.Invalidation("project:1", "viewer"), []byte("1"))
	kv.AssertCalled(t, "Put", mock.Anything, cachekey.Invalidation("project:2", "viewer"), []byte("1"))
}

func TestCacheLayerInvalidateNoOpOnEmptyOrNilBucket(t *testing.T) {
	kv := new(MockNatsKeyValue)
	cache := CacheLayer{bucket: kv}
	cache.invalidate(context.Background(), nil)
	kv.AssertNotCalled(t, "Put", mock.Anything, mock.Anything, mock.Anything)

	nilCache := CacheLayer{}
	nilCache.invalidate(context.Background(), []invalidationPair{{object: "project:1", relation: "viewer"}})
}

func TestInvalidationLookupMemoizesPerPair(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "viewer")).
		Return(nil, jetstream.ErrKeyNotFound).Once()
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	first := lookup.get(context.Background(), "project:1", "viewer")
	second := lookup.get(context.Background(), "project:1", "viewer")

	assert.Equal(t, first, second)
	kv.AssertNumberOfCalls(t, "Get", 1)
}

func TestInvalidationLookupTreatsErrorAsJustInvalidated(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "viewer")).
		Return(nil, assert.AnError)
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	before := time.Now()
	got := lookup.get(context.Background(), "project:1", "viewer")
	after := time.Now()

	assert.False(t, got.Before(before))
	assert.False(t, got.After(after))
}

func TestExpandCascadingPairs(t *testing.T) {
	tests := []struct {
		name     string
		object   string
		relation string
		want     []invalidationPair
	}{
		{
			name:     "cascading project relation gets a wildcard pair",
			object:   "project:1",
			relation: "writer",
			want:     []invalidationPair{{object: "project:*", relation: "writer"}},
		},
		{
			name:     "cascading b2b_org relation gets a wildcard pair",
			object:   "b2b_org:1",
			relation: "auditor",
			want:     []invalidationPair{{object: "b2b_org:*", relation: "auditor"}},
		},
		{
			name:     "non-cascading project relation gets nothing extra",
			object:   "project:1",
			relation: "viewer",
			want:     nil,
		},
		{
			name:     "non-cascading type gets nothing extra",
			object:   "committee:1",
			relation: "writer",
			want:     nil,
		},
		{
			name:     "project parent edge write fans out to every cascading project relation",
			object:   "project:1",
			relation: "parent",
			want: []invalidationPair{
				{object: "project:*", relation: "owner"},
				{object: "project:*", relation: "writer"},
				{object: "project:*", relation: "auditor"},
				{object: "project:*", relation: "marketing_ops"},
				{object: "project:*", relation: "marketing_auditor"},
			},
		},
		{
			name:     "b2b_org child edge write fans out to every cascading b2b_org relation",
			object:   "b2b_org:1",
			relation: "child",
			want: []invalidationPair{
				{object: "b2b_org:*", relation: "writer"},
				{object: "b2b_org:*", relation: "auditor"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandCascadingPairs(tt.object, tt.relation)
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestInvalidationLookupCascadingRelationConsultsWildcardMarker(t *testing.T) {
	kv := new(MockNatsKeyValue)
	objectEntry := &MockKeyValueEntry{created: time.Now().Add(-time.Hour)}
	wildcardEntry := &MockKeyValueEntry{created: time.Now()}
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "writer")).
		Return(objectEntry, nil)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "writer")).
		Return(wildcardEntry, nil)
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	got := lookup.get(context.Background(), "project:1", "writer")

	assert.Equal(t, wildcardEntry.created, got, "the later wildcard marker must win over the object-scoped one")
	kv.AssertNumberOfCalls(t, "Get", 2)
}

func TestInvalidationLookupNonCascadingRelationSkipsWildcardLookup(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "viewer")).
		Return(nil, jetstream.ErrKeyNotFound)
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	lookup.get(context.Background(), "project:1", "viewer")

	kv.AssertNumberOfCalls(t, "Get", 1)
	kv.AssertNotCalled(t, "Get", mock.Anything, cachekey.Invalidation("project:*", "viewer"))
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
