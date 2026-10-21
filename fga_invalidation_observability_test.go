// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"testing"
	"time"

	openfga "github.com/openfga/go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	. "github.com/openfga/go-sdk/client"
)

// spanStringAttrs returns a span's string attributes by key.
func spanStringAttrs(s sdktrace.ReadOnlySpan) map[string]string {
	out := make(map[string]string)
	for _, kv := range s.Attributes() {
		if kv.Value.Type().String() == "STRING" {
			out[string(kv.Key)] = kv.Value.AsString()
		}
	}
	return out
}

// TestInvalidationCutoffsReportMarkerKind pins which marker is credited for a
// check's cutoff, since the stale-hit log and the lookup span report it.
func TestInvalidationCutoffsReportMarkerKind(t *testing.T) {
	at := func(minutes int) time.Time { return invalidationTestBase.Add(time.Duration(minutes) * time.Minute) }
	item := ClientBatchCheckItem{Object: "meeting:m1", User: "user:a"}
	userKey := userMarker{typ: "meeting", user: "user:a"}

	tests := []struct {
		name     string
		cutoffs  invalidationCutoffs
		item     ClientBatchCheckItem
		wantKind markerKind
		wantAt   func() time.Time
	}{
		{
			name:     "only the global marker",
			cutoffs:  invalidationCutoffs{global: at(5)},
			item:     item,
			wantKind: markerKindGlobal,
			wantAt:   func() time.Time { return at(5) },
		},
		{
			name: "a newer type marker wins over global",
			cutoffs: invalidationCutoffs{
				global: at(5),
				types:  map[string]markerRead{"meeting": {at: at(7)}},
			},
			item:     item,
			wantKind: markerKindType,
			wantAt:   func() time.Time { return at(7) },
		},
		{
			name: "a newer user marker wins over type and global",
			cutoffs: invalidationCutoffs{
				global: at(5),
				types:  map[string]markerRead{"meeting": {at: at(7)}},
				users:  map[userMarker]markerRead{userKey: {at: at(9)}},
			},
			item:     item,
			wantKind: markerKindUser,
			wantAt:   func() time.Time { return at(9) },
		},
		{
			name: "an older user marker does not override a newer type marker",
			cutoffs: invalidationCutoffs{
				global: at(5),
				types:  map[string]markerRead{"meeting": {at: at(9)}},
				users:  map[userMarker]markerRead{userKey: {at: at(7)}},
			},
			item:     item,
			wantKind: markerKindType,
			wantAt:   func() time.Time { return at(9) },
		},
		{
			name: "a tie goes to the broader marker",
			cutoffs: invalidationCutoffs{
				global: at(5),
				types:  map[string]markerRead{"meeting": {at: at(5)}},
			},
			item:     item,
			wantKind: markerKindGlobal,
			wantAt:   func() time.Time { return at(5) },
		},
		{
			name: "a marker for another type or user is ignored",
			cutoffs: invalidationCutoffs{
				global: at(5),
				types:  map[string]markerRead{"vote": {at: at(9)}},
				users:  map[userMarker]markerRead{{typ: "meeting", user: "user:b"}: {at: at(9)}},
			},
			item:     item,
			wantKind: markerKindGlobal,
			wantAt:   func() time.Time { return at(5) },
		},
		{
			name: "an unreadable type marker",
			cutoffs: invalidationCutoffs{
				global: at(5),
				types:  map[string]markerRead{"meeting": {failed: true}},
			},
			item:     item,
			wantKind: markerKindUnreadable,
		},
		{
			name: "an unreadable user marker",
			cutoffs: invalidationCutoffs{
				global: at(5),
				users:  map[userMarker]markerRead{userKey: {failed: true}},
			},
			item:     item,
			wantKind: markerKindUnreadable,
		},
		{
			name:     "a malformed object falls back to global",
			cutoffs:  invalidationCutoffs{global: at(5), types: map[string]markerRead{"meeting": {at: at(9)}}},
			item:     ClientBatchCheckItem{Object: "meeting", User: "user:a"},
			wantKind: markerKindGlobal,
			wantAt:   func() time.Time { return at(5) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cutoffs.cutoffFor(tt.item)
			assert.Equal(t, tt.wantKind, got.kind)
			if tt.wantAt != nil {
				assert.True(t, tt.wantAt().Equal(got.at), "want %v got %v", tt.wantAt(), got.at)
				assert.True(t, got.at.Equal(tt.cutoffs.forTuple(tt.item)), "forTuple must agree with cutoffFor")
			} else {
				assert.True(t, got.at.After(time.Now()), "an unreadable marker must make entries stale")
			}
		})
	}
}

