// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"

	natsgo "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("github.com/linuxfoundation/lfx-v2-fga-sync")

// startConsumerSpan extracts trace context from NATS message headers and
// starts the shared "nats.process" consumer span used by both the core NATS
// subscription path and the JetStream access mutation consumer. baseCtx is
// the context to extract the propagated trace context into; callers pass
// context.Background() when the message handler must not inherit an
// unrelated ambient context (e.g. the core NATS callback), or the service
// context otherwise.
func startConsumerSpan(baseCtx context.Context, headers natsgo.Header, subject string) (context.Context, trace.Span) {
	msgCtx := otel.GetTextMapPropagator().Extract(baseCtx, natsHeaderCarrier(headers))
	return tracer.Start(
		msgCtx,
		"nats.process",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", subject),
			attribute.String("messaging.operation.type", "process"),
		),
	)
}

// natsHeaderCarrier adapts nats.Header to propagation.TextMapCarrier.
type natsHeaderCarrier natsgo.Header

// Get reads a header value, tolerating a nil carrier: reading from a nil Go
// map is well-defined and returns the zero value, so a message with no
// headers (nil nats.Header) safely yields no propagated trace context
// instead of panicking.
func (c natsHeaderCarrier) Get(key string) string {
	vals := c[key]
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func (c natsHeaderCarrier) Set(key, value string) {
	if c == nil {
		// Cannot set on nil carrier; silently drop the value.
		// This matches the behavior of Extract, which safely reads from nil maps.
		return
	}
	c[key] = []string{value}
}

func (c natsHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

var _ propagation.TextMapCarrier = natsHeaderCarrier{}
