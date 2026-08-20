// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
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

// concurrencyTrackingKV is an INatsKeyValue fake whose Get and Put each sleep
// briefly and track their own peak number of calls in flight at once, so a
// test can assert the cache-lookup and cache-write fan-outs actually overlap
// instead of running serially.
type concurrencyTrackingKV struct {
	mu         sync.Mutex
	current    int
	peak       int
	putCurrent int
	putPeak    int
	sleep      time.Duration
	entries    map[string]jetstream.KeyValueEntry
}

func (k *concurrencyTrackingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	k.mu.Lock()
	k.current++
	if k.current > k.peak {
		k.peak = k.current
	}
	k.mu.Unlock()

	select {
	case <-time.After(k.sleep):
	case <-ctx.Done():
	}

	k.mu.Lock()
	k.current--
	k.mu.Unlock()

	if entry, ok := k.entries[key]; ok {
		return entry, nil
	}
	return nil, jetstream.ErrKeyNotFound
}

func (k *concurrencyTrackingKV) Put(ctx context.Context, _ string, _ []byte) (uint64, error) {
	k.mu.Lock()
	k.putCurrent++
	if k.putCurrent > k.putPeak {
		k.putPeak = k.putCurrent
	}
	k.mu.Unlock()

	select {
	case <-time.After(k.sleep):
	case <-ctx.Done():
	}

	k.mu.Lock()
	k.putCurrent--
	k.mu.Unlock()

	return 1, nil
}

func (k *concurrencyTrackingKV) PutString(context.Context, string, string) (uint64, error) {
	return 1, nil
}

// TestCheckRelationshipsBoundsCacheLookupConcurrency proves the cache-lookup
// fan-out in CheckRelationships actually overlaps (not a hidden serial loop
// in disguise) while staying within cacheLookupConcurrency, the invariant the
// bounded-concurrency rewrite depends on.
func TestCheckRelationshipsBoundsCacheLookupConcurrency(t *testing.T) {
	useCache = true
	t.Cleanup(func() { useCache = false })

	const tupleCount = cacheLookupConcurrency * 3

	kv := &concurrencyTrackingKV{
		sleep:   20 * time.Millisecond,
		entries: map[string]jetstream.KeyValueEntry{"inv": nil},
	}
	// "inv" has no fixed entry above (invalidation lookup goes through a
	// separate path); give it a real entry so getLastCacheInvalidation
	// succeeds without needing another fake type.
	kv.entries["inv"] = fixedEntry{value: []byte("1"), created: time.Now().Add(-time.Hour)}

	tuples := make([]ClientCheckRequest, 0, tupleCount)
	expectedResults := make(map[string]openfga.BatchCheckSingleResult, tupleCount)
	for i := range tupleCount {
		user := "user:user" + strconv.Itoa(i)
		tuples = append(tuples, ClientCheckRequest{
			User:     user,
			Relation: "viewer",
			Object:   "obj" + strconv.Itoa(i),
		})
		expectedResults[strconv.Itoa(i+1)] = openfga.BatchCheckSingleResult{Allowed: openfga.PtrBool(true)}
	}

	fgaClient := new(MockFgaClient)
	fgaClient.
		On("BatchCheck", mock.Anything, mock.Anything).
		Return(&openfga.BatchCheckResponse{Result: &expectedResults}, nil)

	service := FgaService{client: fgaClient, cacheBucket: kv}

	start := time.Now()
	result, err := service.CheckRelationships(context.Background(), tuples)
	elapsed := time.Since(start)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(string(result), "\n"), "\n")
	assert.Len(t, lines, tupleCount)

	kv.mu.Lock()
	peak := kv.peak
	putPeak := kv.putPeak
	kv.mu.Unlock()

	assert.Greater(t, peak, 1, "cache lookups ran serially instead of concurrently")
	assert.LessOrEqual(t, peak, cacheLookupConcurrency, "cache lookup concurrency exceeded cacheLookupConcurrency")

	// Every tuple here is a cache miss resolved via OpenFGA, so
	// appendToMessage's write-back fan-out also runs for the full batch;
	// assert it overlaps and stays bounded the same way the lookup fan-out
	// does.
	assert.Greater(t, putPeak, 1, "cache write-backs ran serially instead of concurrently")
	assert.LessOrEqual(t, putPeak, cacheLookupConcurrency, "cache write-back concurrency exceeded cacheLookupConcurrency")

	// A fully serial implementation would take roughly
	// tupleCount * kv.sleep (~%dms); bounded concurrency should finish in a
	// small fraction of that.
	serialEstimate := time.Duration(tupleCount) * kv.sleep
	assert.Less(t, elapsed, serialEstimate/2, "cache lookups took as long as a serial implementation")

	fgaClient.AssertExpectations(t)
}

