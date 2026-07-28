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
				On("Write", mock.Anything, mock.Anything).
				Return(&client.ClientWriteResponse{}, tt.writeErr)
			cache := &cacheWriteRecorder{writes: make(chan string, 1)}
			service := FgaService{client: fgaClient, cacheBucket: cache}
			tuple := service.TupleKey("user:alice", "writer", "project:resource-1")

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

			// The positive case waits for the async seeding goroutine, so it uses a
			// longer deadline to avoid false failures on a busy CI worker. The
			// negative case only needs to prove no write ever arrives, so it keeps a
			// short bounded wait.
			deadline := 100 * time.Millisecond
			if tt.wantCacheSeed {
				deadline = 2 * time.Second
			}
			select {
			case <-cache.writes:
				assert.True(t, tt.wantCacheSeed, "cache was seeded after failed OpenFGA write")
			case <-time.After(deadline):
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
		})).
		Return((*client.ClientWriteResponse)(nil), makeValidationError(
			"Invalid tuple 'project:resource-1#writer@user:alice'. Reason: relation 'project#writer' not found",
		)).
		Once()
	// Retry with only bob's viewer grant succeeds.
	fgaClient.
		On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
			return len(req.Writes) == 1 && req.Writes[0].User == "user:bob"
		})).
		Return(&client.ClientWriteResponse{}, nil).
		Once()

	cache := &cacheWriteRecorder{writes: make(chan string, 2)}
	service := FgaService{client: fgaClient, cacheBucket: cache}

	writes := []client.ClientTupleKey{
		service.TupleKey("user:alice", "writer", "project:resource-1"),
		service.TupleKey("user:bob", "viewer", "project:resource-1"),
	}

	_, _, err := service.SyncObjectTuples(context.Background(), "project:resource-1", writes)
	require.NoError(t, err)

	survivingCacheKey := "rel." + cacheKeyEncoder.EncodeToString(
		[]byte("project:resource-1#viewer@user:bob"),
	)

	select {
	case key := <-cache.writes:
		assert.Equal(t, survivingCacheKey, key, "seeded cache key should be for the tuple OpenFGA actually stored")
	case <-time.After(2 * time.Second):
		t.Fatal("expected the surviving tuple's cache entry to be seeded")
	}

	select {
	case key := <-cache.writes:
		t.Fatalf("unexpected second cache write for %q; the skipped invalid tuple must not be seeded", key)
	case <-time.After(200 * time.Millisecond):
		// No further writes: the skipped tuple's cache key was correctly excluded.
	}
}
