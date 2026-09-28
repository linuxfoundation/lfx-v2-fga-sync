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
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "*")).
		Return(nil, jetstream.ErrKeyNotFound).Once()
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	first := lookup.get(context.Background(), "project:1", "viewer")
	second := lookup.get(context.Background(), "project:1", "viewer")

	assert.Equal(t, first, second)
	// Two distinct pairs are consulted (the object-scoped marker and the
	// type-wide blanket marker), each memoized, so the second get() call
	// hits the memo for both rather than re-fetching either from the KV.
	kv.AssertNumberOfCalls(t, "Get", 2)
}

func TestInvalidationLookupTreatsErrorAsJustInvalidated(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "viewer")).
		Return(nil, assert.AnError)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "*")).
		Return(nil, assert.AnError)
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	before := time.Now()
	got := lookup.get(context.Background(), "project:1", "viewer")
	after := time.Now()

	assert.False(t, got.Before(before))
	assert.False(t, got.After(after))
}

func TestExpandTypeWidePairs(t *testing.T) {
	tests := []struct {
		name   string
		object string
		want   []invalidationPair
	}{
		{
			name:   "leaf type with no dependents gets only its own blanket marker",
			object: "committee_invite:1",
			want:   []invalidationPair{{object: "committee_invite:*", relation: "*"}},
		},
		{
			// committee's own direct dependents are committee_invite,
			// groupsio_mailing_list, meeting, v1_meeting, vote, survey; the
			// closure then absorbs each of THEIR dependents too (meeting ->
			// meeting_attachment/past_meeting -> past_meeting_attachment,
			// v1_meeting -> v1_past_meeting, vote -> vote_response, survey
			// -> survey_response), since a committee change can ripple
			// through any of those chains.
			name:   "committee write blanket-invalidates its own and every transitive dependent type",
			object: "committee:1",
			want: []invalidationPair{
				{object: "committee:*", relation: "*"},
				{object: "committee_invite:*", relation: "*"},
				{object: "groupsio_mailing_list:*", relation: "*"},
				{object: "meeting:*", relation: "*"},
				{object: "meeting_attachment:*", relation: "*"},
				{object: "past_meeting:*", relation: "*"},
				{object: "past_meeting_attachment:*", relation: "*"},
				{object: "v1_meeting:*", relation: "*"},
				{object: "v1_past_meeting:*", relation: "*"},
				{object: "vote:*", relation: "*"},
				{object: "vote_response:*", relation: "*"},
				{object: "survey:*", relation: "*"},
				{object: "survey_response:*", relation: "*"},
			},
		},
		{
			name:   "b2b_org/project_membership cycle resolves to a finite closure",
			object: "b2b_org:1",
			want: []invalidationPair{
				{object: "b2b_org:*", relation: "*"},
				{object: "project_membership:*", relation: "*"},
				{object: "crowdfunding_initiative:*", relation: "*"},
			},
		},
		{
			name:   "unknown type still gets its own blanket marker",
			object: "user:1",
			want:   []invalidationPair{{object: "user:*", relation: "*"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandTypeWidePairs(tt.object)
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestComputeTypeInvalidationFanoutReachesProjectTransitively(t *testing.T) {
	fanout := computeTypeInvalidationFanout(crossTypeDependents)

	// mentorship_task depends on mentorship_application, which depends on
	// mentorship_program, which depends on project: a three-hop chain that
	// only resolves correctly if the closure keeps iterating past one pass.
	assert.True(t, fanout["project"]["mentorship_task"],
		"project's fanout must transitively reach mentorship_task through mentorship_program and mentorship_application")
	assert.True(t, fanout["project"]["committee_invite"],
		"project's fanout must transitively reach committee_invite through committee")
}

func TestInvalidationLookupAlwaysConsultsTypeWideWildcardMarker(t *testing.T) {
	kv := new(MockNatsKeyValue)
	objectEntry := &MockKeyValueEntry{created: time.Now().Add(-time.Hour)}
	wildcardEntry := &MockKeyValueEntry{created: time.Now()}
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "writer")).
		Return(objectEntry, nil)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "*")).
		Return(wildcardEntry, nil)
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	got := lookup.get(context.Background(), "project:1", "writer")

	assert.Equal(t, wildcardEntry.created, got, "the later blanket marker must win over the object-scoped one")
	kv.AssertNumberOfCalls(t, "Get", 2)
}

func TestInvalidationLookupConsultsWildcardMarkerForAnyRelation(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "viewer")).
		Return(nil, jetstream.ErrKeyNotFound)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "*")).
		Return(nil, jetstream.ErrKeyNotFound)
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	lookup.get(context.Background(), "project:1", "viewer")

	kv.AssertNumberOfCalls(t, "Get", 2)
	kv.AssertCalled(t, "Get", mock.Anything, cachekey.Invalidation("project:*", "*"))
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

	survivingCacheKey := cachekey.Entry("project:resource-1#viewer@user:bob")

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
