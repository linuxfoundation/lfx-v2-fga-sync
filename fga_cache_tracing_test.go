// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	. "github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// useRecordingTracer swaps the package-level tracer for one backed by a
// SpanRecorder for the duration of the test, so spans started inside
// CacheLayer and FgaService can be inspected. Tests using it must not call
// t.Parallel, since tracer is shared package state.
func useRecordingTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := tracer
	tracer = tp.Tracer("test")
	t.Cleanup(func() {
		tracer = prev
		_ = tp.Shutdown(context.Background())
	})
	return sr
}

// spansNamed returns every ended span with the given name.
func spansNamed(sr *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// spanIntAttrs collects a span's int attributes keyed by attribute name.
func spanIntAttrs(s sdktrace.ReadOnlySpan) map[string]int64 {
	out := make(map[string]int64)
	for _, kv := range s.Attributes() {
		if kv.Value.Type() == attribute.INT64 {
			out[string(kv.Key)] = kv.Value.AsInt64()
		}
	}
	return out
}

// TestCheckRelationshipsTracesCacheLookupAndWriteBack confirms that a
// cache-enabled CheckRelationships batch emits a fga_sync.cache.lookup span
// carrying hit/stale/miss counts and a fga_sync.cache.write_back span
// carrying the number of cache Puts, both nested under the caller's span.
func TestCheckRelationshipsTracesCacheLookupAndWriteBack(t *testing.T) {
	sr := useRecordingTracer(t)

	now := time.Now()
	invalidatedBefore := now.Add(-time.Hour)
	hitKey := "rel." + cacheKeyEncoder.EncodeToString([]byte("obj1#viewer@user:userA"))
	staleKey := "rel." + cacheKeyEncoder.EncodeToString([]byte("obj3#viewer@user:userC"))
	errKey := "rel." + cacheKeyEncoder.EncodeToString([]byte("obj4#viewer@user:userD"))

	kv := &perKeyErrorKV{
		entries: map[string]jetstream.KeyValueEntry{
			"inv":    fixedEntry{value: []byte("1"), created: invalidatedBefore},
			hitKey:   fixedEntry{value: []byte("true"), created: now},
			staleKey: fixedEntry{value: []byte("true"), created: invalidatedBefore.Add(-time.Hour)},
		},
		errs: map[string]error{errKey: errors.New("unexpected NATS KV error")},
	}

	fgaClient := new(MockFgaClient)
	fgaClient.
		On("BatchCheck", mock.Anything, mock.Anything).
		Return(&openfga.BatchCheckResponse{
			Result: &map[string]openfga.BatchCheckSingleResult{
				"1": {Allowed: openfga.PtrBool(false)},
				"2": {Allowed: openfga.PtrBool(true)},
				"3": {Allowed: openfga.PtrBool(false)},
			},
		}, nil)

	service := newFgaService(fgaClient, kv, true)

	ctx, parent := tracer.Start(context.Background(), "parent")
	_, err := service.CheckRelationships(ctx, []ClientCheckRequest{
		{User: "user:userA", Relation: "viewer", Object: "obj1"}, // hit
		{User: "user:userB", Relation: "viewer", Object: "obj2"}, // miss
		{User: "user:userC", Relation: "viewer", Object: "obj3"}, // stale hit
		{User: "user:userD", Relation: "viewer", Object: "obj4"}, // KV error -> miss
	})
	parent.End()
	require.NoError(t, err)

	lookups := spansNamed(sr, "fga_sync.cache.lookup")
	require.Len(t, lookups, 1)
	assert.Equal(t, map[string]int64{
		"fga_sync.cache.lookup.requested":  4,
		"fga_sync.cache.lookup.hits":       1,
		"fga_sync.cache.lookup.stale_hits": 1,
		"fga_sync.cache.lookup.misses":     2,
	}, spanIntAttrs(lookups[0]))
	assert.Equal(t, parent.SpanContext().SpanID(), lookups[0].Parent().SpanID())

	writeBacks := spansNamed(sr, "fga_sync.cache.write_back")
	require.Len(t, writeBacks, 1)
	assert.Equal(t, map[string]int64{"fga_sync.cache.write_back.puts": 3}, spanIntAttrs(writeBacks[0]))
	assert.Equal(t, parent.SpanContext().SpanID(), writeBacks[0].Parent().SpanID())

	fgaClient.AssertExpectations(t)
}

// TestCheckRelationshipsNoLookupSpanWhenCacheDisabled confirms no cache
// lookup span is emitted when USE_CACHE is off, since no lookup happens.
func TestCheckRelationshipsNoLookupSpanWhenCacheDisabled(t *testing.T) {
	sr := useRecordingTracer(t)

	kv := &perKeyErrorKV{entries: map[string]jetstream.KeyValueEntry{}}
	fgaClient := new(MockFgaClient)
	fgaClient.
		On("BatchCheck", mock.Anything, mock.Anything).
		Return(&openfga.BatchCheckResponse{
			Result: &map[string]openfga.BatchCheckSingleResult{
				"1": {Allowed: openfga.PtrBool(true)},
			},
		}, nil)

	service := newFgaService(fgaClient, kv, false)
	_, err := service.CheckRelationships(context.Background(), []ClientCheckRequest{
		{User: "user:userA", Relation: "viewer", Object: "obj1"},
	})
	require.NoError(t, err)

	assert.Empty(t, spansNamed(sr, "fga_sync.cache.lookup"))
}

// TestCacheLayerInvalidateTracesPut covers the fga_sync.cache.invalidate
// span: emitted around the inv Put, marked as errored when the Put fails,
// and skipped entirely when there is no bucket.
func TestCacheLayerInvalidateTracesPut(t *testing.T) {
	tests := []struct {
		name       string
		bucket     func() INatsKeyValue
		wantSpan   bool
		wantErrSet bool
	}{
		{
			name: "successful put",
			bucket: func() INatsKeyValue {
				kv := new(MockNatsKeyValue)
				kv.On("Put", mock.Anything, "inv", []byte("1")).Return(uint64(1), nil)
				return kv
			},
			wantSpan: true,
		},
		{
			name: "failed put",
			bucket: func() INatsKeyValue {
				kv := new(MockNatsKeyValue)
				kv.On("Put", mock.Anything, "inv", []byte("1")).Return(uint64(0), assert.AnError)
				return kv
			},
			wantSpan:   true,
			wantErrSet: true,
		},
		{
			name:   "nil bucket",
			bucket: func() INatsKeyValue { return nil },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr := useRecordingTracer(t)

			err := CacheLayer{bucket: tt.bucket()}.invalidate(context.Background())
			assert.Equal(t, tt.wantErrSet, err != nil)

			spans := spansNamed(sr, "fga_sync.cache.invalidate")
			if !tt.wantSpan {
				assert.Empty(t, spans)
				return
			}
			require.Len(t, spans, 1)
			if tt.wantErrSet {
				assert.Equal(t, codes.Error, spans[0].Status().Code)
			} else {
				assert.NotEqual(t, codes.Error, spans[0].Status().Code)
			}
		})
	}
}

