// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package utils provides shared utilities for OpenTelemetry and other helpers.
package utils

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// TestOTelConfigFromEnv_Defaults verifies that OTelConfigFromEnv returns
// sensible default values when no environment variables are set.
func TestOTelConfigFromEnv_Defaults(t *testing.T) {
	cfg := OTelConfigFromEnv()

	if cfg.ServiceName != "lfx-v2-fga-sync" {
		t.Errorf("expected default ServiceName 'lfx-v2-fga-sync', got %q", cfg.ServiceName)
	}
	if cfg.ServiceVersion != "" {
		t.Errorf("expected empty ServiceVersion, got %q", cfg.ServiceVersion)
	}
	if cfg.Protocol != OTelProtocolGRPC {
		t.Errorf("expected default Protocol %q, got %q", OTelProtocolGRPC, cfg.Protocol)
	}
	if cfg.Endpoint != "" {
		t.Errorf("expected empty Endpoint, got %q", cfg.Endpoint)
	}
	if cfg.Insecure != false {
		t.Errorf("expected Insecure false, got %t", cfg.Insecure)
	}
	if cfg.TracesExporter != OTelExporterNone {
		t.Errorf("expected default TracesExporter %q, got %q", OTelExporterNone, cfg.TracesExporter)
	}
	if cfg.MetricsExporter != OTelExporterNone {
		t.Errorf("expected default MetricsExporter %q, got %q", OTelExporterNone, cfg.MetricsExporter)
	}
	if cfg.LogsExporter != OTelExporterNone {
		t.Errorf("expected default LogsExporter %q, got %q", OTelExporterNone, cfg.LogsExporter)
	}
}

// TestOTelConfigFromEnv_CustomValues verifies that OTelConfigFromEnv correctly
// reads and parses all supported OTEL_* environment variables.
func TestOTelConfigFromEnv_CustomValues(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "test-service")
	t.Setenv("OTEL_SERVICE_VERSION", "1.2.3")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")

	cfg := OTelConfigFromEnv()

	if cfg.ServiceName != "test-service" {
		t.Errorf("expected ServiceName 'test-service', got %q", cfg.ServiceName)
	}
	if cfg.ServiceVersion != "1.2.3" {
		t.Errorf("expected ServiceVersion '1.2.3', got %q", cfg.ServiceVersion)
	}
	if cfg.Protocol != OTelProtocolHTTP {
		t.Errorf("expected Protocol %q, got %q", OTelProtocolHTTP, cfg.Protocol)
	}
	if cfg.Endpoint != "localhost:4318" {
		t.Errorf("expected Endpoint 'localhost:4318', got %q", cfg.Endpoint)
	}
	if cfg.Insecure != true {
		t.Errorf("expected Insecure true, got %t", cfg.Insecure)
	}
	if cfg.TracesExporter != OTelExporterOTLP {
		t.Errorf("expected TracesExporter %q, got %q", OTelExporterOTLP, cfg.TracesExporter)
	}
	if cfg.MetricsExporter != OTelExporterOTLP {
		t.Errorf("expected MetricsExporter %q, got %q", OTelExporterOTLP, cfg.MetricsExporter)
	}
	if cfg.LogsExporter != OTelExporterOTLP {
		t.Errorf("expected LogsExporter %q, got %q", OTelExporterOTLP, cfg.LogsExporter)
	}
}

// TestOTelConfigFromEnv_UnsupportedProtocol verifies that an unsupported protocol
// value is passed through as-is (defaults to gRPC behavior in the provider functions).
func TestOTelConfigFromEnv_UnsupportedProtocol(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "unsupported")

	cfg := OTelConfigFromEnv()

	if cfg.Protocol != "unsupported" {
		t.Errorf("expected Protocol 'unsupported', got %q", cfg.Protocol)
	}
}

