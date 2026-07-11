// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	. "github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/mock"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// fakeStatusErr is a test error that implements fgaStatusCoder, allowing tests
// to simulate OpenFGA SDK errors with a specific HTTP status code.
type fakeStatusErr struct{ code int }

func (e fakeStatusErr) Error() string           { return fmt.Sprintf("HTTP %d", e.code) }
func (e fakeStatusErr) ResponseStatusCode() int { return e.code }

func newOtelTracer(t *testing.T) (trace.Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("test"), sr
}

func hasException(spans []sdktrace.ReadOnlySpan) bool {
	for _, s := range spans {
		for _, e := range s.Events() {
			if e.Name == "exception" {
				return true
			}
		}
	}
	return false
}

func hasErrorStatus(spans []sdktrace.ReadOnlySpan) bool {
	for _, s := range spans {
		if s.Status().Code == codes.Error {
			return true
		}
	}
	return false
}

// TestFgaService_RecordsErrorOnSpan verifies that each FgaService method calls
// span.RecordError and span.SetStatus(codes.Error) when the underlying FGA
// client returns a non-4xx error, and that 4xx errors are not recorded on the
// span (they represent expected client-side conditions, not server failures).
func TestFgaService_RecordsErrorOnSpan(t *testing.T) {
	tests := []struct {
		name     string
		wantSpan bool // true = expect exception event + error status on span
		setup    func() FgaService
		run      func(ctx context.Context, svc FgaService) error
	}{
		// 5xx SDK errors — span must be marked as errored. Uses fakeStatusErr{code:500}
		// (which implements fgaStatusCoder) to verify that status-coded server errors
		// are not suppressed by the fgaIs4xx guard (which only skips 400–499).
		{
			name:     "ReadObjectTuples/5xx",
			wantSpan: true,
			setup: func() FgaService {
				mc := new(MockFgaClient)
				mc.On("Read", mock.Anything, mock.Anything, mock.Anything).
					Return((*ClientReadResponse)(nil), fakeStatusErr{code: 500})
				return FgaService{client: mc}
			},
			run: func(ctx context.Context, svc FgaService) error {
				_, err := svc.ReadObjectTuples(ctx, "project:123")
				return err
			},
		},
		{
			name:     "ReadUserTuples/5xx",
			wantSpan: true,
			setup: func() FgaService {
				mc := new(MockFgaClient)
				mc.On("Read", mock.Anything, mock.Anything, mock.Anything).
					Return((*ClientReadResponse)(nil), fakeStatusErr{code: 500})
				return FgaService{client: mc}
			},
			run: func(ctx context.Context, svc FgaService) error {
				_, err := svc.ReadUserTuples(ctx, "user:alice", "project")
				return err
			},
		},
		{
			name:     "ListObjectsByUserAndRelation/5xx",
			wantSpan: true,
			setup: func() FgaService {
				mc := new(MockFgaClient)
				mc.On("ListObjects", mock.Anything, mock.Anything, mock.Anything).
					Return((*ClientListObjectsResponse)(nil), fakeStatusErr{code: 500})
				return FgaService{client: mc}
			},
			run: func(ctx context.Context, svc FgaService) error {
				_, err := svc.ListObjectsByUserAndRelation(ctx, "project", "writer", "user:alice")
				return err
			},
		},
		{
			name:     "WriteAndDeleteTuples/5xx",
			wantSpan: true,
			setup: func() FgaService {
				mc := new(MockFgaClient)
				// fakeStatusErr{code:500} is not a validation error, so the retry
				// path is not taken and the error is returned immediately.
				mc.On("Write", mock.Anything, mock.Anything).
					Return((*ClientWriteResponse)(nil), fakeStatusErr{code: 500})
				return FgaService{client: mc}
			},
			run: func(ctx context.Context, svc FgaService) error {
				return svc.WriteAndDeleteTuples(ctx,
					[]ClientTupleKey{{User: "user:alice", Relation: "writer", Object: "project:123"}},
					nil,
				)
			},
		},
		{
			name:     "CheckRelationships/5xx",
			wantSpan: true,
			setup: func() FgaService {
				mc := new(MockFgaClient)
				mc.On("BatchCheck", mock.Anything, mock.Anything).
					Return((*openfga.BatchCheckResponse)(nil), fakeStatusErr{code: 500})
				mockKV := new(MockNatsKeyValue)
				mockKV.On("Get", mock.Anything, "inv").Return(nil, jetstream.ErrKeyNotFound)
				return FgaService{client: mc, cacheBucket: mockKV}
			},
			run: func(ctx context.Context, svc FgaService) error {
				_, err := svc.CheckRelationships(ctx, []ClientCheckRequest{
					{User: "user:alice", Relation: "writer", Object: "project:123"},
				})
				return err
			},
		},
		// 4xx errors — span must NOT be marked as errored (expected client conditions).
		{
			name:     "ReadObjectTuples/4xx",
			wantSpan: false,
			setup: func() FgaService {
				mc := new(MockFgaClient)
				mc.On("Read", mock.Anything, mock.Anything, mock.Anything).
					Return((*ClientReadResponse)(nil), fakeStatusErr{code: 422})
				return FgaService{client: mc}
			},
			run: func(ctx context.Context, svc FgaService) error {
				_, err := svc.ReadObjectTuples(ctx, "project:123")
				return err
			},
		},
		{
			name:     "ReadUserTuples/4xx",
			wantSpan: false,
			setup: func() FgaService {
				mc := new(MockFgaClient)
				mc.On("Read", mock.Anything, mock.Anything, mock.Anything).
					Return((*ClientReadResponse)(nil), fakeStatusErr{code: 422})
				return FgaService{client: mc}
			},
			run: func(ctx context.Context, svc FgaService) error {
				_, err := svc.ReadUserTuples(ctx, "user:alice", "project")
				return err
			},
		},
		{
			name:     "ListObjectsByUserAndRelation/4xx",
			wantSpan: false,
			setup: func() FgaService {
				mc := new(MockFgaClient)
				mc.On("ListObjects", mock.Anything, mock.Anything, mock.Anything).
					Return((*ClientListObjectsResponse)(nil), fakeStatusErr{code: 422})
				return FgaService{client: mc}
			},
			run: func(ctx context.Context, svc FgaService) error {
				_, err := svc.ListObjectsByUserAndRelation(ctx, "project", "writer", "user:alice")
				return err
			},
		},
		{
			name:     "WriteAndDeleteTuples/4xx",
			wantSpan: false,
			setup: func() FgaService {
				mc := new(MockFgaClient)
				mc.On("Write", mock.Anything, mock.Anything).
					Return((*ClientWriteResponse)(nil), fakeStatusErr{code: 422})
				return FgaService{client: mc}
			},
			run: func(ctx context.Context, svc FgaService) error {
				return svc.WriteAndDeleteTuples(ctx,
					[]ClientTupleKey{{User: "user:alice", Relation: "writer", Object: "project:123"}},
					nil,
				)
			},
		},
		{
			name:     "CheckRelationships/4xx",
			wantSpan: false,
			setup: func() FgaService {
				mc := new(MockFgaClient)
				mc.On("BatchCheck", mock.Anything, mock.Anything).
					Return((*openfga.BatchCheckResponse)(nil), fakeStatusErr{code: 422})
				mockKV := new(MockNatsKeyValue)
				mockKV.On("Get", mock.Anything, "inv").Return(nil, jetstream.ErrKeyNotFound)
				return FgaService{client: mc, cacheBucket: mockKV}
			},
			run: func(ctx context.Context, svc FgaService) error {
				_, err := svc.CheckRelationships(ctx, []ClientCheckRequest{
					{User: "user:alice", Relation: "writer", Object: "project:123"},
				})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tracer, sr := newOtelTracer(t)
			ctx, span := tracer.Start(context.Background(), "op")

			svc := tc.setup()
			err := tc.run(ctx, svc)
			span.End()

			if err == nil {
				t.Fatal("expected error")
			}
			if tc.wantSpan {
				if !hasException(sr.Ended()) {
					t.Errorf("RecordError should have been called on the span when %s fails", tc.name)
				}
				if !hasErrorStatus(sr.Ended()) {
					t.Errorf("SetStatus(codes.Error) should have been called on the span when %s fails", tc.name)
				}
			} else {
				if hasException(sr.Ended()) {
					t.Errorf("RecordError must not be called on the span for a 4xx error in %s", tc.name)
				}
				if hasErrorStatus(sr.Ended()) {
					t.Errorf("SetStatus(codes.Error) must not be called on the span for a 4xx error in %s", tc.name)
				}
			}
		})
	}
}
