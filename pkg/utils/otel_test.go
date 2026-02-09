// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utils

import (
	"context"
	"testing"
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
	if cfg.TracesSampleRatio != 1.0 {
		t.Errorf("expected default TracesSampleRatio 1.0, got %f", cfg.TracesSampleRatio)
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
	t.Setenv("OTEL_TRACES_SAMPLE_RATIO", "0.5")
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
	if cfg.TracesSampleRatio != 0.5 {
		t.Errorf("expected TracesSampleRatio 0.5, got %f", cfg.TracesSampleRatio)
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

// TestSetupOTelSDKWithConfig_AllDisabled verifies that the SDK can be
// initialized successfully when all exporters (traces, metrics, logs) are
// disabled, and that the returned shutdown function works correctly.
func TestSetupOTelSDKWithConfig_AllDisabled(t *testing.T) {
	cfg := OTelConfig{
		ServiceName:       "test-service",
		ServiceVersion:    "1.0.0",
		Protocol:          OTelProtocolGRPC,
		TracesExporter:    OTelExporterNone,
		TracesSampleRatio: 1.0,
		MetricsExporter:   OTelExporterNone,
		LogsExporter:      OTelExporterNone,
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
