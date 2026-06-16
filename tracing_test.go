// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"reflect"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/propagation"
)

func TestNatsHeaderCarrier_Get(t *testing.T) {
	tests := []struct {
		name   string
		header natsgo.Header
		key    string
		want   string
	}{
		{
			name:   "existing key",
			header: natsgo.Header{"traceparent": []string{"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}},
			key:    "traceparent",
			want:   "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		},
		{
			name:   "missing key",
			header: natsgo.Header{},
			key:    "traceparent",
			want:   "",
		},
		{
			name:   "nil header",
			header: nil,
			key:    "traceparent",
			want:   "",
		},
		{
			name:   "multi-value key returns first",
			header: natsgo.Header{"key": []string{"value1", "value2"}},
			key:    "key",
			want:   "value1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := natsHeaderCarrier(tt.header)
			got := c.Get(tt.key)
			if got != tt.want {
				t.Errorf("Get(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestNatsHeaderCarrier_Set(t *testing.T) {
	tests := []struct {
		name       string
		header     natsgo.Header
		key        string
		value      string
		wantPanic  bool
		wantHeader natsgo.Header
	}{
		{
			name:       "set on non-nil header",
			header:     natsgo.Header{},
			key:        "traceparent",
			value:      "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
			wantPanic:  false,
			wantHeader: natsgo.Header{"traceparent": []string{"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}},
		},
		{
			name:       "set on nil header does not panic",
			header:     nil,
			key:        "traceparent",
			value:      "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
			wantPanic:  false,
			wantHeader: nil,
		},
		{
			name:       "overwrite existing key",
			header:     natsgo.Header{"key": []string{"old"}},
			key:        "key",
			value:      "new",
			wantPanic:  false,
			wantHeader: natsgo.Header{"key": []string{"new"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := natsHeaderCarrier(tt.header)
			defer func() {
				r := recover()
				if tt.wantPanic && r == nil {
					t.Error("expected panic, got none")
				}
				if !tt.wantPanic && r != nil {
					t.Errorf("unexpected panic: %v", r)
				}
			}()
			c.Set(tt.key, tt.value)
			if !tt.wantPanic {
				if !reflect.DeepEqual(tt.header, tt.wantHeader) {
					t.Errorf("Set(%q, %q) header = %#v, want %#v", tt.key, tt.value, tt.header, tt.wantHeader)
				}
			}
		})
	}
}

func TestNatsHeaderCarrier_Keys(t *testing.T) {
	tests := []struct {
		name      string
		header    natsgo.Header
		wantLen   int
		wantPanic bool
	}{
		{
			name:      "empty header",
			header:    natsgo.Header{},
			wantLen:   0,
			wantPanic: false,
		},
		{
			name:      "nil header",
			header:    nil,
			wantLen:   0,
			wantPanic: false,
		},
		{
			name:      "header with keys",
			header:    natsgo.Header{"key1": []string{"val1"}, "key2": []string{"val2"}},
			wantLen:   2,
			wantPanic: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if tt.wantPanic && r == nil {
					t.Error("expected panic, got none")
				}
				if !tt.wantPanic && r != nil {
					t.Errorf("unexpected panic: %v", r)
				}
			}()
			c := natsHeaderCarrier(tt.header)
			keys := c.Keys()
			if len(keys) != tt.wantLen {
				t.Errorf("Keys() returned %d keys, want %d", len(keys), tt.wantLen)
			}
		})
	}
}

func TestNatsHeaderCarrier_TextMapCarrier(t *testing.T) {
	// Verify that natsHeaderCarrier implements propagation.TextMapCarrier
	var _ propagation.TextMapCarrier = natsHeaderCarrier{}

	// Test that a propagator can use it
	header := natsgo.Header{
		"traceparent": []string{"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
	}
	c := natsHeaderCarrier(header)

	// Verify interface methods work
	if c.Get("traceparent") == "" {
		t.Error("Get should return the traceparent value")
	}

	// Verify Keys works
	keys := c.Keys()
	if len(keys) == 0 {
		t.Error("Keys should return at least one key")
	}

	// Verify Set doesn't panic on non-nil carrier
	c.Set("tracestate", "vendor=value")
	if c.Get("tracestate") != "vendor=value" {
		t.Error("Set and Get should work together")
	}
}
