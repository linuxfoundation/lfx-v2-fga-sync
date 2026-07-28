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

			select {
			case <-cache.writes:
				assert.True(t, tt.wantCacheSeed, "cache was seeded after failed OpenFGA write")
			case <-time.After(100 * time.Millisecond):
				assert.False(t, tt.wantCacheSeed, "cache was not seeded after successful OpenFGA write")
			}
		})
	}
}