// TestOTelConfigFromEnv_TraceSamplerEnvVars verifies that OTEL_TRACES_SAMPLER
// and OTEL_TRACES_SAMPLER_ARG environment variables are correctly parsed and
// stored in the OTelConfig, with leading/trailing whitespace trimmed.
func TestOTelConfigFromEnv_TraceSamplerEnvVars(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "  parentbased_traceidratio  ")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "  0.5  ")

	cfg := OTelConfigFromEnv()

	if cfg.TracesSampler != "parentbased_traceidratio" {
		t.Errorf("expected TracesSampler 'parentbased_traceidratio' (trimmed), got %q", cfg.TracesSampler)
	}
	if cfg.TracesSamplerArg != "0.5" {
		t.Errorf("expected TracesSamplerArg '0.5' (trimmed), got %q", cfg.TracesSamplerArg)
	}
}

// TestSetupOTelSDKWithConfig_AllDisabled verifies that the SDK can be
// initialized successfully when all exporters (traces, metrics, logs) are
// disabled, and that the returned shutdown function works correctly.
func TestSetupOTelSDKWithConfig_AllDisabled(t *testing.T) {
	cfg := OTelConfig{
		ServiceName:     "test-service",
		ServiceVersion:  "1.0.0",
		Protocol:        OTelProtocolGRPC,
		TracesExporter:  OTelExporterNone,
		MetricsExporter: OTelExporterNone,
		LogsExporter:    OTelExporterNone,
	}

	ctx := context.Background()
	shutdown, err := SetupOTelSDKWithConfig(ctx, cfg)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}

	// Call shutdown to ensure it works without error
	err = shutdown(ctx)
	if err != nil {
		t.Errorf("shutdown returned unexpected error: %v", err)
	}
}

// TestNewResource verifies that newResource creates a valid OpenTelemetry
// resource with the expected service.name attribute for various input values.
func TestNewResource(t *testing.T) {
	tests := []struct {
		name           string
		serviceName    string
		serviceVersion string
	}{
		{"basic", "test-service", "1.0.0"},
		{"empty version", "test-service", ""},
		{"special chars", "test-service-123", "1.0.0-beta.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := OTelConfig{
				ServiceName:    tt.serviceName,
				ServiceVersion: tt.serviceVersion,
			}

			res, err := newResource(cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if res == nil {
				t.Fatal("expected non-nil resource")
			}

			// Verify resource contains expected attributes
			attrs := res.Attributes()
			found := false
			for _, attr := range attrs {
				if string(attr.Key) == "service.name" && attr.Value.AsString() == tt.serviceName {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("resource missing service.name attribute with value %q", tt.serviceName)
			}
		})
	}
}

// TestNewPropagator verifies that newPropagator returns a composite
// TextMapPropagator that includes the standard W3C trace context fields.
func TestNewPropagator(t *testing.T) {
	cfg := OTelConfig{
		Propagators: "tracecontext,baggage",
	}
	prop := newPropagator(cfg)

	if prop == nil {
		t.Fatal("expected non-nil propagator")
	}

	// Verify it's a composite propagator with expected fields
	fields := prop.Fields()
	if len(fields) == 0 {
		t.Error("expected propagator to have fields")
	}

	// Check for expected propagation fields (traceparent, tracestate, baggage)
	expectedFields := map[string]bool{
		"traceparent": false,
		"tracestate":  false,
		"baggage":     false,
	}

	for _, field := range fields {
		expectedFields[field] = true
	}

	for field, found := range expectedFields {
		if !found {
			t.Errorf("expected propagator to include field %q", field)
		}
	}
}

// TestOTelConstants verifies that the exported OTel constants have their
// expected string values, ensuring API compatibility.
func TestOTelConstants(t *testing.T) {
	if OTelProtocolGRPC != "grpc" {
		t.Errorf("expected OTelProtocolGRPC to be 'grpc', got %q", OTelProtocolGRPC)
	}
	if OTelProtocolHTTP != "http" {
		t.Errorf("expected OTelProtocolHTTP to be 'http', got %q", OTelProtocolHTTP)
	}
	if OTelExporterOTLP != "otlp" {
		t.Errorf("expected OTelExporterOTLP to be 'otlp', got %q", OTelExporterOTLP)
	}
	if OTelExporterNone != "none" {
		t.Errorf("expected OTelExporterNone to be 'none', got %q", OTelExporterNone)
	}
}

