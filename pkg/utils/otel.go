// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utils

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/propagators/jaeger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

const (
	// OTelProtocolGRPC configures OTLP exporters to use gRPC protocol.
	OTelProtocolGRPC = "grpc"
	// OTelProtocolHTTP configures OTLP exporters to use HTTP protocol.
	OTelProtocolHTTP = "http"

	// OTelExporterOTLP configures signals to export via OTLP.
	OTelExporterOTLP = "otlp"
	// OTelExporterNone disables exporting for a signal.
	OTelExporterNone = "none"
)

// OTelConfig holds OpenTelemetry configuration options.
type OTelConfig struct {
	// ServiceName is the name of the service for resource identification.
	// Env: OTEL_SERVICE_NAME (default: "lfx-v2-fga-sync")
	ServiceName string
	// ServiceVersion is the version of the service.
	// Env: OTEL_SERVICE_VERSION
	ServiceVersion string
	// Protocol specifies the OTLP protocol to use: "grpc" or "http".
	// Env: OTEL_EXPORTER_OTLP_PROTOCOL (default: "grpc")
	Protocol string
	// Endpoint is the OTLP collector endpoint.
	// For gRPC: typically "localhost:4317"
	// For HTTP: typically "localhost:4318"
	// Env: OTEL_EXPORTER_OTLP_ENDPOINT
	Endpoint string
	// Insecure disables TLS for the connection.
	// Env: OTEL_EXPORTER_OTLP_INSECURE (set to "true" for insecure connections)
	Insecure bool
	// TracesExporter specifies the traces exporter: "otlp" or "none".
	// Env: OTEL_TRACES_EXPORTER (default: "none")
	TracesExporter string
	// TracesSampleRatio specifies the sampling ratio for traces (0.0 to 1.0).
	// A value of 1.0 means all traces are sampled, 0.5 means 50% are sampled.
	// Env: OTEL_TRACES_SAMPLE_RATIO (default: 1.0)
	TracesSampleRatio float64
	// MetricsExporter specifies the metrics exporter: "otlp" or "none".
	// Env: OTEL_METRICS_EXPORTER (default: "none")
	MetricsExporter string
	// LogsExporter specifies the logs exporter: "otlp" or "none".
	// Env: OTEL_LOGS_EXPORTER (default: "none")
	LogsExporter string
	// Propagators specifies the context propagators to use.
	// Comma-separated list of: "tracecontext", "baggage", "jaeger"
	// Env: OTEL_PROPAGATORS (default: "tracecontext,baggage")
	Propagators string
}

// OTelConfigFromEnv creates an OTelConfig from environment variables.
// See OTelConfig struct fields for supported environment variables.
func OTelConfigFromEnv() OTelConfig {
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "lfx-v2-fga-sync"
	}

	serviceVersion := os.Getenv("OTEL_SERVICE_VERSION")

	protocol := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	if protocol == "" {
		protocol = OTelProtocolGRPC
	}

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")

	insecure := os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true"

	tracesExporter := os.Getenv("OTEL_TRACES_EXPORTER")
	if tracesExporter == "" {
		tracesExporter = OTelExporterNone
	}

	metricsExporter := os.Getenv("OTEL_METRICS_EXPORTER")
	if metricsExporter == "" {
		metricsExporter = OTelExporterNone
	}

	logsExporter := os.Getenv("OTEL_LOGS_EXPORTER")
	if logsExporter == "" {
		logsExporter = OTelExporterNone
	}

	propagators := os.Getenv("OTEL_PROPAGATORS")
	if propagators == "" {
		propagators = "tracecontext,baggage"
	}

	tracesSampleRatio := 1.0
	if ratio := os.Getenv("OTEL_TRACES_SAMPLE_RATIO"); ratio != "" {
		if parsed, err := strconv.ParseFloat(ratio, 64); err == nil {
			if parsed >= 0.0 && parsed <= 1.0 {
				tracesSampleRatio = parsed
			} else {
				slog.Warn("OTEL_TRACES_SAMPLE_RATIO must be between 0.0 and 1.0, using default 1.0",
					"provided-value", ratio)
			}
		} else {
			slog.Warn("invalid OTEL_TRACES_SAMPLE_RATIO value, using default 1.0",
				"provided-value", ratio, "error", err)
		}
	}

	slog.With(
		"service-name", serviceName,
		"version", serviceVersion,
		"protocol", protocol,
		"endpoint", endpoint,
		"insecure", insecure,
		"traces-exporter", tracesExporter,
		"traces-sample-ratio", tracesSampleRatio,
		"metrics-exporter", metricsExporter,
		"logs-exporter", logsExporter,
		"propagators", propagators,
	).Debug("OTelConfig")

	return OTelConfig{
		ServiceName:       serviceName,
		ServiceVersion:    serviceVersion,
		Protocol:          protocol,
		Endpoint:          endpoint,
		Insecure:          insecure,
		TracesExporter:    tracesExporter,
		TracesSampleRatio: tracesSampleRatio,
		MetricsExporter:   metricsExporter,
		LogsExporter:      logsExporter,
		Propagators:       propagators,
	}
}

