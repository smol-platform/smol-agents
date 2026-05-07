// Package observability wires OpenTelemetry tracing, metrics, and logs.
//
// Production exports go to an OTLP gRPC endpoint. Tests can substitute the
// no-op providers by leaving Endpoint empty.
package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config configures Init.
type Config struct {
	ServiceName    string
	ServiceVersion string
	OTLPEndpoint   string
	Environment    string
	Insecure       bool
}

// Shutdown releases all observability providers.
type Shutdown func(context.Context) error

// Init wires global providers and returns a Shutdown. If cfg.OTLPEndpoint
// is empty, providers default to no-ops (suitable for tests).
func Init(ctx context.Context, cfg Config) (Shutdown, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "knative-agent"
	}
	if cfg.OTLPEndpoint == "" {
		// No-op providers are already the package default.
		return func(context.Context) error { return nil }, nil
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			attribute.String("deployment.environment", cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("observability: resource: %w", err)
	}
	traceOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlptracegrpc.WithTimeout(10 * time.Second),
	}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
	}
	traceExp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("observability: trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
	)
	otel.SetTracerProvider(tp)

	metricOpts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlpmetricgrpc.WithTimeout(10 * time.Second),
	}
	if cfg.Insecure {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}
	metricExp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("observability: metric exporter: %w", err)
	}
	mp := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(metricExp, metric.WithInterval(15*time.Second))),
	)
	otel.SetMeterProvider(mp)

	return func(c context.Context) error {
		var firstErr error
		if err := tp.Shutdown(c); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := mp.Shutdown(c); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	}, nil
}

// MustLogger returns a JSON-structured slog.Logger writing to stderr.
func MustLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// JoinShutdown collects multiple Shutdowns into one.
func JoinShutdown(s ...Shutdown) Shutdown {
	return func(ctx context.Context) error {
		var errs []error
		for _, fn := range s {
			if fn == nil {
				continue
			}
			if err := fn(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}