// TestLookupSpanReportsModeAndStaleMarkerKinds drives a batch where each stale
// hit is caused by a different marker kind and checks the lookup span says so,
// along with the active invalidation mode.
func TestLookupSpanReportsModeAndStaleMarkerKinds(t *testing.T) {
	sr := useRecordingTracer(t)

	at := func(minutes int) time.Time { return invalidationTestBase.Add(time.Duration(minutes) * time.Minute) }
	entryKey := func(relationKey string) string {
		return "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
	}

	kv := newFlakyKV()
	// Cached answers, all created at minute 1 except the fresh one.
	kv.PutAt(entryKey("meeting:m1#viewer@user:a"), []byte("true"), at(1))
	kv.PutAt(entryKey("meeting:m2#viewer@user:b"), []byte("true"), at(1))
	kv.PutAt(entryKey("vote:v1#viewer@user:a"), []byte("true"), at(1))
	kv.PutAt(entryKey("project:p1#viewer@user:a"), []byte("true"), at(20))
	// Markers: global at 3, meeting type at 5, (meeting, user:a) at 7.
	kv.PutAt(globalInvalidationKey, []byte("1"), at(3))
	kv.PutAt(typeMarkerKey("meeting"), []byte("1"), at(5))
	kv.PutAt(userMarkerKey("meeting", "user:a"), []byte("1"), at(7))

	fgaClient := new(MockFgaClient)
	fgaClient.
		On("BatchCheck", mock.Anything, mock.Anything).
		Return(&openfga.BatchCheckResponse{
			Result: &map[string]openfga.BatchCheckSingleResult{
				"1": {Allowed: openfga.PtrBool(true)},
				"2": {Allowed: openfga.PtrBool(true)},
				"3": {Allowed: openfga.PtrBool(true)},
			},
		}, nil)

	service := newFgaService(fgaClient, kv, true).withInvalidation(invalidationReadScoped, nil)

	_, err := service.CheckRelationships(context.Background(), []ClientCheckRequest{
		{User: "user:a", Relation: "viewer", Object: "meeting:m1"}, // stale: user marker
		{User: "user:b", Relation: "viewer", Object: "meeting:m2"}, // stale: type marker
		{User: "user:a", Relation: "viewer", Object: "vote:v1"},    // stale: global marker
		{User: "user:a", Relation: "viewer", Object: "project:p1"}, // hit
	})
	require.NoError(t, err)

	lookups := spansNamed(sr, "fga_sync.cache.lookup")
	require.Len(t, lookups, 1)
	assert.Equal(t, map[string]int64{
		"fga_sync.cache.lookup.requested":         4,
		"fga_sync.cache.lookup.hits":              1,
		"fga_sync.cache.lookup.stale_hits":        3,
		"fga_sync.cache.lookup.misses":            0,
		"fga_sync.cache.lookup.stale_hits.global": 1,
		"fga_sync.cache.lookup.stale_hits.type":   1,
		"fga_sync.cache.lookup.stale_hits.user":   1,
	}, spanIntAttrs(lookups[0]), "no unreadable attribute when no marker was unreadable")
	assert.Equal(t, "read-scoped", spanStringAttrs(lookups[0])["fga_sync.cache.invalidation_mode"])
}