// SetupOTelSDK bootstraps the OpenTelemetry pipeline with OTLP exporters.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func SetupOTelSDK(ctx context.Context) (shutdown func(context.Context) error, err error) {
	return SetupOTelSDKWithConfig(ctx, OTelConfigFromEnv())
}

// SetupOTelSDKWithConfig bootstraps the OpenTelemetry pipeline with the provided configuration.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func SetupOTelSDKWithConfig(ctx context.Context, cfg OTelConfig) (shutdown func(context.Context) error, err error) {
	var shutdownFuncs []func(context.Context) error

	// shutdown calls cleanup functions registered via shutdownFuncs.
	// The errors from the calls are joined.
	// Each registered cleanup will be invoked once.
	shutdown = func(ctx context.Context) error {
		var sErr error
		for _, fn := range shutdownFuncs {
			sErr = errors.Join(sErr, fn(ctx))
		}
		shutdownFuncs = nil
		return sErr
	}

	// handleErr calls shutdown for cleanup and makes sure that all errors are returned.
	handleErr := func(inErr error) {
		err = errors.Join(inErr, shutdown(ctx))
	}

	// Normalize endpoint to include a URL scheme so WithEndpointURL can
	// parse it. Bare IP:port values like "127.0.0.1:4317" cause url.Parse
	// to fail with "first path segment in URL cannot contain colon".
	if cfg.Endpoint != "" {
		cfg.Endpoint = endpointURL(cfg.Endpoint, cfg.Insecure)
	}

	// Create resource with service information.
	res, err := newResource(cfg)
	if err != nil {
		handleErr(err)
		return
	}

	// Set up propagator.
	prop := newPropagator(cfg)
	otel.SetTextMapPropagator(prop)

	// Set up trace provider if enabled.
	if cfg.TracesExporter != OTelExporterNone {
		var tracerProvider *trace.TracerProvider
		tracerProvider, err = newTraceProvider(ctx, cfg, res)
		if err != nil {
			handleErr(err)
			return
		}
		shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
		otel.SetTracerProvider(tracerProvider)
	}

	// Set up metrics provider if enabled.
	if cfg.MetricsExporter != OTelExporterNone {
		var metricsProvider *metric.MeterProvider
		metricsProvider, err = newMetricsProvider(ctx, cfg, res)
		if err != nil {
			handleErr(err)
			return
		}
		shutdownFuncs = append(shutdownFuncs, metricsProvider.Shutdown)
		otel.SetMeterProvider(metricsProvider)
	}

	// Set up logger provider if enabled.
	if cfg.LogsExporter != OTelExporterNone {
		var loggerProvider *log.LoggerProvider
		loggerProvider, err = newLoggerProvider(ctx, cfg, res)
		if err != nil {
			handleErr(err)
			return
		}
		shutdownFuncs = append(shutdownFuncs, loggerProvider.Shutdown)
		global.SetLoggerProvider(loggerProvider)
	}

	return
}

// newResource creates an OpenTelemetry resource with service name and version attributes.
func newResource(cfg OTelConfig) (*resource.Resource, error) {
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
}