// TestEndpointURL verifies that endpointURL prepends the correct scheme
// when missing and preserves existing schemes.
func TestEndpointURL(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		insecure bool
		want     string
	}{
		{
			name:     "IP:port insecure",
			raw:      "127.0.0.1:4317",
			insecure: true,
			want:     "http://127.0.0.1:4317",
		},
		{
			name:     "IP:port secure",
			raw:      "127.0.0.1:4317",
			insecure: false,
			want:     "https://127.0.0.1:4317",
		},
		{
			name:     "localhost:port insecure",
			raw:      "localhost:4317",
			insecure: true,
			want:     "http://localhost:4317",
		},
		{
			name:     "hostname without port",
			raw:      "collector",
			insecure: true,
			want:     "http://collector",
		},
		{
			name:     "http URL preserved",
			raw:      "http://collector.example.com:4318",
			insecure: false,
			want:     "http://collector.example.com:4318",
		},
		{
			name:     "https URL preserved",
			raw:      "https://collector.example.com:4318",
			insecure: true,
			want:     "https://collector.example.com:4318",
		},
		{
			name:     "https URL with path preserved",
			raw:      "https://collector.example.com:4318/v1/traces",
			insecure: false,
			want:     "https://collector.example.com:4318/v1/traces",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := endpointURL(tt.raw, tt.insecure)
			if got != tt.want {
				t.Errorf("endpointURL(%q, %t) = %q, want %q", tt.raw, tt.insecure, got, tt.want)
			}
		})
	}
}

// TestSetupOTelSDKWithConfig_IPEndpoint verifies that SetupOTelSDKWithConfig
// normalizes a bare IP:port endpoint to include a scheme, preventing the
// "first path segment in URL cannot contain colon" error from the SDK.
func TestSetupOTelSDKWithConfig_IPEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "127.0.0.1:4317")

	cfg := OTelConfig{
		ServiceName:     "test-service",
		ServiceVersion:  "1.0.0",
		Protocol:        OTelProtocolGRPC,
		Endpoint:        "127.0.0.1:4317",
		Insecure:        true,
		TracesExporter:  OTelExporterOTLP,
		MetricsExporter: OTelExporterNone,
		LogsExporter:    OTelExporterNone,
		Propagators:     "tracecontext,baggage",
	}

	ctx := context.Background()
	shutdown, err := SetupOTelSDKWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}

	_ = shutdown(ctx)
}

// TestNewSampler verifies that newSampler creates correct samplers for all
// supported OTEL_TRACES_SAMPLER values and validates their behavior via
// ShouldSample decisions and parent span context awareness.
func TestNewSampler(t *testing.T) {
	cfg := OTelConfig{}

	tests := []struct {
		name            string
		samplerType     string
		samplerArg      string
		wantParentBased bool // whether sampler should respect parent sampling
		wantNeverSample bool // whether sampler should always drop (always_off)
	}{
		{"default (empty)", "", "", true, false},
		{"always_on", "always_on", "", false, false},
		{"always_off", "always_off", "", false, true},
		{"traceidratio", "traceidratio", "1.0", false, false},
		{"parentbased_always_on", "parentbased_always_on", "", true, false},
		{"parentbased_always_off", "parentbased_always_off", "", true, false},
		{"parentbased_traceidratio", "parentbased_traceidratio", "0.3", true, false},
		{"unknown (defaults to parentbased)", "unknown_sampler", "", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg.TracesSampler = tt.samplerType
			cfg.TracesSamplerArg = tt.samplerArg

			s := newSampler(cfg)
			if s == nil {
				t.Errorf("newSampler returned nil for %q", tt.samplerType)
				return
			}

			if tt.wantParentBased {
				// Validate parent-based behavior: sampler should honor the
				// parent's sampling decision when a remote parent is present.
				if !isParentBasedSampler(t, s) {
					t.Errorf("sampler %q should respect parent span context but doesn't", tt.name)
				}
			} else if tt.wantNeverSample {
				res := s.ShouldSample(trace.SamplingParameters{
					ParentContext: context.Background(),
					TraceID:       oteltrace.TraceID{1},
					Name:          "test",
				})
				if res.Decision != trace.Drop {
					t.Errorf("sampler %q: expected Drop, got %v", tt.name, res.Decision)
				}
			} else {
				// Non-parent-based, always-sample (ratio 1.0): should record.
				res := s.ShouldSample(trace.SamplingParameters{
					ParentContext: context.Background(),
					TraceID:       oteltrace.TraceID{1},
					Name:          "test",
				})
				if res.Decision != trace.RecordAndSample {
					t.Errorf("sampler %q: expected RecordAndSample, got %v", tt.name, res.Decision)
				}
			}
		})
	}
}