// TestLookupSpanReportsUnreadableMarkers covers the unreadable kind: a marker
// read that fails makes the entry stale and is reported as such.
func TestLookupSpanReportsUnreadableMarkers(t *testing.T) {
	sr := useRecordingTracer(t)

	kv := newFlakyKV()
	kv.PutAt("rel."+cacheKeyEncoder.EncodeToString([]byte("meeting:m1#viewer@user:a")), []byte("true"), invalidationTestBase)
	kv.failGet = func(key string) error {
		if key == typeMarkerKey("meeting") {
			return assert.AnError
		}
		return nil
	}

	fgaClient := new(MockFgaClient)
	fgaClient.
		On("BatchCheck", mock.Anything, mock.Anything).
		Return(&openfga.BatchCheckResponse{
			Result: &map[string]openfga.BatchCheckSingleResult{"1": {Allowed: openfga.PtrBool(true)}},
		}, nil)

	service := newFgaService(fgaClient, kv, true).withInvalidation(invalidationScoped, nil)
	_, err := service.CheckRelationships(context.Background(), []ClientCheckRequest{
		{User: "user:a", Relation: "viewer", Object: "meeting:m1"},
	})
	require.NoError(t, err)

	lookups := spansNamed(sr, "fga_sync.cache.lookup")
	require.Len(t, lookups, 1)
	ints := spanIntAttrs(lookups[0])
	assert.Equal(t, int64(1), ints["fga_sync.cache.lookup.stale_hits.unreadable"])
	assert.Equal(t, "scoped", spanStringAttrs(lookups[0])["fga_sync.cache.invalidation_mode"])
}

// TestPlanInvalidationReportsGlobalReason covers every reason a plan can be
// global, since each is recorded on the invalidate span.
func TestPlanInvalidationReportsGlobalReason(t *testing.T) {
	g := platformGraph(t)
	user := "user:a"

	tests := []struct {
		name       string
		graph      *modelGraph
		tuples     []tupleRef
		wantReason string
	}{
		{name: "no graph", graph: nil, tuples: []tupleRef{{user: user, relation: "participant", object: "meeting:m1"}}, wantReason: globalReasonNoGraph},
		{name: "no tuples", graph: g, tuples: nil, wantReason: globalReasonNoTuples},
		{
			name: "conditioned tuple", graph: g, wantReason: globalReasonConditionedTuple,
			tuples: []tupleRef{{user: user, relation: "participant", object: "meeting:m1", conditioned: true}},
		},
		{
			name: "malformed object", graph: g, wantReason: globalReasonMalformedObject,
			tuples: []tupleRef{{user: user, relation: "participant", object: "meeting"}},
		},
		{
			name: "unknown relation", graph: g, wantReason: globalReasonUnknownRelation,
			tuples: []tupleRef{{user: user, relation: "no_such_relation", object: "meeting:m1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := tt.graph.planInvalidation(tt.tuples)
			assert.True(t, plan.global)
			assert.Equal(t, tt.wantReason, plan.reason)
		})
	}

	t.Run("a scoped plan has no reason", func(t *testing.T) {
		plan := g.planInvalidation([]tupleRef{{user: user, relation: "participant", object: "meeting:m1"}})
		assert.False(t, plan.global)
		assert.Empty(t, plan.reason)
	})
}

// TestServiceInvalidationPlanReasonByMode checks the reason recorded when the
// service itself, not the tuples, decides to stay global.
func TestServiceInvalidationPlanReasonByMode(t *testing.T) {
	writes := []ClientTupleKey{{User: "user:a", Relation: "participant", Object: "meeting:m1"}}
	base := newFgaService(new(MockFgaClient), newFlakyKV(), true)

	tests := []struct {
		name       string
		service    FgaService
		wantGlobal bool
		wantReason string
	}{
		{name: "global mode", service: base.withInvalidation(invalidationGlobal, nil), wantGlobal: true, wantReason: globalReasonModeGlobal},
		{name: "read-scoped mode", service: base.withInvalidation(invalidationReadScoped, platformGraph(t)), wantGlobal: true, wantReason: globalReasonModeGlobal},
		{name: "scoped mode without a graph", service: base.withInvalidation(invalidationScoped, nil), wantGlobal: true, wantReason: globalReasonNoGraph},
		{name: "scoped mode with a graph", service: base.withInvalidation(invalidationScoped, platformGraph(t)), wantGlobal: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := tt.service.invalidationPlan(writes, nil)
			assert.Equal(t, tt.wantGlobal, plan.global)
			assert.Equal(t, tt.wantReason, plan.reason)
		})
	}
}

