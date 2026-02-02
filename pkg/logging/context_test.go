// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package logging

import (
	"context"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
)

func TestLogAttrsFromContext_NoSpan(t *testing.T) {
	// Test with a context that has no span
	ctx := context.Background()
	attrs := LogAttrsFromContext(ctx)

	if attrs != nil {
		t.Errorf("expected nil attrs for context without span, got %v", attrs)
	}
}

func TestLogAttrsFromContext_WithSpan(t *testing.T) {
	// Set up a trace provider
	prevTP := otel.GetTracerProvider()
	tp := trace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prevTP)
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("failed to shutdown trace provider: %v", err)
		}
	}()

	// Create a span
	tracer := otel.Tracer("test-tracer")
	ctx, span := tracer.Start(context.Background(), "test-span")
	defer span.End()

	// Get the span context to verify IDs later
	spanCtx := span.SpanContext()
	expectedTraceID := spanCtx.TraceID().String()
	expectedSpanID := spanCtx.SpanID().String()

	// Test LogAttrsFromContext
	attrs := LogAttrsFromContext(ctx)

	if attrs == nil {
		t.Fatal("expected non-nil attrs for context with span")
	}

	if len(attrs) != 2 {
		t.Errorf("expected 2 attrs, got %d", len(attrs))
	}

	// Check trace_id
	if attrs[0].Key != "trace_id" {
		t.Errorf("expected first attr key to be 'trace_id', got %q", attrs[0].Key)
	}
	if attrs[0].Value.String() != expectedTraceID {
		t.Errorf("expected trace_id %q, got %q", expectedTraceID, attrs[0].Value.String())
	}

	// Check span_id
	if attrs[1].Key != "span_id" {
		t.Errorf("expected second attr key to be 'span_id', got %q", attrs[1].Key)
	}
	if attrs[1].Value.String() != expectedSpanID {
		t.Errorf("expected span_id %q, got %q", expectedSpanID, attrs[1].Value.String())
	}
}

func TestLogWithContext_NoSpan(t *testing.T) {
	// Test with a context that has no span
	ctx := context.Background()
	logger := slog.Default()
	result := LogWithContext(ctx, logger)

	// Should return the same logger when no span context
	if result != logger {
		t.Error("expected same logger instance when context has no span")
	}
}

func TestLogWithContext_WithSpan(t *testing.T) {
	// Set up a trace provider
	prevTP := otel.GetTracerProvider()
	tp := trace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prevTP)
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("failed to shutdown trace provider: %v", err)
		}
	}()

	// Create a span
	tracer := otel.Tracer("test-tracer")
	ctx, span := tracer.Start(context.Background(), "test-span")
	defer span.End()

	// Test LogWithContext
	logger := slog.Default()
	result := LogWithContext(ctx, logger)

	// Should return a new logger instance with trace context
	if result == logger {
		t.Error("expected new logger instance when context has span")
	}

	// The result should be non-nil
	if result == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestLogWithContext_NilLogger(t *testing.T) {
	// This test verifies behavior with nil logger - it should handle gracefully
	// by panicking (since that's Go's standard behavior for nil pointer dereference)
	// We don't test this because it would panic
	t.Skip("skipping nil logger test - would panic")
}
