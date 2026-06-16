// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	natsgo "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

var tracer = otel.Tracer("github.com/linuxfoundation/lfx-v2-fga-sync")

// natsHeaderCarrier adapts nats.Header to propagation.TextMapCarrier.
type natsHeaderCarrier natsgo.Header

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