// TestAppendToMessageBoundsCachePutConcurrency isolates appendToMessage's
// cache write-back fan-out from the cache-lookup fan-out exercised above,
// proving directly that Put calls overlap while staying within
// cacheLookupConcurrency, rather than relying only on the indirect coverage
// from a full CheckRelationships run.
func TestAppendToMessageBoundsCachePutConcurrency(t *testing.T) {
	const tupleCount = cacheLookupConcurrency * 3

	kv := &concurrencyTrackingKV{sleep: 20 * time.Millisecond}
	service := FgaService{cacheBucket: kv}

	result := make(map[string]openfga.BatchCheckSingleResult, tupleCount)
	mapCorrelationIDToTuple := make(map[string]ClientBatchCheckItem, tupleCount)
	for i := range tupleCount {
		correlationID := strconv.Itoa(i)
		result[correlationID] = openfga.BatchCheckSingleResult{Allowed: openfga.PtrBool(true)}
		mapCorrelationIDToTuple[correlationID] = ClientBatchCheckItem{
			User:     "user:user" + correlationID,
			Relation: "viewer",
			Object:   "obj" + correlationID,
		}
	}

	start := time.Now()
	message := service.appendToMessage(context.Background(), nil, result, mapCorrelationIDToTuple)
	elapsed := time.Since(start)

	lines := strings.Split(strings.TrimRight(string(message), "\n"), "\n")
	assert.Len(t, lines, tupleCount)

	kv.mu.Lock()
	putPeak := kv.putPeak
	kv.mu.Unlock()

	assert.Greater(t, putPeak, 1, "cache write-backs ran serially instead of concurrently")
	assert.LessOrEqual(t, putPeak, cacheLookupConcurrency, "cache write-back concurrency exceeded cacheLookupConcurrency")

	serialEstimate := time.Duration(tupleCount) * kv.sleep
	assert.Less(t, elapsed, serialEstimate/2, "cache write-backs took as long as a serial implementation")
}

// TestCheckRelationshipsBoundsServiceWideCacheConcurrency proves cacheOpSem
// actually caps JetStream KV concurrency across concurrent CheckRelationships
// calls (e.g. concurrent NATS handlers), not just within a single request.
// Each of the concurrentRequests calls below fans out cacheLookupConcurrency
// lookups on its own, so without a service-wide bound the observed peak would
// reach concurrentRequests*cacheLookupConcurrency; with it, peak must stay at
// or below cacheOpConcurrency.
func TestCheckRelationshipsBoundsServiceWideCacheConcurrency(t *testing.T) {
	useCache = true
	t.Cleanup(func() { useCache = false })

	const concurrentRequests = 4
	const tupleCount = cacheLookupConcurrency

	kv := &concurrencyTrackingKV{
		sleep:   20 * time.Millisecond,
		entries: map[string]jetstream.KeyValueEntry{"inv": nil},
	}
	kv.entries["inv"] = fixedEntry{value: []byte("1"), created: time.Now().Add(-time.Hour)}

	requestTuples := make([][]ClientCheckRequest, concurrentRequests)
	for r := range concurrentRequests {
		tuples := make([]ClientCheckRequest, 0, tupleCount)
		for i := range tupleCount {
			user := fmt.Sprintf("user:req%d-user%d", r, i)
			object := fmt.Sprintf("obj%d-%d", r, i)
			tuple := ClientCheckRequest{User: user, Relation: "viewer", Object: object}
			tuples = append(tuples, tuple)

			relationKey := object + "#viewer@" + user
			cacheKey := "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
			kv.entries[cacheKey] = fixedEntry{value: []byte("true"), created: time.Now().Add(-time.Minute)}
		}
		requestTuples[r] = tuples
	}

	service := FgaService{cacheBucket: kv}

	var wg sync.WaitGroup
	for r := range concurrentRequests {
		wg.Add(1)
		go func(tuples []ClientCheckRequest) {
			defer wg.Done()
			_, err := service.CheckRelationships(context.Background(), tuples)
			assert.NoError(t, err)
		}(requestTuples[r])
	}
	wg.Wait()

	kv.mu.Lock()
	peak := kv.peak
	kv.mu.Unlock()

	assert.Greater(t, peak, cacheLookupConcurrency,
		"requests did not overlap; assertion below would be vacuous")
	assert.LessOrEqual(t, peak, cacheOpConcurrency,
		"cache lookup concurrency exceeded the service-wide cacheOpConcurrency budget")
}
