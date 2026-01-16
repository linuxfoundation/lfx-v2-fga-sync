// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package logging

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// LogAttrsFromContext extracts trace_id and span_id from the context and returns them as slog attributes.
// Use this to add tracing context to log messages for correlation.
func LogAttrsFromContext(ctx context.Context) []slog.Attr {
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		return nil
	}
	return []slog.Attr{
		slog.String("trace_id", spanCtx.TraceID().String()),
		slog.String("span_id", spanCtx.SpanID().String()),
	}
}

// LogWithContext returns a logger with trace context attributes added.
// Usage: logging.LogWithContext(ctx, slog.Default()).Info("message", "key", "value")
func LogWithContext(ctx context.Context, logger *slog.Logger) *slog.Logger {
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		return logger
	}
	return logger.With(
		slog.String("trace_id", spanCtx.TraceID().String()),
		slog.String("span_id", spanCtx.SpanID().String()),
	)
}
