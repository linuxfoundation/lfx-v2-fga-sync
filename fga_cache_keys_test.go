// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	"github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cacheKeyGolden mirrors testdata/cache_key_golden.json, which is shared byte
// for byte with lfx-v2-indexer-service.
type cacheKeyGolden struct {
	InvalidationKey string `json:"invalidation_key"`
	Cases           []struct {
		Name     string `json:"name"`
		Object   string `json:"object"`
		Relation string `json:"relation"`
		User     string `json:"user"`
		Want     string `json:"want"`
	} `json:"cases"`
}

// keyRecordingKV records every key passed to Get and Put.
type keyRecordingKV struct {
	mu   sync.Mutex
	gets []string
	puts []string
}

func (k *keyRecordingKV) Get(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gets = append(k.gets, key)
	return nil, jetstream.ErrKeyNotFound
}

func (k *keyRecordingKV) Put(_ context.Context, key string, _ []byte) (uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.puts = append(k.puts, key)
	return 1, nil
}

func (k *keyRecordingKV) PutString(_ context.Context, key, _ string) (uint64, error) {
	return k.Put(context.Background(), key, nil)
}

func loadCacheKeyGolden(t *testing.T) cacheKeyGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/cache_key_golden.json")
	require.NoError(t, err)
	var golden cacheKeyGolden
	require.NoError(t, json.Unmarshal(raw, &golden))
	require.NotEmpty(t, golden.Cases)
	return golden
}

func TestCacheKeyMatchesGolden(t *testing.T) {
	golden := loadCacheKeyGolden(t)

	for _, tc := range golden.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := "rel." + cacheKeyEncoder.EncodeToString([]byte(tc.Object+"#"+tc.Relation+"@"+tc.User))
			assert.Equal(t, tc.Want, got)

			kv := &keyRecordingKV{}
			cache := CacheLayer{bucket: kv}
			tuple := client.ClientBatchCheckItem{Object: tc.Object, Relation: tc.Relation, User: tc.User}

			cache.lookupEntry(context.Background(), tuple, time.Time{})
			assert.Equal(t, []string{tc.Want}, kv.gets, "lookupEntry read key")

			result := map[string]openfga.BatchCheckSingleResult{"1": {Allowed: openfga.PtrBool(true)}}
			cache.buildResponseAndWriteBack(context.Background(), nil, result,
				map[string]client.ClientBatchCheckItem{"1": tuple})
			assert.Equal(t, []string{tc.Want}, kv.puts, "buildResponseAndWriteBack write key")
		})
	}
}

// TestCacheKeyBoundForLongestAcceptedLine tests that every key read or written
// for an accepted check line stays under half of the NATS server's default
// 4096-byte control-line limit, even for a direct get, whose subject carries
// the key.
func TestCacheKeyBoundForLongestAcceptedLine(t *testing.T) {
	const maxKeyBytes = 1643 // "rel." + base32 (no padding) of maxRelationKeyBytes
	directGetPrefix := "$JS.API.DIRECT.GET.KV_" + constants.KVBucketNameSyncCache + "."

	lines := map[string]string{
		"longest ASCII line": "project:" + strings.Repeat("a", 248) + "#" + strings.Repeat("r", 50) +
			"@user:" + strings.Repeat("u", 507),
		"1024-byte line of multi-byte characters": "project:" + strings.Repeat("a", 248) + "#viewers@user:" +
			strings.Repeat("é", 377),
	}

	fgaService := newFgaService(&MockFgaClient{}, nil, false)
	for name, l := range lines {
		t.Run(name, func(t *testing.T) {
			req, err := fgaService.parseCheckRequest([]byte(l))
			require.NoError(t, err, "line must be accepted")

			kv := &keyRecordingKV{}
			cache := CacheLayer{bucket: kv}
			tuple := client.ClientBatchCheckItem{Object: req.Object, Relation: req.Relation, User: req.User}
			cache.lookupEntry(context.Background(), tuple, time.Time{})
			result := map[string]openfga.BatchCheckSingleResult{"1": {Allowed: openfga.PtrBool(true)}}
			cache.buildResponseAndWriteBack(context.Background(), nil, result,
				map[string]client.ClientBatchCheckItem{"1": tuple})

			for _, key := range append(kv.gets, kv.puts...) {
				assert.LessOrEqual(t, len(key), maxKeyBytes)
				assert.Less(t, len(directGetPrefix+key), 4096/2)
			}
		})
	}
}

func TestInvalidationKeyMatchesGolden(t *testing.T) {
	golden := loadCacheKeyGolden(t)

	kv := &keyRecordingKV{}
	require.NoError(t, CacheLayer{bucket: kv}.invalidate(context.Background()))
	assert.Equal(t, []string{golden.InvalidationKey}, kv.puts)

	_, err := CacheLayer{bucket: kv}.getLastInvalidation(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{golden.InvalidationKey}, kv.gets)
}
