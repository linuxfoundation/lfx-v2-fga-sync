// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	. "github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// perKeyErrorKV is a minimal INatsKeyValue implementation that lets a test
// script a distinct outcome (hit / not-found / stale-hit / arbitrary error)
// per cache key, which MockKeyValue's single global SetError cannot express.
type perKeyErrorKV struct {
	entries map[string]jetstream.KeyValueEntry
	errs    map[string]error
}

func (k *perKeyErrorKV) Get(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
	if err, ok := k.errs[key]; ok {
		return nil, err
	}
	if entry, ok := k.entries[key]; ok {
		return entry, nil
	}
	return nil, jetstream.ErrKeyNotFound
}

func (k *perKeyErrorKV) Put(context.Context, string, []byte) (uint64, error) {
	return 1, nil
}

func (k *perKeyErrorKV) PutString(context.Context, string, string) (uint64, error) {
	return 1, nil
}

// fixedEntry is a jetstream.KeyValueEntry with a fixed value/creation time,
// used to script cache hits and stale hits deterministically.
type fixedEntry struct {
	value   []byte
	created time.Time
}

func (e fixedEntry) Bucket() string                  { return "test" }
func (e fixedEntry) Key() string                     { return "" }
func (e fixedEntry) Value() []byte                   { return e.value }
func (e fixedEntry) Revision() uint64                { return 1 }
func (e fixedEntry) Created() time.Time              { return e.created }
func (e fixedEntry) Delta() uint64                   { return 0 }
func (e fixedEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

// TestCheckRelationshipsMixedCacheOutcomes exercises CheckRelationships with
// one tuple of each cache outcome — fresh hit, miss, stale hit, and an
// unexpected cache Get error — to confirm the bounded-concurrency rewrite of
// the cache-lookup loop preserves per-tuple semantics: a fresh hit resolves
// straight from the cache, and everything else (including the previously
// break-the-whole-batch cache-error case) is routed to OpenFGA individually
// instead of dropped or mis-classifying unrelated tuples.
func TestCheckRelationshipsMixedCacheOutcomes(t *testing.T) {
	useCache = true
	t.Cleanup(func() { useCache = false })

	now := time.Now()
	invalidatedBefore := now.Add(-time.Hour)

	hitKey := "rel." + cacheKeyEncoder.EncodeToString([]byte("obj1#viewer@user:userA"))
	staleKey := "rel." + cacheKeyEncoder.EncodeToString([]byte("obj3#viewer@user:userC"))
	errKey := "rel." + cacheKeyEncoder.EncodeToString([]byte("obj4#viewer@user:userD"))
	// obj2 is a deliberate miss: no entry, no configured error.

	kv := &perKeyErrorKV{
		entries: map[string]jetstream.KeyValueEntry{
			"inv":  fixedEntry{value: []byte("1"), created: invalidatedBefore},
			hitKey: fixedEntry{value: []byte("true"), created: now},
			// Created before the invalidation cutoff, so this is a stale hit.
			staleKey: fixedEntry{value: []byte("true"), created: invalidatedBefore.Add(-time.Hour)},
		},
		errs: map[string]error{
			errKey: errors.New("unexpected NATS KV error"),
		},
	}

	fgaClient := new(MockFgaClient)
	fgaClient.
		On("BatchCheck", mock.Anything, mock.Anything).
		Return(&openfga.BatchCheckResponse{
			Result: &map[string]openfga.BatchCheckSingleResult{
				// Correlation IDs are assigned in tuplesToCheck order, which the
				// index-ordered fold-back keeps deterministic: obj2 (miss),
				// obj3 (stale), obj4 (cache error) in that relative order.
				"1": {Allowed: openfga.PtrBool(false)}, // obj2 miss
				"2": {Allowed: openfga.PtrBool(true)},  // obj3 stale hit
				"3": {Allowed: openfga.PtrBool(false)}, // obj4 cache error
			},
		}, nil)

	service := FgaService{client: fgaClient, cacheBucket: kv}

	tuples := []ClientCheckRequest{
		{User: "user:userA", Relation: "viewer", Object: "obj1"},
		{User: "user:userB", Relation: "viewer", Object: "obj2"},
		{User: "user:userC", Relation: "viewer", Object: "obj3"},
		{User: "user:userD", Relation: "viewer", Object: "obj4"},
	}

	result, err := service.CheckRelationships(context.Background(), tuples)
	require.NoError(t, err)

	lines := strings.Split(string(result), "\n")
	got := make(map[string]string, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, "\t", 2)
		require.Len(t, parts, 2, "malformed response line: %q", line)
		got[parts[0]] = parts[1]
	}

	assert.Equal(t, map[string]string{
		"obj1#viewer@user:userA": "true",  // fresh cache hit, never touched OpenFGA
		"obj2#viewer@user:userB": "false", // cache miss, resolved via OpenFGA
		"obj3#viewer@user:userC": "true",  // stale hit, re-resolved via OpenFGA
		"obj4#viewer@user:userD": "false", // cache Get error, routed to OpenFGA as a miss
	}, got)

	fgaClient.AssertExpectations(t)
}