// newPropagator creates a composite text map propagator based on the configured propagators.
// Supported propagators: "tracecontext", "baggage", "jaeger"
func newPropagator(cfg OTelConfig) propagation.TextMapPropagator {
	var propagators []propagation.TextMapPropagator

	for _, p := range strings.Split(cfg.Propagators, ",") {
		switch strings.TrimSpace(p) {
		case "tracecontext":
			propagators = append(propagators, propagation.TraceContext{})
		case "baggage":
			propagators = append(propagators, propagation.Baggage{})
		case "jaeger":
			propagators = append(propagators, jaeger.Jaeger{})
		default:
			slog.Warn("unknown propagator, skipping", "propagator", p)
		}
	}

	if len(propagators) == 0 {
		// Default to tracecontext and baggage if no valid propagators configured
		propagators = []propagation.TextMapPropagator{
			propagation.TraceContext{},
			propagation.Baggage{},
		}
	}

	return propagation.NewCompositeTextMapPropagator(propagators...)
}

// endpointURL ensures the endpoint has a URL scheme. The OTel SDK internally
// reads OTEL_EXPORTER_OTLP_ENDPOINT and parses it with url.Parse, which fails
// for bare IP:port values like "127.0.0.1:4317" with "first path segment in
// URL cannot contain colon". Prepending a scheme based on the insecure flag
// produces a valid URL the SDK can parse.
func endpointURL(raw string, insecure bool) string {
	if strings.Contains(raw, "://") {
		return raw
	}
	if insecure {
		return "http://" + raw
	}
	return "https://" + raw
}

// newTraceProvider creates a TracerProvider with an OTLP exporter configured based on the protocol setting.
func newTraceProvider(ctx context.Context, cfg OTelConfig, res *resource.Resource) (*trace.TracerProvider, error) {
	var exporter trace.SpanExporter
	var err error

	if cfg.Protocol == OTelProtocolHTTP {
		opts := []otlptracehttp.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exporter, err = otlptracehttp.New(ctx, opts...)
	} else {
		opts := []otlptracegrpc.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exporter, err = otlptracegrpc.New(ctx, opts...)
	}

	if err != nil {
		return nil, err
	}

	traceProvider := trace.NewTracerProvider(
		trace.WithResource(res),
		trace.WithSampler(trace.TraceIDRatioBased(cfg.TracesSampleRatio)),
		trace.WithBatcher(exporter,
			trace.WithBatchTimeout(time.Second),
		),
	)
	return traceProvider, nil
}

// newMetricsProvider creates a MeterProvider with an OTLP exporter configured based on the protocol setting.
func newMetricsProvider(ctx context.Context, cfg OTelConfig, res *resource.Resource) (*metric.MeterProvider, error) {
	var exporter metric.Exporter
	var err error

	if cfg.Protocol == OTelProtocolHTTP {
		opts := []otlpmetrichttp.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		exporter, err = otlpmetrichttp.New(ctx, opts...)
	} else {
		opts := []otlpmetricgrpc.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		exporter, err = otlpmetricgrpc.New(ctx, opts...)
	}

	if err != nil {
		return nil, err
	}

	metricsProvider := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter,
			metric.WithInterval(30*time.Second),
		)),
	)
	return metricsProvider, nil
}

// newLoggerProvider creates a LoggerProvider with an OTLP exporter configured based on the protocol setting.
func newLoggerProvider(ctx context.Context, cfg OTelConfig, res *resource.Resource) (*log.LoggerProvider, error) {
	var exporter log.Exporter
	var err error

	if cfg.Protocol == OTelProtocolHTTP {
		opts := []otlploghttp.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlploghttp.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlploghttp.WithInsecure())
		}
		exporter, err = otlploghttp.New(ctx, opts...)
	} else {
		opts := []otlploggrpc.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlploggrpc.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlploggrpc.WithInsecure())
		}
		exporter, err = otlploggrpc.New(ctx, opts...)
	}

	if err != nil {
		return nil, err
	}

	loggerProvider := log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(exporter)),
	)
	return loggerProvider, nil
}
