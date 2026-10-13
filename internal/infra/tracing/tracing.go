// Package tracing provides OpenTelemetry initialization for the RTC Agent server.
//
// Configuration Example (Jaeger with OTLP):
//
// To connect to a Jaeger instance using the OTLP protocol:
//
//	cfg := tracing.Config{
//	    ServiceName:    "rtc-agent",
//	    ServiceVersion: "1.0.0",
//	    Environment:    "production",
//	    Endpoint:       "jaeger:4317",  // Jaeger OTLP gRPC endpoint
//	    Insecure:       false,          // Use TLS in production
//	    SampleRate:     0.1,            // Sample 10% of traces in production
//	}
//
// For local development with Jaeger running on localhost:
//
//	cfg := tracing.Config{
//	    ServiceName:    "rtc-agent",
//	    ServiceVersion: "1.0.0",
//	    Environment:    "development",
//	    Endpoint:       "localhost:4317",
//	    Insecure:       true,           // Disable TLS for local development
//	    SampleRate:     1.0,            // Sample all traces in development
//	}
//
// Jaeger OTLP Endpoint Format:
//   - gRPC: "hostname:4317" (default OTLP gRPC port)
//   - HTTP: "hostname:4318" (default OTLP HTTP port, not used by this package)
//
// For Docker Compose setups, use the service name (e.g., "jaeger:4317").
// For Kubernetes, use the service DNS name (e.g., "jaeger.tracing.svc.cluster.local:4317").
package tracing

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Config holds OpenTelemetry configuration.
type Config struct {
	// ServiceName is the name of the service.
	ServiceName string
	// ServiceVersion is the version of the service (e.g., "1.0.0").
	// Optional but recommended for multi-version deployments and canary releases.
	ServiceVersion string
	// Environment is the deployment environment (e.g., "production", "staging", "development").
	// Optional but helps distinguish traces from different environments.
	Environment string
	// Endpoint is the OTLP endpoint (e.g., "localhost:4317").
	Endpoint string
	// Insecure indicates whether to use an unencrypted connection (development only).
	Insecure bool
	// SampleRate is the trace sampling rate (0.0 - 1.0).
	// Recommended values: 0.1-0.3 for production, 1.0 for development.
	SampleRate float64
}

// Init initializes the OpenTelemetry tracer provider.
// Returns a shutdown function to flush span data on program exit.
func Init(cfg Config) (func(context.Context) error, error) {
	ctx := context.Background()

	// Create OTLP exporter.
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	// Create resource (identifies the service).
	// Include service name, version, and environment for better trace context.
	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironment(cfg.Environment))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	// Create tracer provider.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithExportTimeout(30*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SampleRate),
		)),
	)

	// Set as global tracer provider.
	otel.SetTracerProvider(tp)

	// Set global propagator (supports W3C Trace Context).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Return shutdown function.
	shutdown := func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}

	return shutdown, nil
}