// TestWriteAndDeleteTuplesInvalidateSpanKeepsParent confirms the invalidate
// span stays nested under the caller's span even though invalidation runs on
// a detached (WithoutCancel) context.
func TestWriteAndDeleteTuplesInvalidateSpanKeepsParent(t *testing.T) {
	sr := useRecordingTracer(t)

	fgaClient := new(MockFgaClient)
	fgaClient.
		On("Write", mock.Anything, mock.Anything, mock.Anything).
		Return(&ClientWriteResponse{}, nil)
	kv := new(MockNatsKeyValue)
	kv.On("Put", mock.Anything, "inv", []byte("1")).Return(uint64(1), nil)
	service := newFgaService(fgaClient, kv, false)

	ctx, parent := tracer.Start(context.Background(), "parent")
	_, err := service.WriteAndDeleteTuples(ctx,
		[]ClientTupleKey{{User: "user:alice", Relation: "writer", Object: "project:123"}},
		nil,
	)
	parent.End()
	require.NoError(t, err)

	spans := spansNamed(sr, "fga_sync.cache.invalidate")
	require.Len(t, spans, 1)
	assert.Equal(t, parent.SpanContext().SpanID(), spans[0].Parent().SpanID())
}

// TestCacheLayerSeedPositiveEntriesTracesKeys covers the
// fga_sync.cache.seed span: emitted with the key count when there is work,
// and skipped when there are no keys to seed.
func TestCacheLayerSeedPositiveEntriesTracesKeys(t *testing.T) {
	t.Run("with keys", func(t *testing.T) {
		sr := useRecordingTracer(t)
		cache := &cacheWriteRecorder{writes: make(chan string, 2)}

		CacheLayer{bucket: cache}.seedPositiveEntries(context.Background(), []string{"rel.A", "rel.B"})

		spans := spansNamed(sr, "fga_sync.cache.seed")
		require.Len(t, spans, 1)
		assert.Equal(t, map[string]int64{"fga_sync.cache.seed.keys": 2}, spanIntAttrs(spans[0]))
		assert.Len(t, cache.writes, 2)
	})

	t.Run("no keys", func(t *testing.T) {
		sr := useRecordingTracer(t)

		CacheLayer{bucket: &cacheWriteRecorder{writes: make(chan string)}}.
			seedPositiveEntries(context.Background(), nil)

		assert.Empty(t, spansNamed(sr, "fga_sync.cache.seed"))
	})
}