// TestApplyInvalidationSpanAttributes checks the scope, marker count and
// reason on every invalidate span, so dashboards can split global from scoped
// and say why a write was global.
func TestApplyInvalidationSpanAttributes(t *testing.T) {
	ctx := context.Background()

	t.Run("global plans carry scope, one marker and their reason", func(t *testing.T) {
		for _, reason := range []string{
			globalReasonModeGlobal, globalReasonNoGraph, globalReasonConditionedTuple,
			globalReasonMalformedObject, globalReasonUnknownRelation,
		} {
			sr := useRecordingTracer(t)
			require.NoError(t, CacheLayer{bucket: newFlakyKV()}.applyInvalidation(ctx, globalPlanFor(reason)))

			spans := spansNamed(sr, "fga_sync.cache.invalidate")
			require.Len(t, spans, 1, reason)
			assert.Equal(t, map[string]string{
				"fga_sync.cache.invalidate.scope":  "global",
				"fga_sync.cache.invalidate.reason": reason,
			}, spanStringAttrs(spans[0]), reason)
			assert.Equal(t, int64(1), spanIntAttrs(spans[0])["fga_sync.cache.invalidate.markers"], reason)
		}
	})

	t.Run("a plan with no reason is recorded as no_tuples", func(t *testing.T) {
		sr := useRecordingTracer(t)
		require.NoError(t, CacheLayer{bucket: newFlakyKV()}.applyInvalidation(ctx, globalPlan()))

		spans := spansNamed(sr, "fga_sync.cache.invalidate")
		require.Len(t, spans, 1)
		assert.Equal(t, globalReasonNoTuples, spanStringAttrs(spans[0])["fga_sync.cache.invalidate.reason"])
	})

	t.Run("scoped plans carry scope and no reason", func(t *testing.T) {
		sr := useRecordingTracer(t)
		plan := invalidationPlan{types: []string{"vote"}}
		require.NoError(t, CacheLayer{bucket: newFlakyKV()}.applyInvalidation(ctx, plan))

		spans := spansNamed(sr, "fga_sync.cache.invalidate")
		require.Len(t, spans, 1)
		assert.Equal(t, map[string]string{"fga_sync.cache.invalidate.scope": "scoped"}, spanStringAttrs(spans[0]))
	})

	t.Run("a failed scoped write falls back with its own reason", func(t *testing.T) {
		sr := useRecordingTracer(t)
		kv := newFlakyKV()
		kv.failPut = func(key string) error {
			if key == globalInvalidationKey {
				return nil
			}
			return assert.AnError
		}
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, invalidationPlan{types: []string{"vote"}}))

		var scoped, fallback int
		for _, s := range spansNamed(sr, "fga_sync.cache.invalidate") {
			attrs := spanStringAttrs(s)
			switch attrs["fga_sync.cache.invalidate.scope"] {
			case "scoped":
				scoped++
				assert.Equal(t, codes.Error, s.Status().Code)
			case "global":
				fallback++
				assert.Equal(t, globalReasonScopedWriteFailed, attrs["fga_sync.cache.invalidate.reason"])
				assert.NotEqual(t, codes.Error, s.Status().Code)
			}
		}
		assert.Equal(t, 1, scoped)
		assert.Equal(t, 1, fallback)
	})
}