// TestCheckRelationshipsMeetingAccessAllowedAndDenied uses the object/relation
// shapes lfx-v2-query-service actually emits for meeting resources
// (nats_publisher.go points every child resource's AccessCheckObject at the
// parent v1_meeting/v1_past_meeting, never at a child-specific object) to
// confirm both a granted and a denied outcome resolve correctly in the same
// batch. Because query-service dedupes on AccessCheckObject#AccessCheckRelation,
// a single line here effectively stands in for every child resource (e.g. all
// registrants of one meeting) that shares that parent's grant.
func TestCheckRelationshipsMeetingAccessAllowedAndDenied(t *testing.T) {
	useCache = true
	t.Cleanup(func() { useCache = false })

	kv := &perKeyErrorKV{entries: map[string]jetstream.KeyValueEntry{}}

	fgaClient := new(MockFgaClient)
	fgaClient.
		On("BatchCheck", mock.Anything, mock.Anything).
		Return(&openfga.BatchCheckResponse{
			Result: &map[string]openfga.BatchCheckSingleResult{
				// tuplesToCheck order: viewer (granted), recording_viewer (denied).
				"1": {Allowed: openfga.PtrBool(true)},
				"2": {Allowed: openfga.PtrBool(false)},
			},
		}, nil)

	service := FgaService{client: fgaClient, cacheBucket: kv}

	tuples := []ClientCheckRequest{
		// Stands in for a meeting registrant/participant/rsvp: the check is
		// against the parent meeting, and the meeting grants viewer.
		{User: "user:userA", Relation: "viewer", Object: "v1_meeting:79915658043"},
		// Stands in for a past-meeting recording: the parent past_meeting does
		// not grant recording_viewer to this user.
		{User: "user:userB", Relation: "recording_viewer", Object: "v1_past_meeting:79915658043-occ1"},
	}

	result, err := service.CheckRelationships(context.Background(), tuples)
	require.NoError(t, err)

	lines := strings.Split(string(result), "\n")
	got := make(map[string]string, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, "\t", 2)
		require.Len(t, parts, 2, "malformed response line: %q", line)
		got[parts[0]] = parts[1]
	}

	assert.Equal(t, map[string]string{
		"v1_meeting:79915658043#viewer@user:userA":                     "true",
		"v1_past_meeting:79915658043-occ1#recording_viewer@user:userB": "false",
	}, got)

	fgaClient.AssertExpectations(t)
}

// TestCheckRelationshipsRevokedAccessOverridesStaleCache confirms that a
// previously-granted result cached as "true" is not trusted once it is stale:
// OpenFGA re-resolves it and, if access has since been revoked, the fresh
// "false" wins. A meeting-registrant style check always resolves through the
// parent object, so this also covers the case where the parent's grant is
// removed after being cached.
func TestCheckRelationshipsRevokedAccessOverridesStaleCache(t *testing.T) {
	useCache = true
	t.Cleanup(func() { useCache = false })

	now := time.Now()
	invalidatedBefore := now.Add(-time.Hour)
	staleKey := "rel." + cacheKeyEncoder.EncodeToString(
		[]byte("v1_meeting:79915658043#viewer@user:userA"),
	)

	kv := &perKeyErrorKV{
		entries: map[string]jetstream.KeyValueEntry{
			"inv": fixedEntry{value: []byte("1"), created: invalidatedBefore},
			// Cached as allowed before the last invalidation, so it's stale
			// and must be re-checked rather than trusted.
			staleKey: fixedEntry{value: []byte("true"), created: invalidatedBefore.Add(-time.Hour)},
		},
	}

	fgaClient := new(MockFgaClient)
	fgaClient.
		On("BatchCheck", mock.Anything, mock.Anything).
		Return(&openfga.BatchCheckResponse{
			Result: &map[string]openfga.BatchCheckSingleResult{
				"1": {Allowed: openfga.PtrBool(false)}, // access was revoked
			},
		}, nil)

	service := FgaService{client: fgaClient, cacheBucket: kv}

	tuples := []ClientCheckRequest{
		{User: "user:userA", Relation: "viewer", Object: "v1_meeting:79915658043"},
	}

	result, err := service.CheckRelationships(context.Background(), tuples)
	require.NoError(t, err)

	assert.Equal(t, "v1_meeting:79915658043#viewer@user:userA\tfalse", string(result))
	fgaClient.AssertExpectations(t)
}