// isParentBasedSampler checks if a sampler respects parent sampling decisions
// by testing with a sampled and non-sampled parent context.
func isParentBasedSampler(t *testing.T, sampler trace.Sampler) bool {
	// Create span contexts with sampled and non-sampled flags
	sampledCtx := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    oteltrace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     oteltrace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: oteltrace.TraceFlags(1), // sampled (0x01)
		Remote:     true,
	})

	nonSampledCtx := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    oteltrace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     oteltrace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: oteltrace.TraceFlags(0), // not sampled (0x00)
		Remote:     true,
	})

	sampledParent := oteltrace.ContextWithSpanContext(context.Background(), sampledCtx)
	nonSampledParent := oteltrace.ContextWithSpanContext(context.Background(), nonSampledCtx)

	// For parent-based samplers:
	// - When parent is sampled, decision should be RECORD_AND_SAMPLE
	// - When parent is not sampled, decision should be DROP
	// For non-parent-based samplers, both should make independent decisions.

	sampledResult := sampler.ShouldSample(trace.SamplingParameters{
		ParentContext: sampledParent,
		TraceID:       sampledCtx.TraceID(),
		Name:          "test",
	})
	nonSampledResult := sampler.ShouldSample(trace.SamplingParameters{
		ParentContext: nonSampledParent,
		TraceID:       nonSampledCtx.TraceID(),
		Name:          "test",
	})

	// Parent-based samplers honor parent: sampled parent → sampled span, non-sampled → dropped
	// Non-parent-based samplers may make independent decisions regardless of parent
	return sampledResult.Decision == trace.RecordAndSample && nonSampledResult.Decision == trace.Drop
}

// TestNewSampler_InvalidArg verifies that invalid OTEL_TRACES_SAMPLER_ARG
// values (parse errors and out-of-range) fall back to default ratio of 1.0.
func TestNewSampler_InvalidArg(t *testing.T) {
	tests := []struct {
		name        string
		samplerType string
		samplerArg  string
	}{
		{"non-numeric arg", "parentbased_traceidratio", "invalid"},
		{"out of range (>1.0)", "parentbased_traceidratio", "1.5"},
		{"out of range (<0.0)", "traceidratio", "-0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := OTelConfig{TracesSampler: tt.samplerType, TracesSamplerArg: tt.samplerArg}
			s := newSampler(cfg)
			if s == nil {
				t.Error("newSampler returned nil for invalid arg")
				return
			}

			// Verify fallback behavior: invalid args should fall back to ratio 1.0
			// For non-parent-based samplers, ratio 1.0 means always sample.
			// For parent-based samplers, ratio 1.0 means honor parent (and sample if no parent).
			if tt.samplerType == "traceidratio" {
				// Non-parent-based: should always sample (ratio 1.0)
				res := s.ShouldSample(trace.SamplingParameters{
					ParentContext: context.Background(),
					TraceID:       oteltrace.TraceID{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9},
					Name:          "fallback-check",
				})
				if res.Decision != trace.RecordAndSample {
					t.Errorf("expected fallback ratio 1.0 to RecordAndSample, got %v", res.Decision)
				}
			} else if tt.samplerType == "parentbased_traceidratio" {
				// Parent-based: should honor parent, and with no parent should sample (ratio 1.0 default)
				res := s.ShouldSample(trace.SamplingParameters{
					ParentContext: context.Background(),
					TraceID:       oteltrace.TraceID{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9},
					Name:          "fallback-check",
				})
				// With no parent context and fallback ratio 1.0, should sample
				if res.Decision != trace.RecordAndSample {
					t.Errorf("expected parentbased fallback (ratio 1.0, no parent) to RecordAndSample, got %v", res.Decision)
				}
			}
		})
	}
}
